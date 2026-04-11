package nfsadapter

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"

	"netdisk/db"
	"netdisk/utils"
)

// MultiUserHandler 根据挂载路径动态映射不同用户文件系统。
// 支持路径格式：
//   - /users/{username}
//   - /{username}
//
// 当提供 defaultOwnerID 时，/ 会回退到该用户。
type MultiUserHandler struct {
	defaultOwnerID   int64
	requireMountAuth bool
	authMode         mountAuthMode
}

type mountAuthMode string

const (
	mountAuthNone     mountAuthMode = "none"
	mountAuthToken    mountAuthMode = "token"
	mountAuthPassword mountAuthMode = "password"
	mountAuthEither   mountAuthMode = "either"
)

func NewMultiUserHandler(defaultOwnerID int64, requireMountAuth bool, modeRaw string) *MultiUserHandler {
	mode := normalizeMountAuthMode(modeRaw)
	if !requireMountAuth {
		mode = mountAuthNone
	}
	return &MultiUserHandler{defaultOwnerID: defaultOwnerID, requireMountAuth: requireMountAuth, authMode: mode}
}

func (h *MultiUserHandler) Mount(_ context.Context, _ net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	ownerID, err := h.resolveMountRequest(req.Dirpath)
	if err != nil {
		if os.IsNotExist(err) {
			return nfs.MountStatusErrNoEnt, nil, nil
		}
		if os.IsPermission(err) {
			return nfs.MountStatusErrAcces, nil, nil
		}
		return nfs.MountStatusErrServerFault, nil, nil
	}

	return nfs.MountStatusOk, NewNetDiskFS(ownerID), []nfs.AuthFlavor{nfs.AuthFlavorNull, nfs.AuthFlavorUnix}
}

func (h *MultiUserHandler) Change(fs billy.Filesystem) billy.Change {
	if c, ok := fs.(billy.Change); ok {
		return c
	}
	return nil
}

func (h *MultiUserHandler) FSStat(_ context.Context, _ billy.Filesystem, _ *nfs.FSStat) error {
	return nil
}

// ToHandle / FromHandle / InvalidateHandle / HandleLimit 会由 CachingHandler 覆盖。
func (h *MultiUserHandler) ToHandle(_ billy.Filesystem, _ []string) []byte {
	return []byte{}
}

func (h *MultiUserHandler) FromHandle(_ []byte) (billy.Filesystem, []string, error) {
	return nil, []string{}, nil
}

func (h *MultiUserHandler) InvalidateHandle(_ billy.Filesystem, _ []byte) error {
	return nil
}

func (h *MultiUserHandler) HandleLimit() int {
	return -1
}

func (h *MultiUserHandler) resolveMountRequest(dirpath []byte) (int64, error) {
	username, credType, credValue, isRoot, err := parseMountPath(dirpath)
	if err != nil {
		return 0, err
	}
	if isRoot {
		return h.resolveRootOwner()
	}

	if username == "" {
		return 0, os.ErrPermission
	}

	userID, salt, passwordHash, err := h.lookupUser(username)
	if err != nil {
		return 0, err
	}

	if h.requireMountAuth {
		if err := h.verifyCredential(userID, salt, passwordHash, credType, credValue); err != nil {
			return 0, err
		}
	}

	return userID, nil
}

func parseMountPath(dirpath []byte) (username string, credType string, credValue string, isRoot bool, err error) {
	raw := strings.TrimSpace(string(dirpath))
	if raw == "" {
		return "", "", "", true, nil
	}

	clean := path.Clean("/" + raw)
	if clean == "/" {
		return "", "", "", true, nil
	}

	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) == 0 {
		return "", "", "", false, os.ErrPermission
	}

	idx := 0
	switch {
	case len(parts) >= 2 && parts[0] == "users":
		idx = 1
	case len(parts) >= 1:
		idx = 0
	}

	username = strings.TrimSpace(parts[idx])
	username = strings.TrimSpace(username)
	if username == "" || username == "." || username == ".." {
		return "", "", "", false, os.ErrPermission
	}

	remaining := parts[idx+1:]
	if len(remaining) == 0 {
		return username, "", "", false, nil
	}
	if len(remaining) != 2 {
		return "", "", "", false, os.ErrPermission
	}

	credType = strings.ToLower(strings.TrimSpace(remaining[0]))
	if credType != "token" && credType != "password" {
		return "", "", "", false, os.ErrPermission
	}
	decoded, decErr := url.PathUnescape(remaining[1])
	if decErr != nil {
		return "", "", "", false, os.ErrPermission
	}
	credValue = strings.TrimSpace(decoded)
	if credValue == "" {
		return "", "", "", false, os.ErrPermission
	}

	return username, credType, credValue, false, nil
}

func (h *MultiUserHandler) lookupUser(username string) (int64, string, string, error) {
	var userID int64
	var salt string
	var passwordHash string
	err := db.DB.QueryRow("SELECT id, password_salt, password_hash FROM users WHERE username = ? LIMIT 1", username).Scan(&userID, &salt, &passwordHash)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, "", "", os.ErrNotExist
		}
		return 0, "", "", err
	}
	if userID <= 0 {
		return 0, "", "", os.ErrNotExist
	}
	return userID, salt, passwordHash, nil
}

func (h *MultiUserHandler) verifyCredential(userID int64, salt string, passwordHash string, credType string, credValue string) error {
	switch h.authMode {
	case mountAuthToken:
		if credType != "token" {
			return os.ErrPermission
		}
		if err := verifyTokenForUser(userID, credValue); err != nil {
			return err
		}
	case mountAuthPassword:
		if credType != "password" {
			return os.ErrPermission
		}
		if !verifyPassword(credValue, salt, passwordHash) {
			return os.ErrPermission
		}
	case mountAuthEither:
		if credType == "token" {
			if err := verifyTokenForUser(userID, credValue); err != nil {
				return err
			}
		} else if credType == "password" {
			if !verifyPassword(credValue, salt, passwordHash) {
				return os.ErrPermission
			}
		} else {
			return os.ErrPermission
		}
	default:
		return nil
	}

	return nil
}

func (h *MultiUserHandler) resolveRootOwner() (int64, error) {
	if h.requireMountAuth {
		return 0, os.ErrPermission
	}
	if h.defaultOwnerID > 0 {
		return h.defaultOwnerID, nil
	}
	return 0, os.ErrPermission
}

func verifyTokenForUser(userID int64, token string) error {
	var exists int
	err := db.DB.QueryRow(
		`SELECT 1 FROM auth_tokens WHERE token = ? AND user_id = ? AND revoked = 0 AND expires_at_unix > ? LIMIT 1`,
		token,
		userID,
		time.Now().Unix(),
	).Scan(&exists)
	if err != nil {
		if err == sql.ErrNoRows {
			return os.ErrPermission
		}
		return err
	}
	return nil
}

func verifyPassword(password string, salt string, passwordHash string) bool {
	computed := utils.HashPassword(password, salt)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(passwordHash)) == 1
}

func normalizeMountAuthMode(v string) mountAuthMode {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "token":
		return mountAuthToken
	case "password":
		return mountAuthPassword
	case "either":
		return mountAuthEither
	default:
		return mountAuthToken
	}
}

package utils

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"

	"netdisk/models"
)

// WriteJSON 写入 JSON 响应
func WriteJSON(w http.ResponseWriter, status int, body models.APIResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteError 写入错误响应
func WriteError(w http.ResponseWriter, status int, code int, message string) {
	WriteJSON(w, status, models.APIResponse{Code: code, Message: message})
}

// GenerateID 生成 24 位十六进制随机 ID
func GenerateID() (string, error) {
	b := make([]byte, 12) // 12 字节 = 24 位十六进制字符串
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenerateToken 生成 64 位十六进制 token
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HashPassword 使用 salt 对密码进行 SHA256 hash
func HashPassword(password string, salt string) string {
	v := sha256.Sum256([]byte(salt + ":" + password))
	return hex.EncodeToString(v[:])
}

// ParseFileAction 解析文件动作路径
// /api/v1/files/{id}         -> action=""（用于 DELETE）
// /api/v1/files/{id}/download -> action="download"
// /api/v1/files/{id}/rename   -> action="rename"
func ParseFileAction(path string) (id string, action string, err error) {
	const prefix = "/api/v1/files/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", os.ErrNotExist
	}

	rest := strings.TrimPrefix(path, prefix)
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", "", os.ErrNotExist
	}
	if len(parts) == 1 {
		return parts[0], "", nil
	}
	if len(parts) == 2 {
		return parts[0], parts[1], nil
	}
	return "", "", os.ErrNotExist
}

// ParseFolderAction 解析文件夹动作路径
// /api/v1/folders/{id}           -> action=""（用于 DELETE）
// /api/v1/folders/{id}/rename    -> action="rename"
// /api/v1/folders/{id}/children  -> action="children"
// /api/v1/folders/root           -> id=0（根目录特殊处理）
// /api/v1/folders/root/children  -> id=0, action="children"
func ParseFolderAction(path string) (id int64, action string, err error) {
	const prefix = "/api/v1/folders/"
	if !strings.HasPrefix(path, prefix) {
		return 0, "", os.ErrNotExist
	}

	rest := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return 0, "", os.ErrNotExist
	}

	// 特殊处理 root 标识符
	if parts[0] == "root" {
		if len(parts) == 2 {
			return 0, parts[1], nil
		}
		return 0, "", nil
	}

	id, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", os.ErrNotExist
	}

	if len(parts) == 1 {
		return id, "", nil
	}
	if len(parts) == 2 {
		return id, parts[1], nil
	}
	return 0, "", os.ErrNotExist
}

// ParseOptionalInt64 解析可选的 int64 参数
func ParseOptionalInt64(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Int64Value 获取 *int64 的值，nil 时返回 nil
func Int64Value(v *int64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

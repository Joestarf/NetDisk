package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"netdisk/db"
	"netdisk/middleware"
	"netdisk/models"
	"netdisk/utils"
)

type shareView struct {
	ID               int64     `json:"id"`
	Token            string    `json:"token"`
	ShareURL         string    `json:"share_url"`
	NodeType         string    `json:"node_type"`
	NodeID           string    `json:"node_id"`
	Name             string    `json:"name"`
	ExpiresAtUnix    *int64    `json:"expires_at_unix,omitempty"`
	MaxVisits        *int      `json:"max_visits,omitempty"`
	VisitCount       int       `json:"visit_count"`
	RemainingVisits  *int      `json:"remaining_visits,omitempty"`
	RequiresPassword bool      `json:"requires_password"`
	Revoked          bool      `json:"revoked"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// SharesCollectionHandler 创建分享或查询我的分享
func SharesCollectionHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	switch r.Method {
	case http.MethodPost:
		createShareHandler(w, r, user.ID)
	case http.MethodGet:
		items, err := db.ListSharesByOwner(user.ID)
		if err != nil {
			utils.WriteError(w, http.StatusInternalServerError, 10101, "failed to list shares")
			return
		}
		views := make([]shareView, 0, len(items))
		for i := range items {
			views = append(views, toShareView(items[i]))
		}
		utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "ok", Data: views})
	default:
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
	}
}

// ShareItemHandler 删除或撤销分享
func ShareItemHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	shareID, err := parseShareID(r.URL.Path)
	if err != nil {
		utils.WriteError(w, http.StatusNotFound, 10004, "not found")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		if err := db.DeleteShareByID(user.ID, shareID); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				utils.WriteError(w, http.StatusNotFound, 10102, "share not found")
				return
			}
			utils.WriteError(w, http.StatusInternalServerError, 10103, "failed to delete share")
			return
		}
		utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "deleted"})
	case http.MethodPatch:
		if err := db.RevokeShareByID(user.ID, shareID); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				utils.WriteError(w, http.StatusNotFound, 10102, "share not found")
				return
			}
			utils.WriteError(w, http.StatusInternalServerError, 10115, "failed to revoke share")
			return
		}
		utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "revoked"})
	default:
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
	}
}

// PublicShareHandler 公开访问分享
func PublicShareHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	token := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/s/"))
	if token == "" {
		utils.WriteError(w, http.StatusNotFound, 10004, "not found")
		return
	}

	share, err := db.GetShareByToken(token)
	if err != nil {
		utils.WriteError(w, http.StatusNotFound, 10102, "share not found")
		return
	}
	if share.Revoked || isShareExpired(share.ExpiresAtUnix) || isShareExhausted(share.MaxVisits, share.VisitCount) {
		utils.WriteError(w, http.StatusGone, 10104, "share expired or revoked")
		return
	}
	if share.PasswordHash != nil {
		password := strings.TrimSpace(r.URL.Query().Get("password"))
		if password == "" {
			utils.WriteError(w, http.StatusUnauthorized, 10112, "password required")
			return
		}
		if utils.HashPassword(password, token[:16]) != *share.PasswordHash {
			utils.WriteError(w, http.StatusUnauthorized, 10113, "invalid password")
			return
		}
	}

	switch share.NodeType {
	case "file":
		fileRec, err := GetFileRecordForOwner(share.NodeID, share.OwnerID)
		if err != nil {
			utils.WriteError(w, http.StatusNotFound, 10005, "file not found")
			return
		}
		if r.Method == http.MethodGet {
			if err := db.IncrementShareVisitByToken(token); err != nil {
				utils.WriteError(w, http.StatusInternalServerError, 10114, "failed to record share access")
				return
			}
		}
		w.Header().Set("Content-Disposition", "attachment; filename=\""+share.Name+"\"")
		http.ServeFile(w, r, fileRec.DiskPath)
	case "folder":
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		folderID, err := strconv.ParseInt(share.NodeID, 10, 64)
		if err != nil {
			utils.WriteError(w, http.StatusBadRequest, 10105, "invalid folder id")
			return
		}
		items, err := buildSharedFolderTree(share.OwnerID, &folderID)
		if err != nil {
			utils.WriteError(w, http.StatusInternalServerError, 10106, "failed to list shared folder")
			return
		}
		if err := db.IncrementShareVisitByToken(token); err != nil {
			utils.WriteError(w, http.StatusInternalServerError, 10114, "failed to record share access")
			return
		}
		utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "ok", Data: map[string]interface{}{"share": toShareView(*share), "children": items}})
	default:
		utils.WriteError(w, http.StatusBadRequest, 10107, "unsupported share type")
	}
}

func createShareHandler(w http.ResponseWriter, r *http.Request, ownerID int64) {
	type createShareRequest struct {
		NodeType      string `json:"node_type"`
		NodeID        string `json:"node_id"`
		Password      string `json:"password"`
		ExpiresAtUnix *int64 `json:"expires_at_unix"`
		MaxVisits     *int   `json:"max_visits"`
	}

	var req createShareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.NodeType = strings.TrimSpace(req.NodeType)
	req.NodeID = strings.TrimSpace(req.NodeID)
	if req.NodeType != "file" && req.NodeType != "folder" {
		utils.WriteError(w, http.StatusBadRequest, 10108, "invalid node_type")
		return
	}
	if req.NodeID == "" {
		utils.WriteError(w, http.StatusBadRequest, 10109, "missing node_id")
		return
	}
	if req.ExpiresAtUnix != nil && *req.ExpiresAtUnix <= time.Now().Unix() {
		utils.WriteError(w, http.StatusBadRequest, 10116, "expires_at_unix must be in the future")
		return
	}
	if req.MaxVisits != nil && *req.MaxVisits <= 0 {
		utils.WriteError(w, http.StatusBadRequest, 10117, "max_visits must be greater than 0")
		return
	}

	name, err := resolveShareNodeName(ownerID, req.NodeType, req.NodeID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			utils.WriteError(w, http.StatusNotFound, 10005, "node not found")
			return
		}
		utils.WriteError(w, http.StatusInternalServerError, 10110, "failed to resolve node")
		return
	}

	token, err := utils.GenerateToken()
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10111, "failed to create share")
		return
	}

	var passwordHash *string
	if strings.TrimSpace(req.Password) != "" {
		h := utils.HashPassword(req.Password, token[:16])
		passwordHash = &h
	}

	now := time.Now().Unix()
	share := &models.ShareRecord{
		Token:         token,
		OwnerID:       ownerID,
		NodeType:      req.NodeType,
		NodeID:        req.NodeID,
		Name:          name,
		PasswordHash:  passwordHash,
		ExpiresAtUnix: req.ExpiresAtUnix,
		MaxVisits:     req.MaxVisits,
		VisitCount:    0,
		Revoked:       false,
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
	}

	shareID, err := db.CreateShare(share)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10111, "failed to create share")
		return
	}

	utils.WriteJSON(w, http.StatusCreated, models.APIResponse{
		Code:    0,
		Message: "share created",
		Data: toShareView(models.ShareRecord{
			ID:            shareID,
			Token:         token,
			OwnerID:       ownerID,
			NodeType:      req.NodeType,
			NodeID:        req.NodeID,
			Name:          name,
			PasswordHash:  passwordHash,
			ExpiresAtUnix: req.ExpiresAtUnix,
			MaxVisits:     req.MaxVisits,
			VisitCount:    0,
			Revoked:       false,
			CreatedAtUnix: now,
			UpdatedAtUnix: now,
			CreatedAt:     time.Unix(now, 0),
			UpdatedAt:     time.Unix(now, 0),
		}),
	})
}

func parseShareID(path string) (int64, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "shares" {
		return 0, os.ErrNotExist
	}
	shareID, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return 0, os.ErrNotExist
	}
	return shareID, nil
}

func isShareExpired(expiresAtUnix *int64) bool {
	if expiresAtUnix == nil {
		return false
	}
	return time.Now().Unix() > *expiresAtUnix
}

func isShareExhausted(maxVisits *int, visitCount int) bool {
	if maxVisits == nil {
		return false
	}
	return visitCount >= *maxVisits
}

func shareStatus(share models.ShareRecord) string {
	if share.Revoked {
		return "revoked"
	}
	if isShareExpired(share.ExpiresAtUnix) {
		return "expired"
	}
	if isShareExhausted(share.MaxVisits, share.VisitCount) {
		return "exhausted"
	}
	return "active"
}

func remainingVisits(maxVisits *int, visitCount int) *int {
	if maxVisits == nil {
		return nil
	}
	v := *maxVisits - visitCount
	if v < 0 {
		v = 0
	}
	return &v
}

func toShareView(share models.ShareRecord) shareView {
	return shareView{
		ID:               share.ID,
		Token:            share.Token,
		ShareURL:         "/s/" + share.Token,
		NodeType:         share.NodeType,
		NodeID:           share.NodeID,
		Name:             share.Name,
		ExpiresAtUnix:    share.ExpiresAtUnix,
		MaxVisits:        share.MaxVisits,
		VisitCount:       share.VisitCount,
		RemainingVisits:  remainingVisits(share.MaxVisits, share.VisitCount),
		RequiresPassword: share.PasswordHash != nil,
		Revoked:          share.Revoked,
		Status:           shareStatus(share),
		CreatedAt:        share.CreatedAt,
		UpdatedAt:        share.UpdatedAt,
	}
}

func buildSharedFolderTree(ownerID int64, folderID *int64) ([]map[string]interface{}, error) {
	items, err := listChildrenByParent(ownerID, folderID)
	if err != nil {
		return nil, err
	}

	for i := range items {
		if items[i]["type"] != "folder" {
			continue
		}
		childID, ok := items[i]["id"].(int64)
		if !ok {
			return nil, fmt.Errorf("invalid folder id type")
		}
		children, err := buildSharedFolderTree(ownerID, &childID)
		if err != nil {
			return nil, err
		}
		items[i]["children"] = children
	}

	return items, nil
}

func resolveShareNodeName(ownerID int64, nodeType string, nodeID string) (string, error) {
	switch nodeType {
	case "file":
		rec, err := GetFileRecordForOwner(nodeID, ownerID)
		if err != nil {
			return "", err
		}
		return rec.Name, nil
	case "folder":
		folderID, err := strconv.ParseInt(nodeID, 10, 64)
		if err != nil {
			return "", os.ErrNotExist
		}
		rec, err := GetFolderByIDForOwner(ownerID, folderID)
		if err != nil {
			return "", err
		}
		return rec.Name, nil
	default:
		return "", os.ErrNotExist
	}
}

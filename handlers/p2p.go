package handlers

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"netdisk/db"
	"netdisk/middleware"
	"netdisk/models"
	"netdisk/utils"
)

type p2pOfferPayload struct {
	ListenAddr string `json:"listen_addr"`
	FileName   string `json:"file_name"`
	FileSize   int64  `json:"file_size_bytes"`
	FileID     string `json:"file_id,omitempty"`
}

type p2pAnswerPayload struct {
	ReceiverAddr string `json:"receiver_addr"`
}

// P2PSignalMuxHandler 分发 offer/answer 子路由。
func P2PSignalMuxHandler(w http.ResponseWriter, r *http.Request) {
	_, action, err := parseP2PSignalAction(r.URL.Path)
	if err != nil {
		utils.WriteError(w, http.StatusNotFound, 10004, "not found")
		return
	}
	if action == "offer" {
		if r.Method == http.MethodPost {
			middleware.AuthMiddleware(P2PSignalOfferHandler)(w, r)
			return
		}
		P2PSignalOfferHandler(w, r)
		return
	}
	if action == "answer" {
		if r.Method == http.MethodGet {
			middleware.AuthMiddleware(P2PSignalAnswerHandler)(w, r)
			return
		}
		P2PSignalAnswerHandler(w, r)
		return
	}
	utils.WriteError(w, http.StatusNotFound, 10004, "not found")
}

// P2PSignalOfferHandler 处理 offer 的发布与查询。
// POST /api/v1/p2p/signals/{share_token}/offer  (owner + bearer token)
// GET  /api/v1/p2p/signals/{share_token}/offer  (public + optional share password)
func P2PSignalOfferHandler(w http.ResponseWriter, r *http.Request) {
	shareToken, action, err := parseP2PSignalAction(r.URL.Path)
	if err != nil || action != "offer" {
		utils.WriteError(w, http.StatusNotFound, 10004, "not found")
		return
	}

	switch r.Method {
	case http.MethodPost:
		user, ok := middleware.CurrentUser(r)
		if !ok {
			utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
			return
		}
		share, err := db.GetShareByToken(shareToken)
		if err != nil {
			utils.WriteError(w, http.StatusNotFound, 10102, "share not found")
			return
		}
		if share.OwnerID != user.ID {
			utils.WriteError(w, http.StatusForbidden, 10010, "forbidden")
			return
		}
		if share.ShareType != "p2p_file" {
			utils.WriteError(w, http.StatusBadRequest, 10201, "share is not p2p file")
			return
		}
		if share.Revoked || isShareExpired(share.ExpiresAtUnix) || isShareExhausted(share.MaxVisits, share.VisitCount) {
			utils.WriteError(w, http.StatusGone, 10104, "share expired or revoked")
			return
		}

		var req p2pOfferPayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
			return
		}
		req.ListenAddr = strings.TrimSpace(req.ListenAddr)
		req.FileName = strings.TrimSpace(req.FileName)
		req.FileID = strings.TrimSpace(req.FileID)
		if req.ListenAddr == "" {
			utils.WriteError(w, http.StatusBadRequest, 10202, "listen_addr required")
			return
		}
		if req.FileName == "" {
			req.FileName = share.Name
		}
		if req.FileSize < 0 {
			utils.WriteError(w, http.StatusBadRequest, 10203, "file_size_bytes invalid")
			return
		}

		offerEnvelope := map[string]interface{}{
			"listen_addr":     req.ListenAddr,
			"file_name":       req.FileName,
			"file_size_bytes": req.FileSize,
			"file_id":         req.FileID,
			"updated_at_unix": time.Now().Unix(),
			"share_token":     shareToken,
		}
		offerBytes, _ := json.Marshal(offerEnvelope)
		if err := db.UpsertP2POffer(user.ID, shareToken, string(offerBytes)); err != nil {
			utils.WriteError(w, http.StatusInternalServerError, 10204, "failed to publish offer")
			return
		}

		utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "offer published", Data: offerEnvelope})
	case http.MethodGet:
		share, err := db.GetShareByToken(shareToken)
		if err != nil {
			utils.WriteError(w, http.StatusNotFound, 10102, "share not found")
			return
		}
		if share.ShareType != "p2p_file" {
			utils.WriteError(w, http.StatusBadRequest, 10201, "share is not p2p file")
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
			if utils.HashPassword(password, share.Token[:16]) != *share.PasswordHash {
				utils.WriteError(w, http.StatusUnauthorized, 10113, "invalid password")
				return
			}
		}

		offerRaw, err := db.GetP2POffer(shareToken)
		if err != nil {
			utils.WriteError(w, http.StatusNotFound, 10205, "offer not ready")
			return
		}

		if err := db.IncrementShareVisitByToken(shareToken); err != nil {
			utils.WriteError(w, http.StatusInternalServerError, 10114, "failed to record share access")
			return
		}

		var offerObj map[string]interface{}
		if err := json.Unmarshal([]byte(offerRaw), &offerObj); err != nil {
			utils.WriteError(w, http.StatusInternalServerError, 10206, "invalid offer payload")
			return
		}
		utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "ok", Data: map[string]interface{}{
			"share": toShareView(*share),
			"offer": offerObj,
		}})
	default:
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
	}
}

// P2PSignalAnswerHandler 处理 answer 的发布与查询。
// POST /api/v1/p2p/signals/{share_token}/answer (public + optional share password)
// GET  /api/v1/p2p/signals/{share_token}/answer (owner + bearer token)
func P2PSignalAnswerHandler(w http.ResponseWriter, r *http.Request) {
	shareToken, action, err := parseP2PSignalAction(r.URL.Path)
	if err != nil || action != "answer" {
		utils.WriteError(w, http.StatusNotFound, 10004, "not found")
		return
	}

	switch r.Method {
	case http.MethodPost:
		share, err := db.GetShareByToken(shareToken)
		if err != nil {
			utils.WriteError(w, http.StatusNotFound, 10102, "share not found")
			return
		}
		if share.ShareType != "p2p_file" {
			utils.WriteError(w, http.StatusBadRequest, 10201, "share is not p2p file")
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
			if utils.HashPassword(password, share.Token[:16]) != *share.PasswordHash {
				utils.WriteError(w, http.StatusUnauthorized, 10113, "invalid password")
				return
			}
		}

		if _, err := db.GetP2POffer(shareToken); err != nil {
			utils.WriteError(w, http.StatusNotFound, 10205, "offer not ready")
			return
		}

		var req p2pAnswerPayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
			return
		}
		req.ReceiverAddr = strings.TrimSpace(req.ReceiverAddr)
		if req.ReceiverAddr == "" {
			utils.WriteError(w, http.StatusBadRequest, 10207, "receiver_addr required")
			return
		}

		answerEnvelope := map[string]interface{}{
			"receiver_addr":   req.ReceiverAddr,
			"updated_at_unix": time.Now().Unix(),
			"share_token":     shareToken,
		}
		answerBytes, _ := json.Marshal(answerEnvelope)
		if err := db.UpsertP2PAnswer(shareToken, string(answerBytes)); err != nil {
			utils.WriteError(w, http.StatusInternalServerError, 10208, "failed to publish answer")
			return
		}

		utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "answer published", Data: answerEnvelope})
	case http.MethodGet:
		user, ok := middleware.CurrentUser(r)
		if !ok {
			utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
			return
		}
		share, err := db.GetShareByToken(shareToken)
		if err != nil {
			utils.WriteError(w, http.StatusNotFound, 10102, "share not found")
			return
		}
		if share.OwnerID != user.ID {
			utils.WriteError(w, http.StatusForbidden, 10010, "forbidden")
			return
		}
		answerRaw, err := db.GetP2PAnswerForOwner(user.ID, shareToken)
		if err != nil {
			utils.WriteError(w, http.StatusNotFound, 10209, "answer not ready")
			return
		}
		var answerObj map[string]interface{}
		if err := json.Unmarshal([]byte(answerRaw), &answerObj); err != nil {
			utils.WriteError(w, http.StatusInternalServerError, 10210, "invalid answer payload")
			return
		}
		utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "ok", Data: answerObj})
	default:
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
	}
}

func parseP2PSignalAction(path string) (string, string, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 6 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "p2p" || parts[3] != "signals" {
		return "", "", os.ErrNotExist
	}
	shareToken := strings.TrimSpace(parts[4])
	action := strings.TrimSpace(parts[5])
	if shareToken == "" || (action != "offer" && action != "answer") {
		return "", "", os.ErrNotExist
	}
	if len(shareToken) < 16 || len(shareToken) > 128 {
		return "", "", os.ErrNotExist
	}
	return shareToken, action, nil
}

package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"netdisk/db"
	"netdisk/middleware"
	"netdisk/models"
	"netdisk/utils"
)

const tokenTTL = 7 * 24 * time.Hour

// RegisterHandler 用户注册
func RegisterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	type registerRequest struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Password) < 6 {
		utils.WriteError(w, http.StatusBadRequest, 10011, "invalid username or password")
		return
	}

	salt, err := utils.GenerateID()
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to create user")
		return
	}
	hashed := utils.HashPassword(req.Password, salt)

	result, err := db.DB.Exec(
		"INSERT INTO users(username, password_salt, password_hash, created_at_unix) VALUES (?, ?, ?, ?)",
		req.Username,
		salt,
		hashed,
		time.Now().Unix(),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			utils.WriteError(w, http.StatusConflict, 10012, "username already exists")
			return
		}
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to create user")
		return
	}

	uid, _ := result.LastInsertId()
	utils.WriteJSON(w, http.StatusCreated, models.APIResponse{
		Code:    0,
		Message: "registered",
		Data: map[string]interface{}{
			"id":       uid,
			"username": req.Username,
		},
	})
}

// LoginHandler 用户登录
func LoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	type loginRequest struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)

	var userID int64
	var username, salt, passwordHash string
	err := db.DB.QueryRow(
		"SELECT id, username, password_salt, password_hash FROM users WHERE username = ?",
		req.Username,
	).Scan(&userID, &username, &salt, &passwordHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			utils.WriteError(w, http.StatusUnauthorized, 10010, "invalid credentials")
			return
		}
		utils.WriteError(w, http.StatusInternalServerError, 10013, "failed to login")
		return
	}

	if utils.HashPassword(req.Password, salt) != passwordHash {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "invalid credentials")
		return
	}

	token, err := utils.GenerateToken()
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10013, "failed to login")
		return
	}
	expiresAt := time.Now().Add(tokenTTL).Unix()
	_, err = db.DB.Exec(
		"INSERT INTO auth_tokens(token, user_id, expires_at_unix, revoked, created_at_unix) VALUES (?, ?, ?, 0, ?)",
		token,
		userID,
		expiresAt,
		time.Now().Unix(),
	)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10013, "failed to login")
		return
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{
		Code:    0,
		Message: "logged in",
		Data: map[string]interface{}{
			"token":      token,
			"expires_at": time.Unix(expiresAt, 0),
			"user": map[string]interface{}{
				"id":       userID,
				"username": username,
			},
		},
	})
}

// LogoutHandler 用户登出
func LogoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	token, ok := middleware.CurrentToken(r)
	if !ok || token == "" {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	_, err := db.DB.Exec("UPDATE auth_tokens SET revoked = 1 WHERE token = ?", token)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10014, "failed to logout")
		return
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "logged out"})
}

// UserMeHandler 获取当前用户信息
func UserMeHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	switch r.Method {
	case http.MethodGet:
		getUserInfoHandler(w, user)
	case http.MethodPatch:
		updateUserInfoHandler(w, r, user)
	case http.MethodDelete:
		DeleteAccountHandler(w, r)
	default:
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
	}
}

func getUserInfoHandler(w http.ResponseWriter, user models.AuthUser) {
	var username string
	var bio sql.NullString
	err := db.DB.QueryRow("SELECT username, bio FROM users WHERE id = ?", user.ID).Scan(&username, &bio)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
			return
		}
		utils.WriteError(w, http.StatusInternalServerError, 10013, "failed to get user info")
		return
	}

	resp := map[string]interface{}{
		"id":       user.ID,
		"username": username,
	}
	if bio.Valid && strings.TrimSpace(bio.String) != "" {
		resp["bio"] = bio.String
	}
	utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "ok", Data: resp})
}

func updateUserInfoHandler(w http.ResponseWriter, r *http.Request, user models.AuthUser) {
	var req struct {
		Bio *string `json:"bio,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}

	if req.Bio != nil {
		if _, err := db.DB.Exec("UPDATE users SET bio = ? WHERE id = ?", *req.Bio, user.ID); err != nil {
			utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to update bio")
			return
		}
	}

	var username string
	var bio sql.NullString
	err := db.DB.QueryRow("SELECT username, bio FROM users WHERE id = ?", user.ID).Scan(&username, &bio)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
			return
		}
		utils.WriteError(w, http.StatusInternalServerError, 10013, "failed to get user info")
		return
	}

	resp := map[string]interface{}{
		"id":       user.ID,
		"username": username,
	}
	if bio.Valid && strings.TrimSpace(bio.String) != "" {
		resp["bio"] = bio.String
	}
	utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "updated", Data: resp})
}

// UpdatePasswordHandler 修改当前用户密码
func UpdatePasswordHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	if r.Method != http.MethodPatch {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}

	req.OldPassword = strings.TrimSpace(req.OldPassword)
	req.NewPassword = strings.TrimSpace(req.NewPassword)

	if req.OldPassword == "" || req.NewPassword == "" {
		utils.WriteError(w, http.StatusBadRequest, 10011, "invalid password")
		return
	}
	if len(req.NewPassword) < 6 {
		utils.WriteError(w, http.StatusBadRequest, 10011, "password too short")
		return
	}

	var salt, hash string
	err := db.DB.QueryRow(
		"SELECT password_salt, password_hash FROM users WHERE id = ?",
		user.ID,
	).Scan(&salt, &hash)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10013, "failed to verify password")
		return
	}

	if utils.HashPassword(req.OldPassword, salt) != hash {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "invalid old password")
		return
	}

	newSalt, err := utils.GenerateID()
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to update password")
		return
	}
	newHash := utils.HashPassword(req.NewPassword, newSalt)

	_, err = db.DB.Exec(
		"UPDATE users SET password_salt = ?, password_hash = ? WHERE id = ?",
		newSalt,
		newHash,
		user.ID,
	)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to update password")
		return
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "password updated"})
}

// DeleteAccountHandler 删除当前用户账户（需密码确认）
func DeleteAccountHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	if r.Method != http.MethodDelete {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.Password = strings.TrimSpace(req.Password)
	if req.Password == "" {
		utils.WriteError(w, http.StatusBadRequest, 10011, "invalid password")
		return
	}

	var salt, hash string
	err := db.DB.QueryRow(
		"SELECT password_salt, password_hash FROM users WHERE id = ?",
		user.ID,
	).Scan(&salt, &hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
			return
		}
		utils.WriteError(w, http.StatusInternalServerError, 10013, "failed to verify password")
		return
	}

	if utils.HashPassword(req.Password, salt) != hash {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "invalid password")
		return
	}

	if err := db.DeleteUserCascade(user.ID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
			return
		}
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to delete account")
		return
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "account deleted"})
}

package middleware

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"netdisk/db"
	"netdisk/models"
	"netdisk/utils"
)

type contextKey string

const (
	authUserKey  contextKey = "auth_user"
	authTokenKey contextKey = "auth_token"
)

// AuthMiddleware 认证中间件
func AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, token, err := AuthenticateRequest(r)
		if err != nil {
			utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
			return
		}

		ctx := context.WithValue(r.Context(), authUserKey, user)
		ctx = context.WithValue(ctx, authTokenKey, token)
		next(w, r.WithContext(ctx))
	}
}

// CurrentUser 从请求上下文获取当前用户信息
func CurrentUser(r *http.Request) (models.AuthUser, bool) {
	v := r.Context().Value(authUserKey)
	if v == nil {
		return models.AuthUser{}, false
	}
	user, ok := v.(models.AuthUser)
	return user, ok
}

// CurrentToken 从请求上下文获取当前 token
func CurrentToken(r *http.Request) (string, bool) {
	v := r.Context().Value(authTokenKey)
	if v == nil {
		return "", false
	}
	token, ok := v.(string)
	return token, ok
}

// AuthenticateRequest 验证请求的 Bearer token
func AuthenticateRequest(r *http.Request) (models.AuthUser, string, error) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		return models.AuthUser{}, "", errors.New("missing bearer token")
	}

	token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if token == "" {
		return models.AuthUser{}, "", errors.New("empty bearer token")
	}

	var user models.AuthUser
	err := db.DB.QueryRow(
		`SELECT u.id, u.username
		 FROM auth_tokens t
		 JOIN users u ON u.id = t.user_id
		 WHERE t.token = ? AND t.revoked = 0 AND t.expires_at_unix > ?`,
		token,
		time.Now().Unix(),
	).Scan(&user.ID, &user.Username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return models.AuthUser{}, "", errors.New("invalid token")
		}
		return models.AuthUser{}, "", err
	}

	return user, token, nil
}

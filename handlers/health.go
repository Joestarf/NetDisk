package handlers

import (
	"net/http"

	"netdisk/models"
	"netdisk/utils"
)

// HealthHandler 健康检查
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{
		Code:    0,
		Message: "ok",
		Data: map[string]string{
			"status": "up",
		},
	})
}

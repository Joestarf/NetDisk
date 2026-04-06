package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

// apiResponse 定义统一返回结构，后续业务接口也可以复用该格式。
type apiResponse struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func main() {
	// 读取运行端口，未配置时使用默认值 8080。
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// 注册健康检查接口，作为服务存活探针。
	http.HandleFunc("/health", healthHandler)

	log.Printf("server is starting at :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	// 仅允许 GET，避免误用该接口。
	if r.Method != http.MethodGet {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(apiResponse{
			Code:    10001,
			Message: "method not allowed",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(apiResponse{
		Code:    0,
		Message: "ok",
		Data: map[string]string{
			"status": "up",
		},
	})
}

package main

import (
	"log"
	"net/http"
	"os"

	"netdisk/config"
	"netdisk/db"
	"netdisk/handlers"
	"netdisk/middleware"
)

func main() {
	cfg := config.Load()

	if err := os.MkdirAll(cfg.StorageDir, 0o755); err != nil {
		log.Fatalf("failed to init storage dir: %v", err)
	}

	if err := db.Init(cfg.MySQLDSN); err != nil {
		log.Fatalf("failed to init mysql: %v", err)
	}
	defer db.Close()

	if err := db.LoadFilesFromDB(); err != nil {
		log.Fatalf("failed to load file index from mysql: %v", err)
	}

	// 健康检查
	http.HandleFunc("/health", handlers.HealthHandler)

	// 认证接口
	http.HandleFunc("/api/v1/auth/register", handlers.RegisterHandler)
	http.HandleFunc("/api/v1/auth/login", handlers.LoginHandler)
	http.HandleFunc("/api/v1/auth/logout", middleware.AuthMiddleware(handlers.LogoutHandler))
	http.HandleFunc("/api/v1/users/me", middleware.AuthMiddleware(handlers.UserMeHandler))
	http.HandleFunc("/api/v1/users/me/password", middleware.AuthMiddleware(handlers.UpdatePasswordHandler))

	// 文件接口
	http.HandleFunc("/api/v1/files/upload", middleware.AuthMiddleware(handlers.UploadHandler))
	http.HandleFunc("/api/v1/files", middleware.AuthMiddleware(handlers.FilesCollectionHandler))
	http.HandleFunc("/api/v1/files/", middleware.AuthMiddleware(handlers.FileItemHandler))

	// 文件夹接口
	http.HandleFunc("/api/v1/folders", middleware.AuthMiddleware(handlers.FoldersCollectionHandler))
	http.HandleFunc("/api/v1/folders/", middleware.AuthMiddleware(handlers.FolderItemHandler))
	http.HandleFunc("/api/v1/nodes/move", middleware.AuthMiddleware(handlers.MoveNodeHandler))

	// 分享接口
	http.HandleFunc("/api/v1/shares", middleware.AuthMiddleware(handlers.SharesCollectionHandler))
	http.HandleFunc("/api/v1/shares/", middleware.AuthMiddleware(handlers.ShareItemHandler))
	http.HandleFunc("/s/", handlers.PublicShareHandler)

	log.Printf("server is starting at :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

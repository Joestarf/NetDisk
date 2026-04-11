package main

import (
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"netdisk/config"
	"netdisk/db"
	"netdisk/handlers"
	"netdisk/middleware"
	"netdisk/nfsadapter"
	"netdisk/storage"

	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

func initStorage(cfg config.Config) error {
	hasAnyOSSConfig := cfg.OSSEndpoint != "" || cfg.OSSAccessKeyID != "" || cfg.OSSAccessKeySecret != "" || cfg.OSSBucket != ""
	if !hasAnyOSSConfig {
		log.Println("OSS not configured, migration/download-url will use local storage")
		return nil
	}

	if cfg.OSSEndpoint == "" || cfg.OSSAccessKeyID == "" || cfg.OSSAccessKeySecret == "" || cfg.OSSBucket == "" {
		return errors.New("incomplete OSS config: OSS_ENDPOINT/OSS_ACCESS_KEY_ID/OSS_ACCESS_KEY_SECRET/OSS_BUCKET are required")
	}

	ossBackend, err := storage.NewOSSBackend(
		strings.TrimSpace(cfg.OSSEndpoint),
		strings.TrimSpace(cfg.OSSAccessKeyID),
		strings.TrimSpace(cfg.OSSAccessKeySecret),
		strings.TrimSpace(cfg.OSSBucket),
	)
	if err != nil {
		return err
	}

	storage.SetObjectBackend(ossBackend)
	log.Println("OSS backend initialized successfully")
	return nil
}
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
	if err := initStorage(cfg); err != nil {
		log.Fatalf("failed to init storage backends: %v", err)
	}
	startNFSServerFromEnv()
	// 认证接口
	http.HandleFunc("/api/v1/auth/register", handlers.RegisterHandler)
	http.HandleFunc("/api/v1/auth/login", handlers.LoginHandler)
	http.HandleFunc("/api/v1/auth/logout", middleware.AuthMiddleware(handlers.LogoutHandler))
	http.HandleFunc("/api/v1/users/me", middleware.AuthMiddleware(handlers.UserMeHandler))
	http.HandleFunc("/api/v1/users/me/password", middleware.AuthMiddleware(handlers.UpdatePasswordHandler))

	// 文件接口
	http.HandleFunc("/api/v1/files/upload/init", middleware.AuthMiddleware(handlers.ChunkUploadInitHandler))
	http.HandleFunc("/api/v1/files/upload/chunk", middleware.AuthMiddleware(handlers.ChunkUploadPartHandler))
	http.HandleFunc("/api/v1/files/upload/status", middleware.AuthMiddleware(handlers.ChunkUploadStatusHandler))
	http.HandleFunc("/api/v1/files/upload/complete", middleware.AuthMiddleware(handlers.ChunkUploadCompleteHandler))
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

func startNFSServerFromEnv() {
	enabled := strings.TrimSpace(os.Getenv("NFS_ENABLE"))
	if enabled == "" {
		enabled = "1"
	}
	if enabled == "0" || strings.EqualFold(enabled, "false") {
		log.Println("NFS disabled by env NFS_ENABLE")
		return
	}

	ownerID := int64(1)
	if raw := strings.TrimSpace(os.Getenv("NFS_OWNER_ID")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			log.Printf("invalid NFS_OWNER_ID=%q, fallback to 1", raw)
		} else {
			ownerID = parsed
		}
	}

	addr := strings.TrimSpace(os.Getenv("NFS_ADDR"))
	if addr == "" {
		addr = ":2049"
	}

	go func() {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			log.Printf("NFS server failed to listen at %s: %v", addr, err)
			return
		}
		log.Printf("NFS server running at %s (owner_id=%d)", addr, ownerID)

		fs := nfsadapter.NewNetDiskFS(ownerID)
		handler := nfshelper.NewNullAuthHandler(fs)
		cacheHelper := nfshelper.NewCachingHandler(handler, 1024)
		if err := nfs.Serve(listener, cacheHelper); err != nil {
			log.Printf("NFS server error: %v", err)
		}
	}()
}

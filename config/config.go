package config

import (
	"os"
	"strings"
)

// Config 应用配置
type Config struct {
	Port       string
	MySQLDSN   string
	StorageDir string
}

const DefaultStorageDir = "data/uploads"

// Load 从环境变量加载配置
func Load() Config {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	mysqlDSN := strings.TrimSpace(os.Getenv("MYSQL_DSN"))

	storageDir := DefaultStorageDir

	return Config{
		Port:       port,
		MySQLDSN:   mysqlDSN,
		StorageDir: storageDir,
	}
}

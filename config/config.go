package config

import (
	"os"
	"strings"
)

// Config 应用配置
type Config struct {
	Port       string
	MYSQLDN string
	StorageDir string
}

// Load 从环境变量加载配置
func Load() Config {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	mysqlDSN := strings.TrimSpace(os.Getenv("MYSQL_DSN"))
	
	storageDir := "data/uploads"

	return Config{
		Port:       port,
		MYSQLDN: mysqlDSN,
		StorageDir: storageDir,
	}
}

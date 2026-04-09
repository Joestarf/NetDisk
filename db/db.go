package db

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"netdisk/models"

	_ "github.com/go-sql-driver/mysql"
)

var (
	// DB 全局数据库连接
	DB *sql.DB

	// FilesByID 文件内存索引
	FilesByID = make(map[string]*models.FileRecord)
	// FilesMu 保护 FilesByID 的读写锁
	FilesMu sync.RWMutex
)

// Init 初始化数据库连接并建表
func Init(dsn string) error {
	if dsn == "" {
		return errors.New("MYSQL_DSN is empty")
	}

	var err error
	DB, err = sql.Open("mysql", dsn)
	if err != nil {
		return err
	}

	if err := DB.Ping(); err != nil {
		_ = DB.Close()
		return err
	}

	if err := initTables(); err != nil {
		_ = DB.Close()
		return err
	}

	return nil
}

func initTables() error {
	ddls := []string{
		`CREATE TABLE IF NOT EXISTS files (
		  id VARCHAR(64) PRIMARY KEY,
		  name VARCHAR(255) NOT NULL,
		  size_bytes BIGINT NOT NULL,
		  created_at_unix BIGINT NOT NULL,
		  owner_id BIGINT NULL,
		  parent_folder_id BIGINT NULL,
		  disk_path VARCHAR(1024) NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		`CREATE TABLE IF NOT EXISTS folders (
		  id BIGINT PRIMARY KEY AUTO_INCREMENT,
		  owner_id BIGINT NOT NULL,
		  name VARCHAR(255) NOT NULL,
		  parent_id BIGINT NULL,
		  created_at_unix BIGINT NOT NULL,
		  updated_at_unix BIGINT NOT NULL,
		  INDEX idx_folders_owner_parent (owner_id, parent_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		`CREATE TABLE IF NOT EXISTS users (
		  id BIGINT PRIMARY KEY AUTO_INCREMENT,
		  username VARCHAR(128) NOT NULL,
		  password_salt VARCHAR(64) NOT NULL,
		  password_hash VARCHAR(128) NOT NULL,
		  created_at_unix BIGINT NOT NULL,
		  UNIQUE KEY uk_users_username (username)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		`CREATE TABLE IF NOT EXISTS auth_tokens (
		  token VARCHAR(128) PRIMARY KEY,
		  user_id BIGINT NOT NULL,
		  expires_at_unix BIGINT NOT NULL,
		  revoked TINYINT(1) NOT NULL DEFAULT 0,
		  created_at_unix BIGINT NOT NULL,
		  INDEX idx_auth_tokens_user_id (user_id),
		  INDEX idx_auth_tokens_expires_at (expires_at_unix)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
	}

	for _, ddl := range ddls {
		if _, err := DB.Exec(ddl); err != nil {
			_ = DB.Close()
			return err
		}
	}

	// 兼容不同版本的 MySQL，确保 owner_id 列存在
	const ensureOwnerColumn = `
ALTER TABLE files
ADD COLUMN IF NOT EXISTS owner_id BIGINT NULL
`
	if _, err := DB.Exec(ensureOwnerColumn); err != nil {
		if _, err2 := DB.Exec("ALTER TABLE files ADD COLUMN owner_id BIGINT NULL"); err2 != nil {
			if !strings.Contains(strings.ToLower(err2.Error()), "duplicate column") {
				_ = DB.Close()
				return err2
			}
		}
	}

	// 确保 parent_folder_id 列存在
	const ensureParentFolderColumn = `
ALTER TABLE files
ADD COLUMN IF NOT EXISTS parent_folder_id BIGINT NULL
`
	if _, err := DB.Exec(ensureParentFolderColumn); err != nil {
		if _, err2 := DB.Exec("ALTER TABLE files ADD COLUMN parent_folder_id BIGINT NULL"); err2 != nil {
			if !strings.Contains(strings.ToLower(err2.Error()), "duplicate column") {
				_ = DB.Close()
				return err2
			}
		}
	}

	return nil
}

// LoadFilesFromDB 启动时从 MySQL 加载文件索引到内存
func LoadFilesFromDB() error {
	rows, err := DB.Query("SELECT id, name, size_bytes, created_at_unix, owner_id, parent_folder_id, disk_path FROM files")
	if err != nil {
		return err
	}
	defer rows.Close()

	type rowData struct {
		id        string
		name      string
		size      int64
		createdAt int64
		ownerID   sql.NullInt64
		parentID  sql.NullInt64
		diskPath  string
	}

	items := make([]rowData, 0)
	for rows.Next() {
		var r rowData
		if err := rows.Scan(&r.id, &r.name, &r.size, &r.createdAt, &r.ownerID, &r.parentID, &r.diskPath); err != nil {
			return err
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	FilesMu.Lock()
	defer FilesMu.Unlock()
	FilesByID = make(map[string]*models.FileRecord, len(items))

	for _, r := range items {
		if _, err := os.Stat(r.diskPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				_, _ = DB.Exec("DELETE FROM files WHERE id = ?", r.id)
				continue
			}
			return err
		}

		var parentID *int64
		if r.parentID.Valid {
			v := r.parentID.Int64
			parentID = &v
		}

		FilesByID[r.id] = &models.FileRecord{
			ID:        r.id,
			Name:      r.name,
			SizeBytes: r.size,
			CreatedAt: time.Unix(r.createdAt, 0),
			OwnerID:   r.ownerID.Int64,
			ParentID:  parentID,
			DiskPath:  r.diskPath,
		}
	}

	return nil
}

// Close 关闭数据库连接
func Close() error {
	if DB != nil {
		return DB.Close()
	}
	return nil
}

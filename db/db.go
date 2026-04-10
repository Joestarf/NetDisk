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
		  disk_path VARCHAR(1024) NOT NULL,
		  blob_hash VARCHAR(64) DEFAULT NULL,
		  INDEX idx_files_blob_hash (blob_hash)
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
		  bio TEXT DEFAULT NULL,
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
		`CREATE TABLE IF NOT EXISTS shares (
		  id BIGINT PRIMARY KEY AUTO_INCREMENT,
		  token VARCHAR(64) UNIQUE NOT NULL,
		  owner_id BIGINT NOT NULL,
		  node_type ENUM('file', 'folder') NOT NULL,
		  node_id VARCHAR(128) NOT NULL,
		  name VARCHAR(255) NOT NULL,
		  password_hash VARCHAR(128) DEFAULT NULL,
		  expires_at_unix BIGINT DEFAULT NULL,
		  max_visits INT DEFAULT NULL,
		  visit_count INT NOT NULL DEFAULT 0,
		  revoked TINYINT(1) NOT NULL DEFAULT 0,
		  created_at_unix BIGINT NOT NULL,
		  updated_at_unix BIGINT NOT NULL,
		  INDEX idx_shares_token (token),
		  INDEX idx_shares_owner_id (owner_id),
		  INDEX idx_shares_node (node_type, node_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		`CREATE TABLE IF NOT EXISTS file_blobs (
		  hash VARCHAR(64) PRIMARY KEY,
		  size_bytes BIGINT NOT NULL,
		  disk_path VARCHAR(1024) NOT NULL,
		  ref_count INT NOT NULL DEFAULT 1,
		  created_at_unix BIGINT NOT NULL,
		  updated_at_unix BIGINT NOT NULL
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

	// 确保 files.blob_hash 列存在
	const ensureBlobHashColumn = `
ALTER TABLE files
ADD COLUMN IF NOT EXISTS blob_hash VARCHAR(64) DEFAULT NULL
`
	if _, err := DB.Exec(ensureBlobHashColumn); err != nil {
		if _, err2 := DB.Exec("ALTER TABLE files ADD COLUMN blob_hash VARCHAR(64) DEFAULT NULL"); err2 != nil {
			if !strings.Contains(strings.ToLower(err2.Error()), "duplicate column") {
				_ = DB.Close()
				return err2
			}
		}
	}

	// 确保 files.blob_hash 索引存在
	const ensureBlobHashIndex = `
ALTER TABLE files
ADD INDEX IF NOT EXISTS idx_files_blob_hash (blob_hash)
`
	if _, err := DB.Exec(ensureBlobHashIndex); err != nil {
		if _, err2 := DB.Exec("ALTER TABLE files ADD INDEX idx_files_blob_hash (blob_hash)"); err2 != nil {
			errText := strings.ToLower(err2.Error())
			if !strings.Contains(errText, "duplicate key") && !strings.Contains(errText, "already exists") {
				_ = DB.Close()
				return err2
			}
		}
	}

	// 确保 users.bio 列存在
	const ensureBioColumn = `
ALTER TABLE users
ADD COLUMN IF NOT EXISTS bio TEXT DEFAULT NULL
`
	if _, err := DB.Exec(ensureBioColumn); err != nil {
		if _, err2 := DB.Exec("ALTER TABLE users ADD COLUMN bio TEXT DEFAULT NULL"); err2 != nil {
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
	rows, err := DB.Query("SELECT id, name, size_bytes, created_at_unix, owner_id, parent_folder_id, disk_path, blob_hash FROM files")
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
		blobHash  sql.NullString
	}

	items := make([]rowData, 0)
	for rows.Next() {
		var r rowData
		if err := rows.Scan(&r.id, &r.name, &r.size, &r.createdAt, &r.ownerID, &r.parentID, &r.diskPath, &r.blobHash); err != nil {
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

		var blobHash *string
		if r.blobHash.Valid {
			v := r.blobHash.String
			blobHash = &v
		}

		FilesByID[r.id] = &models.FileRecord{
			ID:        r.id,
			Name:      r.name,
			SizeBytes: r.size,
			CreatedAt: time.Unix(r.createdAt, 0),
			OwnerID:   r.ownerID.Int64,
			ParentID:  parentID,
			DiskPath:  r.diskPath,
			BlobHash:  blobHash,
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

// CreateShare 插入一条分享记录
func CreateShare(share *models.ShareRecord) (int64, error) {
	result, err := DB.Exec(
		`INSERT INTO shares(
			token, owner_id, node_type, node_id, name, password_hash,
			expires_at_unix, max_visits, visit_count, revoked,
			created_at_unix, updated_at_unix
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		share.Token,
		share.OwnerID,
		share.NodeType,
		share.NodeID,
		share.Name,
		share.PasswordHash,
		share.ExpiresAtUnix,
		share.MaxVisits,
		share.VisitCount,
		share.Revoked,
		share.CreatedAtUnix,
		share.UpdatedAtUnix,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// ListSharesByOwner 查询用户创建的分享
func ListSharesByOwner(ownerID int64) ([]models.ShareRecord, error) {
	rows, err := DB.Query(
		`SELECT id, token, owner_id, node_type, node_id, name, password_hash,
			expires_at_unix, max_visits, visit_count, revoked,
			created_at_unix, updated_at_unix
		 FROM shares
		 WHERE owner_id = ?
		 ORDER BY id DESC`,
		ownerID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]models.ShareRecord, 0)
	for rows.Next() {
		var share models.ShareRecord
		var passwordHash sql.NullString
		var expiresAt sql.NullInt64
		var maxVisits sql.NullInt64
		var revoked int64
		if err := rows.Scan(
			&share.ID,
			&share.Token,
			&share.OwnerID,
			&share.NodeType,
			&share.NodeID,
			&share.Name,
			&passwordHash,
			&expiresAt,
			&maxVisits,
			&share.VisitCount,
			&revoked,
			&share.CreatedAtUnix,
			&share.UpdatedAtUnix,
		); err != nil {
			return nil, err
		}
		if passwordHash.Valid {
			v := passwordHash.String
			share.PasswordHash = &v
		}
		if expiresAt.Valid {
			v := expiresAt.Int64
			share.ExpiresAtUnix = &v
		}
		if maxVisits.Valid {
			v := int(maxVisits.Int64)
			share.MaxVisits = &v
		}
		share.Revoked = revoked != 0
		share.CreatedAt = time.Unix(share.CreatedAtUnix, 0)
		share.UpdatedAt = time.Unix(share.UpdatedAtUnix, 0)
		items = append(items, share)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return items, nil
}

// GetShareByToken 按 token 获取分享
func GetShareByToken(token string) (*models.ShareRecord, error) {
	var share models.ShareRecord
	var passwordHash sql.NullString
	var expiresAt sql.NullInt64
	var maxVisits sql.NullInt64
	var revoked int64
	err := DB.QueryRow(
		`SELECT id, token, owner_id, node_type, node_id, name, password_hash,
			expires_at_unix, max_visits, visit_count, revoked,
			created_at_unix, updated_at_unix
		 FROM shares
		 WHERE token = ?`,
		token,
	).Scan(
		&share.ID,
		&share.Token,
		&share.OwnerID,
		&share.NodeType,
		&share.NodeID,
		&share.Name,
		&passwordHash,
		&expiresAt,
		&maxVisits,
		&share.VisitCount,
		&revoked,
		&share.CreatedAtUnix,
		&share.UpdatedAtUnix,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	if passwordHash.Valid {
		v := passwordHash.String
		share.PasswordHash = &v
	}
	if expiresAt.Valid {
		v := expiresAt.Int64
		share.ExpiresAtUnix = &v
	}
	if maxVisits.Valid {
		v := int(maxVisits.Int64)
		share.MaxVisits = &v
	}
	share.Revoked = revoked != 0
	share.CreatedAt = time.Unix(share.CreatedAtUnix, 0)
	share.UpdatedAt = time.Unix(share.UpdatedAtUnix, 0)
	return &share, nil
}

// DeleteShareByID 删除用户自己的分享
func DeleteShareByID(ownerID int64, shareID int64) error {
	result, err := DB.Exec("DELETE FROM shares WHERE id = ? AND owner_id = ?", shareID, ownerID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return os.ErrNotExist
	}
	return nil
}

// IncrementShareVisitByToken 记录一次成功访问
func IncrementShareVisitByToken(token string) error {
	result, err := DB.Exec(
		`UPDATE shares
		 SET visit_count = visit_count + 1,
		     updated_at_unix = ?
		 WHERE token = ?`,
		time.Now().Unix(),
		token,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return os.ErrNotExist
	}
	return nil
}

// RevokeShareByID 标记分享为失效
func RevokeShareByID(ownerID int64, shareID int64) error {
	result, err := DB.Exec(
		`UPDATE shares
		 SET revoked = 1,
		     updated_at_unix = ?
		 WHERE id = ? AND owner_id = ?`,
		time.Now().Unix(),
		shareID,
		ownerID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return os.ErrNotExist
	}
	return nil
}

// RevokeSharesByNode 删除资源后，批量标记关联分享失效。
func RevokeSharesByNode(ownerID int64, nodeType string, nodeID string) error {
	_, err := DB.Exec(
		`UPDATE shares
		 SET revoked = 1,
		     updated_at_unix = ?
		 WHERE owner_id = ? AND node_type = ? AND node_id = ?`,
		time.Now().Unix(),
		ownerID,
		nodeType,
		nodeID,
	)
	return err
}

// GetBlobByHash 根据 hash 获取 blob 信息。
func GetBlobByHash(hash string) (*models.FileBlob, error) {
	return getBlobByHashTx(DB, hash)
}

func getBlobByHashTx(queryer interface {
	QueryRow(query string, args ...interface{}) *sql.Row
}, hash string) (*models.FileBlob, error) {
	var blob models.FileBlob
	err := queryer.QueryRow(
		`SELECT hash, size_bytes, disk_path, ref_count, created_at_unix, updated_at_unix
		 FROM file_blobs
		 WHERE hash = ?`,
		hash,
	).Scan(
		&blob.Hash,
		&blob.SizeBytes,
		&blob.DiskPath,
		&blob.RefCount,
		&blob.CreatedAtUnix,
		&blob.UpdatedAtUnix,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	blob.CreatedAt = time.Unix(blob.CreatedAtUnix, 0)
	blob.UpdatedAt = time.Unix(blob.UpdatedAtUnix, 0)
	return &blob, nil
}

// CreateBlob 在事务内创建 blob 记录。
func CreateBlob(tx *sql.Tx, hash string, size int64, diskPath string) error {
	_, err := tx.Exec(
		`INSERT INTO file_blobs(hash, size_bytes, disk_path, ref_count, created_at_unix, updated_at_unix)
		 VALUES (?, ?, ?, 1, ?, ?)`,
		hash,
		size,
		diskPath,
		time.Now().Unix(),
		time.Now().Unix(),
	)
	return err
}

// IncrementBlobRefCount 在事务内增加 blob 引用计数。
func IncrementBlobRefCount(tx *sql.Tx, hash string) error {
	result, err := tx.Exec(
		`UPDATE file_blobs
		 SET ref_count = ref_count + 1,
		     updated_at_unix = ?
		 WHERE hash = ?`,
		time.Now().Unix(),
		hash,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return os.ErrNotExist
	}
	return nil
}

// DecrementBlobRefCount 在事务内减少 blob 引用计数。
func DecrementBlobRefCount(tx *sql.Tx, hash string) (int, bool, error) {
	result, err := tx.Exec(
		`UPDATE file_blobs
		 SET ref_count = ref_count - 1,
		     updated_at_unix = ?
		 WHERE hash = ?`,
		time.Now().Unix(),
		hash,
	)
	if err != nil {
		return 0, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if affected == 0 {
		return 0, false, os.ErrNotExist
	}

	var refCount int
	if err := tx.QueryRow("SELECT ref_count FROM file_blobs WHERE hash = ?", hash).Scan(&refCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, true, nil
		}
		return 0, false, err
	}
	if refCount <= 0 {
		return refCount, true, nil
	}
	return refCount, false, nil
}

// DeleteBlob 在事务内删除 blob 记录。
func DeleteBlob(tx *sql.Tx, hash string) error {
	_, err := tx.Exec("DELETE FROM file_blobs WHERE hash = ?", hash)
	return err
}

// DeleteUserCascade 事务删除用户及其关联数据。
func DeleteUserCascade(userID int64) error {
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM shares WHERE owner_id = ?", userID); err != nil {
		return err
	}

	rows, err := tx.Query("SELECT id, disk_path, blob_hash FROM files WHERE owner_id = ?", userID)
	if err != nil {
		return err
	}
	type fileInfo struct {
		id       string
		path     string
		blobHash sql.NullString
	}
	files := make([]fileInfo, 0)
	for rows.Next() {
		var f fileInfo
		if err := rows.Scan(&f.id, &f.path, &f.blobHash); err != nil {
			rows.Close()
			return err
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if _, err := tx.Exec("DELETE FROM files WHERE owner_id = ?", userID); err != nil {
		return err
	}

	blobRefDec := make(map[string]int)
	blobDiskPath := make(map[string]string)
	legacyPaths := make([]string, 0)
	for _, f := range files {
		if f.blobHash.Valid && strings.TrimSpace(f.blobHash.String) != "" {
			h := f.blobHash.String
			blobRefDec[h]++
			if _, ok := blobDiskPath[h]; !ok {
				blobDiskPath[h] = f.path
			}
		} else {
			legacyPaths = append(legacyPaths, f.path)
		}
	}

	deleteBlobPaths := make([]string, 0)
	for hash, dec := range blobRefDec {
		result, err := tx.Exec(
			`UPDATE file_blobs
			 SET ref_count = ref_count - ?,
			     updated_at_unix = ?
			 WHERE hash = ?`,
			dec,
			time.Now().Unix(),
			hash,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			continue
		}

		var refCount int
		if err := tx.QueryRow("SELECT ref_count FROM file_blobs WHERE hash = ?", hash).Scan(&refCount); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if refCount <= 0 {
			if _, err := tx.Exec("DELETE FROM file_blobs WHERE hash = ?", hash); err != nil {
				return err
			}
			deleteBlobPaths = append(deleteBlobPaths, blobDiskPath[hash])
		}
	}

	if err := deleteAllFolders(tx, userID); err != nil {
		return err
	}

	if _, err := tx.Exec("DELETE FROM auth_tokens WHERE user_id = ?", userID); err != nil {
		return err
	}

	result, err := tx.Exec("DELETE FROM users WHERE id = ?", userID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return os.ErrNotExist
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	FilesMu.Lock()
	for _, p := range legacyPaths {
		_ = os.Remove(p)
	}
	for _, p := range deleteBlobPaths {
		_ = os.Remove(p)
	}
	for _, f := range files {
		delete(FilesByID, f.id)
	}
	FilesMu.Unlock()

	return nil
}

func deleteAllFolders(tx *sql.Tx, userID int64) error {
	for {
		result, err := tx.Exec(
			`DELETE f FROM folders f
			 LEFT JOIN folders sub ON sub.parent_id = f.id AND sub.owner_id = f.owner_id
			 WHERE f.owner_id = ? AND sub.id IS NULL`,
			userID,
		)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			break
		}
	}
	return nil
}

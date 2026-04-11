package handlers

import (
	"database/sql"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"os"
	"strconv"
	"strings"
	"time"

	"netdisk/db"
	"netdisk/models"
	"netdisk/storage"
)

// NFSChild 表示目录下的一个子节点（文件或文件夹）。
type NFSChild struct {
	Name      string
	IsFolder  bool
	SizeBytes int64
	CreatedAt time.Time
	FileID    string
	FolderID  int64
}

// GetFolderByNameForOwner 按父目录和名称查询文件夹。
func GetFolderByNameForOwner(ownerID int64, parentID *int64, name string) (*models.FolderRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, os.ErrNotExist
	}

	var rec models.FolderRecord
	var parent sql.NullInt64
	var createdAtUnix int64
	err := db.DB.QueryRow(
		`SELECT id, name, owner_id, parent_id, created_at_unix
		 FROM folders
		 WHERE owner_id = ? AND name = ? AND ((parent_id IS NULL AND ? IS NULL) OR parent_id = ?)
		 LIMIT 1`,
		ownerID,
		name,
		parentID,
		parentID,
	).Scan(&rec.ID, &rec.Name, &rec.OwnerID, &parent, &createdAtUnix)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}

	rec.CreatedAt = time.Unix(createdAtUnix, 0)
	if parent.Valid {
		v := parent.Int64
		rec.ParentID = &v
	}
	return &rec, nil
}

// GetFileByNameForOwner 按父目录和名称查询文件。
func GetFileByNameForOwner(ownerID int64, parentID *int64, name string) (*models.FileRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, os.ErrNotExist
	}

	db.FilesMu.RLock()
	defer db.FilesMu.RUnlock()
	for _, rec := range db.FilesByID {
		if rec.OwnerID != ownerID || rec.Name != name {
			continue
		}
		if parentID == nil && rec.ParentID == nil {
			return rec, nil
		}
		if parentID != nil && rec.ParentID != nil && *parentID == *rec.ParentID {
			return rec, nil
		}
	}
	return nil, os.ErrNotExist
}

// ListChildrenForOwner 列出目录下子节点。
func ListChildrenForOwner(ownerID int64, parentID *int64) ([]NFSChild, error) {
	raw, err := listChildrenByParent(ownerID, parentID)
	if err != nil {
		return nil, err
	}

	children := make([]NFSChild, 0, len(raw))
	for _, item := range raw {
		t, _ := item["type"].(string)
		switch t {
		case "folder":
			id, ok := item["id"].(int64)
			if !ok {
				continue
			}
			name, _ := item["name"].(string)
			createdAt, _ := item["created_at"].(time.Time)
			children = append(children, NFSChild{
				Name:      name,
				IsFolder:  true,
				FolderID:  id,
				CreatedAt: createdAt,
			})
		case "file":
			id, _ := item["id"].(string)
			name, _ := item["name"].(string)
			size, _ := item["size_bytes"].(int64)
			createdAt, _ := item["created_at"].(time.Time)
			children = append(children, NFSChild{
				Name:      name,
				IsFolder:  false,
				FileID:    id,
				SizeBytes: size,
				CreatedAt: createdAt,
			})
		}
	}
	return children, nil
}

// CreateFolderForOwner 创建文件夹。
func CreateFolderForOwner(ownerID int64, parentID *int64, name string) (*models.FolderRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, os.ErrInvalid
	}

	exists, err := siblingNameExists(ownerID, parentID, name, nil, nil)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, os.ErrExist
	}

	id, err := createFolderInDB(ownerID, name, parentID)
	if err != nil {
		return nil, err
	}
	return GetFolderByIDForOwner(ownerID, id)
}

// RenameFolderForOwner 重命名文件夹。
func RenameFolderForOwner(ownerID int64, folderID int64, newName string) error {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return os.ErrInvalid
	}
	folder, err := GetFolderByIDForOwner(ownerID, folderID)
	if err != nil {
		return err
	}

	exists, err := siblingNameExists(ownerID, folder.ParentID, newName, &folderID, nil)
	if err != nil {
		return err
	}
	if exists {
		return os.ErrExist
	}
	return updateFolderNameInDB(ownerID, folderID, newName)
}

// MoveFolderForOwner 移动文件夹。
func MoveFolderForOwner(ownerID int64, folderID int64, targetParentID *int64) error {
	return moveFolderNode(ownerID, folderID, targetParentID)
}

// DeleteFolderForOwner 删除空文件夹。
func DeleteFolderForOwner(ownerID int64, folderID int64) error {
	if _, err := GetFolderByIDForOwner(ownerID, folderID); err != nil {
		return err
	}

	hasChildren, err := hasChildrenInFolder(ownerID, folderID)
	if err != nil {
		return err
	}
	if hasChildren {
		return os.ErrInvalid
	}

	if err := deleteFolderInDB(ownerID, folderID); err != nil {
		return err
	}
	return db.RevokeSharesByNode(ownerID, "folder", strconvFormatInt(folderID))
}

// RenameFileForOwner 重命名文件。
func RenameFileForOwner(ownerID int64, fileID string, newName string) error {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return os.ErrInvalid
	}

	db.FilesMu.RLock()
	rec, ok := db.FilesByID[fileID]
	db.FilesMu.RUnlock()
	if !ok || rec.OwnerID != ownerID {
		return os.ErrNotExist
	}

	exists, err := siblingNameExists(ownerID, rec.ParentID, newName, nil, &fileID)
	if err != nil {
		return err
	}
	if exists {
		return os.ErrExist
	}

	if err := updateFileNameInDB(fileID, ownerID, newName); err != nil {
		return err
	}

	db.FilesMu.Lock()
	if cur, ok := db.FilesByID[fileID]; ok {
		cur.Name = newName
	}
	db.FilesMu.Unlock()
	return nil
}

// MoveFileForOwner 移动文件。
func MoveFileForOwner(ownerID int64, fileID string, targetParentID *int64) error {
	return moveFileNode(ownerID, fileID, targetParentID)
}

// DeleteFileForOwner 删除文件（含 blob 引用计数与对象存储清理）。
func DeleteFileForOwner(ownerID int64, id string) error {
	tx, err := db.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var diskPath string
	var blobHash sql.NullString
	err = tx.QueryRow(
		"SELECT disk_path, blob_hash FROM files WHERE id = ? AND owner_id = ? FOR UPDATE",
		id,
		ownerID,
	).Scan(&diskPath, &blobHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return os.ErrNotExist
		}
		return err
	}

	if _, err := tx.Exec("DELETE FROM files WHERE id = ? AND owner_id = ?", id, ownerID); err != nil {
		return err
	}

	if _, err := tx.Exec(
		`UPDATE shares SET revoked = 1, updated_at_unix = ?
		 WHERE owner_id = ? AND node_type = 'file' AND node_id = ?`,
		time.Now().Unix(),
		ownerID,
		id,
	); err != nil {
		return err
	}

	removePath := ""
	removeObjectKey := ""
	if blobHash.Valid && strings.TrimSpace(blobHash.String) != "" {
		var blobDiskPath string
		var blobBackend string
		var blobStorageKey string
		err = tx.QueryRow("SELECT disk_path, storage_backend, storage_key FROM file_blobs WHERE hash = ? FOR UPDATE", blobHash.String).Scan(&blobDiskPath, &blobBackend, &blobStorageKey)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		_, shouldDelete, err := db.DecrementBlobRefCount(tx, blobHash.String)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if shouldDelete {
			if err := db.DeleteBlob(tx, blobHash.String); err != nil {
				return err
			}
			if strings.EqualFold(blobBackend, "oss") && strings.TrimSpace(blobStorageKey) != "" {
				removeObjectKey = blobStorageKey
			} else {
				removePath = blobDiskPath
			}
		}
	} else {
		removePath = diskPath
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if removeObjectKey != "" {
		backend := storage.GetObjectBackend()
		if backend == nil {
			log.Printf("warn: object storage backend unavailable when deleting object key=%s", removeObjectKey)
		} else if err := backend.Delete(removeObjectKey); err != nil {
			log.Printf("warn: failed to delete object storage key=%s: %v", removeObjectKey, err)
		}
	}

	if removePath != "" {
		if err := os.Remove(removePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	db.FilesMu.Lock()
	delete(db.FilesByID, id)
	db.FilesMu.Unlock()
	return nil
}

// SaveFileForOwner 保存文件（同目录同名时执行覆盖）。
func SaveFileForOwner(ownerID int64, parentID *int64, fileName string, src *os.File) (*models.FileRecord, error) {
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		return nil, os.ErrInvalid
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	existing, err := GetFileByNameForOwner(ownerID, parentID, fileName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	rec, err := saveUploadedFile(src, &multipart.FileHeader{Filename: fileName}, ownerID, parentID)
	if err != nil {
		return nil, err
	}

	if existing != nil {
		if err := DeleteFileForOwner(ownerID, existing.ID); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

func strconvFormatInt(v int64) string {
	return strconv.FormatInt(v, 10)
}

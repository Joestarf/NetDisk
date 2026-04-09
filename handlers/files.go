package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"netdisk/db"
	"netdisk/middleware"
	"netdisk/models"
	"netdisk/utils"
)

// UploadHandler 文件上传
func UploadHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	if r.Method != http.MethodPost {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	src, hdr, err := r.FormFile("file")
	if err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10002, "missing file form field")
		return
	}
	defer src.Close()

	parentID, err := utils.ParseOptionalInt64(strings.TrimSpace(r.FormValue("parent_id")))
	if err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10015, "invalid parent_id")
		return
	}
	if parentID != nil {
		if _, err := GetFolderByIDForOwner(user.ID, *parentID); err != nil {
			utils.WriteError(w, http.StatusBadRequest, 10016, "parent folder not found")
			return
		}
	}

	record, err := saveUploadedFile(src, hdr, user.ID, parentID)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to save file")
		return
	}

	utils.WriteJSON(w, http.StatusCreated, models.APIResponse{
		Code:    0,
		Message: "uploaded",
		Data: map[string]interface{}{
			"id":           record.ID,
			"name":         record.Name,
			"size_bytes":   record.SizeBytes,
			"parent_id":    record.ParentID,
			"download_url": "/api/v1/files/" + record.ID + "/download",
		},
	})
}

// FilesCollectionHandler 文件列表
func FilesCollectionHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	if r.Method != http.MethodGet {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	db.FilesMu.RLock()
	items := make([]map[string]interface{}, 0, len(db.FilesByID))
	for _, rec := range db.FilesByID {
		if rec.OwnerID != user.ID {
			continue
		}
		items = append(items, map[string]interface{}{
			"id":           rec.ID,
			"name":         rec.Name,
			"size_bytes":   rec.SizeBytes,
			"created_at":   rec.CreatedAt,
			"parent_id":    rec.ParentID,
			"download_url": "/api/v1/files/" + rec.ID + "/download",
		})
	}
	db.FilesMu.RUnlock()

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{
		Code:    0,
		Message: "ok",
		Data:    items,
	})
}

// FileItemHandler 文件项处理（下载、重命名、删除）
func FileItemHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	id, action, err := utils.ParseFileAction(r.URL.Path)
	if err != nil {
		utils.WriteError(w, http.StatusNotFound, 10004, "not found")
		return
	}

	switch {
	case action == "download" && r.Method == http.MethodGet:
		downloadHandler(w, r, id, user.ID)
	case action == "rename" && r.Method == http.MethodPatch:
		renameHandler(w, r, id, user.ID)
	case action == "" && r.Method == http.MethodDelete:
		deleteHandler(w, r, id, user.ID)
	default:
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
	}
}

func downloadHandler(w http.ResponseWriter, r *http.Request, id string, ownerID int64) {
	rec, err := GetFileRecordForOwner(id, ownerID)
	if err != nil {
		utils.WriteError(w, http.StatusNotFound, 10005, "file not found")
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename=\""+rec.Name+"\"")
	http.ServeFile(w, r, rec.DiskPath)
}

func renameHandler(w http.ResponseWriter, r *http.Request, id string, ownerID int64) {
	type renameRequest struct {
		Name string `json:"name"`
	}

	var req renameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		utils.WriteError(w, http.StatusBadRequest, 10007, "name cannot be empty")
		return
	}

	db.FilesMu.Lock()
	rec, ok := db.FilesByID[id]
	if !ok {
		db.FilesMu.Unlock()
		utils.WriteError(w, http.StatusNotFound, 10005, "file not found")
		return
	}
	if rec.OwnerID != ownerID {
		db.FilesMu.Unlock()
		utils.WriteError(w, http.StatusNotFound, 10005, "file not found")
		return
	}
	if err := updateFileNameInDB(id, ownerID, req.Name); err != nil {
		db.FilesMu.Unlock()
		if errors.Is(err, os.ErrNotExist) {
			utils.WriteError(w, http.StatusNotFound, 10005, "file not found")
			return
		}
		utils.WriteError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
		return
	}
	rec.Name = req.Name
	db.FilesMu.Unlock()

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{
		Code:    0,
		Message: "renamed",
		Data: map[string]string{
			"id":   id,
			"name": req.Name,
		},
	})
}

func deleteHandler(w http.ResponseWriter, _ *http.Request, id string, ownerID int64) {
	db.FilesMu.RLock()
	rec, ok := db.FilesByID[id]
	if !ok || rec.OwnerID != ownerID {
		db.FilesMu.RUnlock()
		utils.WriteError(w, http.StatusNotFound, 10005, "file not found")
		return
	}
	db.FilesMu.RUnlock()

	if err := os.Remove(rec.DiskPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		utils.WriteError(w, http.StatusInternalServerError, 10008, "failed to delete file from disk")
		return
	}

	if err := deleteFileFromDB(id, ownerID); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
		return
	}

	db.FilesMu.Lock()
	delete(db.FilesByID, id)
	db.FilesMu.Unlock()

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "deleted"})
}

// 文件数据库操作函数

func saveUploadedFile(src multipart.File, hdr *multipart.FileHeader, ownerID int64, parentID *int64) (*models.FileRecord, error) {
	id, err := utils.GenerateID()
	if err != nil {
		return nil, err
	}

	storageDir := "data/uploads"
	name := filepath.Base(strings.TrimSpace(hdr.Filename))
	if name == "" {
		name = "unnamed"
	}
	ext := filepath.Ext(name)
	diskPath := filepath.Join(storageDir, id+ext)

	dst, err := os.Create(diskPath)
	if err != nil {
		return nil, err
	}
	defer dst.Close()

	size, err := io.Copy(dst, src)
	if err != nil {
		return nil, err
	}

	rec := &models.FileRecord{
		ID:        id,
		Name:      name,
		SizeBytes: size,
		CreatedAt: time.Now(),
		OwnerID:   ownerID,
		ParentID:  parentID,
		DiskPath:  diskPath,
	}

	db.FilesMu.Lock()
	if err := insertFileToDB(rec); err != nil {
		db.FilesMu.Unlock()
		_ = os.Remove(diskPath)
		return nil, err
	}
	db.FilesByID[id] = rec
	db.FilesMu.Unlock()

	return rec, nil
}

func GetFileRecordForOwner(id string, ownerID int64) (*models.FileRecord, error) {
	db.FilesMu.RLock()
	defer db.FilesMu.RUnlock()
	rec, ok := db.FilesByID[id]
	if !ok || rec.OwnerID != ownerID {
		return nil, os.ErrNotExist
	}
	return rec, nil
}

func insertFileToDB(rec *models.FileRecord) error {
	_, err := db.DB.Exec(
		"INSERT INTO files(id, name, size_bytes, created_at_unix, owner_id, parent_folder_id, disk_path) VALUES (?, ?, ?, ?, ?, ?, ?)",
		rec.ID,
		rec.Name,
		rec.SizeBytes,
		rec.CreatedAt.Unix(),
		rec.OwnerID,
		utils.Int64Value(rec.ParentID),
		rec.DiskPath,
	)
	return err
}

func updateFileNameInDB(id string, ownerID int64, name string) error {
	result, err := db.DB.Exec("UPDATE files SET name = ? WHERE id = ? AND owner_id = ?", name, id, ownerID)
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

func deleteFileFromDB(id string, ownerID int64) error {
	_, err := db.DB.Exec("DELETE FROM files WHERE id = ? AND owner_id = ?", id, ownerID)
	return err
}

func MoveFileToFolderInDB(id string, ownerID int64, parentID *int64) error {
	result, err := db.DB.Exec(
		"UPDATE files SET parent_folder_id = ? WHERE id = ? AND owner_id = ?",
		utils.Int64Value(parentID),
		id,
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

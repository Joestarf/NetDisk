package handlers

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"netdisk/config"
	"netdisk/db"
	"netdisk/middleware"
	"netdisk/models"
	"netdisk/utils"
)

type chunkUploadMeta struct {
	OwnerID        int64  `json:"owner_id"`
	Name           string `json:"name"`
	ParentID       *int64 `json:"parent_id,omitempty"`
	TotalChunks    int    `json:"total_chunks"`
	TotalSizeBytes *int64 `json:"total_size_bytes,omitempty"`
	CreatedAtUnix  int64  `json:"created_at_unix"`
}

const (
	errCodeChunkHashMismatch = 10033
	errCodeMissingFileHash   = 10034
)

func chunkRootDir() string {
	return filepath.Join(config.DefaultStorageDir, ".chunks")
}

func chunkUploadDir(uploadID string) string {
	return filepath.Join(chunkRootDir(), uploadID)
}

// ChunkUploadInitHandler 初始化分片上传会话
func ChunkUploadInitHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}
	if r.Method != http.MethodPost {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	var req struct {
		Name           string `json:"name"`
		ParentID       *int64 `json:"parent_id"`
		TotalChunks    int    `json:"total_chunks"`
		TotalSizeBytes *int64 `json:"total_size_bytes"`
		FileHash       string `json:"file_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.FileHash = strings.ToLower(strings.TrimSpace(req.FileHash))
	if req.Name == "" || req.TotalChunks <= 0 {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid init params")
		return
	}
	if req.TotalSizeBytes != nil && *req.TotalSizeBytes < 0 {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid total_size_bytes")
		return
	}
	if req.FileHash != "" && !isValidSHA256Hex(req.FileHash) {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid file_hash")
		return
	}
	if req.ParentID != nil {
		if _, err := GetFolderByIDForOwner(user.ID, *req.ParentID); err != nil {
			utils.WriteError(w, http.StatusBadRequest, 10016, "parent folder not found")
			return
		}
	}

	// 秒传：客户端给出哈希且服务端已存在 blob，则直接生成文件记录。
	if req.FileHash != "" {
		if _, err := db.GetBlobByHash(req.FileHash); err == nil {
			rec, err := createFileByBlobHash(user.ID, req.Name, req.ParentID, req.FileHash)
			if err == nil {
				utils.WriteJSON(w, http.StatusCreated, models.APIResponse{
					Code:    0,
					Message: "instant uploaded",
					Data: map[string]interface{}{
						"instant":      true,
						"id":           rec.ID,
						"name":         rec.Name,
						"size_bytes":   rec.SizeBytes,
						"parent_id":    rec.ParentID,
						"download_url": "/api/v1/files/" + rec.ID + "/download",
					},
				})
				return
			}
			if !errors.Is(err, os.ErrNotExist) {
				utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to init upload")
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to init upload")
			return
		}
	}

	uploadID, err := utils.GenerateID()
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to init upload")
		return
	}
	dir := chunkUploadDir(uploadID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to init upload")
		return
	}

	meta := chunkUploadMeta{
		OwnerID:        user.ID,
		Name:           req.Name,
		ParentID:       req.ParentID,
		TotalChunks:    req.TotalChunks,
		TotalSizeBytes: req.TotalSizeBytes,
		CreatedAtUnix:  time.Now().Unix(),
	}
	if err := writeChunkMeta(uploadID, meta); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to init upload")
		return
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "ok", Data: map[string]interface{}{"upload_id": uploadID}})
}

// ChunkUploadPartHandler 上传单个分片
func ChunkUploadPartHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}
	if r.Method != http.MethodPost {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	uploadID := strings.TrimSpace(r.FormValue("upload_id"))
	idxRaw := strings.TrimSpace(r.FormValue("chunk_index"))
	chunkIndex, err := strconv.Atoi(idxRaw)
	if err != nil || chunkIndex < 0 {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid chunk_index")
		return
	}
	chunkHash := strings.TrimSpace(r.FormValue("chunk_hash"))
	if chunkHash != "" && !isValidSHA256Hex(strings.ToLower(chunkHash)) {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid chunk_hash")
		return
	}
	meta, err := readChunkMeta(uploadID)
	if err != nil {
		utils.WriteError(w, http.StatusNotFound, 10004, "upload session not found")
		return
	}
	if meta.OwnerID != user.ID {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}
	if chunkIndex >= meta.TotalChunks {
		utils.WriteError(w, http.StatusBadRequest, 10006, "chunk_index out of range")
		return
	}

	src, _, err := r.FormFile("chunk")
	if err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10002, "missing chunk form field")
		return
	}
	defer src.Close()

	dir := chunkUploadDir(uploadID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to save chunk")
		return
	}
	partPath := filepath.Join(dir, fmt.Sprintf("%06d.part", chunkIndex))
	tmpPath := partPath + ".tmp"
	dst, err := os.Create(tmpPath)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to save chunk")
		return
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to save chunk")
		return
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to save chunk")
		return
	}
	if err := os.Rename(tmpPath, partPath); err != nil {
		_ = os.Remove(tmpPath)
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to save chunk")
		return
	}

	if chunkHash != "" {
		part, err := os.Open(partPath)
		if err != nil {
			_ = os.Remove(partPath)
			utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to verify chunk hash")
			return
		}
		hasher := sha256.New()
		if _, err := io.Copy(hasher, part); err != nil {
			_ = part.Close()
			_ = os.Remove(partPath)
			utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to verify chunk hash")
			return
		}
		_ = part.Close()
		actualHash := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(actualHash, chunkHash) {
			_ = os.Remove(partPath)
			utils.WriteError(w, http.StatusBadRequest, errCodeChunkHashMismatch, "chunk hash mismatch")
			return
		}
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "chunk uploaded", Data: map[string]interface{}{"chunk_index": chunkIndex}})
}

// ChunkUploadStatusHandler 查询分片上传状态（已上传分片/缺失分片）
func ChunkUploadStatusHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	uploadID := strings.TrimSpace(r.URL.Query().Get("upload_id"))
	meta, err := readChunkMeta(uploadID)
	if err != nil {
		utils.WriteError(w, http.StatusNotFound, 10004, "upload session not found")
		return
	}
	if meta.OwnerID != user.ID {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	uploaded, err := listUploadedChunkIndexes(uploadID)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to query upload status")
		return
	}
	uploadedSet := make(map[int]struct{}, len(uploaded))
	for _, idx := range uploaded {
		uploadedSet[idx] = struct{}{}
	}

	missing := make([]int, 0)
	for i := 0; i < meta.TotalChunks; i++ {
		if _, ok := uploadedSet[i]; !ok {
			missing = append(missing, i)
		}
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{
		Code:    0,
		Message: "ok",
		Data: map[string]interface{}{
			"upload_id":       uploadID,
			"name":            meta.Name,
			"total_chunks":    meta.TotalChunks,
			"uploaded_chunks": uploaded,
			"missing_chunks":  missing,
		},
	})
}

// ChunkUploadCompleteHandler 合并分片并入库
func ChunkUploadCompleteHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}
	if r.Method != http.MethodPost {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	var req struct {
		UploadID string `json:"upload_id"`
		FileHash string `json:"file_hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.UploadID = strings.TrimSpace(req.UploadID)
	req.FileHash = strings.TrimSpace(req.FileHash)
	if req.FileHash == "" {
		utils.WriteError(w, http.StatusBadRequest, errCodeMissingFileHash, "file_hash is required")
		return
	}
	if !isValidSHA256Hex(strings.ToLower(req.FileHash)) {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid file_hash")
		return
	}

	meta, err := readChunkMeta(req.UploadID)
	if err != nil {
		utils.WriteError(w, http.StatusNotFound, 10004, "upload session not found")
		return
	}
	if meta.OwnerID != user.ID {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	merged, err := os.CreateTemp(config.DefaultStorageDir, "merge-*.tmp")
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to complete upload")
		return
	}
	mergedPath := merged.Name()
	cleanupMerged := true
	defer func() {
		if cleanupMerged {
			_ = os.Remove(mergedPath)
		}
	}()

	dir := chunkUploadDir(req.UploadID)
	var mergedBytes int64
	for i := 0; i < meta.TotalChunks; i++ {
		partPath := filepath.Join(dir, fmt.Sprintf("%06d.part", i))
		part, err := os.Open(partPath)
		if err != nil {
			_ = merged.Close()
			utils.WriteError(w, http.StatusBadRequest, 10006, "missing chunk part")
			return
		}
		n, err := io.Copy(merged, part)
		if err != nil {
			_ = part.Close()
			_ = merged.Close()
			utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to complete upload")
			return
		}
		mergedBytes += n
		_ = part.Close()
	}
	if meta.TotalSizeBytes != nil && mergedBytes != *meta.TotalSizeBytes {
		_ = merged.Close()
		utils.WriteError(w, http.StatusBadRequest, 10006, "chunk data size mismatch")
		return
	}
	if err := merged.Close(); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to complete upload")
		return
	}

	hf, err := os.Open(mergedPath)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to verify file hash")
		return
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, hf); err != nil {
		_ = hf.Close()
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to verify file hash")
		return
	}
	_ = hf.Close()
	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actualHash, req.FileHash) {
		utils.WriteError(w, http.StatusBadRequest, 10032, "file hash mismatch")
		return
	}

	src, err := os.Open(mergedPath)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to complete upload")
		return
	}
	defer src.Close()

	rec, err := saveUploadedFile(src, &multipart.FileHeader{Filename: meta.Name}, user.ID, meta.ParentID)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to save file")
		return
	}
	cleanupMerged = false
	_ = os.Remove(mergedPath)
	_ = os.RemoveAll(dir)

	utils.WriteJSON(w, http.StatusCreated, models.APIResponse{
		Code:    0,
		Message: "uploaded",
		Data: map[string]interface{}{
			"id":           rec.ID,
			"name":         rec.Name,
			"size_bytes":   rec.SizeBytes,
			"parent_id":    rec.ParentID,
			"download_url": "/api/v1/files/" + rec.ID + "/download",
		},
	})
}

func writeChunkMeta(uploadID string, meta chunkUploadMeta) error {
	if !isValidUploadID(uploadID) {
		return os.ErrInvalid
	}
	metaPath := filepath.Join(chunkUploadDir(uploadID), "meta.json")
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath, b, 0o644)
}

func readChunkMeta(uploadID string) (chunkUploadMeta, error) {
	if !isValidUploadID(uploadID) {
		return chunkUploadMeta{}, os.ErrNotExist
	}
	metaPath := filepath.Join(chunkUploadDir(uploadID), "meta.json")
	b, err := os.ReadFile(metaPath)
	if err != nil {
		return chunkUploadMeta{}, err
	}
	var meta chunkUploadMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		return chunkUploadMeta{}, err
	}
	return meta, nil
}

func isValidUploadID(uploadID string) bool {
	uploadID = strings.TrimSpace(uploadID)
	if uploadID == "" {
		return false
	}
	if strings.Contains(uploadID, "/") || strings.Contains(uploadID, "\\") || strings.Contains(uploadID, "..") {
		return false
	}
	return true
}

func listUploadedChunkIndexes(uploadID string) ([]int, error) {
	dir := chunkUploadDir(uploadID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []int{}, nil
		}
		return nil, err
	}

	indexes := make([]int, 0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".part") {
			continue
		}
		base := strings.TrimSuffix(name, ".part")
		idx, err := strconv.Atoi(base)
		if err != nil || idx < 0 {
			continue
		}
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	return indexes, nil
}

func isValidSHA256Hex(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, ch := range hash {
		isNum := ch >= '0' && ch <= '9'
		isHexLower := ch >= 'a' && ch <= 'f'
		if !isNum && !isHexLower {
			return false
		}
	}
	return true
}

func createFileByBlobHash(ownerID int64, name string, parentID *int64, blobHash string) (*models.FileRecord, error) {
	tx, err := db.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var size int64
	var diskPath string
	err = tx.QueryRow("SELECT size_bytes, disk_path FROM file_blobs WHERE hash = ? FOR UPDATE", blobHash).Scan(&size, &diskPath)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}

	if err := db.IncrementBlobRefCount(tx, blobHash); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}

	id, err := utils.GenerateID()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	blobHashCopy := blobHash
	rec := &models.FileRecord{
		ID:        id,
		Name:      name,
		SizeBytes: size,
		CreatedAt: now,
		OwnerID:   ownerID,
		ParentID:  parentID,
		DiskPath:  diskPath,
		BlobHash:  &blobHashCopy,
	}
	if err := insertFileToDBTx(tx, rec); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	db.FilesMu.Lock()
	db.FilesByID[id] = rec
	db.FilesMu.Unlock()

	return rec, nil
}

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
	case action == "download" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
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
	tx, err := db.DB.Begin()
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
		return
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
			utils.WriteError(w, http.StatusNotFound, 10005, "file not found")
			return
		}
		utils.WriteError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
		return
	}

	if _, err := tx.Exec("DELETE FROM files WHERE id = ? AND owner_id = ?", id, ownerID); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
		return
	}

	if _, err := tx.Exec(
		`UPDATE shares SET revoked = 1, updated_at_unix = ?
		 WHERE owner_id = ? AND node_type = 'file' AND node_id = ?`,
		time.Now().Unix(),
		ownerID,
		id,
	); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10030, "failed to update related shares")
		return
	}

	removePath := ""
	if blobHash.Valid && strings.TrimSpace(blobHash.String) != "" {
		var blobDiskPath string
		err = tx.QueryRow("SELECT disk_path FROM file_blobs WHERE hash = ? FOR UPDATE", blobHash.String).Scan(&blobDiskPath)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			utils.WriteError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
			return
		}

		_, shouldDelete, err := db.DecrementBlobRefCount(tx, blobHash.String)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			utils.WriteError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
			return
		}
		if shouldDelete {
			if err := db.DeleteBlob(tx, blobHash.String); err != nil {
				utils.WriteError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
				return
			}
			removePath = blobDiskPath
		}
	} else {
		removePath = diskPath
	}

	if err := tx.Commit(); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
		return
	}

	if removePath != "" {
		if err := os.Remove(removePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			utils.WriteError(w, http.StatusInternalServerError, 10008, "failed to delete file from disk")
			return
		}
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

	storageDir := config.DefaultStorageDir
	name := filepath.Base(strings.TrimSpace(hdr.Filename))
	if name == "" {
		name = "unnamed"
	}
	tmpFile, err := os.CreateTemp(storageDir, "upload-*.tmp")
	if err != nil {
		return nil, err
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	teeReader := io.TeeReader(src, hasher)
	size, err := io.Copy(tmpFile, teeReader)
	if err != nil {
		_ = tmpFile.Close()
		return nil, err
	}
	if err := tmpFile.Close(); err != nil {
		return nil, err
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	blobHash := hash
	diskPath := filepath.Join(storageDir, hash)

	tx, err := db.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var existingPath string
	err = tx.QueryRow("SELECT disk_path FROM file_blobs WHERE hash = ? FOR UPDATE", hash).Scan(&existingPath)
	if err == nil {
		diskPath = existingPath
		if err := db.IncrementBlobRefCount(tx, hash); err != nil {
			return nil, err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if err := os.Rename(tmpPath, diskPath); err != nil {
			if _, statErr := os.Stat(diskPath); statErr == nil {
				diskPath = filepath.Clean(diskPath)
			} else {
				return nil, err
			}
		}
		cleanupTmp = false
		if err := db.CreateBlob(tx, hash, size, diskPath); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				if err := db.IncrementBlobRefCount(tx, hash); err != nil {
					return nil, err
				}
			} else {
				return nil, err
			}
		}
	} else {
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
		BlobHash:  &blobHash,
	}

	if err := insertFileToDBTx(tx, rec); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	db.FilesMu.Lock()
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
	var blobHash interface{}
	if rec.BlobHash != nil {
		blobHash = *rec.BlobHash
	}
	_, err := db.DB.Exec(
		"INSERT INTO files(id, name, size_bytes, created_at_unix, owner_id, parent_folder_id, disk_path, blob_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		rec.ID,
		rec.Name,
		rec.SizeBytes,
		rec.CreatedAt.Unix(),
		rec.OwnerID,
		utils.Int64Value(rec.ParentID),
		rec.DiskPath,
		blobHash,
	)
	return err
}

func insertFileToDBTx(tx *sql.Tx, rec *models.FileRecord) error {
	var blobHash interface{}
	if rec.BlobHash != nil {
		blobHash = *rec.BlobHash
	}
	_, err := tx.Exec(
		"INSERT INTO files(id, name, size_bytes, created_at_unix, owner_id, parent_folder_id, disk_path, blob_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		rec.ID,
		rec.Name,
		rec.SizeBytes,
		rec.CreatedAt.Unix(),
		rec.OwnerID,
		utils.Int64Value(rec.ParentID),
		rec.DiskPath,
		blobHash,
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

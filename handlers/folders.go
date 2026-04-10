package handlers

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"netdisk/db"
	"netdisk/middleware"
	"netdisk/models"
	"netdisk/utils"
)

var (
	errNameConflict = errors.New("name conflict")
	errInvalidMove  = errors.New("invalid move")
)

// FoldersCollectionHandler 创建文件夹
func FoldersCollectionHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	if r.Method != http.MethodPost {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	type createFolderRequest struct {
		Name     string `json:"name"`
		ParentID *int64 `json:"parent_id"`
	}

	var req createFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		utils.WriteError(w, http.StatusBadRequest, 10017, "folder name cannot be empty")
		return
	}

	if req.ParentID != nil {
		if _, err := GetFolderByIDForOwner(user.ID, *req.ParentID); err != nil {
			utils.WriteError(w, http.StatusBadRequest, 10016, "parent folder not found")
			return
		}
	}

	exists, err := siblingNameExists(user.ID, req.ParentID, req.Name, nil, nil)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to create folder")
		return
	}
	if exists {
		utils.WriteError(w, http.StatusConflict, 10018, "name already exists in folder")
		return
	}

	folderID, err := createFolderInDB(user.ID, req.Name, req.ParentID)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10003, "failed to create folder")
		return
	}

	utils.WriteJSON(w, http.StatusCreated, models.APIResponse{
		Code:    0,
		Message: "folder created",
		Data: map[string]interface{}{
			"id":        folderID,
			"name":      req.Name,
			"parent_id": req.ParentID,
		},
	})
}

// FolderItemHandler 文件夹项处理（重命名、删除、列出子项）
func FolderItemHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	id, action, err := utils.ParseFolderAction(r.URL.Path)
	if err != nil {
		utils.WriteError(w, http.StatusNotFound, 10004, "not found")
		return
	}

	switch {
	case action == "rename" && r.Method == http.MethodPatch:
		if id == 0 {
			utils.WriteError(w, http.StatusBadRequest, 10028, "root folder cannot be renamed")
			return
		}
		renameFolderHandler(w, r, user.ID, id)
	case action == "download" && r.Method == http.MethodGet:
		if id == 0 {
			utils.WriteError(w, http.StatusBadRequest, 10031, "root folder cannot be downloaded")
			return
		}
		downloadFolderHandler(w, r, user.ID, id)
	case action == "children" && r.Method == http.MethodGet:
		folderChildrenHandler(w, r, user.ID, id)
	case action == "" && r.Method == http.MethodDelete:
		if id == 0 {
			utils.WriteError(w, http.StatusBadRequest, 10029, "root folder cannot be deleted")
			return
		}
		deleteFolderHandler(w, r, user.ID, id)
	default:
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
	}
}

func renameFolderHandler(w http.ResponseWriter, r *http.Request, ownerID int64, folderID int64) {
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
		utils.WriteError(w, http.StatusBadRequest, 10017, "folder name cannot be empty")
		return
	}

	folder, err := GetFolderByIDForOwner(ownerID, folderID)
	if err != nil {
		utils.WriteError(w, http.StatusNotFound, 10019, "folder not found")
		return
	}

	exists, err := siblingNameExists(ownerID, folder.ParentID, req.Name, &folderID, nil)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10020, "failed to rename folder")
		return
	}
	if exists {
		utils.WriteError(w, http.StatusConflict, 10018, "name already exists in folder")
		return
	}

	if err := updateFolderNameInDB(ownerID, folderID, req.Name); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10020, "failed to rename folder")
		return
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{
		Code:    0,
		Message: "folder renamed",
		Data: map[string]interface{}{
			"id":   folderID,
			"name": req.Name,
		},
	})
}

func deleteFolderHandler(w http.ResponseWriter, _ *http.Request, ownerID int64, folderID int64) {
	if _, err := GetFolderByIDForOwner(ownerID, folderID); err != nil {
		utils.WriteError(w, http.StatusNotFound, 10019, "folder not found")
		return
	}

	hasChildren, err := hasChildrenInFolder(ownerID, folderID)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10021, "failed to delete folder")
		return
	}
	if hasChildren {
		utils.WriteError(w, http.StatusConflict, 10022, "folder is not empty")
		return
	}

	if err := deleteFolderInDB(ownerID, folderID); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10021, "failed to delete folder")
		return
	}
	if err := db.RevokeSharesByNode(ownerID, "folder", strconv.FormatInt(folderID, 10)); err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10030, "failed to update related shares")
		return
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "folder deleted"})
}

func folderChildrenHandler(w http.ResponseWriter, _ *http.Request, ownerID int64, folderID int64) {
	var parentID *int64
	if folderID != 0 {
		if _, err := GetFolderByIDForOwner(ownerID, folderID); err != nil {
			utils.WriteError(w, http.StatusNotFound, 10019, "folder not found")
			return
		}
		parentID = &folderID
	}

	children, err := listChildrenByParent(ownerID, parentID)
	if err != nil {
		utils.WriteError(w, http.StatusInternalServerError, 10023, "failed to list children")
		return
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "ok", Data: children})
}

func downloadFolderHandler(w http.ResponseWriter, _ *http.Request, ownerID int64, folderID int64) {
	folder, err := GetFolderByIDForOwner(ownerID, folderID)
	if err != nil {
		utils.WriteError(w, http.StatusNotFound, 10019, "folder not found")
		return
	}

	zipName := folder.Name + ".zip"
	w.Header().Set("Content-Disposition", "attachment; filename=\""+zipName+"\"")
	w.Header().Set("Content-Type", "application/zip")

	zw := zip.NewWriter(w)
	if err := addFolderToZip(zw, ownerID, folderID, folder.Name); err != nil {
		_ = zw.Close()
		return
	}
	_ = zw.Close()
}

func addFolderToZip(zw *zip.Writer, ownerID int64, folderID int64, basePath string) error {
	if basePath != "" {
		dirPath := path.Clean(basePath) + "/"
		if _, err := zw.Create(dirPath); err != nil {
			return err
		}
	}

	children, err := listChildrenByParent(ownerID, &folderID)
	if err != nil {
		return err
	}

	for _, item := range children {
		typeVal, _ := item["type"].(string)
		switch typeVal {
		case "folder":
			subID, ok := item["id"].(int64)
			if !ok {
				return fmt.Errorf("invalid folder id type")
			}
			subName, ok := item["name"].(string)
			if !ok {
				return fmt.Errorf("invalid folder name type")
			}
			nextBase := path.Join(basePath, subName)
			if err := addFolderToZip(zw, ownerID, subID, nextBase); err != nil {
				return err
			}
		case "file":
			fileID, ok := item["id"].(string)
			if !ok {
				return fmt.Errorf("invalid file id type")
			}
			fileName, ok := item["name"].(string)
			if !ok {
				return fmt.Errorf("invalid file name type")
			}
			rec, err := GetFileRecordForOwner(fileID, ownerID)
			if err != nil {
				return err
			}
			f, err := os.Open(rec.DiskPath)
			if err != nil {
				return err
			}
			info, err := f.Stat()
			if err != nil {
				_ = f.Close()
				return err
			}
			h, err := zip.FileInfoHeader(info)
			if err != nil {
				_ = f.Close()
				return err
			}
			h.Name = path.Join(basePath, fileName)
			h.Method = zip.Deflate
			w, err := zw.CreateHeader(h)
			if err != nil {
				_ = f.Close()
				return err
			}
			if _, err := io.Copy(w, f); err != nil {
				_ = f.Close()
				return err
			}
			_ = f.Close()
		}
	}

	return nil
}

// MoveNodeHandler 移动节点（文件或文件夹）
func MoveNodeHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.CurrentUser(r)
	if !ok {
		utils.WriteError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	if r.Method != http.MethodPost {
		utils.WriteError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	type moveRequest struct {
		NodeType       string `json:"node_type"`
		NodeID         string `json:"node_id"`
		TargetFolderID *int64 `json:"target_folder_id"`
	}

	var req moveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.WriteError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.NodeType = strings.TrimSpace(req.NodeType)

	if req.TargetFolderID != nil {
		if _, err := GetFolderByIDForOwner(user.ID, *req.TargetFolderID); err != nil {
			utils.WriteError(w, http.StatusBadRequest, 10016, "target folder not found")
			return
		}
	}

	switch req.NodeType {
	case "file":
		if err := moveFileNode(user.ID, req.NodeID, req.TargetFolderID); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				utils.WriteError(w, http.StatusNotFound, 10005, "file not found")
				return
			}
			if errors.Is(err, errNameConflict) {
				utils.WriteError(w, http.StatusConflict, 10018, "name already exists in folder")
				return
			}
			utils.WriteError(w, http.StatusInternalServerError, 10024, "failed to move node")
			return
		}
	case "folder":
		folderID, err := parseInt64(strings.TrimSpace(req.NodeID))
		if err != nil {
			utils.WriteError(w, http.StatusBadRequest, 10025, "invalid folder id")
			return
		}
		if err := moveFolderNode(user.ID, folderID, req.TargetFolderID); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				utils.WriteError(w, http.StatusNotFound, 10019, "folder not found")
				return
			}
			if errors.Is(err, errInvalidMove) {
				utils.WriteError(w, http.StatusBadRequest, 10026, "invalid move target")
				return
			}
			if errors.Is(err, errNameConflict) {
				utils.WriteError(w, http.StatusConflict, 10018, "name already exists in folder")
				return
			}
			utils.WriteError(w, http.StatusInternalServerError, 10024, "failed to move node")
			return
		}
	default:
		utils.WriteError(w, http.StatusBadRequest, 10027, "unsupported node_type")
		return
	}

	utils.WriteJSON(w, http.StatusOK, models.APIResponse{Code: 0, Message: "moved"})
}

// 文件夹数据库操作函数

func GetFolderByIDForOwner(ownerID int64, folderID int64) (*models.FolderRecord, error) {
	var rec models.FolderRecord
	var parentID sql.NullInt64
	var createdAtUnix int64

	err := db.DB.QueryRow(
		"SELECT id, name, owner_id, parent_id, created_at_unix FROM folders WHERE id = ? AND owner_id = ?",
		folderID,
		ownerID,
	).Scan(&rec.ID, &rec.Name, &rec.OwnerID, &parentID, &createdAtUnix)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	rec.CreatedAt = time.Unix(createdAtUnix, 0)
	if parentID.Valid {
		v := parentID.Int64
		rec.ParentID = &v
	}
	return &rec, nil
}

func createFolderInDB(ownerID int64, name string, parentID *int64) (int64, error) {
	result, err := db.DB.Exec(
		"INSERT INTO folders(owner_id, name, parent_id, created_at_unix, updated_at_unix) VALUES (?, ?, ?, ?, ?)",
		ownerID,
		name,
		utils.Int64Value(parentID),
		time.Now().Unix(),
		time.Now().Unix(),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func updateFolderNameInDB(ownerID int64, folderID int64, name string) error {
	result, err := db.DB.Exec(
		"UPDATE folders SET name = ?, updated_at_unix = ? WHERE id = ? AND owner_id = ?",
		name,
		time.Now().Unix(),
		folderID,
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

func deleteFolderInDB(ownerID int64, folderID int64) error {
	result, err := db.DB.Exec("DELETE FROM folders WHERE id = ? AND owner_id = ?", folderID, ownerID)
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

func hasChildrenInFolder(ownerID int64, folderID int64) (bool, error) {
	var countFolders int64
	if err := db.DB.QueryRow(
		"SELECT COUNT(1) FROM folders WHERE owner_id = ? AND parent_id = ?",
		ownerID,
		folderID,
	).Scan(&countFolders); err != nil {
		return false, err
	}
	if countFolders > 0 {
		return true, nil
	}

	var countFiles int64
	if err := db.DB.QueryRow(
		"SELECT COUNT(1) FROM files WHERE owner_id = ? AND parent_folder_id = ?",
		ownerID,
		folderID,
	).Scan(&countFiles); err != nil {
		return false, err
	}
	return countFiles > 0, nil
}

func siblingNameExists(ownerID int64, parentID *int64, name string, excludeFolderID *int64, excludeFileID *string) (bool, error) {
	parent := utils.Int64Value(parentID)

	queryFolders := "SELECT COUNT(1) FROM folders WHERE owner_id = ? AND name = ? AND ((parent_id IS NULL AND ? IS NULL) OR parent_id = ?)"
	argsFolders := []interface{}{ownerID, name, parent, parent}
	if excludeFolderID != nil {
		queryFolders += " AND id <> ?"
		argsFolders = append(argsFolders, *excludeFolderID)
	}
	var folderCount int64
	if err := db.DB.QueryRow(queryFolders, argsFolders...).Scan(&folderCount); err != nil {
		return false, err
	}
	if folderCount > 0 {
		return true, nil
	}

	queryFiles := "SELECT COUNT(1) FROM files WHERE owner_id = ? AND name = ? AND ((parent_folder_id IS NULL AND ? IS NULL) OR parent_folder_id = ?)"
	argsFiles := []interface{}{ownerID, name, parent, parent}
	if excludeFileID != nil {
		queryFiles += " AND id <> ?"
		argsFiles = append(argsFiles, *excludeFileID)
	}
	var fileCount int64
	if err := db.DB.QueryRow(queryFiles, argsFiles...).Scan(&fileCount); err != nil {
		return false, err
	}

	return fileCount > 0, nil
}

func listChildrenByParent(ownerID int64, parentID *int64) ([]map[string]interface{}, error) {
	items := make([]map[string]interface{}, 0)

	queryFolders := "SELECT id, name, created_at_unix FROM folders WHERE owner_id = ? AND ((parent_id IS NULL AND ? IS NULL) OR parent_id = ?) ORDER BY name ASC"
	fRows, err := db.DB.Query(queryFolders, ownerID, utils.Int64Value(parentID), utils.Int64Value(parentID))
	if err != nil {
		return nil, err
	}
	defer fRows.Close()

	for fRows.Next() {
		var id int64
		var name string
		var created int64
		if err := fRows.Scan(&id, &name, &created); err != nil {
			return nil, err
		}
		var parentCopy interface{}
		if parentID != nil {
			parentCopy = *parentID
		}
		items = append(items, map[string]interface{}{
			"type":       "folder",
			"id":         id,
			"name":       name,
			"parent_id":  parentCopy,
			"created_at": time.Unix(created, 0),
		})
	}
	if err := fRows.Err(); err != nil {
		return nil, err
	}

	db.FilesMu.RLock()
	for _, rec := range db.FilesByID {
		if rec.OwnerID != ownerID {
			continue
		}
		if parentID == nil {
			if rec.ParentID != nil {
				continue
			}
		} else {
			if rec.ParentID == nil || *rec.ParentID != *parentID {
				continue
			}
		}

		items = append(items, map[string]interface{}{
			"type":         "file",
			"id":           rec.ID,
			"name":         rec.Name,
			"size_bytes":   rec.SizeBytes,
			"parent_id":    rec.ParentID,
			"created_at":   rec.CreatedAt,
			"download_url": "/api/v1/files/" + rec.ID + "/download",
		})
	}
	db.FilesMu.RUnlock()

	return items, nil
}

func moveFileNode(ownerID int64, fileID string, targetParentID *int64) error {
	db.FilesMu.Lock()
	rec, ok := db.FilesByID[fileID]
	if !ok || rec.OwnerID != ownerID {
		db.FilesMu.Unlock()
		return os.ErrNotExist
	}

	exists, err := siblingNameExists(ownerID, targetParentID, rec.Name, nil, &fileID)
	if err != nil {
		db.FilesMu.Unlock()
		return err
	}
	if exists {
		db.FilesMu.Unlock()
		return errNameConflict
	}

	if err := MoveFileToFolderInDB(fileID, ownerID, targetParentID); err != nil {
		db.FilesMu.Unlock()
		return err
	}
	rec.ParentID = targetParentID
	db.FilesMu.Unlock()
	return nil
}

func moveFolderNode(ownerID int64, folderID int64, targetParentID *int64) error {
	folder, err := GetFolderByIDForOwner(ownerID, folderID)
	if err != nil {
		return err
	}

	if targetParentID != nil {
		if *targetParentID == folderID {
			return errInvalidMove
		}
		if _, err := GetFolderByIDForOwner(ownerID, *targetParentID); err != nil {
			return os.ErrNotExist
		}
		if isDesc, err := isDescendantFolder(ownerID, folderID, *targetParentID); err != nil {
			return err
		} else if isDesc {
			return errInvalidMove
		}
	}

	exists, err := siblingNameExists(ownerID, targetParentID, folder.Name, &folderID, nil)
	if err != nil {
		return err
	}
	if exists {
		return errNameConflict
	}

	return moveFolderToParentInDB(ownerID, folderID, targetParentID)
}

func isDescendantFolder(ownerID int64, ancestorID int64, candidateID int64) (bool, error) {
	current := &candidateID
	for current != nil {
		if *current == ancestorID {
			return true, nil
		}

		var parent sql.NullInt64
		err := db.DB.QueryRow("SELECT parent_id FROM folders WHERE id = ? AND owner_id = ?", *current, ownerID).Scan(&parent)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, os.ErrNotExist
			}
			return false, err
		}
		if parent.Valid {
			v := parent.Int64
			current = &v
		} else {
			current = nil
		}
	}
	return false, nil
}

func moveFolderToParentInDB(ownerID int64, folderID int64, parentID *int64) error {
	result, err := db.DB.Exec(
		"UPDATE folders SET parent_id = ?, updated_at_unix = ? WHERE id = ? AND owner_id = ?",
		utils.Int64Value(parentID),
		time.Now().Unix(),
		folderID,
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

// parseInt64 解析字符串为 int64
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

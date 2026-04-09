package main // 可执行程序入口

import ( // 依赖
	"context"
	"crypto/rand" // 生成随机字节，用来创建文件 id（避免重复）
	"crypto/sha256"
	"database/sql"
	"encoding/hex"   // 把随机字节转成十六进制字符串，便于作为可读 id
	"encoding/json"  // 输出 json 格式
	"errors"         // 判断错误类型，例如是否是文件不存在
	"io"             // 处理流式读写（上传文件时按流复制）
	"log"            // 日志输出
	"mime/multipart" // 处理 multipart/form-data 上传请求
	"net/http"       // http 服务器
	"os"             // 环境变量读取
	"path/filepath"  // 路径拼接和文件名清理，避免路径注入
	"strconv"
	"strings" // 字符串处理（trim、前缀判断、分割）
	"sync"    // 并发锁，保护 map 读写安全
	"time"    // 记录创建时间

	_ "github.com/go-sql-driver/mysql"
)

// apiResponse 统一响应体：第一步 health 和第二步文件接口都复用该结构。
type apiResponse struct {
	Code    int         `json:"code"`           // 状态码，0 表示成功，非 0 表示失败
	Message string      `json:"message"`        // 说明
	Data    interface{} `json:"data,omitempty"` // 可选数据体
}

// fileRecord 是第二步新增的文件元数据模型。
// 为了先快速演示，元数据先放内存，文件内容落磁盘。
type fileRecord struct {
	ID        string    `json:"id"`         // 文件唯一 id
	Name      string    `json:"name"`       // 文件名（可重命名）
	SizeBytes int64     `json:"size_bytes"` // 文件大小
	CreatedAt time.Time `json:"created_at"` // 创建时间
	OwnerID   int64     `json:"-"`          // 所属用户 id
	ParentID  *int64    `json:"-"`          // 所属文件夹（nil 表示根目录）
	DiskPath  string    `json:"-"`          // 磁盘真实路径，不对外返回
}

type folderRecord struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	OwnerID   int64     `json:"-"`
	ParentID  *int64    `json:"parent_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type authUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type contextKey string

var (
	// storageDir 第二步新增：上传文件落盘目录。
	storageDir = "data/uploads" // 上传文件保存目录
	// dbConn 是 MySQL 连接，用于持久化文件元数据。
	dbConn *sql.DB
	// filesMu + filesByID 第二步新增：内存索引与并发保护。
	// RWMutex 含义：
	// 1) RLock/RUnlock 用于读操作，可并发读。
	// 2) Lock/Unlock 用于写操作，写时独占。
	filesMu sync.RWMutex
	// filesByID 作用：
	// 1) key 是文件 id。
	// 2) value 是文件元数据（名字、大小、磁盘路径等）。
	// 当前作为读缓存，服务重启时会从 MySQL 自动恢复。
	filesByID = make(map[string]*fileRecord)
)

const (
	authUserKey  contextKey    = "auth_user"
	authTokenKey contextKey    = "auth_token"
	tokenTTL     time.Duration = 7 * 24 * time.Hour
)

var (
	errNameConflict = errors.New("name conflict")
	errInvalidMove  = errors.New("invalid move")
)

func main() {
	// 读取运行端口，未配置时使用默认值 8080。
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// 第二步新增：启动时确保本地存储目录存在。
	// 目的：避免上传时目录不存在导致 os.Create 失败。
	if err := os.MkdirAll(storageDir, 0o755); err != nil {
		log.Fatalf("failed to init storage dir: %v", err)
	}

	var err error
	dbConn, err = initMySQL()
	if err != nil {
		log.Fatalf("failed to init mysql: %v", err)
	}
	defer dbConn.Close()

	if err := loadFilesFromDB(); err != nil {
		log.Fatalf("failed to load file index from mysql: %v", err)
	}

	// 第一步接口：健康检查。
	http.HandleFunc("/health", healthHandler)
	// 第三步接口：鉴权与用户最小集。
	http.HandleFunc("/api/v1/auth/register", registerHandler)
	http.HandleFunc("/api/v1/auth/login", loginHandler)
	http.HandleFunc("/api/v1/auth/logout", authMiddleware(logoutHandler))
	http.HandleFunc("/api/v1/users/me", authMiddleware(userMeHandler))
	// 第二步接口：上传、列表、单文件动作路由分发。
	http.HandleFunc("/api/v1/files/upload", authMiddleware(uploadHandler))   // 上传
	http.HandleFunc("/api/v1/files", authMiddleware(filesCollectionHandler)) // 列表
	http.HandleFunc("/api/v1/files/", authMiddleware(fileItemHandler))       // 下载/重命名/删除
	// 第四步接口：文件夹与节点操作。
	http.HandleFunc("/api/v1/folders", authMiddleware(foldersCollectionHandler))
	http.HandleFunc("/api/v1/folders/", authMiddleware(folderItemHandler))
	http.HandleFunc("/api/v1/nodes/move", authMiddleware(moveNodeHandler))

	// 启动服务器，监听指定端口。
	// ListenAndServe 会阻塞在这里，直到服务退出或报错。
	log.Printf("server is starting at :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, token, err := authenticateRequest(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, 10010, "unauthorized")
			return
		}

		ctx := context.WithValue(r.Context(), authUserKey, user)
		ctx = context.WithValue(ctx, authTokenKey, token)
		next(w, r.WithContext(ctx))
	}
}

func currentUser(r *http.Request) (authUser, bool) {
	v := r.Context().Value(authUserKey)
	if v == nil {
		return authUser{}, false
	}
	user, ok := v.(authUser)
	return user, ok
}

func currentToken(r *http.Request) (string, bool) {
	v := r.Context().Value(authTokenKey)
	if v == nil {
		return "", false
	}
	token, ok := v.(string)
	return token, ok
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	type registerRequest struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Password) < 6 {
		writeError(w, http.StatusBadRequest, 10011, "invalid username or password")
		return
	}

	salt, err := generateID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, 10003, "failed to create user")
		return
	}
	hashed := hashPassword(req.Password, salt)

	result, err := dbConn.Exec(
		"INSERT INTO users(username, password_salt, password_hash, created_at_unix) VALUES (?, ?, ?, ?)",
		req.Username,
		salt,
		hashed,
		time.Now().Unix(),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			writeError(w, http.StatusConflict, 10012, "username already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, 10003, "failed to create user")
		return
	}

	uid, _ := result.LastInsertId()
	writeJSON(w, http.StatusCreated, apiResponse{
		Code:    0,
		Message: "registered",
		Data: map[string]interface{}{
			"id":       uid,
			"username": req.Username,
		},
	})
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	type loginRequest struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)

	var userID int64
	var username, salt, passwordHash string
	err := dbConn.QueryRow(
		"SELECT id, username, password_salt, password_hash FROM users WHERE username = ?",
		req.Username,
	).Scan(&userID, &username, &salt, &passwordHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, 10010, "invalid credentials")
			return
		}
		writeError(w, http.StatusInternalServerError, 10013, "failed to login")
		return
	}

	if hashPassword(req.Password, salt) != passwordHash {
		writeError(w, http.StatusUnauthorized, 10010, "invalid credentials")
		return
	}

	token, err := generateToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, 10013, "failed to login")
		return
	}
	expiresAt := time.Now().Add(tokenTTL).Unix()
	_, err = dbConn.Exec(
		"INSERT INTO auth_tokens(token, user_id, expires_at_unix, revoked, created_at_unix) VALUES (?, ?, ?, 0, ?)",
		token,
		userID,
		expiresAt,
		time.Now().Unix(),
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 10013, "failed to login")
		return
	}

	writeJSON(w, http.StatusOK, apiResponse{
		Code:    0,
		Message: "logged in",
		Data: map[string]interface{}{
			"token":      token,
			"expires_at": time.Unix(expiresAt, 0),
			"user": map[string]interface{}{
				"id":       userID,
				"username": username,
			},
		},
	})
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	token, ok := currentToken(r)
	if !ok || token == "" {
		writeError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	_, err := dbConn.Exec("UPDATE auth_tokens SET revoked = 1 WHERE token = ?", token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 10014, "failed to logout")
		return
	}

	writeJSON(w, http.StatusOK, apiResponse{Code: 0, Message: "logged out"})
}

func userMeHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, apiResponse{
			Code:    0,
			Message: "ok",
			Data: map[string]interface{}{
				"id":       user.ID,
				"username": user.Username,
			},
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
	}
}

func authenticateRequest(r *http.Request) (authUser, string, error) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		return authUser{}, "", errors.New("missing bearer token")
	}

	token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if token == "" {
		return authUser{}, "", errors.New("empty bearer token")
	}

	var user authUser
	err := dbConn.QueryRow(
		`SELECT u.id, u.username
		 FROM auth_tokens t
		 JOIN users u ON u.id = t.user_id
		 WHERE t.token = ? AND t.revoked = 0 AND t.expires_at_unix > ?`,
		token,
		time.Now().Unix(),
	).Scan(&user.ID, &user.Username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return authUser{}, "", errors.New("invalid token")
		}
		return authUser{}, "", err
	}

	return user, token, nil
}

// healthHandler：w 是响应写入器，r 是请求对象。
func healthHandler(w http.ResponseWriter, r *http.Request) {
	// 仅允许 GET；不满足时返回 405（方法不被允许）。
	// 这样做可以避免其它方法误调用。
	if r.Method != http.MethodGet {
		// 这里的常量是 405，表示方法不被允许。
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, apiResponse{
		Code:    0,
		Message: "ok",
		Data: map[string]string{
			"status": "up",
		},
	})
}

// uploadHandler 第二步新增：接收 multipart/form-data 中的 file 字段并落盘。
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	// 上传接口只允许 POST。
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	// FormFile 会返回上传文件流 src 与文件头 hdr。
	// 约定：前端必须用字段名 file 传文件。
	src, hdr, err := r.FormFile("file") // 从 multipart 表单读取 file 字段
	if err != nil {
		writeError(w, http.StatusBadRequest, 10002, "missing file form field")
		return
	}
	defer src.Close() // 及时关闭上传流

	parentID, err := parseOptionalInt64(strings.TrimSpace(r.FormValue("parent_id")))
	if err != nil {
		writeError(w, http.StatusBadRequest, 10015, "invalid parent_id")
		return
	}
	if parentID != nil {
		if _, err := getFolderByIDForOwner(user.ID, *parentID); err != nil {
			writeError(w, http.StatusBadRequest, 10016, "parent folder not found")
			return
		}
	}

	record, err := saveUploadedFile(src, hdr, user.ID, parentID) // 保存到本地并写入内存索引
	if err != nil {
		writeError(w, http.StatusInternalServerError, 10003, "failed to save file")
		return
	}

	// 上传成功返回文件 id，后续下载/重命名/删除都依赖该 id。
	// download_url 是为了让你调试时可以直接复制访问。
	writeJSON(w, http.StatusCreated, apiResponse{
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

// filesCollectionHandler 第二步新增：返回当前内存索引里的全部文件元数据。
func filesCollectionHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	// 列表接口只允许 GET。
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	filesMu.RLock()                                            // 读锁，允许并发读取
	items := make([]map[string]interface{}, 0, len(filesByID)) // 预分配容量减少扩容开销
	// 遍历内存索引，组装响应数组。
	for _, rec := range filesByID {
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
	filesMu.RUnlock() // 读完释放锁

	// 列表接口输出 download_url，便于直接演示下载链路。
	writeJSON(w, http.StatusOK, apiResponse{
		Code:    0,
		Message: "ok",
		Data:    items,
	})
}

// fileItemHandler 第二步新增：把 /api/v1/files/{id}/{action} 分发到具体处理器。
func fileItemHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	id, action, err := parseFileAction(r.URL.Path) // 从路径中拆出 id 和动作
	if err != nil {
		writeError(w, http.StatusNotFound, 10004, "not found")
		return
	}

	// 按 action + method 分发到具体处理器。
	// 例如：
	// 1) GET /api/v1/files/{id}/download
	// 2) PATCH /api/v1/files/{id}/rename
	// 3) DELETE /api/v1/files/{id}
	switch {
	case action == "download" && r.Method == http.MethodGet:
		downloadHandler(w, r, id, user.ID)
	case action == "rename" && r.Method == http.MethodPatch:
		renameHandler(w, r, id, user.ID)
	case action == "" && r.Method == http.MethodDelete:
		deleteHandler(w, r, id, user.ID)
	default:
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
	}
}

func foldersCollectionHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	type createFolderRequest struct {
		Name     string `json:"name"`
		ParentID *int64 `json:"parent_id"`
	}

	var req createFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, 10017, "folder name cannot be empty")
		return
	}

	if req.ParentID != nil {
		if _, err := getFolderByIDForOwner(user.ID, *req.ParentID); err != nil {
			writeError(w, http.StatusBadRequest, 10016, "parent folder not found")
			return
		}
	}

	exists, err := siblingNameExists(user.ID, req.ParentID, req.Name, nil, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 10003, "failed to create folder")
		return
	}
	if exists {
		writeError(w, http.StatusConflict, 10018, "name already exists in folder")
		return
	}

	folderID, err := createFolderInDB(user.ID, req.Name, req.ParentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 10003, "failed to create folder")
		return
	}

	writeJSON(w, http.StatusCreated, apiResponse{
		Code:    0,
		Message: "folder created",
		Data: map[string]interface{}{
			"id":        folderID,
			"name":      req.Name,
			"parent_id": req.ParentID,
		},
	})
}

func folderItemHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	id, action, err := parseFolderAction(r.URL.Path)
	if err != nil {
		writeError(w, http.StatusNotFound, 10004, "not found")
		return
	}

	switch {
	case action == "rename" && r.Method == http.MethodPatch:
		if id == 0 {
			writeError(w, http.StatusBadRequest, 10028, "root folder cannot be renamed")
			return
		}
		renameFolderHandler(w, r, user.ID, id)
	case action == "children" && r.Method == http.MethodGet:
		folderChildrenHandler(w, r, user.ID, id)
	case action == "" && r.Method == http.MethodDelete:
		if id == 0 {
			writeError(w, http.StatusBadRequest, 10029, "root folder cannot be deleted")
			return
		}
		deleteFolderHandler(w, r, user.ID, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
	}
}

func renameFolderHandler(w http.ResponseWriter, r *http.Request, ownerID int64, folderID int64) {
	type renameRequest struct {
		Name string `json:"name"`
	}

	var req renameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, 10017, "folder name cannot be empty")
		return
	}

	folder, err := getFolderByIDForOwner(ownerID, folderID)
	if err != nil {
		writeError(w, http.StatusNotFound, 10019, "folder not found")
		return
	}

	exists, err := siblingNameExists(ownerID, folder.ParentID, req.Name, &folderID, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 10020, "failed to rename folder")
		return
	}
	if exists {
		writeError(w, http.StatusConflict, 10018, "name already exists in folder")
		return
	}

	if err := updateFolderNameInDB(ownerID, folderID, req.Name); err != nil {
		writeError(w, http.StatusInternalServerError, 10020, "failed to rename folder")
		return
	}

	writeJSON(w, http.StatusOK, apiResponse{
		Code:    0,
		Message: "folder renamed",
		Data: map[string]interface{}{
			"id":   folderID,
			"name": req.Name,
		},
	})
}

func deleteFolderHandler(w http.ResponseWriter, _ *http.Request, ownerID int64, folderID int64) {
	if _, err := getFolderByIDForOwner(ownerID, folderID); err != nil {
		writeError(w, http.StatusNotFound, 10019, "folder not found")
		return
	}

	hasChildren, err := hasChildrenInFolder(ownerID, folderID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 10021, "failed to delete folder")
		return
	}
	if hasChildren {
		writeError(w, http.StatusConflict, 10022, "folder is not empty")
		return
	}

	if err := deleteFolderInDB(ownerID, folderID); err != nil {
		writeError(w, http.StatusInternalServerError, 10021, "failed to delete folder")
		return
	}

	writeJSON(w, http.StatusOK, apiResponse{Code: 0, Message: "folder deleted"})
}

func folderChildrenHandler(w http.ResponseWriter, _ *http.Request, ownerID int64, folderID int64) {
	var parentID *int64
	if folderID != 0 {
		if _, err := getFolderByIDForOwner(ownerID, folderID); err != nil {
			writeError(w, http.StatusNotFound, 10019, "folder not found")
			return
		}
		parentID = &folderID
	}

	children, err := listChildrenByParent(ownerID, parentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 10023, "failed to list children")
		return
	}

	writeJSON(w, http.StatusOK, apiResponse{Code: 0, Message: "ok", Data: children})
}

func moveNodeHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, 10010, "unauthorized")
		return
	}

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	type moveRequest struct {
		NodeType       string `json:"node_type"`
		NodeID         string `json:"node_id"`
		TargetFolderID *int64 `json:"target_folder_id"`
	}

	var req moveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.NodeType = strings.TrimSpace(req.NodeType)

	if req.TargetFolderID != nil {
		if _, err := getFolderByIDForOwner(user.ID, *req.TargetFolderID); err != nil {
			writeError(w, http.StatusBadRequest, 10016, "target folder not found")
			return
		}
	}

	switch req.NodeType {
	case "file":
		if err := moveFileNode(user.ID, req.NodeID, req.TargetFolderID); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, 10005, "file not found")
				return
			}
			if errors.Is(err, errNameConflict) {
				writeError(w, http.StatusConflict, 10018, "name already exists in folder")
				return
			}
			writeError(w, http.StatusInternalServerError, 10024, "failed to move node")
			return
		}
	case "folder":
		folderID, err := strconv.ParseInt(strings.TrimSpace(req.NodeID), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, 10025, "invalid folder id")
			return
		}
		if err := moveFolderNode(user.ID, folderID, req.TargetFolderID); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, 10019, "folder not found")
				return
			}
			if errors.Is(err, errInvalidMove) {
				writeError(w, http.StatusBadRequest, 10026, "invalid move target")
				return
			}
			if errors.Is(err, errNameConflict) {
				writeError(w, http.StatusConflict, 10018, "name already exists in folder")
				return
			}
			writeError(w, http.StatusInternalServerError, 10024, "failed to move node")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, 10027, "unsupported node_type")
		return
	}

	writeJSON(w, http.StatusOK, apiResponse{Code: 0, Message: "moved"})
}

// downloadHandler 第二步新增：按 id 找到磁盘文件并以附件方式下载。
func downloadHandler(w http.ResponseWriter, r *http.Request, id string, ownerID int64) {
	rec, err := getFileRecordForOwner(id, ownerID) // 根据 id 取元数据
	if err != nil {
		writeError(w, http.StatusNotFound, 10005, "file not found")
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename=\""+rec.Name+"\"") // 让浏览器按附件下载
	// ServeFile 会自动处理文件读取和输出，适合当前阶段快速实现下载。
	http.ServeFile(w, r, rec.DiskPath) // 直接把磁盘文件内容回给客户端
}

// renameHandler 第二步新增：修改文件显示名（仅改元数据，不改磁盘文件名）。
func renameHandler(w http.ResponseWriter, r *http.Request, id string, ownerID int64) {
	type renameRequest struct {
		Name string `json:"name"` // 目标新文件名
	}

	var req renameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { // 解析 json 请求体
		writeError(w, http.StatusBadRequest, 10006, "invalid json body")
		return
	}
	req.Name = strings.TrimSpace(req.Name) // 去除首尾空格，避免空名字
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, 10007, "name cannot be empty")
		return
	}

	filesMu.Lock() // 写锁，准备修改元数据
	rec, ok := filesByID[id]
	if !ok {
		filesMu.Unlock()
		writeError(w, http.StatusNotFound, 10005, "file not found")
		return
	}
	if rec.OwnerID != ownerID {
		filesMu.Unlock()
		writeError(w, http.StatusNotFound, 10005, "file not found")
		return
	}
	if err := updateFileNameInDB(id, ownerID, req.Name); err != nil {
		filesMu.Unlock()
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, 10005, "file not found")
			return
		}
		writeError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
		return
	}
	rec.Name = req.Name // 只改显示名，不改磁盘文件名
	filesMu.Unlock()

	writeJSON(w, http.StatusOK, apiResponse{
		Code:    0,
		Message: "renamed",
		Data: map[string]string{
			"id":   id,
			"name": req.Name,
		},
	})
}

// deleteHandler 第二步新增：先删内存索引，再删磁盘文件。
func deleteHandler(w http.ResponseWriter, _ *http.Request, id string, ownerID int64) {
	filesMu.RLock()
	rec, ok := filesByID[id]
	if !ok || rec.OwnerID != ownerID {
		filesMu.RUnlock()
		writeError(w, http.StatusNotFound, 10005, "file not found")
		return
	}
	filesMu.RUnlock()

	if err := os.Remove(rec.DiskPath); err != nil && !errors.Is(err, os.ErrNotExist) { // 再删磁盘文件
		writeError(w, http.StatusInternalServerError, 10008, "failed to delete file from disk")
		return
	}

	if err := deleteFileFromDB(id, ownerID); err != nil {
		writeError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
		return
	}

	filesMu.Lock()
	delete(filesByID, id)
	filesMu.Unlock()

	writeJSON(w, http.StatusOK, apiResponse{Code: 0, Message: "deleted"})
}

// saveUploadedFile 第二步新增：生成 id -> 流式写入磁盘 -> 建立内存索引。
func saveUploadedFile(src multipart.File, hdr *multipart.FileHeader, ownerID int64, parentID *int64) (*fileRecord, error) {
	id, err := generateID() // 生成随机文件 id
	if err != nil {
		return nil, err
	}

	name := filepath.Base(strings.TrimSpace(hdr.Filename)) // 防止路径穿越，只保留文件名
	if name == "" {
		name = "unnamed" // 没有文件名时给默认值
	}
	ext := filepath.Ext(name)                     // 保留扩展名
	diskPath := filepath.Join(storageDir, id+ext) // 磁盘保存路径用 id 命名

	dst, err := os.Create(diskPath) // 创建目标文件
	if err != nil {
		return nil, err
	}
	defer dst.Close() // 及时关闭文件句柄

	size, err := io.Copy(dst, src) // 流式拷贝，避免整文件进内存
	if err != nil {
		return nil, err
	}

	rec := &fileRecord{
		ID:        id,
		Name:      name,
		SizeBytes: size,
		CreatedAt: time.Now(),
		OwnerID:   ownerID,
		ParentID:  parentID,
		DiskPath:  diskPath,
	}

	filesMu.Lock() // 写入内存索引
	if err := insertFileToDB(rec); err != nil {
		filesMu.Unlock()
		_ = os.Remove(diskPath)
		return nil, err
	}
	filesByID[id] = rec
	filesMu.Unlock()

	return rec, nil
}

// getFileRecordForOwner 第二步新增：线程安全读取当前用户可访问的文件元数据。
func getFileRecordForOwner(id string, ownerID int64) (*fileRecord, error) {
	filesMu.RLock() // 读锁保护 map
	defer filesMu.RUnlock()
	rec, ok := filesByID[id]
	if !ok || rec.OwnerID != ownerID {
		return nil, os.ErrNotExist
	}
	return rec, nil
}

// parseFileAction 第二步新增：
// /api/v1/files/{id}         -> action=""（用于 DELETE）
// /api/v1/files/{id}/download -> action="download"
// /api/v1/files/{id}/rename   -> action="rename"
func parseFileAction(path string) (id string, action string, err error) {
	const prefix = "/api/v1/files/"
	// 路径必须以 /api/v1/files/ 开头。
	if !strings.HasPrefix(path, prefix) {
		return "", "", os.ErrNotExist
	}

	rest := strings.TrimPrefix(path, prefix)             // 去掉前缀
	parts := strings.Split(strings.Trim(rest, "/"), "/") // 例如 [id], [id download], [id rename]
	if len(parts) == 0 || parts[0] == "" {
		return "", "", os.ErrNotExist
	}
	if len(parts) == 1 {
		return parts[0], "", nil
	}
	if len(parts) == 2 {
		return parts[0], parts[1], nil
	}
	return "", "", os.ErrNotExist
}

func parseFolderAction(path string) (id int64, action string, err error) {
	const prefix = "/api/v1/folders/"
	if !strings.HasPrefix(path, prefix) {
		return 0, "", os.ErrNotExist
	}

	rest := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return 0, "", os.ErrNotExist
	}
	if parts[0] == "root" {
		if len(parts) == 2 {
			return 0, parts[1], nil
		}
		return 0, "", nil
	}

	id, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", os.ErrNotExist
	}

	if len(parts) == 1 {
		return id, "", nil
	}
	if len(parts) == 2 {
		return id, parts[1], nil
	}
	return 0, "", os.ErrNotExist
}

func parseOptionalInt64(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func int64Value(v *int64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func getFolderByIDForOwner(ownerID int64, folderID int64) (*folderRecord, error) {
	var rec folderRecord
	var parentID sql.NullInt64
	var createdAtUnix int64

	err := dbConn.QueryRow(
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
	result, err := dbConn.Exec(
		"INSERT INTO folders(owner_id, name, parent_id, created_at_unix, updated_at_unix) VALUES (?, ?, ?, ?, ?)",
		ownerID,
		name,
		int64Value(parentID),
		time.Now().Unix(),
		time.Now().Unix(),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func updateFolderNameInDB(ownerID int64, folderID int64, name string) error {
	result, err := dbConn.Exec(
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
	result, err := dbConn.Exec("DELETE FROM folders WHERE id = ? AND owner_id = ?", folderID, ownerID)
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
	if err := dbConn.QueryRow(
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
	if err := dbConn.QueryRow(
		"SELECT COUNT(1) FROM files WHERE owner_id = ? AND parent_folder_id = ?",
		ownerID,
		folderID,
	).Scan(&countFiles); err != nil {
		return false, err
	}
	return countFiles > 0, nil
}

func siblingNameExists(ownerID int64, parentID *int64, name string, excludeFolderID *int64, excludeFileID *string) (bool, error) {
	parent := int64Value(parentID)

	queryFolders := "SELECT COUNT(1) FROM folders WHERE owner_id = ? AND name = ? AND ((parent_id IS NULL AND ? IS NULL) OR parent_id = ?)"
	argsFolders := []interface{}{ownerID, name, parent, parent}
	if excludeFolderID != nil {
		queryFolders += " AND id <> ?"
		argsFolders = append(argsFolders, *excludeFolderID)
	}
	var folderCount int64
	if err := dbConn.QueryRow(queryFolders, argsFolders...).Scan(&folderCount); err != nil {
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
	if err := dbConn.QueryRow(queryFiles, argsFiles...).Scan(&fileCount); err != nil {
		return false, err
	}

	return fileCount > 0, nil
}

func listChildrenByParent(ownerID int64, parentID *int64) ([]map[string]interface{}, error) {
	items := make([]map[string]interface{}, 0)

	queryFolders := "SELECT id, name, created_at_unix FROM folders WHERE owner_id = ? AND ((parent_id IS NULL AND ? IS NULL) OR parent_id = ?) ORDER BY name ASC"
	fRows, err := dbConn.Query(queryFolders, ownerID, int64Value(parentID), int64Value(parentID))
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

	filesMu.RLock()
	for _, rec := range filesByID {
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
	filesMu.RUnlock()

	return items, nil

}

func moveFileNode(ownerID int64, fileID string, targetParentID *int64) error {
	filesMu.Lock()
	rec, ok := filesByID[fileID]
	if !ok || rec.OwnerID != ownerID {
		filesMu.Unlock()
		return os.ErrNotExist
	}

	exists, err := siblingNameExists(ownerID, targetParentID, rec.Name, nil, &fileID)
	if err != nil {
		filesMu.Unlock()
		return err
	}
	if exists {
		filesMu.Unlock()
		return errNameConflict
	}

	if err := moveFileToFolderInDB(fileID, ownerID, targetParentID); err != nil {
		filesMu.Unlock()
		return err
	}
	rec.ParentID = targetParentID
	filesMu.Unlock()
	return nil
}

func moveFolderNode(ownerID int64, folderID int64, targetParentID *int64) error {
	folder, err := getFolderByIDForOwner(ownerID, folderID)
	if err != nil {
		return err
	}

	if targetParentID != nil {
		if *targetParentID == folderID {
			return errInvalidMove
		}
		if _, err := getFolderByIDForOwner(ownerID, *targetParentID); err != nil {
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
		err := dbConn.QueryRow("SELECT parent_id FROM folders WHERE id = ? AND owner_id = ?", *current, ownerID).Scan(&parent)
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

// generateID 第二步新增：生成 24 位十六进制随机 id。
func generateID() (string, error) {
	b := make([]byte, 12) // 12 字节 = 24 位十六进制字符串
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashPassword(password string, salt string) string {
	v := sha256.Sum256([]byte(salt + ":" + password))
	return hex.EncodeToString(v[:])
}

// writeJSON 恢复第一步解释口径：
// 1) 先设置 Header（告诉客户端返回 json + utf-8）
// 2) 再写状态码
// 3) 最后 json.NewEncoder(w).Encode 把结构体编码成 json 写入响应体
func writeJSON(w http.ResponseWriter, status int, body apiResponse) {
	// 响应头类型：响应体返回 json，用 utf-8 编码。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// json.NewEncoder(w) 创建一个 json 编码器，Encode 方法将结构体编码为 json 并写入响应体。
	_ = json.NewEncoder(w).Encode(body)
}

// writeError 第二步新增：统一错误返回，避免每个 handler 重复写样板。
func writeError(w http.ResponseWriter, status int, code int, message string) {
	writeJSON(w, status, apiResponse{Code: code, Message: message}) // 统一错误出口
}

// initMySQL 初始化数据库连接并确保 files 表存在。
// 需要配置环境变量 MYSQL_DSN，例如：
// root:password@tcp(127.0.0.1:3306)/netdisk?charset=utf8mb4&loc=Local
func initMySQL() (*sql.DB, error) {
	dsn := strings.TrimSpace(os.Getenv("MYSQL_DSN"))
	if dsn == "" {
		return nil, errors.New("MYSQL_DSN is empty")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}

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
		if _, err := db.Exec(ddl); err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	const ensureOwnerColumn = `
ALTER TABLE files
ADD COLUMN IF NOT EXISTS owner_id BIGINT NULL
`
	if _, err := db.Exec(ensureOwnerColumn); err != nil {
		// 兼容不支持 IF NOT EXISTS 的 MySQL 版本。
		if _, err2 := db.Exec("ALTER TABLE files ADD COLUMN owner_id BIGINT NULL"); err2 != nil {
			if !strings.Contains(strings.ToLower(err2.Error()), "duplicate column") {
				_ = db.Close()
				return nil, err2
			}
		}
	}

	const ensureParentFolderColumn = `
ALTER TABLE files
ADD COLUMN IF NOT EXISTS parent_folder_id BIGINT NULL
`
	if _, err := db.Exec(ensureParentFolderColumn); err != nil {
		if _, err2 := db.Exec("ALTER TABLE files ADD COLUMN parent_folder_id BIGINT NULL"); err2 != nil {
			if !strings.Contains(strings.ToLower(err2.Error()), "duplicate column") {
				_ = db.Close()
				return nil, err2
			}
		}
	}

	return db, nil
}

// loadFilesFromDB 启动时读取 MySQL 元数据并恢复到内存索引。
func loadFilesFromDB() error {
	rows, err := dbConn.Query("SELECT id, name, size_bytes, created_at_unix, owner_id, parent_folder_id, disk_path FROM files")
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

	filesMu.Lock()
	defer filesMu.Unlock()
	filesByID = make(map[string]*fileRecord, len(items))

	for _, r := range items {
		if _, err := os.Stat(r.diskPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				_, _ = dbConn.Exec("DELETE FROM files WHERE id = ?", r.id)
				continue
			}
			return err
		}

		var parentID *int64
		if r.parentID.Valid {
			v := r.parentID.Int64
			parentID = &v
		}

		filesByID[r.id] = &fileRecord{
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

func insertFileToDB(rec *fileRecord) error {
	_, err := dbConn.Exec(
		"INSERT INTO files(id, name, size_bytes, created_at_unix, owner_id, parent_folder_id, disk_path) VALUES (?, ?, ?, ?, ?, ?, ?)",
		rec.ID,
		rec.Name,
		rec.SizeBytes,
		rec.CreatedAt.Unix(),
		rec.OwnerID,
		int64Value(rec.ParentID),
		rec.DiskPath,
	)
	return err
}

func updateFileNameInDB(id string, ownerID int64, name string) error {
	result, err := dbConn.Exec("UPDATE files SET name = ? WHERE id = ? AND owner_id = ?", name, id, ownerID)
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
	_, err := dbConn.Exec("DELETE FROM files WHERE id = ? AND owner_id = ?", id, ownerID)
	return err
}

func moveFileToFolderInDB(id string, ownerID int64, parentID *int64) error {
	result, err := dbConn.Exec(
		"UPDATE files SET parent_folder_id = ? WHERE id = ? AND owner_id = ?",
		int64Value(parentID),
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

func moveFolderToParentInDB(ownerID int64, folderID int64, parentID *int64) error {
	result, err := dbConn.Exec(
		"UPDATE folders SET parent_id = ?, updated_at_unix = ? WHERE id = ? AND owner_id = ?",
		int64Value(parentID),
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

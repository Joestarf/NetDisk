package main // 可执行程序入口

import ( // 依赖
	"crypto/rand" // 生成随机字节，用来创建文件 id（避免重复）
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
	"strings"        // 字符串处理（trim、前缀判断、分割）
	"sync"           // 并发锁，保护 map 读写安全
	"time"           // 记录创建时间

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
	DiskPath  string    `json:"-"`          // 磁盘真实路径，不对外返回
}

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
	// 第二步接口：上传、列表、单文件动作路由分发。
	http.HandleFunc("/api/v1/files/upload", uploadHandler)   // 上传
	http.HandleFunc("/api/v1/files", filesCollectionHandler) // 列表
	http.HandleFunc("/api/v1/files/", fileItemHandler)       // 下载/重命名/删除

	// 启动服务器，监听指定端口。
	// ListenAndServe 会阻塞在这里，直到服务退出或报错。
	log.Printf("server is starting at :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
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

	record, err := saveUploadedFile(src, hdr) // 保存到本地并写入内存索引
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
			"download_url": "/api/v1/files/" + record.ID + "/download",
		},
	})
}

// filesCollectionHandler 第二步新增：返回当前内存索引里的全部文件元数据。
func filesCollectionHandler(w http.ResponseWriter, r *http.Request) {
	// 列表接口只允许 GET。
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
		return
	}

	filesMu.RLock()                                            // 读锁，允许并发读取
	items := make([]map[string]interface{}, 0, len(filesByID)) // 预分配容量减少扩容开销
	// 遍历内存索引，组装响应数组。
	for _, rec := range filesByID {
		items = append(items, map[string]interface{}{
			"id":           rec.ID,
			"name":         rec.Name,
			"size_bytes":   rec.SizeBytes,
			"created_at":   rec.CreatedAt,
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
		downloadHandler(w, r, id)
	case action == "rename" && r.Method == http.MethodPatch:
		renameHandler(w, r, id)
	case action == "" && r.Method == http.MethodDelete:
		deleteHandler(w, r, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, 10001, "method not allowed")
	}
}

// downloadHandler 第二步新增：按 id 找到磁盘文件并以附件方式下载。
func downloadHandler(w http.ResponseWriter, r *http.Request, id string) {
	rec, err := getFileRecord(id) // 根据 id 取元数据
	if err != nil {
		writeError(w, http.StatusNotFound, 10005, "file not found")
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename=\""+rec.Name+"\"") // 让浏览器按附件下载
	// ServeFile 会自动处理文件读取和输出，适合当前阶段快速实现下载。
	http.ServeFile(w, r, rec.DiskPath) // 直接把磁盘文件内容回给客户端
}

// renameHandler 第二步新增：修改文件显示名（仅改元数据，不改磁盘文件名）。
func renameHandler(w http.ResponseWriter, r *http.Request, id string) {
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
	if err := updateFileNameInDB(id, req.Name); err != nil {
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
func deleteHandler(w http.ResponseWriter, _ *http.Request, id string) {
	filesMu.RLock()
	rec, ok := filesByID[id]
	if !ok {
		filesMu.RUnlock()
		writeError(w, http.StatusNotFound, 10005, "file not found")
		return
	}
	filesMu.RUnlock()

	if err := os.Remove(rec.DiskPath); err != nil && !errors.Is(err, os.ErrNotExist) { // 再删磁盘文件
		writeError(w, http.StatusInternalServerError, 10008, "failed to delete file from disk")
		return
	}

	if err := deleteFileFromDB(id); err != nil {
		writeError(w, http.StatusInternalServerError, 10009, "failed to persist file metadata")
		return
	}

	filesMu.Lock()
	delete(filesByID, id)
	filesMu.Unlock()

	writeJSON(w, http.StatusOK, apiResponse{Code: 0, Message: "deleted"})
}

// saveUploadedFile 第二步新增：生成 id -> 流式写入磁盘 -> 建立内存索引。
func saveUploadedFile(src multipart.File, hdr *multipart.FileHeader) (*fileRecord, error) {
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

// getFileRecord 第二步新增：线程安全读取文件元数据。
func getFileRecord(id string) (*fileRecord, error) {
	filesMu.RLock() // 读锁保护 map
	defer filesMu.RUnlock()
	rec, ok := filesByID[id]
	if !ok {
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

// generateID 第二步新增：生成 24 位十六进制随机 id。
func generateID() (string, error) {
	b := make([]byte, 12) // 12 字节 = 24 位十六进制字符串
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
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

	const ddl = `
CREATE TABLE IF NOT EXISTS files (
  id VARCHAR(64) PRIMARY KEY,
  name VARCHAR(255) NOT NULL,
  size_bytes BIGINT NOT NULL,
  created_at_unix BIGINT NOT NULL,
  disk_path VARCHAR(1024) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`
	if _, err := db.Exec(ddl); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

// loadFilesFromDB 启动时读取 MySQL 元数据并恢复到内存索引。
func loadFilesFromDB() error {
	rows, err := dbConn.Query("SELECT id, name, size_bytes, created_at_unix, disk_path FROM files")
	if err != nil {
		return err
	}
	defer rows.Close()

	type rowData struct {
		id        string
		name      string
		size      int64
		createdAt int64
		diskPath  string
	}

	items := make([]rowData, 0)
	for rows.Next() {
		var r rowData
		if err := rows.Scan(&r.id, &r.name, &r.size, &r.createdAt, &r.diskPath); err != nil {
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

		filesByID[r.id] = &fileRecord{
			ID:        r.id,
			Name:      r.name,
			SizeBytes: r.size,
			CreatedAt: time.Unix(r.createdAt, 0),
			DiskPath:  r.diskPath,
		}
	}

	return nil
}

func insertFileToDB(rec *fileRecord) error {
	_, err := dbConn.Exec(
		"INSERT INTO files(id, name, size_bytes, created_at_unix, disk_path) VALUES (?, ?, ?, ?, ?)",
		rec.ID,
		rec.Name,
		rec.SizeBytes,
		rec.CreatedAt.Unix(),
		rec.DiskPath,
	)
	return err
}

func updateFileNameInDB(id string, name string) error {
	result, err := dbConn.Exec("UPDATE files SET name = ? WHERE id = ?", name, id)
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

func deleteFileFromDB(id string) error {
	_, err := dbConn.Exec("DELETE FROM files WHERE id = ?", id)
	return err
}

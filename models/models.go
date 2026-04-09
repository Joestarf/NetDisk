package models

import "time"

// APIResponse 统一响应体
type APIResponse struct {
	Code    int         `json:"code"`           // 状态码，0 表示成功，非 0 表示失败
	Message string      `json:"message"`        // 说明
	Data    interface{} `json:"data,omitempty"` // 可选数据体
}

// FileRecord 文件元数据模型
type FileRecord struct {
	ID        string    `json:"id"`         // 文件唯一 id
	Name      string    `json:"name"`       // 文件名（可重命名）
	SizeBytes int64     `json:"size_bytes"` // 文件大小
	CreatedAt time.Time `json:"created_at"` // 创建时间
	OwnerID   int64     `json:"-"`          // 所属用户 id
	ParentID  *int64    `json:"-"`          // 所属文件夹（nil 表示根目录）
	DiskPath  string    `json:"-"`          // 磁盘真实路径，不对外返回
}

// FolderRecord 文件夹元数据模型
type FolderRecord struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	OwnerID   int64     `json:"-"`
	ParentID  *int64    `json:"parent_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// AuthUser 认证后的用户信息
type AuthUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

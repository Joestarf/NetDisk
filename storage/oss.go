// storage/oss.go
package storage

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

type ObjectBackend interface {
	Name() string
	Save(reader io.Reader, key string, size int64) (string, error)
	Delete(key string) error
	GetDownloadURL(key string, filename string) (string, error)
}

var (
	objectBackendMu sync.RWMutex
	objectBackend   ObjectBackend
)

func SetObjectBackend(b ObjectBackend) {
	objectBackendMu.Lock()
	defer objectBackendMu.Unlock()
	objectBackend = b
}

func GetObjectBackend() ObjectBackend {
	objectBackendMu.RLock()
	defer objectBackendMu.RUnlock()
	return objectBackend
}

type OSSBackend struct {
	bucket *oss.Bucket
}

// NewOSSBackend 创建 OSS 后端实例
func NewOSSBackend(endpoint, accessKeyID, accessKeySecret, bucketName string) (*OSSBackend, error) {
	client, err := oss.New(endpoint, accessKeyID, accessKeySecret)
	if err != nil {
		return nil, fmt.Errorf("OSS client init failed: %w", err)
	}
	bucket, err := client.Bucket(bucketName)
	if err != nil {
		return nil, fmt.Errorf("OSS bucket init failed: %w", err)
	}
	return &OSSBackend{bucket: bucket}, nil
}

// Save 上传文件到 OSS
func (o *OSSBackend) Save(reader io.Reader, key string, size int64) (string, error) {
	options := []oss.Option{
		oss.ContentLength(size),
	}
	err := o.bucket.PutObject(key, reader, options...)
	if err != nil {
		return "", fmt.Errorf("OSS upload failed: %w", err)
	}
	return key, nil
}

// Delete 从 OSS 删除文件
func (o *OSSBackend) Delete(key string) error {
	err := o.bucket.DeleteObject(key)
	if err != nil {
		return fmt.Errorf("OSS delete failed: %w", err)
	}
	return nil
}

// GetDownloadURL 生成预签名下载 URL（有效期 1 小时）
func (o *OSSBackend) GetDownloadURL(key string, filename string) (string, error) {
	cd := fmt.Sprintf("attachment; filename=\"%s\"", sanitizeFilename(filename))
	options := []oss.Option{
		oss.ResponseContentDisposition(cd),
	}
	signedURL, err := o.bucket.SignURL(key, oss.HTTPGet, 3600, options...)
	if err != nil {
		return "", fmt.Errorf("OSS sign URL failed: %w", err)
	}
	return signedURL, nil
}

// Name 返回后端标识
func (o *OSSBackend) Name() string {
	return "oss"
}

func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer("\r", "", "\n", "", "\"", "")
	cleaned := strings.TrimSpace(replacer.Replace(name))
	if cleaned == "" {
		return "download.bin"
	}
	return cleaned
}

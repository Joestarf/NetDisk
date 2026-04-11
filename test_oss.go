// test_oss.go
package main

import (
    "log"
    "os"
    "strings"
    "github.com/aliyun/aliyun-oss-go-sdk/oss"
)

func main() {
    endpoint := os.Getenv("OSS_ENDPOINT")
    accessKeyID := os.Getenv("OSS_ACCESS_KEY_ID")
    accessKeySecret := os.Getenv("OSS_ACCESS_KEY_SECRET")
    bucketName := os.Getenv("OSS_BUCKET")

    log.Printf("Endpoint: %s, Bucket: %s", endpoint, bucketName)

    if endpoint == "" || accessKeyID == "" || accessKeySecret == "" || bucketName == "" {
        log.Fatal("Missing required OSS environment variables")
    }

    client, err := oss.New(endpoint, accessKeyID, accessKeySecret)
    if err != nil {
        log.Fatalf("OSS client init failed: %v", err)
    }
    bucket, err := client.Bucket(bucketName)
    if err != nil {
        log.Fatalf("OSS bucket init failed: %v", err)
    }

    // 尝试上传一个小对象
    err = bucket.PutObject("test/hello.txt", strings.NewReader("Hello OSS"))
    if err != nil {
        log.Fatalf("Upload failed: %v", err)
    }
    log.Println("✅ OSS upload succeeded!")
}
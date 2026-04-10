package handlers

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestListUploadedChunkIndexes(t *testing.T) {
	uploadID := "chunk-status-test"
	dir := chunkUploadDir(uploadID)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.RemoveAll(dir)

	files := []string{"000001.part", "000000.part", "meta.json", "000010.part", "bad.part", "a.txt"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got, err := listUploadedChunkIndexes(uploadID)
	if err != nil {
		t.Fatalf("listUploadedChunkIndexes error: %v", err)
	}
	want := []int{0, 1, 10}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("indexes = %v, want %v", got, want)
	}
}

package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"netdisk/db"
	"netdisk/models"
)

func TestDownloadHandlerRangePartialContent(t *testing.T) {
	rec, cleanup := prepareDownloadTestRecord(t, "abcdefghijklmnopqrstuvwxyz")
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/"+rec.ID+"/download", nil)
	req.Header.Set("Range", "bytes=0-3")
	w := httptest.NewRecorder()

	downloadHandler(w, req, rec.ID, rec.OwnerID)

	resp := w.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusPartialContent)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 0-3/26" {
		t.Fatalf("Content-Range = %q, want %q", got, "bytes 0-3/26")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "abcd" {
		t.Fatalf("body = %q, want %q", string(body), "abcd")
	}
}

func TestDownloadHandlerRangeNotSatisfiable(t *testing.T) {
	rec, cleanup := prepareDownloadTestRecord(t, "abcdefghijklmnopqrstuvwxyz")
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/"+rec.ID+"/download", nil)
	req.Header.Set("Range", "bytes=100-200")
	w := httptest.NewRecorder()

	downloadHandler(w, req, rec.ID, rec.OwnerID)

	resp := w.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusRequestedRangeNotSatisfiable)
	}
}

func TestDownloadHandlerFullContent(t *testing.T) {
	rec, cleanup := prepareDownloadTestRecord(t, "abcdefghijklmnopqrstuvwxyz")
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/"+rec.ID+"/download", nil)
	w := httptest.NewRecorder()

	downloadHandler(w, req, rec.ID, rec.OwnerID)

	resp := w.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("body = %q, want full content", string(body))
	}
}

func prepareDownloadTestRecord(t *testing.T, content string) (*models.FileRecord, func()) {
	t.Helper()

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	rec := &models.FileRecord{
		ID:        "test-file-id",
		Name:      "sample.txt",
		SizeBytes: int64(len(content)),
		CreatedAt: time.Now(),
		OwnerID:   1001,
		DiskPath:  path,
	}

	db.FilesMu.Lock()
	old := db.FilesByID
	db.FilesByID = map[string]*models.FileRecord{rec.ID: rec}
	db.FilesMu.Unlock()

	cleanup := func() {
		db.FilesMu.Lock()
		db.FilesByID = old
		db.FilesMu.Unlock()
	}
	return rec, cleanup
}

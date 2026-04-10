package db

import (
	"fmt"
	"os"
	"testing"
	"time"

	"netdisk/models"
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("MYSQL_DSN")
	if stringsTrimSpace(dsn) == "" {
		os.Exit(0)
	}

	if err := Init(dsn); err != nil {
		fmt.Fprintf(os.Stderr, "failed to init test db: %v\n", err)
		os.Exit(1)
	}
	defer Close()

	if err := cleanupShares(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to cleanup test shares: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	_ = cleanupShares()
	os.Exit(code)
}

func TestShareCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping db integration test in short mode")
	}

	cleanupSharesOrFail(t)

	now := time.Now().Unix()
	share := &models.ShareRecord{
		Token:         fmt.Sprintf("test-token-%d", now),
		OwnerID:       1001,
		NodeType:      "file",
		NodeID:        "file-001",
		Name:          "hello.txt",
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
	}

	shareID, err := CreateShare(share)
	if err != nil {
		t.Fatalf("CreateShare error: %v", err)
	}
	if shareID <= 0 {
		t.Fatalf("CreateShare returned invalid id: %d", shareID)
	}

	fetched, err := GetShareByToken(share.Token)
	if err != nil {
		t.Fatalf("GetShareByToken error: %v", err)
	}
	if fetched.Token != share.Token || fetched.OwnerID != share.OwnerID || fetched.NodeID != share.NodeID {
		t.Fatalf("fetched share mismatch: %+v", fetched)
	}

	items, err := ListSharesByOwner(share.OwnerID)
	if err != nil {
		t.Fatalf("ListSharesByOwner error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("ListSharesByOwner len = %d, want 1", len(items))
	}

	if err := IncrementShareVisitByToken(share.Token); err != nil {
		t.Fatalf("IncrementShareVisitByToken error: %v", err)
	}
	fetched, err = GetShareByToken(share.Token)
	if err != nil {
		t.Fatalf("GetShareByToken after increment error: %v", err)
	}
	if fetched.VisitCount != 1 {
		t.Fatalf("VisitCount = %d, want 1", fetched.VisitCount)
	}

	if err := RevokeShareByID(share.OwnerID, shareID); err != nil {
		t.Fatalf("RevokeShareByID error: %v", err)
	}
	fetched, err = GetShareByToken(share.Token)
	if err != nil {
		t.Fatalf("GetShareByToken after revoke error: %v", err)
	}
	if !fetched.Revoked {
		t.Fatal("share should be revoked")
	}

	if err := DeleteShareByID(share.OwnerID, shareID); err != nil {
		t.Fatalf("DeleteShareByID error: %v", err)
	}
	if _, err := GetShareByToken(share.Token); err == nil {
		t.Fatal("GetShareByToken should fail after delete")
	}
}

func TestShareMissingRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping db integration test in short mode")
	}

	cleanupSharesOrFail(t)

	if _, err := GetShareByToken("missing-token"); err == nil {
		t.Fatal("GetShareByToken should fail for missing token")
	}
	if err := IncrementShareVisitByToken("missing-token"); err == nil {
		t.Fatal("IncrementShareVisitByToken should fail for missing token")
	}
	if err := RevokeShareByID(1, 999999); err == nil {
		t.Fatal("RevokeShareByID should fail for missing share")
	}
	if err := DeleteShareByID(1, 999999); err == nil {
		t.Fatal("DeleteShareByID should fail for missing share")
	}
}

func TestRevokeSharesByNode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping db integration test in short mode")
	}

	cleanupSharesOrFail(t)

	now := time.Now().Unix()
	fileToken := fmt.Sprintf("test-node-file-%d", now)
	folderToken := fmt.Sprintf("test-node-folder-%d", now)

	_, err := CreateShare(&models.ShareRecord{
		Token:         fileToken,
		OwnerID:       2001,
		NodeType:      "file",
		NodeID:        "file-xyz",
		Name:          "a.txt",
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
	})
	if err != nil {
		t.Fatalf("CreateShare file error: %v", err)
	}

	_, err = CreateShare(&models.ShareRecord{
		Token:         folderToken,
		OwnerID:       2001,
		NodeType:      "folder",
		NodeID:        "100",
		Name:          "docs",
		CreatedAtUnix: now,
		UpdatedAtUnix: now,
	})
	if err != nil {
		t.Fatalf("CreateShare folder error: %v", err)
	}

	if err := RevokeSharesByNode(2001, "file", "file-xyz"); err != nil {
		t.Fatalf("RevokeSharesByNode error: %v", err)
	}

	fileShare, err := GetShareByToken(fileToken)
	if err != nil {
		t.Fatalf("GetShareByToken(file) error: %v", err)
	}
	if !fileShare.Revoked {
		t.Fatal("file share should be revoked")
	}

	folderShare, err := GetShareByToken(folderToken)
	if err != nil {
		t.Fatalf("GetShareByToken(folder) error: %v", err)
	}
	if folderShare.Revoked {
		t.Fatal("folder share should remain active")
	}
}

func cleanupShares() error {
	if DB == nil {
		return nil
	}
	_, err := DB.Exec("DELETE FROM shares")
	return err
}

func cleanupSharesOrFail(t *testing.T) {
	t.Helper()
	if err := cleanupShares(); err != nil {
		t.Fatalf("cleanupShares error: %v", err)
	}
}

func stringsTrimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

package handlers

import (
	"testing"
	"time"

	"netdisk/models"
)

func TestParseShareID(t *testing.T) {
	shareID, err := parseShareID("/api/v1/shares/123")
	if err != nil {
		t.Fatalf("parseShareID returned error: %v", err)
	}
	if shareID != 123 {
		t.Fatalf("parseShareID = %d, want 123", shareID)
	}

	if _, err := parseShareID("/api/v1/shares/abc"); err == nil {
		t.Fatal("parseShareID should fail for non-numeric id")
	}

	if _, err := parseShareID("/api/v1/files/123"); err == nil {
		t.Fatal("parseShareID should fail for wrong prefix")
	}
}

func TestIsShareExpired(t *testing.T) {
	if isShareExpired(nil) {
		t.Fatal("nil expiry should not be expired")
	}

	past := int64(1)
	if !isShareExpired(&past) {
		t.Fatal("past timestamp should be expired")
	}
}

func TestIsShareExhausted(t *testing.T) {
	if isShareExhausted(nil, 10) {
		t.Fatal("nil maxVisits should not be exhausted")
	}

	maxVisits := 3
	if isShareExhausted(&maxVisits, 2) {
		t.Fatal("visit count below max should not be exhausted")
	}
	if !isShareExhausted(&maxVisits, 3) {
		t.Fatal("visit count equal to max should be exhausted")
	}
}

func TestRemainingVisits(t *testing.T) {
	if remainingVisits(nil, 10) != nil {
		t.Fatal("remainingVisits should be nil when maxVisits is nil")
	}

	maxVisits := 5
	v := remainingVisits(&maxVisits, 2)
	if v == nil || *v != 3 {
		t.Fatalf("remainingVisits = %v, want 3", v)
	}

	v = remainingVisits(&maxVisits, 10)
	if v == nil || *v != 0 {
		t.Fatalf("remainingVisits = %v, want 0", v)
	}
}

func TestShareStatus(t *testing.T) {
	now := time.Now().Unix()
	past := now - 10
	maxVisits := 3

	if got := shareStatus(models.ShareRecord{Revoked: true}); got != "revoked" {
		t.Fatalf("shareStatus(revoked) = %q, want revoked", got)
	}
	if got := shareStatus(models.ShareRecord{ExpiresAtUnix: &past}); got != "expired" {
		t.Fatalf("shareStatus(expired) = %q, want expired", got)
	}
	if got := shareStatus(models.ShareRecord{MaxVisits: &maxVisits, VisitCount: 3}); got != "exhausted" {
		t.Fatalf("shareStatus(exhausted) = %q, want exhausted", got)
	}
	if got := shareStatus(models.ShareRecord{MaxVisits: &maxVisits, VisitCount: 1}); got != "active" {
		t.Fatalf("shareStatus(active) = %q, want active", got)
	}
}

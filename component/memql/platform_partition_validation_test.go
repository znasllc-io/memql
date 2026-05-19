package memql

import (
	"strings"
	"testing"
)

// validatePlatformPartitionPayload guards the partition slug rules
// at the insert path. The slug is load-bearing in three places (PK,
// event topic segment, grant lookup key), so a typo or mis-spelled
// reserved name corrupts data permanently. These tests pin the
// invariants the validator enforces.

func TestValidatePlatformPartitionPayload_RejectsInvalidSlug(t *testing.T) {
	// The validator delegates to id.ValidatePartitionName which
	// normalizes case + dashes before checking shape. Only inputs
	// that survive normalization to something invalid (or are
	// reserved / too long / empty) should reject here.
	cases := []struct {
		name string
		slug string
	}{
		{"leading-underscore-system", "_system"},
		{"empty", ""},
		{"too-long", strings.Repeat("a", 100)},
		{"contains-invalid-chars", "bad name!"}, // normalizer collapses to ?, depends on rules
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"name": tc.slug}
			err := validatePlatformPartitionPayload(payload, "")
			if err == nil {
				t.Fatalf("slug %q should have been rejected, got nil", tc.slug)
			}
		})
	}
}

// TestValidatePlatformPartitionPayload_NormalizesCase covers the
// delegation to id.ValidatePartitionName. Mixed-case input gets
// lowercased on its way to the row.
func TestValidatePlatformPartitionPayload_NormalizesCase(t *testing.T) {
	payload := map[string]any{"name": "AcmeWorkspace"}
	err := validatePlatformPartitionPayload(payload, "acmeworkspace")
	if err != nil {
		t.Fatalf("expected nil error after normalization, got %v", err)
	}
	if got, _ := payload["name"].(string); got != "acmeworkspace" {
		t.Errorf("name = %q, want normalized \"acmeworkspace\"", got)
	}
}

func TestValidatePlatformPartitionPayload_AcceptsValidSlug(t *testing.T) {
	payload := map[string]any{"name": "acme-workspace"}
	err := validatePlatformPartitionPayload(payload, "acme-workspace")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	// Display name auto-derived when absent.
	if dn, _ := payload["displayName"].(string); dn != "Acme Workspace" {
		t.Errorf("displayName = %q, want \"Acme Workspace\"", dn)
	}
}

func TestValidatePlatformPartitionPayload_IdMismatchRejected(t *testing.T) {
	// The mutation id must equal the slug. Mismatched values would
	// leave partitionAccess grants pointing at orphan rows.
	payload := map[string]any{"name": "acme"}
	err := validatePlatformPartitionPayload(payload, "different-id")
	if err == nil {
		t.Fatal("expected error for id != slug, got nil")
	}
	if !strings.Contains(err.Error(), "must equal payload.name") {
		t.Errorf("error should explain the slug==id rule, got %v", err)
	}
}

func TestValidatePlatformPartitionPayload_DisplayNameLengthCap(t *testing.T) {
	payload := map[string]any{
		"name":        "acme",
		"displayName": strings.Repeat("a", 200),
	}
	err := validatePlatformPartitionPayload(payload, "acme")
	if err == nil {
		t.Fatal("expected error for over-length displayName, got nil")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("error should mention length, got %v", err)
	}
}

func TestValidatePlatformPartitionPayload_DescriptionLengthCap(t *testing.T) {
	payload := map[string]any{
		"name":        "acme",
		"description": strings.Repeat("d", 300),
	}
	err := validatePlatformPartitionPayload(payload, "acme")
	if err == nil {
		t.Fatal("expected error for over-length description, got nil")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Errorf("error should mention length, got %v", err)
	}
}

func TestValidatePlatformPartitionPayload_EmptyDisplayNameDeleted(t *testing.T) {
	// Whitespace-only displayName becomes auto-derived rather than
	// landing as an empty string in the row.
	payload := map[string]any{
		"name":        "joses-workspace",
		"displayName": "   ",
	}
	err := validatePlatformPartitionPayload(payload, "joses-workspace")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if dn, _ := payload["displayName"].(string); dn != "Joses Workspace" {
		t.Errorf("displayName = %q, want auto-derived \"Joses Workspace\"", dn)
	}
}

func TestDefaultDisplayNameForSlug(t *testing.T) {
	cases := []struct{ slug, want string }{
		{"acme", "Acme"},
		{"acme-workspace", "Acme Workspace"},
		{"joses-personal-space", "Joses Personal Space"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.slug, func(t *testing.T) {
			got := defaultDisplayNameForSlug(tc.slug)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

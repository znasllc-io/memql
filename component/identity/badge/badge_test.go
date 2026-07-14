package badge

import (
	"strings"
	"testing"
)

func TestHashBadgeId(t *testing.T) {
	h := HashBadgeId("04:A3:1B:9C")
	if len(h) != 64 {
		t.Fatalf("hash length = %d, want 64 hex chars", len(h))
	}
	if h != HashBadgeId("  04:A3:1B:9C  ") {
		t.Errorf("whitespace framing must not change the hash")
	}
	if h == HashBadgeId("04:A3:1B:9D") {
		t.Errorf("distinct badge ids must not collide")
	}
	if HashBadgeId("") != "" || HashBadgeId("   ") != "" {
		t.Errorf("empty badge id must hash to empty (never a valid lookup key)")
	}
}

func TestCanonicalId(t *testing.T) {
	if got := CanonicalId("abc123"); got != "v1:identity:identity:abc123" {
		t.Errorf("CanonicalId(bare) = %q", got)
	}
	full := "v1:identity:identity:abc123"
	if got := CanonicalId(full); got != full {
		t.Errorf("CanonicalId(full) = %q, want unchanged", got)
	}
	if got := bareSlug(full); got != "abc123" {
		t.Errorf("bareSlug(full) = %q", got)
	}
}

func TestNewIdShape(t *testing.T) {
	id, err := NewId()
	if err != nil {
		t.Fatalf("NewId: %v", err)
	}
	if strings.TrimSpace(id) == "" {
		t.Fatalf("NewId returned empty id")
	}
	if strings.HasPrefix(id, "v1:") {
		t.Errorf("NewId must return a bare slug, got %q", id)
	}
}

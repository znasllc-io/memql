package identity

import (
	"regexp"
	"testing"
)

// uuidv4Re matches the canonical UUIDv4 string format core/id.NewShortId
// returns. Pinned here so the test catches a future regression that
// swaps the minter back to a non-UUID format (per memql#103).
var uuidv4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNewRandomId_IsUUIDv4 asserts that every identity row mints its
// shortId via core/id.NewShortId. NewRandomId is the central minter
// for user / authSession / authCode / magicLinkRequest / PAT row /
// workerToken row / workerPairing row; the conformance check is on
// the function rather than on every call site so future call sites
// inherit the format without each one needing its own assertion.
//
// Why this matters (memql#103): pre-#103 the identity service minted
// 32-char hex shortIds while the rest of the system used 36-char
// UUIDv4. Same kind of row, three+ different shortId formats. The
// migration consolidated everything onto NewShortId; this test pins
// it so the next "I'll just hex-encode random bytes here" attempt
// surfaces as a red test.
func TestNewRandomId_IsUUIDv4(t *testing.T) {
	for i := 0; i < 50; i++ {
		got, err := NewRandomId("")
		if err != nil {
			t.Fatalf("NewRandomId returned error: %v", err)
		}
		if !uuidv4Re.MatchString(got) {
			t.Fatalf("NewRandomId returned non-UUIDv4 string %q", got)
		}
	}
}

// TestNewAuditEventId_IsUUIDv4 mirrors the NewRandomId check for the
// auditEvent row's separate minter. Both must produce the canonical
// UUIDv4 shape.
func TestNewAuditEventId_IsUUIDv4(t *testing.T) {
	for i := 0; i < 50; i++ {
		got, err := newAuditEventId()
		if err != nil {
			t.Fatalf("newAuditEventId returned error: %v", err)
		}
		if !uuidv4Re.MatchString(got) {
			t.Fatalf("newAuditEventId returned non-UUIDv4 string %q", got)
		}
	}
}

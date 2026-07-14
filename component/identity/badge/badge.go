// Package badge implements registered operator badges for shared
// terminals (memql#2513).
//
// A badge maps a physical badge/device identifier (NFC card UID, QR
// payload, device serial) to a v1:identity:user. It is stored as a
// v1:identity:identity row of identityType="badge", carrying only the
// SHA-256 hex hash of the identifier -- plaintext badge ids are never
// persisted.
//
// A badge is NOT a bearer credential: presenting the badge id alone
// authenticates nothing. The identity service's POST /auth/badge/grant
// exchange requires an already-authenticated terminal bearer alongside
// the badge id, and mints a short-lived, role-ceiling-clamped
// class="badge" JWT for the badge's owner so writes during the
// operator's window carry their actor.userId.
//
// Unlike worker tokens (mql_wkr_*, server-minted 32-byte secrets), the
// badge id comes from the physical world, so its hash is a lookup key,
// not a brute-force-resistant secret store. The compensating controls
// live at the grant exchange: terminal bearer required, per-IP rate
// limiting, audit trail, immediate revocation for future grants.
package badge

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/znasllc-io/memql/component/identity"
)

// canonicalIdentityIdPrefix mirrors workertoken / pat.
const canonicalIdentityIdPrefix = "v1:identity:identity:"

// HashBadgeId returns the SHA-256 hex digest of a badge/device
// identifier, the credentials.keyHash lookup key. Whitespace is
// trimmed so scanner framing differences don't split identities.
func HashBadgeId(badgeId string) string {
	trimmed := strings.TrimSpace(badgeId)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])
}

// NewId mints a new badge identity slug.
func NewId() (string, error) {
	return identity.NewRandomId("")
}

// CanonicalId converts a bare slug to the canonical id form.
func CanonicalId(slugOrFull string) string {
	if strings.HasPrefix(slugOrFull, canonicalIdentityIdPrefix) {
		return slugOrFull
	}
	return canonicalIdentityIdPrefix + slugOrFull
}

func bareSlug(id string) string {
	if strings.HasPrefix(id, canonicalIdentityIdPrefix) {
		return strings.TrimPrefix(id, canonicalIdentityIdPrefix)
	}
	return id
}

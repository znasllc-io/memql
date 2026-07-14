package identity

// IssueBadgeGrantToken claim-shape tests (memql#2513): the grant must
// verify through the standard path with sub = the operator's user id,
// class = badge, the role ceiling stamped, and the short TTL applied.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newBadgeTestIssuer(t *testing.T) *JWTIssuer {
	t.Helper()
	dir := t.TempDir()
	km, err := NewKeyManager(dir, "")
	require.NoError(t, err)
	require.NoError(t, km.Load())
	iss, err := NewJWTIssuer(km, Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		KeyDir:      dir,
	})
	require.NoError(t, err)
	return iss
}

func TestIssueBadgeGrantToken_Claims(t *testing.T) {
	iss := newBadgeTestIssuer(t)
	now := time.Now().UTC()

	token, expiresAt, err := iss.IssueBadgeGrantToken(BadgeGrantIssueInput{
		UserId:          "v1:identity:user:op-1",
		RoleCeiling:     "writer",
		BadgeIdentityId: "v1:identity:identity:bdg-1",
	}, now)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	// Default TTL applies (DefaultBadgeGrantTTLSeconds).
	require.WithinDuration(t, now.Add(time.Duration(DefaultBadgeGrantTTLSeconds)*time.Second), expiresAt, time.Second)

	claims, err := iss.VerifyAccessToken(token, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, "v1:identity:user:op-1", claims.Subject)
	require.Equal(t, ClassBadge, claims.Class)
	require.Equal(t, "writer", claims.RoleCeiling)
	require.Equal(t, "v1:identity:identity:bdg-1", claims.NodeId,
		"badge identity id rides the NodeId audit slot (voice-agent convention)")

	// Expired grant fails verification.
	_, err = iss.VerifyAccessToken(token, expiresAt.Add(2*time.Minute))
	require.Error(t, err)
}

func TestIssueBadgeGrantToken_TTLOverride(t *testing.T) {
	iss := newBadgeTestIssuer(t)
	now := time.Now().UTC()
	_, expiresAt, err := iss.IssueBadgeGrantToken(BadgeGrantIssueInput{
		UserId:      "v1:identity:user:op-1",
		RoleCeiling: "reader",
		TTLOverride: 42 * time.Second,
	}, now)
	require.NoError(t, err)
	require.WithinDuration(t, now.Add(42*time.Second), expiresAt, time.Second)
}

func TestIssueBadgeGrantToken_RequiredFields(t *testing.T) {
	iss := newBadgeTestIssuer(t)
	now := time.Now().UTC()

	_, _, err := iss.IssueBadgeGrantToken(BadgeGrantIssueInput{RoleCeiling: "writer"}, now)
	require.Error(t, err, "UserId required")

	_, _, err = iss.IssueBadgeGrantToken(BadgeGrantIssueInput{UserId: "v1:identity:user:op-1"}, now)
	require.Error(t, err, "RoleCeiling required -- an unclamped grant must never mint")
}

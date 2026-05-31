package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testSeedB64() string {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return base64.StdEncoding.EncodeToString(seed)
}

// TestNewKeyManagerFromSeed_Deterministic is the #550 invariant: the same
// seed always yields the same keypair + kid, so multiple identity replicas
// handed the same IDENTITY_SIGNING_KEY_B64 publish identical JWKS.
func TestNewKeyManagerFromSeed_Deterministic(t *testing.T) {
	seed := testSeedB64()

	km1, err := NewKeyManagerFromSeed(seed)
	require.NoError(t, err)
	km2, err := NewKeyManagerFromSeed(seed)
	require.NoError(t, err)

	c1, c2 := km1.Current(), km2.Current()
	require.NotNil(t, c1)
	require.NotNil(t, c2)
	assert.Equal(t, c1.KID, c2.KID, "same seed must derive the same kid")
	assert.Equal(t, c1.PublicB64, c2.PublicB64, "same seed must derive the same public key")
	assert.Len(t, []byte(c1.Private), ed25519.PrivateKeySize)
}

// Load is a no-op in env mode (no disk) and leaves the key intact.
func TestKeyManagerFromSeed_LoadIsNoOp(t *testing.T) {
	km, err := NewKeyManagerFromSeed(testSeedB64())
	require.NoError(t, err)
	before := km.Current().KID
	require.NoError(t, km.Load())
	assert.Equal(t, before, km.Current().KID)
	assert.False(t, km.RotationSupported(), "env mode disables rotation")
}

// Rotate is refused in env mode with a clear, actionable error.
func TestKeyManagerFromSeed_RotateRefused(t *testing.T) {
	km, err := NewKeyManagerFromSeed(testSeedB64())
	require.NoError(t, err)
	_, err = km.Rotate(24 * time.Hour)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rotation is disabled")
	// Sweep is a harmless no-op.
	swept, serr := km.SweepRetiredPrevious(time.Now())
	require.NoError(t, serr)
	assert.False(t, swept)
}

// Malformed seeds are rejected at construction.
func TestNewKeyManagerFromSeed_BadInput(t *testing.T) {
	_, err := NewKeyManagerFromSeed("not!base64!")
	require.Error(t, err)

	short := base64.StdEncoding.EncodeToString([]byte("too-short"))
	_, err = NewKeyManagerFromSeed(short)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must decode to")
}

// A valid env signing key satisfies the at-rest requirement: production
// (non-localhost) config validates WITHOUT IDENTITY_KEY_ENCRYPTION_KEY.
func TestConfigValidate_SigningKeySatisfiesAtRest(t *testing.T) {
	base := Config{
		Enabled:             true,
		BaseURL:             "https://identity.staging.copresent.ai",
		RegistrationMode:    RegistrationModeOpen,
		InternalDefaultRole: "writer",
		RiskThreshold:       50,
	}

	// Without a key or encryption secret -> rejected.
	noKey := base
	require.Error(t, noKey.Validate(), "non-localhost prod with neither key must fail")

	// With a valid env seed -> accepted (no encryption key needed).
	withSeed := base
	withSeed.SigningKeyB64 = testSeedB64()
	require.NoError(t, withSeed.Validate())

	// A malformed seed is rejected by Validate too.
	badSeed := base
	badSeed.SigningKeyB64 = "xxx"
	require.Error(t, badSeed.Validate())
}

// The env key serves in the JWKS set just like an on-disk key.
func TestKeyManagerFromSeed_PublicKeySet(t *testing.T) {
	km, err := NewKeyManagerFromSeed(testSeedB64())
	require.NoError(t, err)
	set := km.PublicKeySet(time.Now())
	require.Len(t, set, 1, "env mode has exactly the current key (no previous)")
	assert.Equal(t, km.Current().KID, set[0].KID)
}

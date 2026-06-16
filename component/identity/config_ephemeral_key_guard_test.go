package identity

import (
	"strings"
	"testing"
	"time"
)

// baseValidConfig returns a minimal Config that passes Validate() so each
// guard test can flip exactly one field and assert on the guard's behaviour
// without tripping an unrelated validation rule.
func baseValidConfig() Config {
	return Config{
		Enabled:             true,
		BaseURL:             "https://identity.acme.com",
		JWTAudience:         "memql",
		RegistrationMode:    RegistrationModeOpen,
		InternalDefaultRole: "writer",
		RiskThreshold:       DefaultRiskThreshold,
		AccessTokenTTL:      time.Duration(DefaultAccessTokenTTLSeconds) * time.Second,
	}
}

// TestValidate_EphemeralKeyGuard exercises the #1515 fail-fast guard: a
// non-localhost deployment with no IDENTITY_SIGNING_KEY_B64 must refuse to
// start (per-pod ephemeral key -> JWKS divergence -> ~50% auth failures)
// unless it sets a shared seed OR explicitly opts into ephemeral keys.
func TestValidate_EphemeralKeyGuard(t *testing.T) {
	seedB64 := testSeedB64() // shared helper: 32-byte Ed25519 seed, std-base64

	cases := []struct {
		name          string
		mutate        func(*Config)
		wantErr       bool
		wantErrSubstr string
	}{
		{
			name: "no seed, no opt-in, non-localhost -> REFUSE",
			mutate: func(c *Config) {
				c.SigningKeyB64 = ""
				c.AllowEphemeralKey = false
				// KeyEncryptionKey set so we isolate the ephemeral guard
				// from the at-rest-encryption requirement that follows it.
				c.KeyEncryptionKey = "encryption-secret-16b"
			},
			wantErr:       true,
			wantErrSubstr: "IDENTITY_SIGNING_KEY_B64 is not set",
		},
		{
			name: "shared seed set -> ALLOW",
			mutate: func(c *Config) {
				c.SigningKeyB64 = seedB64
				c.AllowEphemeralKey = false
				c.KeyEncryptionKey = ""
			},
			wantErr: false,
		},
		{
			name: "ephemeral opt-in set -> ALLOW",
			mutate: func(c *Config) {
				c.SigningKeyB64 = ""
				c.AllowEphemeralKey = true
				c.KeyEncryptionKey = "encryption-secret-16b"
			},
			wantErr: false,
		},
		{
			name: "localhost origin exempt (dev) -> ALLOW even with no seed + no opt-in",
			mutate: func(c *Config) {
				c.BaseURL = "http://localhost:8081"
				c.SigningKeyB64 = ""
				c.AllowEphemeralKey = false
				c.KeyEncryptionKey = ""
			},
			wantErr: false,
		},
		{
			name: "dev-wildcard origin exempt (identity.local.znas.io) -> ALLOW",
			mutate: func(c *Config) {
				c.BaseURL = "https://identity.local.znas.io"
				c.SigningKeyB64 = ""
				c.AllowEphemeralKey = false
				c.KeyEncryptionKey = ""
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValidConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want error")
				}
				if tc.wantErrSubstr != "" && !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Fatalf("Validate() error = %q, want substring %q", err.Error(), tc.wantErrSubstr)
				}
			} else if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestValidate_EphemeralGuardErrorIsActionable asserts the refusal message
// names the fix (set the seed) and the dev opt-in, so the operator can
// copy-paste their way out instead of guessing.
func TestValidate_EphemeralGuardErrorIsActionable(t *testing.T) {
	cfg := baseValidConfig()
	cfg.SigningKeyB64 = ""
	cfg.AllowEphemeralKey = false
	cfg.KeyEncryptionKey = "encryption-secret-16b"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected a refusal error")
	}
	for _, want := range []string{
		"IDENTITY_SIGNING_KEY_B64",
		"IDENTITY_ALLOW_EPHEMERAL_KEY",
		"jwks",
	} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("refusal message missing %q; got: %s", want, err.Error())
		}
	}
}

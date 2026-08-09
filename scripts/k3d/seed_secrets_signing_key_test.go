package k3d

import (
	"crypto/ed25519"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// seed_secrets_signing_key_test.go -- memql#3400.
//
// deploy/k8s/base/identity.yaml declares `replicas: 2` and states the design
// out loud: "the signing key comes from the envelope (same seed on every pod
// -> identical JWKS), so there is NO single-writer key PVC". Locally that key
// was never provided by anything. identity therefore fell through
// KeyManager.Load() to generateAndWriteCurrent() and EVERY POD MINTED ITS OWN
// Ed25519 keypair -- observed on a live local cluster as two replicas serving
// kids 859BkEwzf6g and biMg9gN11xg behind one Service. A token minted by one
// is structurally unverifiable by any node that fetched JWKS from the other,
// so `make scale N=2` (the documented multi-node command) produced coin-flip
// auth failures reported as "invalid or expired token".
//
// seed-secrets.sh already owns the local analogue of the ESO/Key Vault path
// for the master key and the front-door TLS pair, so it owns this too.
//
// The tests drive the REAL script against a fake kubectl and assert on the
// recorded `kubectl create secret` argv, matching the master-key suite: the
// defect was never in the arithmetic of key selection, it was in what the
// script ultimately WRITES to the cluster.

// signingKeyB64 is the shape the runtime accepts: base64-std of exactly 32
// bytes (component/identity/keys.go NewKeyManagerFromSeed, which requires
// ed25519.SeedSize). 32 bytes -> 44 base64 characters ending in one '='.
var signingKeyB64 = regexp.MustCompile(`^[A-Za-z0-9+/]{43}=$`)

// seedB64 builds a valid, obviously-synthetic 32-byte seed. Built rather than
// written as a literal for the same reason the master-key fixtures are: a
// high-entropy-looking constant in source is what gitleaks' generic rules hunt
// for, and history is append-only.
func seedB64(fill byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(fill), ed25519.SeedSize)))
}

// TestSeedSecretsSeedsASharedSigningKey is the regression test for the issue:
// a from-nothing bring-up must leave a shared Ed25519 seed in memql-secrets,
// which every identity replica reads via `envFrom` and derives one key + kid +
// JWKS from.
func TestSeedSecretsSeedsASharedSigningKey(t *testing.T) {
	stdout, calls, code := runSeedSecrets(t, scenario{})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := seededLiteral(t, calls, "MEMQL_IDENTITY_SIGNING_KEY_B64")
	if !signingKeyB64.MatchString(got) {
		t.Fatalf("seeded MEMQL_IDENTITY_SIGNING_KEY_B64 = %q, which is not base64-std of 32 bytes.\n"+
			"Without a usable shared seed every identity replica mints its OWN key and ~half of all "+
			"token verifications fail with 'unknown kid' (memql#3400).", got)
	}
	if src := parseEnvelope(t, stdout).Result.SigningKeySource; src != "generated" {
		t.Errorf("signingKeySource = %q, want \"generated\"", src)
	}
}

// TestSeedSecretsNeverRotatesAnExistingSigningKey is the idempotency
// invariant. `make secrets` runs on every `make up`, and rotating the signing
// key invalidates every live session and every minted mesh node token. A
// routine re-run must be a no-op for this field -- the same property memql#2958
// established for the master key, on a value with the same blast radius.
func TestSeedSecretsNeverRotatesAnExistingSigningKey(t *testing.T) {
	existing := seedB64('q')

	stdout, calls, code := runSeedSecrets(t, scenario{
		secretState: "present", clusterKey: goodCluster, clusterSigning: existing,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := seededLiteral(t, calls, "MEMQL_IDENTITY_SIGNING_KEY_B64"); got != existing {
		t.Errorf("seeded MEMQL_IDENTITY_SIGNING_KEY_B64 = %q, want the cluster's existing seed %q.\n"+
			"Re-running the seeder must not rotate the signing key: rotation invalidates every live "+
			"session and every minted node token.", got, existing)
	}
	if src := parseEnvelope(t, stdout).Result.SigningKeySource; src != "cluster" {
		t.Errorf("signingKeySource = %q, want \"cluster\"", src)
	}
}

// TestSeedSecretsUsesTheEnvSigningKeyVerbatim covers the deliberate path: an
// operator who exports a seed (to match a sealed envelope, or to rotate on
// purpose) gets exactly that value.
func TestSeedSecretsUsesTheEnvSigningKeyVerbatim(t *testing.T) {
	want := seedB64('z')
	_, calls, code := runSeedSecrets(t, scenario{
		envSigningKey: want, secretState: "present",
		clusterKey: goodCluster, clusterSigning: seedB64('q'),
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := seededLiteral(t, calls, "MEMQL_IDENTITY_SIGNING_KEY_B64"); got != want {
		t.Errorf("seeded MEMQL_IDENTITY_SIGNING_KEY_B64 = %q, want the env value %q verbatim", got, want)
	}
}

// TestSeedSecretsRejectsAnInvalidEnvSigningKey pins that a malformed seed
// supplied deliberately fails loudly BEFORE any mutation instead of being
// written. A seed the runtime cannot decode does not degrade to ephemeral
// mode -- identity refuses to boot on it (Config.Validate), so writing it
// would take auth down cluster-wide.
func TestSeedSecretsRejectsAnInvalidEnvSigningKey(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{"not base64", "this is not base64 at all"},
		{"base64 but 16 bytes", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 16)))},
		{"base64 but 64 bytes", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 64)))},
		{"hex master key by mistake", strings.Repeat("ab", 32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, calls, code := runSeedSecrets(t, scenario{envSigningKey: tc.key})
			if code != 2 {
				t.Errorf("exit code = %d, want 2 (capability contract: bad param)", code)
			}
			if muts := mutatedAnything(calls); len(muts) > 0 {
				t.Errorf("the run mutated the cluster before rejecting a bad signing seed:\n  %s",
					strings.Join(muts, "\n  "))
			}
			env := parseEnvelope(t, stdout)
			if env.OK {
				t.Error("envelope reports ok=true for an invalid signing seed")
			}
			if env.Error == nil || env.Error.Code != 2 {
				t.Errorf("envelope error = %+v, want code 2", env.Error)
			}
		})
	}
}

// TestSeedSecretsReplacesAnUnusableStoredSigningKey covers the repair path. A
// stored value the runtime rejects signs nothing, so replacing it loses
// nothing -- and leaving it in place would keep identity refusing to boot with
// no way out but hand-editing the Secret.
func TestSeedSecretsReplacesAnUnusableStoredSigningKey(t *testing.T) {
	stdout, calls, code := runSeedSecrets(t, scenario{
		secretState: "present", clusterKey: goodCluster,
		clusterSigning: "not-a-valid-seed",
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := seededLiteral(t, calls, "MEMQL_IDENTITY_SIGNING_KEY_B64")
	if !signingKeyB64.MatchString(got) {
		t.Errorf("seeded MEMQL_IDENTITY_SIGNING_KEY_B64 = %q, want a freshly generated valid seed", got)
	}
	if src := parseEnvelope(t, stdout).Result.SigningKeySource; src != "generated" {
		t.Errorf("signingKeySource = %q, want \"generated\"", src)
	}
}

// TestSeedSecretsPreservesAWhitespacePaddedSigningKey mirrors the master-key
// case: the runtime TrimSpaces before validating, so a seed stored with a
// stray newline IS usable by every replica. Judging it garbage and rotating it
// would destroy a working key -- the precise harm this change prevents.
func TestSeedSecretsPreservesAWhitespacePaddedSigningKey(t *testing.T) {
	want := seedB64('q')
	_, calls, code := runSeedSecrets(t, scenario{
		secretState: "present", clusterKey: goodCluster,
		clusterSigning: want + "\n",
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := seededLiteral(t, calls, "MEMQL_IDENTITY_SIGNING_KEY_B64"); got != want {
		t.Errorf("seeded MEMQL_IDENTITY_SIGNING_KEY_B64 = %q, want the trimmed existing seed %q", got, want)
	}
}

// TestSeedSecretsNeverLogsTheSigningSeed pins that the human log stream does
// not print the private seed. seed-secrets.sh runs in operator terminals and
// its stderr lands in CI logs; a key that leaks there is a key that must be
// rotated, which is exactly the operation the rest of this file works to
// avoid.
func TestSeedSecretsNeverLogsTheSigningSeed(t *testing.T) {
	secret := seedB64('q')
	stderr := runSeedSecretsStderr(t, scenario{
		secretState: "present", clusterKey: goodCluster, clusterSigning: secret,
	})
	if strings.Contains(stderr, secret) {
		t.Errorf("the signing seed appears in the script's human log output.\nstderr:\n%s", stderr)
	}
}

package k3d

import (
	"regexp"
	"strings"
	"testing"
)

// seed_secrets_node_bootstrap_test.go -- memql#3784.
//
// THE DEFECT. seed-secrets.sh seeded no MEMQL_NODE_BOOTSTRAP_TOKEN, so nothing
// on a local cluster had the shared secret that gates node-token minting. The
// consequence is mesh-wide and permanent:
//
//   - identity reads the token into Cfg.NodeBootstrapToken
//     (component/identity/config.go). Empty, so /node/bootstrap answers every
//     request 503 bootstrap_disabled.
//   - every leaf node needs a class="node" JWT for its outbound
//     NodeService.Stream. With no token to present it has nothing to mint with,
//     so it dials with no Authorization header and the peer refuses it
//     Unauthenticated -- forever, on a 30s reconnect loop.
//
// So `component/node/routing.go`'s forward rules never fire on a local cluster:
// no cross-node event is delivered, and the cross-node delivery invariant that
// test/clustere2e exists to protect cannot be exercised at all. A single-node
// pass was the only kind of pass available, which is precisely the false signal
// CLAUDE.md's "multi-node is the DEFAULT" section warns about.
//
// Observed on a 2-replica k3d cluster in TWO distinct signatures, which is why
// the reports looked contradictory. Nodes carrying MEMQL_GENESIS_AUTOLOAD=false
// (cognition, agent, ...) never attempted a mint at all -- maybeBootstrapNodeToken
// returns early on an empty secret -- and logged only "authorization header
// missing". edge, which the local overlay's autoload-off patch omits, autoloaded
// the operator's genesis envelope, found a token there, and DID attempt: its logs
// carry identity's bootstrap_disabled refusal. Same root cause, two symptoms.
//
// This file follows seed_secrets_signing_key_test.go (memql#3400) deliberately:
// same defect shape -- a shared cluster-internal secret that staging gets from
// ESO/Key Vault and local got from nothing -- so the same remedy and the same
// assertions. The tests drive the REAL script against a fake kubectl and assert
// on the recorded `kubectl create secret` argv, because the bug was never in the
// arithmetic of token selection; it was in what the script WRITES to the cluster.

// generatedBootstrapToken is the shape a GENERATED token takes: 64 hex
// characters (32 CSPRNG bytes).
//
// Only the generated path is shape-checked. The runtime imposes NO format --
// identity compares the presented value against the configured one with
// subtle.ConstantTimeCompare over raw bytes -- so validating an
// operator-supplied token would invent a constraint the mesh does not have, and
// would reject a perfectly good secret delivered from Key Vault in staging.
// Hex is chosen for the value we mint ourselves because it travels in an
// `Authorization: Bootstrap <token>` header, where base64's '+' and '=' are
// avoidable questions rather than answered ones.
var generatedBootstrapToken = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestSeedSecretsSeedsANodeBootstrapToken is the regression test for the issue:
// a from-nothing bring-up must leave a usable shared bootstrap secret in
// memql-secrets, which identity and every leaf node read via `envFrom`.
func TestSeedSecretsSeedsANodeBootstrapToken(t *testing.T) {
	stdout, calls, code := runSeedSecrets(t, scenario{})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := seededLiteral(t, calls, "MEMQL_NODE_BOOTSTRAP_TOKEN")
	if !generatedBootstrapToken.MatchString(got) {
		t.Fatalf("seeded MEMQL_NODE_BOOTSTRAP_TOKEN = %q, which is not 64 hex characters.\n"+
			"Without it identity answers every /node/bootstrap 503 bootstrap_disabled and every "+
			"leaf node dials its peers with no Authorization header, so the cross-node event "+
			"mesh never forms (memql#3784).", got)
	}
	if src := parseEnvelope(t, stdout).Result.NodeBootstrapTokenSource; src != "generated" {
		t.Errorf("nodeBootstrapTokenSource = %q, want \"generated\"", src)
	}
}

// TestSeedSecretsNeverRotatesAnExistingNodeBootstrapToken is the idempotency
// invariant, and it is the one with teeth. `make secrets` runs on every
// `make up`, and container environment is read ONCE at container start -- so a
// token regenerated on each run would write a NEW secret into memql-secrets
// while every running pod still holds the old one. identity would then refuse
// the very nodes it had just been agreeing with, and the mesh would break until
// something restarted every pod. That is the #2958 shape exactly: a routine
// re-run breaking a working cluster.
func TestSeedSecretsNeverRotatesAnExistingNodeBootstrapToken(t *testing.T) {
	existing := "b3" + strings.Repeat("7a", 31)

	stdout, calls, code := runSeedSecrets(t, scenario{
		secretState: "present", clusterKey: goodCluster, clusterBootstrapToken: existing,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := seededLiteral(t, calls, "MEMQL_NODE_BOOTSTRAP_TOKEN"); got != existing {
		t.Errorf("seeded MEMQL_NODE_BOOTSTRAP_TOKEN = %q, want the cluster's existing token %q.\n"+
			"Re-running the seeder must not rotate it: running pods read their environment at "+
			"start, so a rotated token breaks every peer connection until all of them restart.", got, existing)
	}
	if src := parseEnvelope(t, stdout).Result.NodeBootstrapTokenSource; src != "cluster" {
		t.Errorf("nodeBootstrapTokenSource = %q, want \"cluster\"", src)
	}
}

// TestSeedSecretsUsesTheEnvNodeBootstrapTokenVerbatim covers the deliberate
// path, and it is what keeps local a parity cluster rather than a special case:
// staging and prod receive this token from ESO/Key Vault, so an operator
// matching a cluster to an externally-managed secret must get exactly the value
// they exported. VERBATIM matters -- any normalisation here would silently
// disagree with the identity service's byte comparison.
func TestSeedSecretsUsesTheEnvNodeBootstrapTokenVerbatim(t *testing.T) {
	want := "an-operator-supplied/secret+with=punctuation"
	_, calls, code := runSeedSecrets(t, scenario{
		envBootstrapToken: want, secretState: "present",
		clusterKey: goodCluster, clusterBootstrapToken: "d4" + strings.Repeat("1c", 31),
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := seededLiteral(t, calls, "MEMQL_NODE_BOOTSTRAP_TOKEN"); got != want {
		t.Errorf("seeded MEMQL_NODE_BOOTSTRAP_TOKEN = %q, want the env value %q verbatim.\n"+
			"identity compares this with subtle.ConstantTimeCompare over raw bytes, so the "+
			"seeder must not reshape it.", got, want)
	}
}

// TestSeedSecretsPreservesAWhitespacePaddedNodeBootstrapToken mirrors the
// signing-key case. BOTH readers TrimSpace before using the value
// (component/node/bootstrap_token.go and component/identity/config.go), so a
// token stored with a stray newline is genuinely in use by the whole mesh.
// Judging it garbage and replacing it would break a working cluster -- the
// precise harm the reuse branch exists to prevent.
func TestSeedSecretsPreservesAWhitespacePaddedNodeBootstrapToken(t *testing.T) {
	want := "e5" + strings.Repeat("9b", 31)
	_, calls, code := runSeedSecrets(t, scenario{
		secretState: "present", clusterKey: goodCluster,
		clusterBootstrapToken: want + "\n",
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := seededLiteral(t, calls, "MEMQL_NODE_BOOTSTRAP_TOKEN"); got != want {
		t.Errorf("seeded MEMQL_NODE_BOOTSTRAP_TOKEN = %q, want the trimmed existing token %q", got, want)
	}
}

// TestSeedSecretsRefusesWhenTheStoredNodeBootstrapTokenIsUnreadable is the
// fail-CLOSED case, and it is here for the reason memql#2958 established: a
// guard whose job is "do not disrupt something already working" must not map
// "I could not tell what is there" onto "there is nothing there".
//
// If an unreadable value were treated as absent, the script would mint a fresh
// token over a live one -- which is the rotation the test above forbids,
// arrived at by a different route.
func TestSeedSecretsRefusesWhenTheStoredNodeBootstrapTokenIsUnreadable(t *testing.T) {
	stdout, calls, code := runSeedSecrets(t, scenario{
		secretState: "present", clusterKey: goodCluster,
		bootstrapTokenReadFails: true,
	})
	if code != 5 {
		t.Errorf("exit code = %d, want 5 (capability contract: op failed)", code)
	}
	if wroteMemqlSecrets(calls) {
		t.Error("memql-secrets was written despite the existing bootstrap token being unreadable; " +
			"that silently rotates a token the running mesh is still using")
	}
	env := parseEnvelope(t, stdout)
	if env.OK {
		t.Error("envelope reports ok=true for a run that could not read the existing token")
	}
}

// TestSeedSecretsNeverLogsTheNodeBootstrapToken pins that the human log stream
// does not print the shared secret. seed-secrets.sh runs in operator terminals
// and its stderr lands in CI logs; anyone holding this value can mint a
// class="node" JWT for any node type in the mesh.
func TestSeedSecretsNeverLogsTheNodeBootstrapToken(t *testing.T) {
	secret := "f6" + strings.Repeat("2e", 31)
	stderr := runSeedSecretsStderr(t, scenario{
		secretState: "present", clusterKey: goodCluster, clusterBootstrapToken: secret,
	})
	if strings.Contains(stderr, secret) {
		t.Errorf("the node bootstrap token appears in the script's human log output.\nstderr:\n%s", stderr)
	}
}

// Package k3d holds tests for the local-cluster capability scripts.
package k3d

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// seed_secrets_master_key_test.go -- memql#2958.
//
// seed-secrets.sh used to fall back to the literal
// "local-dev-placeholder-not-for-production" whenever MEMQL_MASTER_KEY was
// unset. That string is not hex, and the runtime requires 64 hex characters
// (component/secret/encryption.go masterKey), so every node that read it died
// at boot. The script is documented idempotent and `make up` calls it, so an
// ordinary re-run with the variable unset REPLACED a working key with one the
// runtime rejects -- observed taking 7 deployments into CrashLoopBackOff on a
// shared cluster. If the genesis envelope had been sealed under the replaced
// key and no pod still held it in memory, it was unrecoverable.
//
// These tests drive the REAL script against a fake kubectl rather than testing
// the resolver in isolation, because the defect was never in the arithmetic of
// key selection -- it was in what the script ultimately WRITES to the cluster.
// Asserting on the recorded `kubectl create secret` argv is the only form that
// would actually have caught it.
//
// The same reasoning drives the fail-CLOSED cases below. A guard whose job is
// "do not destroy something irreplaceable" must not map "I could not tell"
// onto "there is nothing there" -- that is the original bug via a different
// route, so the read-failure paths are tested as first-class behaviour rather
// than assumed.

// fakeKubectl answers the queries seed-secrets.sh makes and records every
// invocation. Behaviour is driven by env vars so one template serves every
// scenario:
//
//	FAKE_SECRET_STATE  absent | present | error
//	FAKE_MASTER_KEY_B64, FAKE_GENESIS_B64   base64 payloads when present
//	FAKE_JSONPATH_FAILS  non-empty -> the value reads fail while the secret exists
const fakeKubectlTemplate = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_KUBECTL_LOG"
args="$*"

# Existence probe: 'get secret memql-secrets ... -o name'.
case "$args" in
  *"get secret memql-secrets"*"-o name"*)
    case "$FAKE_SECRET_STATE" in
      present) printf 'secret/memql-secrets\n'; exit 0 ;;
      error)   printf 'Error from server: etcdserver: request timed out\n' >&2; exit 1 ;;
      *)       printf 'Error from server (NotFound): secrets "memql-secrets" not found\n' >&2; exit 1 ;;
    esac
    ;;
esac

# Value reads.
case "$args" in
  *"get secret memql-secrets"*jsonpath*MEMQL_MASTER_KEY*)
    [ -n "$FAKE_JSONPATH_FAILS" ] && { printf 'Error from server\n' >&2; exit 1; }
    printf '%s' "$FAKE_MASTER_KEY_B64"; exit 0 ;;
  *"get secret memql-secrets"*jsonpath*MEMQL_GENESIS_B64*)
    [ -n "$FAKE_JSONPATH_FAILS" ] && { printf 'Error from server\n' >&2; exit 1; }
    printf '%s' "$FAKE_GENESIS_B64"; exit 0 ;;
esac

# Internal CA already seeded -> generator skipped.
case "$args" in
  *"get secret memql-ca identity-tls"*) exit 0 ;;
esac

# No Deployments yet -> the voice-lane gate returns early.
case "$args" in
  *"get deploy"*) exit 1 ;;
esac

# 'apply -f -' must drain stdin so the upstream pipe does not SIGPIPE.
case "$args" in
  *"apply"*) cat > /dev/null 2>&1 || true ;;
esac
exit 0
`

// seedResult is the capability-script JSON envelope on stdout.
type seedResult struct {
	OK      bool `json:"ok"`
	Changed bool `json:"changed"`
	Result  struct {
		MasterKeySource string `json:"masterKeySource"`
		Source          string `json:"source"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// scenario configures one run of the script against the fake cluster.
type scenario struct {
	envMasterKey  string // exported only when non-empty
	secretState   string // "absent" (default) | "present" | "error"
	clusterKey    string // plaintext; encoded into the fake when secretState=present
	clusterB64    string // plaintext genesis payload the "cluster" holds
	jsonpathFails bool
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// runSeedSecrets executes scripts/k3d/seed-secrets.sh against a fake kubectl.
// Returns stdout (the JSON envelope), the recorded kubectl argv lines, and the
// exit code.
func runSeedSecrets(t *testing.T, sc scenario) (string, []string, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "k3d", "seed-secrets.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("seed-secrets.sh not found at %s: %v", script, err)
	}

	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "kubectl.log")
	fake := filepath.Join(tmp, "kubectl")
	if err := os.WriteFile(fake, []byte(fakeKubectlTemplate), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}

	state := sc.secretState
	if state == "" {
		state = "absent"
	}
	enc := func(s string) string {
		if s == "" {
			return ""
		}
		return base64.StdEncoding.EncodeToString([]byte(s))
	}
	jsonpathFails := ""
	if sc.jsonpathFails {
		jsonpathFails = "1"
	}

	cmd := exec.Command("bash", script)
	cmd.Dir = root
	env := []string{
		"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + tmp, // no genesis file at $HOME/.memql/genesis.znas
		"FAKE_KUBECTL_LOG=" + logPath,
		"FAKE_SECRET_STATE=" + state,
		"FAKE_MASTER_KEY_B64=" + enc(sc.clusterKey),
		"FAKE_GENESIS_B64=" + enc(sc.clusterB64),
		"FAKE_JSONPATH_FAILS=" + jsonpathFails,
		"MEMQL_K3D_NAMESPACE=memql",
	}
	if sc.envMasterKey != "" {
		env = append(env, "MEMQL_MASTER_KEY="+sc.envMasterKey)
	}
	cmd.Env = env

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running seed-secrets.sh: %v\nstderr:\n%s", err, stderr.String())
	}

	raw, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read kubectl log: %v", readErr)
	}
	var calls []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			calls = append(calls, line)
		}
	}
	t.Logf("exit=%d\nstdout: %s\nstderr:\n%s", code, stdout.String(), stderr.String())
	return stdout.String(), calls, code
}

// seededLiteral extracts a --from-literal value the script asked kubectl to
// write into memql-secrets. Fails when no such create was recorded -- "never
// wrote the secret" must not read as "wrote the right thing".
func seededLiteral(t *testing.T, calls []string, key string) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(key) + `=([^ ]*)`)
	for _, c := range calls {
		if !strings.Contains(c, "create secret generic memql-secrets") {
			continue
		}
		if m := re.FindStringSubmatch(c); m != nil {
			return m[1]
		}
		return "" // the create happened but carried no value for this key
	}
	t.Fatalf("no `create secret generic memql-secrets` was recorded.\ncalls:\n  %s",
		strings.Join(calls, "\n  "))
	return ""
}

func wroteMemqlSecrets(calls []string) bool {
	for _, c := range calls {
		if strings.Contains(c, "create secret generic memql-secrets") {
			return true
		}
	}
	return false
}

// mutatedAnything reports whether the run applied ANY state to the cluster.
// Used to pin that a rejected parameter aborts before the first mutation.
func mutatedAnything(calls []string) []string {
	var muts []string
	for _, c := range calls {
		if strings.Contains(c, "create secret") || strings.Contains(c, "apply") ||
			strings.Contains(c, "scale deploy") {
			muts = append(muts, c)
		}
	}
	return muts
}

func parseEnvelope(t *testing.T, stdout string) seedResult {
	t.Helper()
	var got seedResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &got); err != nil {
		t.Fatalf("stdout is not a single JSON envelope (%v): %q", err, stdout)
	}
	return got
}

// Test keys are BUILT rather than written as literals. A 64-character hex
// string in source is exactly the shape gitleaks' generic-api-key rule looks
// for, and it does not care that this one is a test fixture: an earlier
// revision of this file spelled validHexKey out in full and tripped the
// full-history scan, which fails the merge-queue build for every PR in the
// repo, not just this one. History is append-only, so the cheapest correct
// answer is not to write the shape down at all.
//
// strings.Repeat keeps them valid hex and obviously synthetic.
var (
	validHexKey = strings.Repeat("ab", 32) // 64 hex chars
	goodCluster = strings.Repeat("c7", 32) // 64 hex chars, distinct from the above
)

var hex64 = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// TestSeedSecretsPreservesAnExistingKeyWhenEnvIsUnset is the regression test
// for the reported incident: `make up` re-run WITHOUT MEMQL_MASTER_KEY
// exported, against a cluster that already holds a good key.
func TestSeedSecretsPreservesAnExistingKeyWhenEnvIsUnset(t *testing.T) {
	stdout, calls, code := runSeedSecrets(t, scenario{
		secretState: "present", clusterKey: goodCluster,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := seededLiteral(t, calls, "MEMQL_MASTER_KEY"); got != goodCluster {
		t.Errorf("seeded MEMQL_MASTER_KEY = %q, want the cluster's existing key %q.\n"+
			"Re-running the seeder without MEMQL_MASTER_KEY exported must be a no-op for the "+
			"master key -- overwriting it is memql#2958, which bricked a shared cluster.",
			got, goodCluster)
	}
	if src := parseEnvelope(t, stdout).Result.MasterKeySource; src != "cluster" {
		t.Errorf("masterKeySource = %q, want \"cluster\"", src)
	}
}

// TestSeedSecretsPreservesTheExistingGenesisEnvelope covers the sibling field.
// MEMQL_GENESIS_B64 is written in the same kubectl call three lines from the
// master key and had the same unconditional-overwrite shape: an operator with
// no genesis file on THIS machine used to blank the envelope every other node
// was running on, with only a WARN. Guarding the key while wiping the thing it
// protects would have been a hollow fix.
func TestSeedSecretsPreservesTheExistingGenesisEnvelope(t *testing.T) {
	const envelope = "c2VhbGVkLWVudmVsb3BlLXBheWxvYWQ="

	stdout, calls, code := runSeedSecrets(t, scenario{
		secretState: "present", clusterKey: goodCluster, clusterB64: envelope,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := seededLiteral(t, calls, "MEMQL_GENESIS_B64"); got != envelope {
		t.Errorf("seeded MEMQL_GENESIS_B64 = %q, want the cluster's existing envelope %q.\n"+
			"A re-run with no local genesis file must not blank the envelope the mesh is running on.",
			got, envelope)
	}
	if src := parseEnvelope(t, stdout).Result.Source; src != "cluster" {
		t.Errorf("source = %q, want \"cluster\"", src)
	}
}

// TestSeedSecretsNeverWritesANonHexKey is the invariant the issue reduces to:
// whatever branch resolution takes, the value written is one the runtime can
// actually load.
func TestSeedSecretsNeverWritesANonHexKey(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sc         scenario
		wantSource string
	}{
		{"env supplies a valid key", scenario{envMasterKey: validHexKey}, "env"},
		{"env unset, cluster has a key",
			scenario{secretState: "present", clusterKey: strings.Repeat("f", 64)}, "cluster"},
		{"env unset, no secret at all", scenario{}, "dev-default"},
		// The exact value the old code wrote. A cluster already poisoned by it
		// must be repairable by re-running, not permanently stuck.
		{"env unset, cluster holds the old placeholder",
			scenario{secretState: "present", clusterKey: "local-dev-placeholder-not-for-production"},
			"dev-default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, calls, code := runSeedSecrets(t, tc.sc)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			got := seededLiteral(t, calls, "MEMQL_MASTER_KEY")
			if !hex64.MatchString(got) {
				t.Errorf("seeded MEMQL_MASTER_KEY = %q, which is not 64 hex characters.\n"+
					"The runtime rejects it at boot with \"MEMQL_MASTER_KEY is not valid hex\", "+
					"so every node that reads it crash-loops.", got)
			}
			if src := parseEnvelope(t, stdout).Result.MasterKeySource; src != tc.wantSource {
				t.Errorf("masterKeySource = %q, want %q", src, tc.wantSource)
			}
		})
	}
}

// TestSeedSecretsPreservesAWhitespacePaddedKey pins that the script's notion of
// "valid" matches the RUNTIME's. component/secret/encryption.go applies
// strings.TrimSpace before validating, so a key stored with a stray newline is
// usable by every node. Judging it garbage and replacing it would destroy a
// working key -- the precise harm this change exists to prevent.
func TestSeedSecretsPreservesAWhitespacePaddedKey(t *testing.T) {
	for _, padded := range []string{
		goodCluster + "\n",
		" " + goodCluster,
		"\t" + goodCluster + " \n",
	} {
		t.Run(fmt.Sprintf("%q", padded), func(t *testing.T) {
			stdout, calls, code := runSeedSecrets(t, scenario{
				secretState: "present", clusterKey: padded,
			})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if got := seededLiteral(t, calls, "MEMQL_MASTER_KEY"); got != goodCluster {
				t.Errorf("seeded MEMQL_MASTER_KEY = %q, want the trimmed existing key %q.\n"+
					"The runtime TrimSpaces before validating, so this key works and must be kept.",
					got, goodCluster)
			}
			if src := parseEnvelope(t, stdout).Result.MasterKeySource; src != "cluster" {
				t.Errorf("masterKeySource = %q, want \"cluster\"", src)
			}
		})
	}
}

// TestSeedSecretsFailsClosedWhenTheClusterCannotBeRead is the fail-open guard.
// If the read that decides "is there already a key here?" errors, the script
// must NOT conclude the cluster is empty and seed over it. That conclusion is
// memql#2958 reached by a different trigger, and it would print a false
// explanation ("the cluster has no usable key") while doing it.
func TestSeedSecretsFailsClosedWhenTheClusterCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		sc   scenario
	}{
		{"the existence probe errors", scenario{secretState: "error"}},
		{"the secret exists but its values cannot be read",
			scenario{secretState: "present", jsonpathFails: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, calls, code := runSeedSecrets(t, tc.sc)
			if code == 0 {
				t.Errorf("exit code = 0; a failed read must not be treated as an empty cluster")
			}
			if wroteMemqlSecrets(calls) {
				t.Errorf("memql-secrets was written despite being unable to read what it holds.\ncalls:\n  %s",
					strings.Join(calls, "\n  "))
			}
			if env := parseEnvelope(t, stdout); env.OK {
				t.Error("envelope reports ok=true after a failed cluster read")
			}
		})
	}
}

// TestSeedSecretsRejectsAnInvalidEnvKey pins that a bad key supplied
// DELIBERATELY fails loudly instead of being written. Exit 2 is the capability
// contract's "bad param".
func TestSeedSecretsRejectsAnInvalidEnvKey(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{"not hex", "local-dev-placeholder-not-for-production"},
		{"too short", "abcdef"},
		{"hex but 63 chars", strings.Repeat("a", 63)},
		{"hex but 65 chars", strings.Repeat("a", 65)},
		{"64 chars but not all hex", strings.Repeat("a", 62) + "gg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, calls, code := runSeedSecrets(t, scenario{envMasterKey: tc.key})
			if code != 2 {
				t.Errorf("exit code = %d, want 2 (capability contract: bad param)", code)
			}
			// Rejection must precede EVERY mutation, not just the secret write.
			// Resolution used to happen inside the fourth seeding step, so a
			// rejected key aborted a run that had already applied the CA, the
			// front-door TLS and the DB credentials.
			if muts := mutatedAnything(calls); len(muts) > 0 {
				t.Errorf("the run mutated the cluster before rejecting a bad key:\n  %s",
					strings.Join(muts, "\n  "))
			}
			env := parseEnvelope(t, stdout)
			if env.OK {
				t.Error("envelope reports ok=true for an invalid master key")
			}
			if env.Error == nil || env.Error.Code != 2 {
				t.Errorf("envelope error = %+v, want code 2", env.Error)
			}
		})
	}
}

// TestSeedSecretsUsesTheEnvKeyVerbatim guards the ordinary path: an operator
// who exports a real key gets exactly that key.
func TestSeedSecretsUsesTheEnvKeyVerbatim(t *testing.T) {
	// Uppercase on purpose: hex is case-insensitive and the script must not
	// silently rewrite an operator's key.
	upper := strings.ToUpper(validHexKey)
	_, calls, code := runSeedSecrets(t, scenario{
		envMasterKey: upper, secretState: "present", clusterKey: goodCluster,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := seededLiteral(t, calls, "MEMQL_MASTER_KEY"); got != upper {
		t.Errorf("seeded MEMQL_MASTER_KEY = %q, want the env value %q verbatim", got, upper)
	}
}

// TestSeedSecretsReadsTheTargetNamespace pins that the read-back is scoped to
// the namespace being seeded. A read against the wrong namespace would report
// "no existing key" for a cluster that has one -- fail-open by another route.
func TestSeedSecretsReadsTheTargetNamespace(t *testing.T) {
	_, calls, _ := runSeedSecrets(t, scenario{secretState: "present", clusterKey: goodCluster})
	for _, c := range calls {
		if strings.Contains(c, "get secret memql-secrets") &&
			!strings.Contains(c, "--namespace=memql") {
			t.Errorf("read-back did not target the seeded namespace: %s", c)
		}
	}
}

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
	"sync"
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
// shared cluster. If a stored secret had been sealed under the replaced
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
//	FAKE_MASTER_KEY_B64, FAKE_SIGNING_KEY_B64,
//	FAKE_NODE_BOOTSTRAP_TOKEN_B64
//	                   base64 payloads when present
//	FAKE_JSONPATH_FAILS  non-empty -> the value reads fail while the secret exists
//	FAKE_BOOTSTRAP_TOKEN_READ_FAILS  non-empty -> ONLY the bootstrap-token read
//	                   fails, so the fail-closed branch can be exercised without
//	                   taking the other three reads down with it
//	FAKE_CLUSTER_DOMAIN  the memql-domain ConfigMap's MEMQL_DOMAIN, or empty for
//	                     a cluster that is not serving any domain yet
const fakeKubectlTemplate = `#!/usr/bin/env bash
# Pipeline stages run concurrently. Bash may split a long printf into several
# writes, so each process records its argv separately instead of interleaving
# two invocations in one append-only file.
printf '%s\n' "$*" > "$FAKE_KUBECTL_LOG.$$"
args="$*"

# Optional client-only qualification captures the document at the fake API
# boundary. No real client is ever allowed to apply or read cluster resources.
if [[ -n "${FAKE_KUBECTL_CLIENT:-}" ]]; then
  case "$args" in
    'create secret generic memql-secrets '*--dry-run=client*|'annotate --local '*|'patch --local '*)
      exec "$FAKE_KUBECTL_CLIENT" "$@" ;;
    'patch secret memql-secrets '*--patch-file=/dev/stdin*)
      document="$FAKE_KUBECTL_LOG.manifest.$$"
      cat > "$document"
      if [[ "${FAKE_ROTATE_DURING_READ:-}" != 1 ]]; then exit 0; fi
      version="$("$FAKE_KUBECTL_CLIENT" patch --local --type=merge -f "$document" -p '{}' -o 'jsonpath={.metadata.resourceVersion}')"
      if [[ "$version" == 7 && -f "$FAKE_KUBECTL_LOG.rotation" ]]; then exit 0; fi
      printf 'Error from server (Conflict): fixture rotation changed resourceVersion\n' >&2
      exit 1 ;;
    'apply -f -'|'create -f -')
      cat > "$FAKE_KUBECTL_LOG.manifest.$$"; exit 0 ;;
  esac
fi

# The domain this cluster already serves -- the default for --domain, so a run
# with no --domain does not reissue the certificate for a different one.
case "$args" in
  *"get configmap memql-domain"*jsonpath*)
    printf '%s' "$FAKE_CLUSTER_DOMAIN"; exit 0 ;;
esac

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
  *"get secret memql-secrets"*jsonpath*metadata.resourceVersion*)
    rv=7
    if [[ "${FAKE_ROTATE_DURING_READ:-}" == 1 && ! -f "$FAKE_KUBECTL_LOG.rotation" ]]; then rv=6; fi
    printf '%s|%s|%s' "$rv" "${FAKE_SYNC_OPTIONS:-}" "${FAKE_COMPARE_OPTIONS:-}"; exit 0 ;;
  *"get secret memql-secrets"*jsonpath*MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET*)
    [ -n "$FAKE_CAMPAIGN_READ_FAILS" ] && { printf 'Error from server\n' >&2; exit 1; }
    printf '%s' "$FAKE_CAMPAIGN_KEY_B64"; exit 0 ;;
  *"get secret memql-secrets"*jsonpath*MEMQL_MASTER_KEY*)
    [ -n "$FAKE_JSONPATH_FAILS" ] && { printf 'Error from server\n' >&2; exit 1; }
    printf '%s' "$FAKE_MASTER_KEY_B64"
    if [[ "${FAKE_ROTATE_DURING_READ:-}" == 1 ]]; then : > "$FAKE_KUBECTL_LOG.rotation"; fi
    exit 0 ;;
  *"get secret memql-secrets"*jsonpath*MEMQL_IDENTITY_SIGNING_KEY_B64*)
    [ -n "$FAKE_JSONPATH_FAILS" ] && { printf 'Error from server\n' >&2; exit 1; }
    printf '%s' "$FAKE_SIGNING_KEY_B64"; exit 0 ;;
  *"get secret memql-secrets"*jsonpath*MEMQL_NODE_BOOTSTRAP_TOKEN*)
    [ -n "$FAKE_JSONPATH_FAILS" ] && { printf 'Error from server\n' >&2; exit 1; }
    [ -n "$FAKE_BOOTSTRAP_TOKEN_READ_FAILS" ] && { printf 'Error from server\n' >&2; exit 1; }
    printf '%s' "$FAKE_NODE_BOOTSTRAP_TOKEN_B64"; exit 0 ;;
esac

# Internal CA already seeded -> generator skipped.
case "$args" in
  *"get secret memql-ca identity-tls"*) exit 0 ;;
esac

# No Deployments yet.
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
		MasterKeySource    string `json:"masterKeySource"`
		SigningKeySource   string `json:"signingKeySource"`
		Source             string `json:"source"`
		FrontDoorTLSSource string `json:"frontDoorTlsSource"`
		// memql#3784 -- generated | cluster | env.
		NodeBootstrapTokenSource string `json:"nodeBootstrapTokenSource"`
		// The names the seeded certificate had to cover. Absent from the
		// envelope until memql#3730 -- which is why nothing reading it could
		// see that the cert being seeded was for somebody else's domain.
		FrontDoorTLSHostnames string `json:"frontDoorTlsHostnames"`
		// Whether those names were CHECKED, as opposed to hoped for. `existing`
		// with this false is the one outcome that keeps a pair nobody could read.
		FrontDoorTLSCoverageVerified bool `json:"frontDoorTlsCoverageVerified"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// scenario configures one run of the script against the fake cluster.
type scenario struct {
	// Set only for tests that need the actual client-generated Secret payload
	// at the fake API apply boundary, rather than an argv inventory.
	protectedSecret    *[]byte
	syncOptions        string
	compareOptions     string
	rotateDuringRead   bool
	clusterCampaignKey string
	campaignReadFails  bool
	envMasterKey       string // exported only when non-empty
	envSigningKey      string // MEMQL_IDENTITY_SIGNING_KEY_B64; exported only when non-empty
	secretState        string // "absent" (default) | "present" | "error"
	clusterKey         string // plaintext; encoded into the fake when secretState=present
	clusterSigning     string // plaintext signing seed the "cluster" holds
	jsonpathFails      bool

	// memql#3784.
	envBootstrapToken       string // MEMQL_NODE_BOOTSTRAP_TOKEN; exported only when non-empty
	clusterBootstrapToken   string // plaintext bootstrap token the "cluster" holds
	bootstrapTokenReadFails bool   // only that one read fails

	// githubApp is the MEMQL_GITHUB_APP_* environment for this run (epic
	// memql#4912). A MAP rather than six fields, because the interesting
	// cases are "all six" and "some subset", and naming the subset in the
	// test is what makes a partial-configuration case readable.
	githubApp map[string]string
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
	stdout, _, calls, code := runSeedSecretsFull(t, sc)
	return stdout, calls, code
}

// runSeedSecretsStderr returns only the human-log stream, for assertions
// about what the script prints (e.g. that it never prints key material).
func runSeedSecretsStderr(t *testing.T, sc scenario) string {
	t.Helper()
	_, stderr, _, _ := runSeedSecretsFull(t, sc)
	return stderr
}

// runSeedSecretsFull is the whole harness: stdout (the JSON envelope), stderr
// (the human log), the recorded kubectl argv lines, and the exit code.
func runSeedSecretsFull(t *testing.T, sc scenario) (string, string, []string, int) {
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
	// HOME is redirected to tmp below, so this IS the default front-door pair
	// location (scripts/lib/localtls.sh). Planting it keeps these master-key
	// tests off the issuance path, which is covered in
	// seed_secrets_front_door_tls_test.go.
	writeFrontDoorPair(t, tmp)
	// A PAIR ON DISK IS NO LONGER ENOUGH TO KEEP THIS HERMETIC (memql#3730).
	// This block used to rest on "with a pair already on disk the script reuses
	// it and never reaches for mkcert" -- which was true only because of the
	// short-circuit that made the SAN check unreachable, the bug itself. The
	// front-door step now delegates the reuse-vs-reissue decision to
	// install.mkcert on every run, so these tests must supply the tooling it
	// needs: a stub binary (never the runner's real mkcert, whose -install
	// writes the machine's trust store), a CAROOT that already holds a CA (so
	// nothing is created and no confirmation phrase is needed), and the certutil
	// Linux browser-trust check looks for. The planted pair is unparseable, so
	// cert_mismatches takes its cannot-tell branch and keeps it.
	stubMkcert := filepath.Join(tmp, "bin", "mkcert")
	if err := os.MkdirAll(filepath.Dir(stubMkcert), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(stubMkcert, []byte(frontDoorStubMkcert), 0o755); err != nil {
		t.Fatalf("write mkcert stub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "certutil"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write certutil stub: %v", err)
	}
	stubCaroot := filepath.Join(tmp, "caroot")
	if err := os.MkdirAll(stubCaroot, 0o755); err != nil {
		t.Fatalf("mkdir caroot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stubCaroot, "rootCA.pem"), []byte("operator-ca\n"), 0o644); err != nil {
		t.Fatalf("seed CA: %v", err)
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
	bootstrapReadFails := ""
	if sc.bootstrapTokenReadFails {
		bootstrapReadFails = "1"
	}

	campaignReadFails := ""
	if sc.campaignReadFails {
		campaignReadFails = "1"
	}
	cmd := exec.Command("bash", script, "--mkcert="+stubMkcert)
	cmd.Dir = root
	env := []string{
		"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + tmp,
		"FAKE_KUBECTL_LOG=" + logPath,
		"FAKE_SECRET_STATE=" + state,
		"FAKE_SYNC_OPTIONS=" + sc.syncOptions,
		"FAKE_COMPARE_OPTIONS=" + sc.compareOptions,
		"FAKE_CAMPAIGN_KEY_B64=" + enc(sc.clusterCampaignKey),
		"FAKE_CAMPAIGN_READ_FAILS=" + campaignReadFails,
		"FAKE_MASTER_KEY_B64=" + enc(sc.clusterKey),
		"FAKE_SIGNING_KEY_B64=" + enc(sc.clusterSigning),
		"FAKE_NODE_BOOTSTRAP_TOKEN_B64=" + enc(sc.clusterBootstrapToken),
		"FAKE_JSONPATH_FAILS=" + jsonpathFails,
		"FAKE_BOOTSTRAP_TOKEN_READ_FAILS=" + bootstrapReadFails,
		"MEMQL_K3D_NAMESPACE=memql",
		"STUB_CAROOT=" + stubCaroot,
		"STUB_LOG=" + filepath.Join(tmp, "mkcert-stub.log"),
	}
	if sc.rotateDuringRead {
		env = append(env, "FAKE_ROTATE_DURING_READ=1")
	}
	if sc.envMasterKey != "" {
		env = append(env, "MEMQL_MASTER_KEY="+sc.envMasterKey)
	}
	if sc.envSigningKey != "" {
		env = append(env, "MEMQL_IDENTITY_SIGNING_KEY_B64="+sc.envSigningKey)
	}
	if sc.envBootstrapToken != "" {
		env = append(env, "MEMQL_NODE_BOOTSTRAP_TOKEN="+sc.envBootstrapToken)
	}
	for name, value := range sc.githubApp {
		env = append(env, name+"="+value)
	}
	if sc.protectedSecret != nil {
		client, err := exec.LookPath("kubectl")
		if err != nil {
			t.Skip("kubectl client-side codecs unavailable")
		}
		kubeconfig := filepath.Join(tmp, "empty-kubeconfig")
		if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"), 0600); err != nil {
			t.Fatal(err)
		}
		env = append(env, "FAKE_KUBECTL_CLIENT="+client, "KUBECONFIG="+kubeconfig)
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

	if sc.protectedSecret != nil {
		paths, err := filepath.Glob(logPath + ".manifest.*")
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(body)) != "" {
				if len(*sc.protectedSecret) != 0 {
					t.Fatal("multiple Secret documents reached the fake API")
				}
				*sc.protectedSecret = body
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
	}
	calls := readSeedKubectlCalls(t, logPath)
	t.Logf("exit=%d\nstdout: %s\nstderr:\n%s", code, stdout.String(), stderr.String())
	return stdout.String(), stderr.String(), calls, code
}

// The fixture inventories calls; concurrent pipeline invocations have no total
// order. Each file is read only after its process has exited.
func readSeedKubectlCalls(t *testing.T, prefix string) []string {
	t.Helper()
	paths, err := filepath.Glob(prefix + ".*")
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, strings.TrimSuffix(string(raw), "\n"))
	}
	return calls
}

func TestSeedKubectlRecordsConcurrentLongArgumentVectors(t *testing.T) {
	tmp := t.TempDir()
	script, logPath := filepath.Join(tmp, "kubectl"), filepath.Join(tmp, "calls")
	if err := os.WriteFile(script, []byte(fakeKubectlTemplate), 0o755); err != nil {
		t.Fatal(err)
	}
	const count = 24
	want := make(map[string]bool, count)
	start := make(chan struct{})
	errs := make(chan error, count)
	var jobs sync.WaitGroup
	for n := range count {
		args := fmt.Sprintf("fixture-%d=%s", n, strings.Repeat("x", 16<<10))
		want[args] = true
		jobs.Go(func() {
			<-start
			cmd := exec.Command("bash", script, args)
			cmd.Env = append(os.Environ(), "FAKE_KUBECTL_LOG="+logPath)
			errs <- cmd.Run()
		})
	}
	close(start)
	jobs.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	calls := readSeedKubectlCalls(t, logPath)
	if len(calls) != count {
		t.Fatalf("recorded %d calls, want %d", len(calls), count)
	}
	for _, call := range calls {
		if !want[call] {
			t.Fatal("concurrent argument vectors interleaved or repeated")
		}
		delete(want, call)
	}
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

package k3d

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// status_identity_jwks_test.go -- memql#3400.
//
// CLAUDE.md tells operators to run `make status` after `make scale N=2`, and
// calls it the mesh litmus. It checked one thing: that every pod carries a
// unique MEMQL_NODE_ID. It could not see the defect that actually broke the
// cluster -- two identity replicas each publishing their OWN JWKS `kid`, so
// roughly half of all token verifications failed against whichever replica the
// Service happened to pick.
//
// The boot-time guard in Config.Validate cannot catch this either: it is a
// config check and has no idea how many replicas exist. The litmus is the seam
// that DOES see replica count, and it is the exact command the documentation
// already sends the operator to after scaling. So it asserts the property
// directly: every identity replica must serve the same keyset.
//
// The read goes through the apiserver's pod proxy rather than a port-forward.
// Engine images are FROM scratch (no shell), which is why the node-id litmus
// reads the pod SPEC instead of exec'ing (memql#2380) -- and a port-forward per
// pod would put a background process and a port allocation into a reporter.
// `kubectl get --raw /api/v1/.../pods/https:<pod>:8085/proxy/...` needs
// neither.

// fakeStatusK3d answers the one k3d invocation status.sh makes.
const fakeStatusK3d = `#!/usr/bin/env bash
case "$*" in
  "cluster list")
    printf 'NAME    SERVERS   AGENTS\n'
    printf 'memql   1/1       1/1\n'
    exit 0 ;;
esac
exit 0
`

// fakeStatusKubectl answers status.sh's queries. FAKE_IDENTITY_PODS is a
// space-separated pod list; FAKE_JWKS_<pod-with-dashes-as-underscores> is the
// JWKS body that pod serves. An absent body means the proxy read fails.
const fakeStatusKubectl = `#!/usr/bin/env bash
args="$*"

case "$args" in
  *"config use-context"*) exit 0 ;;
  *"get namespace argocd"*) exit 1 ;;
  *"get namespace memql"*) exit 0 ;;
  *"get application"*) exit 1 ;;
esac

# Identity replica listing (label selector).
case "$args" in
  *"get pods"*"app.kubernetes.io/name=identity"*)
    printf '%s' "$FAKE_IDENTITY_PODS"; exit 0 ;;
esac

# apiserver pod-proxy read of a replica's JWKS.
case "$args" in
  *"--raw"*proxy*jwks.json*)
    pod=""
    for p in $FAKE_IDENTITY_PODS; do
      case "$args" in *"$p"*) pod="$p" ;; esac
    done
    [ -z "$pod" ] && exit 1
    var="FAKE_JWKS_$(printf '%s' "$pod" | tr '-' '_')"
    body="${!var:-}"
    [ -z "$body" ] && { printf 'Error from server\n' >&2; exit 1; }
    printf '%s' "$body"; exit 0 ;;
esac

# Mesh pod listing for the node-id litmus.
case "$args" in
  *"get pods"*jsonpath*metadata.name*) printf '%s' "$FAKE_IDENTITY_PODS"; exit 0 ;;
esac

case "$args" in
  *"get pod "*jsonpath*MEMQL_NODE_ID*) printf 'metadata.name'; exit 0 ;;
  *"get pods"*) printf 'NAME  READY  STATUS\n'; exit 0 ;;
  *"get secret"*) exit 0 ;;
esac
exit 0
`

// jwksWithKid renders a minimal JWKS document carrying one key.
func jwksWithKid(kid string) string {
	return `{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"` + kid + `","x":"AAAA","alg":"EdDSA","use":"sig"}]}`
}

type statusResult struct {
	OK     bool `json:"ok"`
	Result struct {
		MeshHealthy        bool  `json:"meshHealthy"`
		IdentityReplicas   int   `json:"identityReplicas"`
		IdentityKeysets    int   `json:"identityKeysets"`
		IdentityKeysShared *bool `json:"identityKeysShared"`
	} `json:"result"`
}

// runStatus executes the real status.sh against the fakes above.
// jwks maps pod name -> served JWKS body ("" means the read fails).
func runStatus(t *testing.T, jwks map[string]string, order []string) (statusResult, string, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "k3d", "status.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("status.sh not found at %s: %v", script, err)
	}

	tmp := t.TempDir()
	for name, body := range map[string]string{"k3d": fakeStatusK3d, "kubectl": fakeStatusKubectl} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	env := []string{
		"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + tmp,
		"MEMQL_K3D_NAMESPACE=memql",
		"MEMQL_K3D_CLUSTER=memql",
		"FAKE_IDENTITY_PODS=" + strings.Join(order, " "),
	}
	for _, pod := range order {
		env = append(env, "FAKE_JWKS_"+strings.ReplaceAll(pod, "-", "_")+"="+jwks[pod])
	}

	cmd := exec.Command("bash", script)
	cmd.Dir = root
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running status.sh: %v\nstderr:\n%s", err, stderr.String())
	}
	t.Logf("exit=%d\nstdout: %s\nstderr:\n%s", code, stdout.String(), stderr.String())

	var got statusResult
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &got); jerr != nil {
		t.Fatalf("stdout is not a single JSON envelope (%v): %q", jerr, stdout.String())
	}
	return got, stderr.String(), code
}

// TestStatusFlagsDivergingIdentityKeysets is the regression test: the exact
// two-replica, two-kid cluster from the issue must be reported as broken.
func TestStatusFlagsDivergingIdentityKeysets(t *testing.T) {
	pods := []string{"identity-5f45458d79-924h5", "identity-5f45458d79-qpcq2"}
	got, stderr, code := runStatus(t, map[string]string{
		pods[0]: jwksWithKid("859BkEwzf6g"),
		pods[1]: jwksWithKid("biMg9gN11xg"),
	}, pods)

	if code != 0 {
		t.Errorf("exit code = %d, want 0 (status is a reporter, like the node-id litmus)", code)
	}
	if got.Result.IdentityKeysShared == nil || *got.Result.IdentityKeysShared {
		t.Fatalf("identityKeysShared = %v, want false.\n"+
			"Two replicas publishing different kids means ~half of all token verifications "+
			"fail with 'unknown kid'; the litmus must say so (memql#3400).", got.Result.IdentityKeysShared)
	}
	if got.Result.IdentityKeysets != 2 {
		t.Errorf("identityKeysets = %d, want 2", got.Result.IdentityKeysets)
	}
	if got.Result.IdentityReplicas != 2 {
		t.Errorf("identityReplicas = %d, want 2", got.Result.IdentityReplicas)
	}
	if !strings.Contains(stderr, "WARN") || !strings.Contains(stderr, "kid") {
		t.Errorf("the human report does not warn about the diverging kids.\nstderr:\n%s", stderr)
	}
}

// TestStatusPassesWhenEveryIdentityReplicaSharesAKeyset is the state a seeded
// cluster reaches: one shared MEMQL_IDENTITY_SIGNING_KEY_B64 -> one derived
// key -> one kid on every replica.
func TestStatusPassesWhenEveryIdentityReplicaSharesAKeyset(t *testing.T) {
	pods := []string{"identity-aaa-1", "identity-aaa-2"}
	body := jwksWithKid("ln-RlCxzK8o")
	got, _, code := runStatus(t, map[string]string{pods[0]: body, pods[1]: body}, pods)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got.Result.IdentityKeysShared == nil || !*got.Result.IdentityKeysShared {
		t.Errorf("identityKeysShared = %v, want true", got.Result.IdentityKeysShared)
	}
	if got.Result.IdentityKeysets != 1 {
		t.Errorf("identityKeysets = %d, want 1", got.Result.IdentityKeysets)
	}
}

// TestStatusReportsUnknownRatherThanPassWhenAReplicaCannotBeRead pins the
// fail-open guard. A litmus that reports "shared" because it could only reach
// one of two replicas is worse than one that reports nothing: it certifies the
// property it failed to measure.
func TestStatusReportsUnknownRatherThanPassWhenAReplicaCannotBeRead(t *testing.T) {
	pods := []string{"identity-bbb-1", "identity-bbb-2"}
	got, stderr, code := runStatus(t, map[string]string{
		pods[0]: jwksWithKid("ln-RlCxzK8o"),
		pods[1]: "", // proxy read fails
	}, pods)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got.Result.IdentityKeysShared != nil {
		t.Errorf("identityKeysShared = %v, want null (unread replica -- the property was not measured)",
			*got.Result.IdentityKeysShared)
	}
	if !strings.Contains(stderr, "UNKNOWN") {
		t.Errorf("an unreadable replica must be reported as unknown, not as a pass.\nstderr:\n%s", stderr)
	}
}

// TestStatusReportsUnknownWithNoIdentityPods keeps a cluster that has not
// synced yet from reading as a pass.
func TestStatusReportsUnknownWithNoIdentityPods(t *testing.T) {
	got, _, code := runStatus(t, map[string]string{}, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got.Result.IdentityKeysShared != nil {
		t.Errorf("identityKeysShared = %v, want null with no identity pods", *got.Result.IdentityKeysShared)
	}
}

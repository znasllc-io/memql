package k3d

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Tests for scripts/k3d/federation-keys.sh (capability k3d.federationKeys,
// design record 2026-11-21-vendor-accounts-and-api-keys, A6).
//
// WHAT IT IS FOR. A vendor can only federate with a cluster whose public keys
// it can read. A cloud cluster publishes them; a local cluster's issuer is
// private, so the keys are pasted into the vendor's console instead, and both
// vendors accept that. The script prints exactly what gets pasted: the issuer,
// the public keys, the subject each engine pod's token carries, and the two
// audiences.
//
// Hermetic: a stub `kubectl` on a PATH prefix answers the three reads and
// records its argv. No cluster is contacted.

type federationKeysEnvelope struct {
	OK         bool   `json:"ok"`
	Capability string `json:"capability"`
	Result     struct {
		Issuer string `json:"issuer"`
		Jwks   struct {
			Keys []map[string]any `json:"keys"`
		} `json:"jwks"`
		Subjects  []string          `json:"subjects"`
		Audiences map[string]string `json:"audiences"`
		Namespace string            `json:"namespace"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

const fakeFederationKubectl = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_ARGV"
if [ "${FAKE_KUBECTL_FAIL:-}" = "1" ]; then
  echo "The connection to the server 127.0.0.1:6443 was refused" >&2
  exit 1
fi
case "$*" in
  *"--raw /.well-known/openid-configuration"*)
    echo '{"issuer":"https://kubernetes.default.svc.cluster.local","jwks_uri":"https://10.0.0.1:6443/openid/v1/jwks","response_types_supported":["id_token"]}' ;;
  *"--raw /openid/v1/jwks"*)
    echo '{"keys":[{"use":"sig","kty":"RSA","kid":"kid-1","alg":"RS256","n":"abc","e":"AQAB"}]}' ;;
  *"get deployments"*)
    printf 'memql-engine\nmemql-engine\nmemql-engine-workbench\nmemql-engine\n\n' ;;
  *)
    echo "unexpected kubectl call: $*" >&2
    exit 9 ;;
esac
`

// runFederationKeys runs the real script against the stub kubectl.
func runFederationKeys(t *testing.T, env []string, args ...string) (stdout, stderr, argv string, code int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(fakeFederationKubectl), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	argvFile := filepath.Join(t.TempDir(), "argv")

	cmd := exec.Command("bash", append([]string{filepath.Join(root, "scripts", "k3d", "federation-keys.sh")}, args...)...)
	cmd.Dir = root
	cmd.Env = append([]string{
		"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + tmp,
		"FAKE_ARGV=" + argvFile,
	}, env...)
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run federation-keys.sh: %v", err)
	}
	recorded, _ := os.ReadFile(argvFile)
	return out.String(), errOut.String(), string(recorded), code
}

func TestFederationKeysPrintsWhatAVendorNeeds(t *testing.T) {
	stdout, stderr, argv, code := runFederationKeys(t, nil)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// Exactly one JSON object on stdout: the contract's envelope and nothing
	// beside it.
	var env federationKeysEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, stdout)
	}
	if !env.OK || env.Capability != "k3d.federationKeys" {
		t.Fatalf("envelope ok=%v capability=%q, want true and k3d.federationKeys", env.OK, env.Capability)
	}
	if env.Result.Issuer != "https://kubernetes.default.svc.cluster.local" {
		t.Errorf("issuer = %q", env.Result.Issuer)
	}
	if len(env.Result.Jwks.Keys) != 1 || env.Result.Jwks.Keys[0]["kid"] != "kid-1" {
		t.Errorf("jwks = %+v, want the cluster's one public key", env.Result.Jwks)
	}
	// One subject per DISTINCT service account the engine pods run as, sorted:
	// the vendor's rule matches on it, and listing a name four times because
	// four Deployments share it is four chances to paste the wrong line.
	wantSubjects := []string{
		"system:serviceaccount:memql:memql-engine",
		"system:serviceaccount:memql:memql-engine-workbench",
	}
	if !reflect.DeepEqual(env.Result.Subjects, wantSubjects) {
		t.Errorf("subjects = %v, want %v", env.Result.Subjects, wantSubjects)
	}
	if env.Result.Audiences["anthropic"] != "https://api.anthropic.com" ||
		env.Result.Audiences["openai"] != "https://api.openai.com/v1" {
		t.Errorf("audiences = %v, want the two the manifests mint tokens for", env.Result.Audiences)
	}

	// The identity node is not asked about. It runs under a service account of
	// its own, never calls a model, and the vendor's rule must not admit it:
	// listing its subject here would invite somebody to paste it.
	for _, line := range strings.Split(argv, "\n") {
		if !strings.Contains(line, "get deployments") {
			continue
		}
		fields := strings.Fields(line)
		named := map[string]bool{}
		for _, field := range fields {
			named[field] = true
		}
		if named["identity"] {
			t.Errorf("the identity Deployment's service account was read: %q", line)
		}
		for _, want := range []string{"bff", "agent", "planner", "workbench", "mcp", "edge"} {
			if !named[want] {
				t.Errorf("the %s Deployment's service account was not read: %q", want, line)
			}
		}
	}

	// A person reads stderr, and it has to say where each value goes.
	for _, want := range []string{"Anthropic", "OpenAI", "public"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the human output does not mention %q:\n%s", want, stderr)
		}
	}
	// Public material only. The script reads two public documents and a list
	// of names; nothing it prints can be a key that signs.
	if strings.Contains(stdout+stderr, "PRIVATE KEY") {
		t.Fatal("the output contains private key material")
	}
}

func TestFederationKeysReadsTheNamedClusterAndNamespace(t *testing.T) {
	stdout, stderr, argv, code := runFederationKeys(t, nil, "--context=k3d-other", "--namespace=memql-lab")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, line := range strings.Split(strings.TrimSpace(argv), "\n") {
		if !strings.Contains(line, "--context k3d-other") && !strings.Contains(line, "--context=k3d-other") {
			t.Errorf("a kubectl call did not name the context it was told to read: %q", line)
		}
	}
	if !strings.Contains(argv, "memql-lab") {
		t.Errorf("no kubectl call named the namespace it was told to read:\n%s", argv)
	}
	var env federationKeysEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v", err)
	}
	if len(env.Result.Subjects) == 0 || !strings.HasPrefix(env.Result.Subjects[0], "system:serviceaccount:memql-lab:") {
		t.Errorf("subjects = %v, want them in the namespace that was named", env.Result.Subjects)
	}
}

func TestFederationKeysSpecNamesItsParams(t *testing.T) {
	stdout, _, _, code := runFederationKeys(t, nil, "--print-spec")
	if code != 0 {
		t.Fatalf("--print-spec exited %d", code)
	}
	for _, want := range []string{"k3d.federationKeys", "context", "namespace"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the spec does not mention %q:\n%s", want, stdout)
		}
	}
}

func TestFederationKeysFailsHonestlyWhenTheClusterCannotBeRead(t *testing.T) {
	stdout, _, _, code := runFederationKeys(t, []string{"FAKE_KUBECTL_FAIL=1"})
	if code != 5 {
		t.Fatalf("an unreadable cluster exited %d, want 5\nstdout: %s", code, stdout)
	}
	var env federationKeysEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, stdout)
	}
	if env.OK || env.Error == nil || env.Error.Code != 5 {
		t.Fatalf("want ok=false error.code=5, got: %s", stdout)
	}
}

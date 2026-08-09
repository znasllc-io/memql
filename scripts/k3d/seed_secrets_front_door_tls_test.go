package k3d

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seed_secrets_front_door_tls_test.go -- memql#3384.
//
// Both local front-door ingresses (cockpit-front-door / identity-front-door)
// name the `local-znas-tls` secret in their spec.tls. seed-secrets.sh used to
// default to a dev.{crt,key} pair inside the repo's old `docker/` tree -- which
// was deleted when the Compose local stack was retired -- and, finding nothing
// there, WARN and move on. Traefik answers a missing referenced secret by serving its own
// "TRAEFIK DEFAULT CERT", so every TLS client saw an untrusted edge while the
// bring-up printed a green summary.
//
// These tests drive the REAL script against a fake kubectl and a STUB mkcert.
// Nothing here may reach the runner's trust store or its real certificate
// directory: the stub records its argv, so "reused rather than reissued" is an
// assertion about what was actually invoked, not about what the envelope
// claims. The point of the fix is what ends up IN the cluster, so the
// assertions are on the recorded `kubectl create secret tls` argv.

// frontDoorStubMkcert is a fake mkcert. It answers the three invocations
// mkcert-setup.sh makes and logs every argv to $STUB_LOG.
//
//	mkcert -CAROOT                      -> prints $STUB_CAROOT
//	mkcert -install                     -> creates $STUB_CAROOT/rootCA.pem
//	mkcert -cert-file X -key-file Y ...  -> writes X and Y
const frontDoorStubMkcert = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG"

case "$1" in
  -CAROOT) printf '%s\n' "$STUB_CAROOT"; exit 0 ;;
  -install)
    mkdir -p "$STUB_CAROOT"
    printf 'stub-root-ca\n' > "$STUB_CAROOT/rootCA.pem"
    exit 0 ;;
esac

cert=""; key=""
while [ $# -gt 0 ]; do
  case "$1" in
    -cert-file) cert="$2"; shift 2 ;;
    -key-file)  key="$2";  shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$cert" ] && printf 'stub-cert\n' > "$cert"
[ -n "$key" ]  && printf 'stub-key\n'  > "$key"
exit 0
`

// frontDoorCert / frontDoorKey are the default pair locations relative to a
// redirected HOME -- i.e. what scripts/lib/localtls.sh resolves to. Spelling
// them out here (rather than importing them) is deliberate: if the default
// moves, this test must be updated consciously, because moving it silently is
// the shape of the original bug.
func frontDoorCert(home string) string {
	return filepath.Join(home, ".memql", "certs", "dev.crt")
}

func frontDoorKey(home string) string {
	return filepath.Join(home, ".memql", "certs", "dev.key")
}

// writeFrontDoorPair plants an already-issued front-door pair at the default
// location under `home`, so the script's reuse branch is taken.
func writeFrontDoorPair(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(frontDoorCert(home)), 0o755); err != nil {
		t.Fatalf("mkdir certs: %v", err)
	}
	if err := os.WriteFile(frontDoorCert(home), []byte("existing-cert\n"), 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(frontDoorKey(home), []byte("existing-key\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// frontDoorEnv is one hermetic run of seed-secrets.sh: a fake kubectl and a
// stub mkcert on a PATH prefix, HOME redirected into t.TempDir() (so the
// default pair location is inside the sandbox), and a CAROOT of our own.
type frontDoorEnv struct {
	home       string
	kubectlLog string
	stubLog    string
	stubMkcert string
	caroot     string
}

func newFrontDoorEnv(t *testing.T) *frontDoorEnv {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	home := t.TempDir()
	e := &frontDoorEnv{
		home:       home,
		kubectlLog: filepath.Join(home, "kubectl.log"),
		stubLog:    filepath.Join(home, "stub.log"),
		stubMkcert: filepath.Join(home, "bin", "mkcert"),
		caroot:     filepath.Join(home, "caroot"),
	}
	if err := os.MkdirAll(filepath.Dir(e.stubMkcert), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.MkdirAll(e.caroot, 0o755); err != nil {
		t.Fatalf("mkdir caroot: %v", err)
	}
	if err := os.WriteFile(e.stubMkcert, []byte(frontDoorStubMkcert), 0o755); err != nil {
		t.Fatalf("write mkcert stub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "kubectl"), []byte(fakeKubectlTemplate), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	return e
}

// seedCA plants a root CA, so mkcert-setup.sh issues without demanding the
// install confirmation phrase.
func (e *frontDoorEnv) seedCA(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.caroot, "rootCA.pem"), []byte("operator-ca\n"), 0o644); err != nil {
		t.Fatalf("seed CA: %v", err)
	}
}

// run executes the real seed-secrets.sh and returns its envelope, the recorded
// kubectl argv lines, and the exit code.
func (e *frontDoorEnv) run(t *testing.T, args ...string) (seedResult, []string, int) {
	t.Helper()
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "k3d", "seed-secrets.sh")

	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = root
	cmd.Stdin = nil
	cmd.Env = []string{
		"PATH=" + e.home + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + e.home,
		"FAKE_KUBECTL_LOG=" + e.kubectlLog,
		"FAKE_SECRET_STATE=absent",
		"FAKE_MASTER_KEY_B64=",
		"FAKE_GENESIS_B64=",
		"FAKE_JSONPATH_FAILS=",
		"MEMQL_K3D_NAMESPACE=memql",
		"STUB_CAROOT=" + e.caroot,
		"STUB_LOG=" + e.stubLog,
	}

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
	t.Logf("exit=%d\nstdout: %s\nstderr:\n%s", code, stdout.String(), stderr.String())

	var res seedResult
	if line := strings.TrimSpace(stdout.String()); line != "" {
		if jerr := json.Unmarshal([]byte(lastJSONObject(line)), &res); jerr != nil {
			t.Fatalf("stdout is not a JSON envelope: %v\nstdout: %s", jerr, line)
		}
	}
	return res, e.kubectlCalls(t), code
}

func (e *frontDoorEnv) kubectlCalls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(e.kubectlLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read kubectl log: %v", err)
	}
	var calls []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			calls = append(calls, l)
		}
	}
	return calls
}

func (e *frontDoorEnv) mkcertLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.stubLog)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read stub log: %v", err)
	}
	return string(b)
}

func lastJSONObject(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			return l
		}
	}
	return ""
}

func tlsSecretCall(calls []string) string {
	for _, c := range calls {
		if strings.Contains(c, "create secret tls local-znas-tls") {
			return c
		}
	}
	return ""
}

//=============================================================================
// THE REGRESSION: the secret is created, not skipped
//=============================================================================

// The defect itself: with no pair on disk the script warned and returned, so
// local-znas-tls was never created and traefik fell back to its default cert.
// It must now ISSUE the pair and seed the secret.
func TestSeedSecretsIssuesTheFrontDoorPairWhenAbsent(t *testing.T) {
	e := newFrontDoorEnv(t)
	e.seedCA(t)

	res, calls, code := e.run(t, "--mkcert="+e.stubMkcert)

	if code != 0 {
		t.Fatalf("exit %d, want 0; envelope: %+v", code, res)
	}
	if !res.OK {
		t.Fatalf("envelope ok=false: %+v", res)
	}
	if _, err := os.Stat(frontDoorCert(e.home)); err != nil {
		t.Errorf("certificate was not issued at the default path %s: %v", frontDoorCert(e.home), err)
	}
	if _, err := os.Stat(frontDoorKey(e.home)); err != nil {
		t.Errorf("key was not issued at the default path %s: %v", frontDoorKey(e.home), err)
	}
	call := tlsSecretCall(calls)
	if call == "" {
		t.Fatalf("local-znas-tls was never seeded -- the ingresses reference it, so this is "+
			"the whole bug (#3384). kubectl calls:\n%s", strings.Join(calls, "\n"))
	}
	if !strings.Contains(call, "--cert="+frontDoorCert(e.home)) {
		t.Errorf("secret seeded from an unexpected certificate: %s", call)
	}
	if res.Result.FrontDoorTLSSource != "issued" {
		t.Errorf("frontDoorTlsSource = %q, want %q", res.Result.FrontDoorTLSSource, "issued")
	}
	if !strings.Contains(e.mkcertLog(t), "-cert-file") {
		t.Errorf("mkcert was never asked to issue a certificate; stub log:\n%s", e.mkcertLog(t))
	}
}

// Idempotency: `make secrets` runs on every `make up`. Rotating the front-door
// certificate as a side effect of a routine re-run would invalidate whatever
// already trusts it, so an existing pair must be loaded verbatim and mkcert
// must not be invoked at all.
func TestSeedSecretsReusesAnExistingFrontDoorPair(t *testing.T) {
	e := newFrontDoorEnv(t)
	e.seedCA(t)
	writeFrontDoorPair(t, e.home)

	res, calls, code := e.run(t, "--mkcert="+e.stubMkcert)

	if code != 0 || !res.OK {
		t.Fatalf("exit %d ok=%v; envelope: %+v", code, res.OK, res)
	}
	if log := e.mkcertLog(t); log != "" {
		t.Errorf("mkcert was invoked despite an existing pair -- a re-run must not "+
			"rotate the front-door certificate.\nstub log:\n%s", log)
	}
	if got, err := os.ReadFile(frontDoorCert(e.home)); err != nil || string(got) != "existing-cert\n" {
		t.Errorf("existing certificate was overwritten: %q (%v)", string(got), err)
	}
	if tlsSecretCall(calls) == "" {
		t.Errorf("local-znas-tls not seeded from the existing pair; kubectl calls:\n%s",
			strings.Join(calls, "\n"))
	}
	if res.Result.FrontDoorTLSSource != "existing" {
		t.Errorf("frontDoorTlsSource = %q, want %q", res.Result.FrontDoorTLSSource, "existing")
	}
}

//=============================================================================
// FAILING LOUDLY -- the second half of the bug
//=============================================================================

// A missing mkcert is a missing PREREQUISITE (exit 4), not something to warn
// about and continue past. Continuing is what produced a green bring-up over a
// broken front door.
func TestSeedSecretsFailsLoudlyWhenMkcertIsMissing(t *testing.T) {
	e := newFrontDoorEnv(t)
	e.seedCA(t)

	res, calls, code := e.run(t, "--mkcert="+filepath.Join(e.home, "no-such-mkcert"))

	if code != 4 {
		t.Fatalf("exit %d, want 4 (prerequisite missing); envelope: %+v", code, res)
	}
	if res.OK {
		t.Errorf("envelope ok=true for a run that could not issue the certificate")
	}
	if res.Error == nil || res.Error.Code != 4 {
		t.Fatalf("want error.code 4; envelope: %+v", res)
	}
	if !strings.Contains(res.Error.Message, "mkcert") {
		t.Errorf("error message does not name the missing prerequisite: %q", res.Error.Message)
	}
	if call := tlsSecretCall(calls); call != "" {
		t.Errorf("a TLS secret was seeded despite issuance failing: %s", call)
	}
}

// No root CA on the machine is also exit 4, and the message must carry the one
// command that fixes it -- creating a CA writes to the system trust store, so
// it stays a deliberate operator step (never a prompt: contract rule 3).
func TestSeedSecretsFailsWithGuidanceWhenNoRootCAExists(t *testing.T) {
	e := newFrontDoorEnv(t) // caroot exists but holds no rootCA.pem

	res, _, code := e.run(t, "--mkcert="+e.stubMkcert)

	if code != 4 {
		t.Fatalf("exit %d, want 4; envelope: %+v", code, res)
	}
	if res.Error == nil || !strings.Contains(res.Error.Message, "--confirm=install-memql-ca") {
		t.Errorf("error message must name the CA-install command; got: %+v", res.Error)
	}
	if strings.Contains(e.mkcertLog(t), "-install") {
		t.Errorf("mkcert -install was run without the operator's confirmation;\nstub log:\n%s",
			e.mkcertLog(t))
	}
}

// Gate for scripts/deploy/verify-internal-tls.sh (znasllc-io/memql#4599).
//
// The failure this detects is silent in the worst way: every pod stays Running
// and Ready, ArgoCD reports Healthy, and the only symptom is a 502 on
// "Continue to sign in". So the check itself has to be exercised against real
// certificates -- a check for an invisible failure that is never observed
// failing is not known to detect anything.
//
// The two CAs in the incident shared a common name (memql-internal-ca) and
// differed only in key, so the same-CN case is the one that matters and is
// asserted explicitly.
package deploy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A kubectl whose `get secret` answers from files the test wrote, in the
// jsonpath shape the script asks for.
const tlsKubectlStub = `#!/usr/bin/env bash
args=("$@")
for ((i=0; i<${#args[@]}; i++)); do
  if [[ "${args[$i]}" == "get" && "${args[$i+1]}" == "secret" ]]; then
    secret="${args[$i+2]}"
  fi
  if [[ "${args[$i]}" == "-o" ]]; then
    jsonpath="${args[$i+1]}"
  fi
done
key=""
case "$jsonpath" in
  *"data.ca\\.crt"*)  key="ca.crt" ;;
  *"data.tls\\.crt"*) key="tls.crt" ;;
esac
f="$FIXTURES/${secret}.${key}.b64"
[ -f "$f" ] && cat "$f"
exit 0
`

type tlsEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type tlsResult struct {
	Matches  *bool  `json:"matches"`
	NextStep string `json:"nextStep"`
}

// openssl generates the fixtures, because the point is to test what the script
// does with certificates a real cluster would hold.
func genCA(t *testing.T, dir, name, subject string) (crt, key string) {
	t.Helper()
	crt = filepath.Join(dir, name+".crt")
	key = filepath.Join(dir, name+".key")
	run(t, "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
		"-keyout", key, "-out", crt, "-days", "3650", "-subj", subject)
	return crt, key
}

func genLeaf(t *testing.T, dir, name, subject, caCrt, caKey string) string {
	t.Helper()
	csr := filepath.Join(dir, name+".csr")
	key := filepath.Join(dir, name+".key")
	crt := filepath.Join(dir, name+".crt")
	run(t, "openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes",
		"-keyout", key, "-out", csr, "-subj", subject)
	run(t, "openssl", "x509", "-req", "-in", csr, "-CA", caCrt, "-CAkey", caKey,
		"-CAcreateserial", "-out", crt, "-days", "825")
	return crt
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// writeSecretKey stages one Secret key as the base64 the stub returns.
func writeSecretKey(t *testing.T, fixtures, secret, key, pemPath string) {
	t.Helper()
	pem, err := os.ReadFile(pemPath)
	if err != nil {
		t.Fatalf("read %s: %v", pemPath, err)
	}
	enc := base64.StdEncoding.EncodeToString(pem)
	dst := filepath.Join(fixtures, secret+"."+key+".b64")
	if err := os.WriteFile(dst, []byte(enc), 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func runVerify(t *testing.T, fixtures string) (tlsEnvelope, tlsResult, int) {
	t.Helper()
	for _, tool := range []string{"bash", "openssl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(tlsKubectlStub), 0o755); err != nil {
		t.Fatalf("write kubectl stub: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	cmd := exec.Command("bash", filepath.Join(wd, "verify-internal-tls.sh"), "--namespace=memql")
	cmd.Stdin = nil
	cmd.Env = append(append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH")),
		"FIXTURES="+fixtures)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	code := 0
	if runErr := cmd.Run(); runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v", runErr)
		}
	}
	t.Logf("exit=%d\nstdout: %s\nstderr:\n%s", code, out.String(), errb.String())

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var env tlsEnvelope
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, out.String())
	}
	var res tlsResult
	if len(env.Result) > 0 {
		_ = json.Unmarshal(env.Result, &res)
	}
	return env, res, code
}

// THE INCIDENT. Two CAs, both named memql-internal-ca, differing only in key.
// identity-tls is signed by one; the mesh mounts the other. Nothing about the
// names distinguishes them, which is exactly why a name comparison would have
// passed this and a 502 would have been the first sign.
func TestVerifyInternalTlsCatchesALeafSignedByASupersededCA(t *testing.T) {
	dir := t.TempDir()
	fixtures := t.TempDir()

	outgoing, outgoingKey := genCA(t, dir, "outgoing", "/CN=memql-internal-ca")
	incoming, _ := genCA(t, dir, "incoming", "/O=MemQL/CN=memql-internal-ca")
	// The leaf was signed by the OUTGOING CA, a second before the bundle was
	// replaced by the INCOMING one.
	leaf := genLeaf(t, dir, "identity", "/CN=identity", outgoing, outgoingKey)

	writeSecretKey(t, fixtures, "memql-ca", "ca.crt", incoming)
	writeSecretKey(t, fixtures, "identity-tls", "tls.crt", leaf)

	env, res, code := runVerify(t, fixtures)

	if code != 3 {
		t.Errorf("exit %d, want 3 (refused). A mismatch must be a REFUSAL a lifecycle step can\n"+
			"branch on -- the whole point is to fail the preflight instead of the sign-in.", code)
	}
	if env.OK {
		t.Errorf("ok=true for a mesh that cannot trust identity")
	}
	if res.Matches == nil || *res.Matches {
		t.Errorf("matches=%v, want false", res.Matches)
	}
	if !strings.Contains(res.NextStep, "identity-tls") {
		t.Errorf("nextStep does not name the Secret to delete: %q", res.NextStep)
	}
}

// The healthy case must be quiet and exit 0, or the check is noise an operator
// learns to ignore -- the same failure mode as a permanently Degraded app.
func TestVerifyInternalTlsPassesWhenTheBundleSignedTheLeaf(t *testing.T) {
	dir := t.TempDir()
	fixtures := t.TempDir()

	ca, caKey := genCA(t, dir, "ca", "/O=MemQL/CN=memql-internal-ca")
	leaf := genLeaf(t, dir, "identity", "/CN=identity", ca, caKey)

	writeSecretKey(t, fixtures, "memql-ca", "ca.crt", ca)
	writeSecretKey(t, fixtures, "identity-tls", "tls.crt", leaf)

	env, res, code := runVerify(t, fixtures)
	if code != 0 {
		t.Errorf("exit %d, want 0 for a bundle that did sign the leaf", code)
	}
	if !env.OK {
		t.Errorf("ok=false for a healthy chain")
	}
	if res.Matches == nil || !*res.Matches {
		t.Errorf("matches=%v, want true", res.Matches)
	}
}

// A missing Secret is a different answer from a mismatched one: nothing is
// wrong with the certificates, the check could not run. Exit 5, not 3, so a
// caller cannot read "I could not look" as "they disagree".
func TestVerifyInternalTlsSeparatesCannotCheckFromDisagree(t *testing.T) {
	fixtures := t.TempDir() // nothing staged
	env, _, code := runVerify(t, fixtures)
	if code != 5 {
		t.Errorf("exit %d, want 5 (the check could not run) -- 3 would claim a mismatch nobody observed", code)
	}
	if env.Error == nil || env.Error.Code != 5 {
		t.Errorf("envelope should carry error.code=5; got %+v", env.Error)
	}
}

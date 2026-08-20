package install

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// mkcert_setup_test.go -- znasllc-io/memql#3362.
//
// scripts/install/mkcert-setup.sh ensures the operator has a trusted local CA
// and issues the front-door wildcard pair the local ingress terminates TLS
// with (the *.memql.localhost cert seed-secrets.sh loads as memql-front-door-tls).
//
// The assertion that matters is the RESTRAINT one: when a rootCA.pem already
// exists, the script reports caPreExisting=true / caInstalled=false and the
// file's bytes are unchanged. mkcert's CA is per-machine, not per-project --
// the operator may already have one signing certificates for half a dozen
// other local stacks, and an installer that REGENERATES it is reaching into
// shared machine state it does not own. Installing MemQL must never be the
// reason someone's other local projects start failing TLS.
//
// That restraint used to be asserted as "never runs `mkcert -install`", which
// is a different and wrong claim (memql#3560): `-install` does not regenerate
// anything, it adds an existing CA to trust stores missing it. Not running it
// is what let a created-but-untrusted CA -- the state an interrupted first run
// leaves behind, because trust needs a password no child process can ask for --
// be mistaken on the next run for a healthy one, and reported as a successful
// install with an untrusted front door. So the tests below assert trust is
// ENSURED, and separately that the CA file is never rewritten.
//
// Every test here drives a STUB mkcert on a PATH prefix, with CAROOT pointed
// at t.TempDir(). Nothing in this file may reach the runner's real trust
// store, and the stub records its argv so the "did not call -install"
// assertion is about what was actually invoked, not about what the script
// claims in its envelope.

//=============================================================================
// HARNESS
//=============================================================================

type mkcertEnvelope struct {
	OK         bool           `json:"ok"`
	Capability string         `json:"capability"`
	Changed    bool           `json:"changed"`
	Result     map[string]any `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func mkcertRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func mkcertScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(mkcertRepoRoot(t), "scripts", "install", "mkcert-setup.sh")
}

func mkcertExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func mkcertLastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			return l
		}
	}
	return ""
}

// mkcertStub is a fake mkcert binary. It answers the three invocations the
// script makes and appends every argv to $STUB_LOG so a test can assert on
// what was called -- most importantly, on what was NOT.
//
//	mkcert -CAROOT                      -> prints $STUB_CAROOT
//	mkcert -install                     -> creates $STUB_CAROOT/rootCA.pem
//	mkcert -cert-file X -key-file Y ... -> writes X and Y
//
// STUB_FAIL_ISSUE makes certificate issuance fail, so the script's handling of
// a real mkcert error is exercised rather than assumed.
//
// STUB_FAIL_INSTALL reproduces the ACTUAL failure a headless `mkcert -install`
// produces on Linux, verbatim off a real run: mkcert creates the CA, then dies
// trying to copy it into the system store because sudo has no terminal to ask
// for a password on. The CA file is left behind, which is the whole point --
// the next run has to cope with a real CA that nothing trusts.
const mkcertStubBody = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG"

case "$1" in
  -CAROOT)
    printf '%s\n' "$STUB_CAROOT"
    exit 0
    ;;
  -install)
    # Creates a CA only when there is none, exactly as mkcert does: -install
    # against an existing CA adds it to trust stores and never rewrites it.
    mkdir -p "$STUB_CAROOT"
    if [ ! -f "$STUB_CAROOT/rootCA.pem" ]; then
      printf 'stub-root-ca\n' > "$STUB_CAROOT/rootCA.pem"
      printf 'stub-root-key\n' > "$STUB_CAROOT/rootCA-key.pem"
    fi
    if [ -n "$STUB_FAIL_INSTALL" ]; then
      printf 'ERROR: failed to execute "tee": exit status 1\n\n' >&2
      printf 'sudo: a terminal is required to read the password; either use the -S option to read from standard input or configure an askpass helper\n' >&2
      printf 'sudo: a password is required\n' >&2
      exit 1
    fi
    exit 0
    ;;
esac

cert=""; key=""
while [ $# -gt 0 ]; do
  case "$1" in
    -cert-file) cert="$2"; shift 2 ;;
    -key-file)  key="$2";  shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$STUB_FAIL_ISSUE" ]; then
  printf 'stub: refusing to issue\n' >&2
  exit 1
fi
[ -n "$cert" ] && printf 'stub-cert\n' > "$cert"
[ -n "$key" ]  && printf 'stub-key\n'  > "$key"
exit 0
`

// mkcertConfirmPhrase gates installing a CA into the machine's trust store.
const mkcertConfirmPhrase = "install-memql-ca"

// mkcertEnv is one hermetic mkcert world: a stub binary on a PATH prefix, a
// CAROOT under t.TempDir(), and a log of every stub invocation.
type mkcertEnv struct {
	binDir  string
	caroot  string
	log     string
	certDir string
	extra   []string // extra KEY=VAL for the child process env
	// confirm is passed on every run unless a test blanks it. Installing a
	// root CA into the machine's trust store is the one system-touching step
	// here, so it is gated on the phrase (contract rule 3).
	confirm string
}

func newMkcertEnv(t *testing.T) *mkcertEnv {
	t.Helper()
	base := t.TempDir()
	e := &mkcertEnv{
		binDir:  filepath.Join(base, "bin"),
		caroot:  filepath.Join(base, "caroot"),
		log:     filepath.Join(base, "stub.log"),
		certDir: filepath.Join(base, "certs"),
		confirm: mkcertConfirmPhrase,
	}
	for _, d := range []string{e.binDir, e.caroot, e.certDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	stub := filepath.Join(e.binDir, "mkcert")
	if err := os.WriteFile(stub, []byte(mkcertStubBody), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	// certutil is a real prerequisite on Linux (browsers read NSS, not the
	// system store) and it is present on some CI images and absent on others.
	// Stubbing it makes every OTHER test in this file independent of which
	// image it landed on; the one test that is about its absence names a path
	// that does not exist.
	certutil := filepath.Join(e.binDir, "certutil")
	if err := os.WriteFile(certutil, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write certutil stub: %v", err)
	}
	return e
}

// seedCA plants a pre-existing root CA -- the shared machine state the script
// must not disturb.
func (e *mkcertEnv) seedCA(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.caroot, "rootCA.pem"), []byte("operator-ca\n"), 0o644); err != nil {
		t.Fatalf("seed CA: %v", err)
	}
}

func (e *mkcertEnv) caPEM() string { return filepath.Join(e.caroot, "rootCA.pem") }
func (e *mkcertEnv) certFile() string {
	return filepath.Join(e.certDir, "dev.crt")
}
func (e *mkcertEnv) keyFile() string { return filepath.Join(e.certDir, "dev.key") }

// stubLog returns every recorded stub invocation.
func (e *mkcertEnv) stubLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.log)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read stub log: %v", err)
	}
	return string(b)
}

// run invokes the script with the stub first on PATH and stdin closed.
func (e *mkcertEnv) run(t *testing.T, args ...string) (mkcertEnvelope, int, string) {
	t.Helper()
	full := []string{
		"--caroot=" + e.caroot,
		"--cert-file=" + e.certFile(),
		"--key-file=" + e.keyFile(),
	}
	if e.confirm != "" {
		full = append(full, "--confirm="+e.confirm)
	}
	full = append(full, args...)

	cmd := exec.Command("bash", append([]string{mkcertScript(t)}, full...)...)
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(),
		"PATH="+e.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_CAROOT="+e.caroot,
		"STUB_LOG="+e.log,
	)
	// NO DISPLAY, EVER. mkcert-setup.sh now offers mkcert's sudo a graphical
	// askpass helper (scripts/lib/elevate.sh), and a suite that inherited a
	// developer's DISPLAY could put a real password dialog in front of whoever
	// ran `go test`. The stub mkcert never shells out to sudo, so this is belt
	// and braces -- which is the right amount for a prompt nobody asked for.
	cmd.Env = append(cmd.Env, "DISPLAY=", "WAYLAND_DISPLAY=")
	cmd.Env = append(cmd.Env, e.extra...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := mkcertExitCode(err)
	combined := "--- stdout ---\n" + stdout.String() + "--- stderr ---\n" + stderr.String()

	var env mkcertEnvelope
	line := mkcertLastJSONLine(stdout.String())
	if line == "" {
		return env, code, combined
	}
	if jerr := json.Unmarshal([]byte(line), &env); jerr != nil {
		t.Fatalf("stdout is not a valid JSON envelope: %v\nline: %s\n%s", jerr, line, combined)
	}
	return env, code, combined
}

func mkcertBool(t *testing.T, env mkcertEnvelope, key string) bool {
	t.Helper()
	v, ok := env.Result[key]
	if !ok {
		t.Fatalf("result has no %q field: %+v", key, env.Result)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("result.%s = %v (%T), want a bool", key, v, v)
	}
	return b
}

//=============================================================================
// THE ASSERTION THAT MATTERS: a pre-existing CA is never regenerated
//=============================================================================

func TestMkcertPreExistingCAIsNotRegenerated(t *testing.T) {
	e := newMkcertEnv(t)
	e.seedCA(t)
	// No --confirm: with a CA already on disk there is nothing to CREATE, and
	// completing trust for a CA the operator already has is not a decision that
	// needs authorising.
	e.confirm = ""
	before, err := os.ReadFile(e.caPEM())
	if err != nil {
		t.Fatalf("read seeded CA: %v", err)
	}

	env, code, out := e.run(t)
	if code != 0 || !env.OK {
		t.Fatalf("run failed (exit %d): %s", code, out)
	}
	if !mkcertBool(t, env, "caPreExisting") {
		t.Errorf("caPreExisting = false, want true -- a rootCA.pem was already on disk: %s", out)
	}
	if mkcertBool(t, env, "caInstalled") {
		t.Errorf("caInstalled = true, want false -- the script must not claim to have created "+
			"a CA that was already there: %s", out)
	}
	after, err := os.ReadFile(e.caPEM())
	if err != nil {
		t.Fatalf("read CA after run: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the operator's root CA was rewritten -- THIS is the restraint that matters\n got: %q\nwant: %q", after, before)
	}
	// It must still have done its actual job.
	if !mkcertBool(t, env, "certIssued") {
		t.Errorf("certIssued = false, want true -- the wildcard pair is the point: %s", out)
	}
}

// The counterpart to the test above, and the memql#3560 regression: a CA on
// disk is NOT evidence that anything trusts it. `mkcert -install` creates the
// CA first and trusts it second, so an interrupted first run leaves exactly
// this state -- and the run that follows must complete the trust rather than
// take the file's presence as proof the work is done.
func TestMkcertEnsuresTrustOnAPreExistingCA(t *testing.T) {
	e := newMkcertEnv(t)
	e.seedCA(t)
	e.confirm = ""

	env, code, out := e.run(t)
	if code != 0 || !env.OK {
		t.Fatalf("run failed (exit %d): %s", code, out)
	}
	if log := e.stubLog(t); !strings.Contains(log, "-install") {
		t.Errorf("`mkcert -install` was never invoked, so nothing established that the CA on "+
			"disk is actually trusted -- an untrusted CA reported as a successful install is "+
			"how memql#3560 shipped an unreachable front door.\nstub log:\n%s", log)
	}
	if !mkcertBool(t, env, "caTrusted") {
		t.Errorf("caTrusted = false after a clean -install: %s", out)
	}
}

//=============================================================================
// THE FRESH-MACHINE PATH
//=============================================================================

func TestMkcertInstallsCAWhenAbsent(t *testing.T) {
	e := newMkcertEnv(t)

	env, code, out := e.run(t)
	if code != 0 || !env.OK {
		t.Fatalf("run failed (exit %d): %s", code, out)
	}
	if mkcertBool(t, env, "caPreExisting") {
		t.Errorf("caPreExisting = true on an empty CAROOT: %s", out)
	}
	if !mkcertBool(t, env, "caInstalled") {
		t.Errorf("caInstalled = false, want true -- there was no CA to reuse: %s", out)
	}
	if !env.Changed {
		t.Errorf("changed = false after installing a CA and issuing a cert: %s", out)
	}
	if log := e.stubLog(t); !strings.Contains(log, "-install") {
		t.Errorf("`mkcert -install` was never invoked on a machine with no CA.\nstub log:\n%s", log)
	}
	if _, err := os.Stat(e.caPEM()); err != nil {
		t.Errorf("no rootCA.pem after install: %v", err)
	}
}

// Installing a root CA rewrites the machine's trust store. Without the
// confirmation phrase the script must refuse (exit 3) and, crucially, must not
// have invoked mkcert at all on the way to refusing.
func TestMkcertCAInstallWithoutConfirmationIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		confirm string
	}{
		{"no phrase", ""},
		{"wrong phrase", "yes"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := newMkcertEnv(t)
			e.confirm = tc.confirm

			env, code, out := e.run(t)
			if code != 3 {
				t.Errorf("exit %d, want 3 (refused): %s", code, out)
			}
			if env.OK || env.Error == nil || env.Error.Code != 3 {
				t.Errorf("envelope should carry ok=false error.code=3: %s", out)
			}
			if log := e.stubLog(t); strings.Contains(log, "-install") {
				t.Errorf("the trust store was touched without confirmation.\nstub log:\n%s", log)
			}
			if _, err := os.Stat(e.caPEM()); err == nil {
				t.Errorf("a CA was created despite the refusal")
			}
		})
	}
}

//=============================================================================
// THE CERTIFICATE
//=============================================================================

func TestMkcertIssuesTheFrontDoorWildcardPair(t *testing.T) {
	e := newMkcertEnv(t)

	env, code, out := e.run(t)
	if code != 0 || !env.OK {
		t.Fatalf("run failed (exit %d): %s", code, out)
	}
	for _, p := range []string{e.certFile(), e.keyFile()} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist: %v", p, err)
		}
	}
	log := e.stubLog(t)
	// The wildcard covers api./identity./anything else the overlay adds;
	// the apex is listed too because a wildcard does not match it.
	for _, want := range []string{"*.memql.localhost", "memql.localhost"} {
		if !strings.Contains(log, want) {
			t.Errorf("mkcert was not asked to cover %q.\nstub log:\n%s", want, log)
		}
	}
	if !strings.Contains(log, "-cert-file "+e.certFile()) {
		t.Errorf("mkcert was not pointed at the requested cert path.\nstub log:\n%s", log)
	}
	if !strings.Contains(log, "-key-file "+e.keyFile()) {
		t.Errorf("mkcert was not pointed at the requested key path.\nstub log:\n%s", log)
	}
	if env.Capability != "install.mkcert" {
		t.Errorf("capability %q, want install.mkcert", env.Capability)
	}
}

func TestMkcertCustomHostnames(t *testing.T) {
	e := newMkcertEnv(t)

	env, code, out := e.run(t, "--hostnames=*.example.test,example.test")
	if code != 0 || !env.OK {
		t.Fatalf("run failed (exit %d): %s", code, out)
	}
	log := e.stubLog(t)
	if !strings.Contains(log, "*.example.test") || !strings.Contains(log, "example.test") {
		t.Errorf("custom hostnames not passed to mkcert.\nstub log:\n%s", log)
	}
	if strings.Contains(log, "memql.localhost") {
		t.Errorf("default hostnames leaked into a custom run.\nstub log:\n%s", log)
	}
}

//=============================================================================
// IDEMPOTENCY
//=============================================================================

func TestMkcertSecondRunIsUnchanged(t *testing.T) {
	e := newMkcertEnv(t)

	if env, code, out := e.run(t); code != 0 || !env.Changed {
		t.Fatalf("first run: exit %d changed %v: %s", code, env.Changed, out)
	}
	if err := os.WriteFile(e.certFile(), []byte("issued-once\n"), 0o644); err != nil {
		t.Fatalf("mark cert: %v", err)
	}

	env, code, out := e.run(t)
	if code != 0 || !env.OK {
		t.Fatalf("second run failed (exit %d): %s", code, out)
	}
	if env.Changed {
		t.Errorf("second run reported changed=true; CA and cert were both already present: %s", out)
	}
	if mkcertBool(t, env, "certIssued") {
		t.Errorf("certIssued = true on a re-run with an existing pair: %s", out)
	}
	got, err := os.ReadFile(e.certFile())
	if err != nil {
		t.Fatalf("read cert after second run: %v", err)
	}
	if string(got) != "issued-once\n" {
		t.Errorf("an existing certificate was reissued without --force: %q", got)
	}
}

// cert_mismatches(), and the `reissued` field it drives, had NO coverage until
// memql#3730 -- the check was correct, cited the domain rename that creates
// stale pairs, and nothing exercised it. That mattered when it turned out its
// only production caller (scripts/k3d/seed-secrets.sh) short-circuited before
// reaching it, and it matters now that the same caller READS `reissued` to
// decide what to put in its own envelope.
//
// The fixture is a REAL certificate with the wrong names, because the failing
// condition is a VALID certificate for a domain nobody is serving. Arbitrary
// bytes take the deliberate cannot-tell branch (covered by
// TestMkcertSecondRunIsUnchanged) and would prove nothing here.
func TestMkcertReissuesAPairThatDoesNotCoverTheHostnames(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not available: cert_mismatches would take its cannot-tell branch, " +
			"so this test could only pass without establishing anything")
	}
	e := newMkcertEnv(t)
	e.seedCA(t)
	writeWrongDomainCert(t, e.certFile(), e.keyFile())

	env, code, out := e.run(t, "--hostnames=*.memql.localhost,memql.localhost")
	if code != 0 || !env.OK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !mkcertBool(t, env, "certIssued") {
		t.Fatalf("certIssued = false: a certificate for wrong.example.com cannot serve "+
			"memql.localhost, so it must be replaced: %s", out)
	}
	// The field seed-secrets reads to report `reissued` rather than `issued`.
	// Without it a caller cannot distinguish "there was nothing here" from "what
	// was here was wrong and is now gone", and the operator whose certificate was
	// just overwritten is entitled to that difference.
	if !mkcertBool(t, env, "reissued") {
		t.Errorf("reissued = false; a replaced wrong-domain pair must be reported as such: %s", out)
	}
	got, err := os.ReadFile(e.certFile())
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	if strings.Contains(string(got), "BEGIN CERTIFICATE") {
		t.Errorf("the wrong-domain certificate is still on disk")
	}
	if !strings.Contains(out, "does not cover") {
		t.Errorf("the operator was not told why the pair was replaced: %s", out)
	}
	// BOTH NAME SETS, and the OLD one is the useful half -- it names which stale
	// domain the operator was serving. It can only be reported from before the
	// write: mkcert overwrites the file in place, so afterwards those names are
	// gone. The runbook claims this message names both sets; this is what makes
	// the claim true.
	if !strings.Contains(out, "wrong.example.com") {
		t.Errorf("the warning does not name what the OLD certificate carried, which is what "+
			"identifies the stale pair: %s", out)
	}
	if !strings.Contains(out, "*.memql.localhost") {
		t.Errorf("the warning does not name what the certificate had to cover: %s", out)
	}
}

// coverageVerified is a MEASUREMENT of the pair this run leaves behind, never an
// inference from what the run did. Callers report "covers" off it
// (scripts/k3d/seed-secrets.sh), so a true here has to mean somebody actually
// read the SANs -- otherwise every caller inherits the memql#3730 defect of
// asserting coverage that was never established.
func TestMkcertReportsWhetherCoverageWasActuallyChecked(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not available: coverage cannot be established either way")
	}

	t.Run("a readable covering pair is verified", func(t *testing.T) {
		e := newMkcertEnv(t)
		e.seedCA(t)
		writeCertWithNames(t, e.certFile(), e.keyFile(), "*.memql.localhost", "memql.localhost")

		env, code, out := e.run(t, "--hostnames=*.memql.localhost,memql.localhost")
		if code != 0 || !env.OK {
			t.Fatalf("exit %d: %s", code, out)
		}
		if mkcertBool(t, env, "certIssued") {
			t.Fatalf("a covering pair must be reused, not reissued: %s", out)
		}
		if !mkcertBool(t, env, "coverageVerified") {
			t.Errorf("coverageVerified = false over a pair whose SANs were read and do cover: %s", out)
		}
		if !strings.Contains(out, "covers") {
			t.Errorf("the log should say coverage was established: %s", out)
		}
	})

	t.Run("an unreadable pair is kept but never claimed to cover", func(t *testing.T) {
		e := newMkcertEnv(t)
		e.seedCA(t)
		// Not a certificate: cert_coverage answers `unknown`, which KEEPS the
		// pair (idempotency) and must not be reported as coverage.
		if err := os.WriteFile(e.certFile(), []byte("not-a-certificate\n"), 0o644); err != nil {
			t.Fatalf("write cert: %v", err)
		}
		if err := os.WriteFile(e.keyFile(), []byte("not-a-key\n"), 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}

		env, code, out := e.run(t)
		if code != 0 || !env.OK {
			t.Fatalf("exit %d: %s", code, out)
		}
		if mkcertBool(t, env, "certIssued") {
			t.Fatalf("an unreadable pair must be KEPT -- reissuing when unsure breaks idempotency: %s", out)
		}
		if mkcertBool(t, env, "coverageVerified") {
			t.Errorf("coverageVerified = true over a file that does not parse as a certificate: %s", out)
		}
		if !strings.Contains(out, "unverified") {
			t.Errorf("the log must admit the names were not read: %s", out)
		}
	})
}

// writeCertWithNames plants a real, parseable certificate carrying exactly
// `names`. Self-signed: cert_coverage reads subjectAltName and verifies no
// chain, so signing it with the runner's own CA would add nothing and would
// touch machine state this suite must not.
func writeCertWithNames(t *testing.T, certPath, keyPath string, names ...string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     names,
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// writeWrongDomainCert -- a real certificate for a domain the front door is not
// served at.
func writeWrongDomainCert(t *testing.T, certPath, keyPath string) {
	t.Helper()
	writeCertWithNames(t, certPath, keyPath, "*.wrong.example.com", "wrong.example.com")
}

func TestMkcertForceReissues(t *testing.T) {
	e := newMkcertEnv(t)
	if _, code, out := e.run(t); code != 0 {
		t.Fatalf("first run failed (exit %d): %s", code, out)
	}
	if err := os.WriteFile(e.certFile(), []byte("issued-once\n"), 0o644); err != nil {
		t.Fatalf("mark cert: %v", err)
	}

	env, code, out := e.run(t, "--force")
	if code != 0 || !env.OK {
		t.Fatalf("forced run failed (exit %d): %s", code, out)
	}
	if !env.Changed || !mkcertBool(t, env, "certIssued") {
		t.Errorf("--force did not reissue: %s", out)
	}
	got, err := os.ReadFile(e.certFile())
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	if string(got) == "issued-once\n" {
		t.Errorf("--force left the stale certificate in place")
	}
}

//=============================================================================
// PREREQUISITES + FAILURES
//=============================================================================

// mkcert is not vendored and cannot be installed on the operator's behalf
// (it touches the system trust store). Its absence is a prerequisite failure
// with an actionable message, not a crash.
func TestMkcertMissingBinaryIsExitFour(t *testing.T) {
	e := newMkcertEnv(t)

	env, code, out := e.run(t, "--mkcert="+filepath.Join(t.TempDir(), "definitely-not-mkcert"))
	if code != 4 {
		t.Errorf("exit %d, want 4 (prerequisite missing): %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 4 {
		t.Errorf("envelope should carry ok=false error.code=4: %s", out)
	}
	// The MESSAGE has to carry the install instruction, not just the code. A
	// cap_fail raised inside a "$(...)" capture would still exit 4 while the
	// envelope degraded to the trap's generic "aborted without an explicit
	// result", so assert on the text the operator actually reads.
	if env.Error != nil && !strings.Contains(env.Error.Message, "mkcert") {
		t.Errorf("the failure message should name mkcert and how to install it; got: %q", env.Error.Message)
	}
	if log := e.stubLog(t); log != "" {
		t.Errorf("nothing should have been invoked when the prerequisite is missing:\n%s", log)
	}
}

//=============================================================================
// TRUST NEEDS A PASSWORD, AND A WIZARD HAS NO TERMINAL (memql#3560)
//=============================================================================

// mkcertString reads a string field off a result envelope.
func mkcertString(t *testing.T, env mkcertEnvelope, key string) string {
	t.Helper()
	v, ok := env.Result[key]
	if !ok {
		t.Fatalf("result has no %q field: %+v", key, env.Result)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("result.%s = %v (%T), want a string", key, v, v)
	}
	return s
}

// The failure the operator actually hit. `mkcert -install` needs root to write
// the system trust store; the wizard runs it as a child process with no
// terminal, so sudo cannot prompt and mkcert dies.
//
// Two things have to be true about how that is reported, and neither was:
//
//   - exit 4, not 5. Five means "may be transient, retry" -- advice that can
//     never come true here, because nothing about an unattended retry gives
//     sudo a terminal.
//   - a remedy. Exit 4 with no command is a dead end; the wizard turns this
//     field into the "open a terminal with this command" button.
func TestMkcertTrustFailureWithoutATerminalIsExitFourWithARemedy(t *testing.T) {
	e := newMkcertEnv(t)
	e.extra = append(e.extra, "STUB_FAIL_INSTALL=1")

	env, code, out := e.run(t)
	if code != 4 {
		t.Errorf("exit %d, want 4 (prerequisite missing) -- exit 5 tells the operator to retry "+
			"something that cannot succeed unattended: %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 4 {
		t.Fatalf("envelope should carry ok=false error.code=4: %s", out)
	}
	remedy := mkcertString(t, env, "remedy")
	if remedy == "" {
		t.Fatalf("no remedy: the wizard has nothing to offer and the operator has nowhere to go: %s", out)
	}
	// NOT `sudo <script>`: as root, mkcert resolves CAROOT under root's home
	// and builds a second CA there -- trusted for a user who is not the one
	// running a browser.
	if strings.HasPrefix(remedy, "sudo ") {
		t.Errorf("remedy runs the whole script as root: %q -- mkcert would then create its CA "+
			"in root's CAROOT, not the operator's", remedy)
	}
	if !strings.Contains(remedy, "mkcert-setup.sh") {
		t.Errorf("remedy does not run this capability: %q", remedy)
	}
	// The paths this run was given have to travel, or the terminal run writes
	// its pair somewhere the retry will not look.
	for _, want := range []string{e.caroot, e.certFile(), e.keyFile(), mkcertConfirmPhrase} {
		if !strings.Contains(remedy, want) {
			t.Errorf("remedy does not carry %q: %q", want, remedy)
		}
	}
}

// The half-finished state must not be laundered into a success. After a failed
// trust step there IS a rootCA.pem on disk -- and the run that follows has to
// keep failing until trust is real, rather than seeing the file and declaring
// the step done.
func TestMkcertUntrustedCAIsNeverReportedAsInstalled(t *testing.T) {
	e := newMkcertEnv(t)
	e.extra = append(e.extra, "STUB_FAIL_INSTALL=1")

	if _, code, out := e.run(t); code != 4 {
		t.Fatalf("first run: exit %d, want 4: %s", code, out)
	}
	if _, err := os.Stat(e.caPEM()); err != nil {
		t.Fatalf("the stub should have left a CA behind (that is the state under test): %v", err)
	}

	// Second run: a real CA on disk, still trusted by nothing.
	env, code, out := e.run(t)
	if code == 0 || env.OK {
		t.Fatalf("the second run reported SUCCESS over an untrusted CA -- the install then "+
			"completes and every browser refuses the front door: %s", out)
	}
	if code != 4 {
		t.Errorf("exit %d, want 4: %s", code, out)
	}
	if v, ok := env.Result["caTrusted"]; ok && v == true {
		t.Errorf("caTrusted = true while `mkcert -install` is failing: %s", out)
	}
}

//=============================================================================
// BROWSER TRUST IS PART OF THE JOB
//=============================================================================

// On Linux, Firefox and Chrome read their own NSS store rather than the system
// one, and mkcert can only write it through certutil. Without certutil mkcert
// exits 0 with a warning -- a "success" that leaves the front door untrusted in
// the one application the install's final step tells the operator to open.
func TestMkcertMissingCertutilOnLinuxIsExitFour(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("NSS is the browser trust store on Linux; macOS trusts through the keychain")
	}
	e := newMkcertEnv(t)

	env, code, out := e.run(t, "--certutil="+filepath.Join(t.TempDir(), "definitely-not-certutil"))
	if code != 4 {
		t.Errorf("exit %d, want 4 (prerequisite missing): %s", code, out)
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "certutil") {
		t.Errorf("the message should name certutil and what it is for: %+v", env.Error)
	}
	// Refused BEFORE anything was created: a machine that cannot finish must
	// not be left holding a half-made CA, which is the exact shape of the bug
	// the trust tests above are about.
	if _, err := os.Stat(e.caPEM()); err == nil {
		t.Errorf("a CA was created on the way to refusing")
	}
	if log := e.stubLog(t); strings.Contains(log, "-install") {
		t.Errorf("the trust store was touched before the prerequisite check.\nstub log:\n%s", log)
	}
}

// A machine with no browser is a real case -- a headless install has nothing to
// trust the CA in. The refusal is a default, not a law.
func TestMkcertMissingCertutilCanBeWaived(t *testing.T) {
	e := newMkcertEnv(t)

	env, code, out := e.run(t,
		"--certutil="+filepath.Join(t.TempDir(), "definitely-not-certutil"),
		"--allow-missing-certutil",
	)
	if code != 0 || !env.OK {
		t.Fatalf("run failed (exit %d): %s", code, out)
	}
	if !mkcertBool(t, env, "caTrusted") {
		t.Errorf("caTrusted = false: %s", out)
	}
}

// A remedy has to reproduce the run it is finishing. A waived run that then
// hits the password wall would otherwise be handed a command that refuses on
// certutil instead -- a second, different dead end.
func TestMkcertRemedyCarriesTheWaiver(t *testing.T) {
	e := newMkcertEnv(t)
	e.extra = append(e.extra, "STUB_FAIL_INSTALL=1")

	env, code, out := e.run(t,
		"--certutil="+filepath.Join(t.TempDir(), "definitely-not-certutil"),
		"--allow-missing-certutil",
	)
	if code != 4 {
		t.Fatalf("exit %d, want 4: %s", code, out)
	}
	if remedy := mkcertString(t, env, "remedy"); !strings.Contains(remedy, "--allow-missing-certutil") {
		t.Errorf("remedy drops the waiver, so running it refuses for a different reason: %q", remedy)
	}
}

func TestMkcertIssuanceFailureIsExitFive(t *testing.T) {
	e := newMkcertEnv(t)
	e.extra = append(e.extra, "STUB_FAIL_ISSUE=1")

	env, code, out := e.run(t)
	if code != 5 {
		t.Errorf("exit %d, want 5 (operation failed): %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 5 {
		t.Errorf("envelope should carry ok=false error.code=5: %s", out)
	}
}

func TestMkcertBadParamsExitTwo(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"empty hostnames", []string{"--hostnames= "}},
		{"hostname with a space", []string{"--hostnames=a b/c"}},
		{"hostname with a metacharacter", []string{"--hostnames=a;rm -rf /"}},
		{"wildcard in the middle", []string{"--hostnames=foo.*.memql.localhost"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			e := newMkcertEnv(t)
			env, code, out := e.run(t, tc.args...)
			if code != 2 {
				t.Errorf("exit %d, want 2 (bad param): %s", code, out)
			}
			if env.OK || env.Error == nil || env.Error.Code != 2 {
				t.Errorf("envelope should carry ok=false error.code=2: %s", out)
			}
		})
	}
}

//=============================================================================
// SAFETY RAIL ON THE TESTS THEMSELVES
//=============================================================================

// Every run in this file goes through mkcertEnv.run, which pins CAROOT to a
// t.TempDir() and puts the stub first on PATH. A test that built its own
// exec.Command could reach the runner's real trust store, so assert the
// harness is the only entry point.
func TestMkcertTestsAlwaysUseTheStubbedEnvironment(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(mkcertRepoRoot(t), "scripts", "install", "mkcert_setup_test.go"))
	if err != nil {
		t.Fatalf("read test file: %v", err)
	}
	// The call appears exactly once: inside mkcertEnv.run. The needle is
	// assembled at runtime so this guard does not count itself.
	needle := "exec." + "Command("
	if n := strings.Count(string(b), needle); n != 1 {
		t.Errorf("exec.Command appears %d times, want 1 (only mkcertEnv.run) -- every run must "+
			"go through the stubbed PATH and the temp CAROOT, never the real trust store", n)
	}
}

//=============================================================================
// PROVENANCE: who created this CA (memql#3576)
//=============================================================================

// caMarker is the file mkcert-setup.sh writes beside key material it created.
const caMarker = ".memql-created"

// The verdict used to be "is rootCA.pem here", recorded once into a receipt an
// uninstall then DELETES -- so a CA the uninstall refused to remove came back
// on the next install as "already here", truthfully, forever. A marker beside
// the key material has none of that: written by the run that creates it,
// survives every receipt, and disappears with the CA it describes.
func TestMkcertStampsProvenanceOnACAItCreates(t *testing.T) {
	e := newMkcertEnv(t)

	env, code, out := e.run(t)
	if code != 0 || !env.OK {
		t.Fatalf("run failed (exit %d): %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(e.caroot, caMarker)); err != nil {
		t.Fatalf("no provenance marker after creating a CA: %v", err)
	}
	if mkcertBool(t, env, "caPreExisting") {
		t.Errorf("caPreExisting = true for a CA this run created: %s", out)
	}
}

// A CA MemQL created stays MemQL's across runs -- which is the whole point.
// Without the marker this second run would see a file on disk and report it as
// the operator's, and nothing could ever remove it again.
func TestMkcertRecognisesItsOwnCAOnASecondRun(t *testing.T) {
	e := newMkcertEnv(t)
	if _, code, out := e.run(t); code != 0 {
		t.Fatalf("first run: exit %d: %s", code, out)
	}

	env, code, out := e.run(t, "--force")
	if code != 0 || !env.OK {
		t.Fatalf("second run failed (exit %d): %s", code, out)
	}
	if mkcertBool(t, env, "caPreExisting") {
		t.Errorf("a CA MemQL created was reported as pre-existing on the next run -- "+
			"that is the ratchet: it can never be removed again: %s", out)
	}
}

// The conservative half, unchanged: a CA that was already here carries no
// marker and is still reported as the operator's.
func TestMkcertLeavesAnUnmarkedCAAsTheOperatorsOwn(t *testing.T) {
	e := newMkcertEnv(t)
	e.seedCA(t) // planted by hand: no marker
	e.confirm = ""

	env, code, out := e.run(t)
	if code != 0 || !env.OK {
		t.Fatalf("run failed (exit %d): %s", code, out)
	}
	if !mkcertBool(t, env, "caPreExisting") {
		t.Errorf("an unmarked CA must stay the operator's: %s", out)
	}
}

// A run that dies creating a CA must leave no claim on one it did not finish.
func TestMkcertDoesNotStampWhenCreationFails(t *testing.T) {
	e := newMkcertEnv(t)
	e.extra = append(e.extra, "STUB_FAIL_INSTALL=1")

	if _, code, _ := e.run(t); code != 4 {
		t.Fatalf("exit %d, want 4", code)
	}
	if _, err := os.Stat(filepath.Join(e.caroot, caMarker)); err == nil {
		t.Errorf("a failed run claimed a CA it never finished creating")
	}
}

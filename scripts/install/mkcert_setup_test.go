package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mkcert_setup_test.go -- znasllc-io/memql#3362.
//
// scripts/install/mkcert-setup.sh ensures the operator has a trusted local CA
// and issues the front-door wildcard pair the local ingress terminates TLS
// with (the *.local.znas.io cert seed-secrets.sh loads as local-znas-tls).
//
// The assertion that matters is the RESTRAINT one: when a rootCA.pem already
// exists, the script reports caPreExisting=true / caInstalled=false and does
// NOT run `mkcert -install`. mkcert's CA is per-machine, not per-project --
// the operator may already have one signing certificates for half a dozen
// other local stacks, and an installer that re-runs `-install` on it (or
// worse, regenerates it) is reaching into shared machine state it does not
// own. Installing memQL must never be the reason someone's other local
// projects start failing TLS.
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
const mkcertStubBody = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG"

case "$1" in
  -CAROOT)
    printf '%s\n' "$STUB_CAROOT"
    exit 0
    ;;
  -install)
    mkdir -p "$STUB_CAROOT"
    printf 'stub-root-ca\n' > "$STUB_CAROOT/rootCA.pem"
    printf 'stub-root-key\n' > "$STUB_CAROOT/rootCA-key.pem"
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
// THE ASSERTION THAT MATTERS: a pre-existing CA is left alone
//=============================================================================

func TestMkcertPreExistingCAIsNotReinstalled(t *testing.T) {
	e := newMkcertEnv(t)
	e.seedCA(t)
	// No --confirm: with a CA already on disk there is no trust-store change
	// to authorise, so the run must succeed without one.
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
		t.Errorf("caInstalled = true, want false -- the script must not adopt an existing CA: %s", out)
	}
	if log := e.stubLog(t); strings.Contains(log, "-install") {
		t.Errorf("`mkcert -install` was invoked against a pre-existing CA -- that CA may be "+
			"signing other projects' certs and is shared machine state we do not own.\nstub log:\n%s", log)
	}
	after, err := os.ReadFile(e.caPEM())
	if err != nil {
		t.Fatalf("read CA after run: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the operator's root CA was rewritten\n got: %q\nwant: %q", after, before)
	}
	// It must still have done its actual job.
	if !mkcertBool(t, env, "certIssued") {
		t.Errorf("certIssued = false, want true -- the wildcard pair is the point: %s", out)
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
	// The wildcard covers cockpit./identity./anything else the overlay adds;
	// the apex is listed too because a wildcard does not match it.
	for _, want := range []string{"*.local.znas.io", "local.znas.io"} {
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
	if strings.Contains(log, "local.znas.io") {
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
		{"wildcard in the middle", []string{"--hostnames=foo.*.local.znas.io"}},
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

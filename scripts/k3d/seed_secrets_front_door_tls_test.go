package k3d

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seed_secrets_front_door_tls_test.go -- memql#3384.
//
// Both local front-door ingresses (api-front-door / identity-front-door)
// name the `memql-front-door-tls` secret in their spec.tls. seed-secrets.sh used to
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

// requireOpenSSL skips a test that cannot mean anything without openssl.
//
// install.mkcert's cert_mismatches() answers "cannot tell" when openssl is
// absent, and cannot-tell KEEPS the pair -- deliberately, so a capability that
// is unsure stays idempotent. A SAN test on such a machine would therefore pass
// while measuring nothing, which is the exact class of false green this issue is
// about. A skip is honest; a pass that established nothing is not.
func requireOpenSSL(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not available: cert_mismatches would take its cannot-tell branch, " +
			"so this test could only pass without establishing anything")
	}
}

// certPEMForNames builds a REAL, parseable certificate carrying exactly the
// given DNS SANs, plus its key.
//
// Self-signed, and that is sufficient for every check in this repo that reads
// SANs (install.mkcert's cert_mismatches, bringup.sh's front-door assertion):
// they read subjectAltName and do not verify a chain, so this reproduces the
// failing condition exactly -- a VALID certificate whose names do not cover the
// domain about to be served. Generated in-process rather than by shelling out to
// mkcert, because mkcert would sign it with the RUNNER'S OWN CA: no test here
// may touch the operator's trust store or CAROOT.
func certPEMForNames(t *testing.T, names ...string) (certPEM, keyPEM []byte) {
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// writeCertPairForNames plants certPEMForNames' output on disk.
func writeCertPairForNames(t *testing.T, certPath, keyPath string, names ...string) {
	t.Helper()
	certPEM, keyPEM := certPEMForNames(t, names...)
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		t.Fatalf("mkdir certs: %v", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// writeFrontDoorPair plants an already-issued front-door pair at the default
// location under `home`, so the script's reuse branch is taken.
//
// The bytes are NOT a certificate, on purpose: this is the fixture for
// cert_mismatches' "cannot tell" branch. The covering-certificate case has its
// own fixture (writeCertPairForNames), because a test that needs real SANs
// cannot be written against this one.
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
	// What the fake cluster's memql-domain ConfigMap holds -- i.e. the domain
	// this cluster is already serving. Empty is a cluster that has none yet.
	clusterDomain string
	// The human-facing log of the last run. seed-secrets forwards a failing
	// child's envelope here, which is the only place its message appears.
	lastStderr string
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
	// mkcert-setup.sh requires certutil on Linux -- browsers read the NSS store,
	// not the system one, and a front door no browser trusts is not a front
	// door. Stubbed so these cases test seed-secrets rather than which packages
	// the runner image happens to carry.
	if err := os.WriteFile(filepath.Join(home, "certutil"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write certutil stub: %v", err)
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
//
// --mkcert ALWAYS points at the stub, ahead of anything the caller passes (a
// later --mkcert= wins, so a test that is about a missing binary still gets to
// name one). This became load-bearing with memql#3730: every run now delegates
// to install.mkcert, so a run that resolved `mkcert` from PATH would reach the
// RUNNER'S REAL BINARY and its real CAROOT -- and `mkcert -install` writes to
// the machine's trust store. No test in this suite may touch either.
func (e *frontDoorEnv) run(t *testing.T, args ...string) (seedResult, []string, int) {
	t.Helper()
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "k3d", "seed-secrets.sh")

	full := append([]string{"--mkcert=" + e.stubMkcert}, args...)
	cmd := exec.Command("bash", append([]string{script}, full...)...)
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
		"FAKE_CLUSTER_DOMAIN=" + e.clusterDomain,
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
	e.lastStderr = stderr.String()

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
		if strings.Contains(c, "create secret tls memql-front-door-tls") {
			return c
		}
	}
	return ""
}

//=============================================================================
// THE REGRESSION: the secret is created, not skipped
//=============================================================================

// The defect itself: with no pair on disk the script warned and returned, so
// memql-front-door-tls was never created and traefik fell back to its default cert.
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
		t.Fatalf("memql-front-door-tls was never seeded -- the ingresses reference it, so this is "+
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
// already trusts it, so an existing pair must be loaded verbatim.
//
// WHAT THIS ASSERTS, AND WHAT IT USED TO (memql#3730). It used to assert that
// mkcert "was not invoked at all", which is a different claim from "the
// certificate was not rotated" -- and the difference is the bug. Deciding
// whether the pair on disk covers the domain about to be served REQUIRES
// reading it, and that decision belongs to install.mkcert (which invokes mkcert
// for -CAROOT before it can decide anything). By measuring the call count
// instead of the outcome, this test certified the short-circuit that made the
// SAN check unreachable: any correct fix turned it red. So it now asserts the
// property it always wanted -- the certificate FILE is unchanged, and the
// envelope says `existing` -- and additionally that mkcert was never asked to
// ISSUE, which is the specific invocation that would rotate the pair.
func TestSeedSecretsReusesAnExistingFrontDoorPair(t *testing.T) {
	e := newFrontDoorEnv(t)
	e.seedCA(t)
	writeFrontDoorPair(t, e.home)

	res, calls, code := e.run(t, "--mkcert="+e.stubMkcert)

	if code != 0 || !res.OK {
		t.Fatalf("exit %d ok=%v; envelope: %+v", code, res.OK, res)
	}
	if strings.Contains(e.mkcertLog(t), "-cert-file") {
		t.Errorf("mkcert was asked to issue a certificate despite an existing pair -- a "+
			"re-run must not rotate the front-door certificate.\nstub log:\n%s", e.mkcertLog(t))
	}
	if got, err := os.ReadFile(frontDoorCert(e.home)); err != nil || string(got) != "existing-cert\n" {
		t.Errorf("existing certificate was overwritten: %q (%v)", string(got), err)
	}
	if tlsSecretCall(calls) == "" {
		t.Errorf("memql-front-door-tls not seeded from the existing pair; kubectl calls:\n%s",
			strings.Join(calls, "\n"))
	}
	if res.Result.FrontDoorTLSSource != "existing" {
		t.Errorf("frontDoorTlsSource = %q, want %q", res.Result.FrontDoorTLSSource, "existing")
	}
}

//=============================================================================
// THE memql#3730 REGRESSION: a pair that does not cover the domain is REPLACED
//=============================================================================

// The defect: this script short-circuited on `[ -s "$TLS_CERT" ]` -- the file
// exists -- and returned before install.mkcert was invoked, so the SAN check
// that already existed one layer down (cert_mismatches, which even cites the
// domain rename that creates stale pairs) could never run. Any machine that ran
// the local stack before memql#3593 therefore kept seeding a VALID mkcert
// certificate for a domain that no longer exists, traefik fell back to its own
// TRAEFIK DEFAULT CERT, and this envelope reported ok:true /
// frontDoorTlsSource:existing over it. `make secrets` could not fix it.
//
// The fixture has to be a REAL certificate with the wrong names. A file of
// arbitrary bytes takes cert_mismatches' deliberate "cannot tell" branch (which
// keeps the pair, preserving idempotency), so it could never exercise this path
// no matter what the caller did -- a test written against that fixture would
// pass for the wrong reason.
func TestSeedSecretsReissuesAPairThatDoesNotCoverTheDomain(t *testing.T) {
	requireOpenSSL(t)
	e := newFrontDoorEnv(t)
	e.seedCA(t)
	writeCertPairForNames(t, frontDoorCert(e.home), frontDoorKey(e.home),
		"*.wrong.example.com", "wrong.example.com")

	res, calls, code := e.run(t, "--mkcert="+e.stubMkcert)

	if code != 0 || !res.OK {
		t.Fatalf("exit %d ok=%v; envelope: %+v", code, res.OK, res)
	}
	if !strings.Contains(e.mkcertLog(t), "-cert-file") {
		t.Fatalf("mkcert was never asked to reissue: a certificate for wrong.example.com "+
			"cannot serve memql.localhost.\nstub log:\n%s", e.mkcertLog(t))
	}
	got, err := os.ReadFile(frontDoorCert(e.home))
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	if strings.Contains(string(got), "BEGIN CERTIFICATE") {
		t.Errorf("the wrong-domain certificate is still on disk -- it was not replaced")
	}
	// `reissued`, NOT `existing` and NOT `issued`. The whole bug was an envelope
	// that said `existing` while the front door served garbage, so a caller must
	// be able to see from the JSON alone that something was REPLACED.
	if res.Result.FrontDoorTLSSource != "reissued" {
		t.Errorf("frontDoorTlsSource = %q, want %q -- an operator reading this envelope has "+
			"to be able to tell that their certificate was replaced",
			res.Result.FrontDoorTLSSource, "reissued")
	}
	if res.Result.FrontDoorTLSHostnames != "*.memql.localhost,memql.localhost" {
		t.Errorf("frontDoorTlsHostnames = %q; the envelope must name the names the cert had to cover",
			res.Result.FrontDoorTLSHostnames)
	}
	if tlsSecretCall(calls) == "" {
		t.Errorf("memql-front-door-tls was not re-seeded from the reissued pair; kubectl calls:\n%s",
			strings.Join(calls, "\n"))
	}
	// THE ACCEPTANCE CRITERION of memql#3730: `make secrets` ALONE heals a stale
	// machine. Nothing here deleted the Secret or moved a file by hand, which is
	// what the operator who found this had to do.
	if !strings.Contains(e.lastStderr, "did NOT cover") {
		t.Errorf("the reissue was not reported to the operator:\n%s", e.lastStderr)
	}
}

// The other half of idempotency, on the merits rather than by accident: a pair
// that genuinely COVERS the domain is kept, twice running. The reuse test above
// plants unparseable bytes, so it proves the "cannot tell" branch keeps a pair;
// this proves the "can tell, and it covers" branch does too -- which is the case
// every real machine is in, and the one a check that reissued whenever it was
// unsure would break.
func TestSeedSecretsKeepsACoveringPairAcrossRuns(t *testing.T) {
	requireOpenSSL(t)
	e := newFrontDoorEnv(t)
	e.seedCA(t)
	writeCertPairForNames(t, frontDoorCert(e.home), frontDoorKey(e.home),
		"*.memql.localhost", "memql.localhost")
	before, err := os.ReadFile(frontDoorCert(e.home))
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}

	for run := 1; run <= 2; run++ {
		res, _, code := e.run(t, "--mkcert="+e.stubMkcert)
		if code != 0 || !res.OK {
			t.Fatalf("run %d: exit %d ok=%v; envelope: %+v", run, code, res.OK, res)
		}
		if res.Result.FrontDoorTLSSource != "existing" {
			t.Errorf("run %d: frontDoorTlsSource = %q, want %q -- a covering pair must be reused",
				run, res.Result.FrontDoorTLSSource, "existing")
		}
		after, rerr := os.ReadFile(frontDoorCert(e.home))
		if rerr != nil {
			t.Fatalf("run %d: read cert: %v", run, rerr)
		}
		if string(after) != string(before) {
			t.Fatalf("run %d: the certificate was rotated by a routine re-run", run)
		}
	}
	if strings.Contains(e.mkcertLog(t), "-cert-file") {
		t.Errorf("mkcert was asked to issue over a pair that already covers the domain:\n%s",
			e.mkcertLog(t))
	}
}

//=============================================================================
// THE SEAM: what this script does with the issuer's verdict
//=============================================================================
//
// The three assertions below are about seed-secrets' OWN logic -- the mapping
// from install.mkcert's envelope onto frontDoorTlsSource, and the fail-closed
// guard that is the single thing standing between a future refactor and a silent
// default back to reporting `existing`. None of it was reachable from a test
// while the issuer path was hardcoded, so `--mkcert-setup` exists to inject a
// fabricated envelope. That does NOT introduce a second decision point: whatever
// sits at that path still owns the reuse-vs-reissue decision completely, which is
// the property this whole fix rests on.

// writeFakeIssuer plants a stand-in for install.mkcert that emits `resultJSON`
// as its result object and creates the pair it was asked for (so the caller's
// non-empty-file check passes and only the envelope is under test).
func writeFakeIssuer(t *testing.T, dir, resultJSON string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-issuer.sh")
	body := `#!/usr/bin/env bash
cert=""; key=""
for a in "$@"; do
  case "$a" in
    --cert-file=*) cert="${a#*=}" ;;
    --key-file=*)  key="${a#*=}" ;;
  esac
done
[ -n "$cert" ] && { mkdir -p "$(dirname "$cert")"; printf 'fake-cert\n' > "$cert"; }
[ -n "$key" ]  && { mkdir -p "$(dirname "$key")";  printf 'fake-key\n'  > "$key"; }
printf '{"ok":true,"capability":"install.mkcert","changed":false,"result":` +
		resultJSON + `,"error":null}\n'
exit 0
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake issuer: %v", err)
	}
	return path
}

// THE INVARIANT THAT PREVENTS RECURRENCE. An issuer envelope this script cannot
// read must be a FAILURE, not a default. The original defect was reporting
// `existing` over a certificate nobody had checked; defaulting to `existing`
// because the child said nothing is the same mistake with a different cause, and
// it is the one a future refactor of install.mkcert's envelope would reintroduce.
func TestSeedSecretsRefusesAnIssuerEnvelopeItCannotRead(t *testing.T) {
	e := newFrontDoorEnv(t)
	e.seedCA(t)
	issuer := writeFakeIssuer(t, e.home, `{"certFile":"x"}`) // no certIssued

	res, calls, code := e.run(t, "--mkcert-setup="+issuer)

	if code != 5 {
		t.Fatalf("exit %d, want 5 (op failed); envelope: %+v", code, res)
	}
	if res.OK {
		t.Errorf("ok=true for a run that could not tell what the issuer did")
	}
	if res.Error == nil || !strings.Contains(res.Error.Message, "certIssued") {
		t.Errorf("the refusal must name the field it could not read: %+v", res.Error)
	}
	if res.Result.FrontDoorTLSSource == "existing" {
		t.Errorf("frontDoorTlsSource = %q -- guessing 'existing' is the memql#3730 defect itself",
			res.Result.FrontDoorTLSSource)
	}
	if call := tlsSecretCall(calls); call != "" {
		t.Errorf("a TLS secret was seeded from a pair of unknown provenance: %s", call)
	}
}

// The field coupling, pinned in both directions: certIssued decides
// existing-vs-not, reissued distinguishes replaced-from-minted, and
// coverageVerified decides whether this script may CLAIM coverage.
func TestSeedSecretsMapsTheIssuersVerdictOntoTheSource(t *testing.T) {
	cases := []struct {
		name           string
		result         string
		wantSource     string
		wantCoverage   bool
		wantInStderr   string
		notWantInStder string
	}{{
		name:         "reused and checked",
		result:       `{"certIssued":false,"reissued":false,"coverageVerified":true}`,
		wantSource:   "existing",
		wantCoverage: true,
		wantInStderr: "covers *.memql.localhost,memql.localhost",
	}, {
		// The line this round of review caught: a reuse whose names were never
		// read must not be logged as "covers". Unverified is a third state, and
		// asserting coverage here is the defect being fixed, in a line the fix
		// added.
		name:           "reused but unverifiable",
		result:         `{"certIssued":false,"reissued":false,"coverageVerified":false}`,
		wantSource:     "existing",
		wantCoverage:   false,
		wantInStderr:   "UNVERIFIED",
		notWantInStder: "covers *.memql.localhost",
	}, {
		name:         "freshly minted",
		result:       `{"certIssued":true,"reissued":false,"coverageVerified":true}`,
		wantSource:   "issued",
		wantCoverage: true,
	}, {
		name:         "replaced because it did not cover",
		result:       `{"certIssued":true,"reissued":true,"coverageVerified":true}`,
		wantSource:   "reissued",
		wantCoverage: true,
		wantInStderr: "did NOT cover",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newFrontDoorEnv(t)
			e.seedCA(t)
			issuer := writeFakeIssuer(t, e.home, tc.result)

			res, _, code := e.run(t, "--mkcert-setup="+issuer)
			if code != 0 || !res.OK {
				t.Fatalf("exit %d ok=%v; envelope: %+v", code, res.OK, res)
			}
			if res.Result.FrontDoorTLSSource != tc.wantSource {
				t.Errorf("frontDoorTlsSource = %q, want %q", res.Result.FrontDoorTLSSource, tc.wantSource)
			}
			if res.Result.FrontDoorTLSCoverageVerified != tc.wantCoverage {
				t.Errorf("frontDoorTlsCoverageVerified = %v, want %v -- the envelope must not "+
					"imply a check that did not happen",
					res.Result.FrontDoorTLSCoverageVerified, tc.wantCoverage)
			}
			if tc.wantInStderr != "" && !strings.Contains(e.lastStderr, tc.wantInStderr) {
				t.Errorf("log does not contain %q:\n%s", tc.wantInStderr, e.lastStderr)
			}
			if tc.notWantInStder != "" && strings.Contains(e.lastStderr, tc.notWantInStder) {
				t.Errorf("log claims %q, which this run never established:\n%s",
					tc.notWantInStder, e.lastStderr)
			}
		})
	}
}

//=============================================================================
// THE DOMAIN THE PAIR IS CHECKED AGAINST
//=============================================================================

// Deriving the SANs from the resolved domain (so the certificate covers what the
// memql-domain ConfigMap says) opened a destructive edge: with no --domain, the
// default was the ENVIRONMENT's domain, so `make secrets` on a cluster serving
// lab.example.com would resolve *.memql.localhost and reissue OVER the operator's
// custom pair in place. The cluster's own answer is the default now.
func TestSeedSecretsDefaultsToTheDomainTheClusterAlreadyServes(t *testing.T) {
	e := newFrontDoorEnv(t)
	e.seedCA(t)
	e.clusterDomain = "lab.example.com"
	issuer := writeFakeIssuer(t, e.home, `{"certIssued":false,"reissued":false,"coverageVerified":true}`)

	res, calls, code := e.run(t, "--mkcert-setup="+issuer)
	if code != 0 || !res.OK {
		t.Fatalf("exit %d ok=%v; envelope: %+v", code, res.OK, res)
	}
	if res.Result.FrontDoorTLSHostnames != "*.lab.example.com,lab.example.com" {
		t.Errorf("frontDoorTlsHostnames = %q, want the cluster's own domain -- checking the pair "+
			"against a domain nobody is serving is what would destroy it",
			res.Result.FrontDoorTLSHostnames)
	}
	// And the ConfigMap is re-seeded with the SAME domain, not reset to the
	// environment default: the two must never disagree, which is the invariant
	// this whole derivation exists to make true.
	var domainCall string
	for _, c := range calls {
		if strings.Contains(c, "create configmap memql-domain") {
			domainCall = c
		}
	}
	if !strings.Contains(domainCall, "MEMQL_DOMAIN=lab.example.com") {
		t.Errorf("memql-domain was re-seeded with a different domain than the cluster serves: %q", domainCall)
	}
}

// An explicit --domain still wins: it is how an operator brings a new domain,
// and up.sh's refuse_domain_change is the one gate that rejects a disagreement.
func TestSeedSecretsPrefersAnExplicitDomainOverTheClusters(t *testing.T) {
	e := newFrontDoorEnv(t)
	e.seedCA(t)
	e.clusterDomain = "lab.example.com"
	issuer := writeFakeIssuer(t, e.home, `{"certIssued":true,"reissued":true,"coverageVerified":true}`)

	res, _, code := e.run(t, "--mkcert-setup="+issuer, "--domain=other.example.com")
	if code != 0 || !res.OK {
		t.Fatalf("exit %d ok=%v; envelope: %+v", code, res.OK, res)
	}
	if res.Result.FrontDoorTLSHostnames != "*.other.example.com,other.example.com" {
		t.Errorf("frontDoorTlsHostnames = %q, want the explicitly requested domain",
			res.Result.FrontDoorTLSHostnames)
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

// When the issuer refuses, seed-secrets has to say WHY -- and only the child
// knows. cap_fail writes its message to the envelope on stdout and nowhere
// else, and this caller used to discard stdout, then guess from the exit code:
// "mkcert is not installed", printed whatever the actual reason was. install.
// mkcert's exit 4 now covers three prerequisites, so the guess is wrong more
// often than it is right (memql#3560).
func TestSeedSecretsForwardsTheIssuersOwnReason(t *testing.T) {
	e := newFrontDoorEnv(t)
	e.seedCA(t)

	// A missing mkcert is simply the exit-4 case that is deterministic to
	// arrange; the assertion is about the message travelling, not about which
	// prerequisite is absent.
	_, _, code := e.run(t, "--mkcert="+filepath.Join(t.TempDir(), "definitely-not-mkcert"))
	if code != 4 {
		t.Fatalf("exit %d, want 4", code)
	}
	if !strings.Contains(e.lastStderr, "install.mkcert said:") {
		t.Errorf("the child's envelope was not forwarded, so its reason is nowhere:\n%s", e.lastStderr)
	}
	if !strings.Contains(e.lastStderr, "mkcert not found") {
		t.Errorf("the issuer's own message did not reach the operator:\n%s", e.lastStderr)
	}
}

// The internal CA is what identity serves TLS with and what every other node
// mounts to trust it. Without those two secrets each of them stalls in
// ContainerCreating on a FailedMount -- forever, since nothing retries a
// missing Secret into existence.
//
// This used to WARN and carry on, so `make secrets` succeeded, k3d.up reported
// the cluster up, the front door answered (traefik terminates TLS at the
// ingress with or without a backend), and the install ran two more steps before
// anything noticed. A skip whose documented consequence is a cluster that can
// never start is not a skip (memql#3570).
func TestSeedSecretsRefusesWithoutTheInternalCAGenerator(t *testing.T) {
	e := newFrontDoorEnv(t)
	e.seedCA(t)
	writeFrontDoorPair(t, e.home)

	// A repo root with no deploy/ tree -- which is exactly what a packaged
	// extension's staged copy is: scripts/ and nothing else.
	res, _, code := e.run(t, "--repo-root="+t.TempDir())
	if code != 4 {
		t.Errorf("exit %d, want 4 -- a cluster that can never start must not be reported as seeded", code)
	}
	// The message is in the ENVELOPE, which is where cap_fail writes it.
	if res.Error == nil || !strings.Contains(res.Error.Message, "ContainerCreating") {
		t.Errorf("the message should say what happens without the CA: %+v", res.Error)
	}
}

// The generator lives under deploy/, which only the CHECKOUT has -- so the root
// has to be the one the caller passes, not the one this script infers from its
// own location. Inferring it is what put the staged tree in that slot.
func TestSeedSecretsFindsTheGeneratorUnderTheGivenRoot(t *testing.T) {
	e := newFrontDoorEnv(t)
	e.seedCA(t)
	writeFrontDoorPair(t, e.home)

	root := t.TempDir()
	gen := filepath.Join(root, "deploy", "k8s", "base", "tls", "gen-internal-ca.sh")
	if err := os.MkdirAll(filepath.Dir(gen), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(gen, []byte("#!/usr/bin/env bash\nprintf 'generated\\n' >&2\n"), 0o755); err != nil {
		t.Fatalf("write generator: %v", err)
	}

	_, _, code := e.run(t, "--repo-root="+root)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e.lastStderr)
	}
	if !strings.Contains(e.lastStderr, "generated") {
		t.Errorf("the generator under --repo-root was not run:\n%s", e.lastStderr)
	}
}

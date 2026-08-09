// Tests for scripts/install/verify-frontdoor.sh (capability
// install.verifyFrontDoor, znasllc-io/memql#3365).
//
// The front door is the ONE connection path clients use in every environment
// (env parity: ingress -> TLS -> gRPC -> bff, dialed as https://cockpit.<domain>).
// Locally that means an /etc/hosts entry pointing the hostname at 127.0.0.1, a
// mkcert-signed wildcard cert traefik serves on 443, and h2 on the wire because
// gRPC cannot exist without it. Any one of those three can be broken while the
// other two look fine.
//
// The assertion that matters, and the shape of every test here: EACH CHECK
// REPORTS ITSELF -- name, host, passed, detail. "The front door is broken" is
// not actionable; "dns for identity.local.znas.io resolves to 10.0.0.5" is. So
// a failure in one host's TLS must not erase the other host's passing DNS, and
// the failing check must name what it actually saw.
//
// The second assertion: DNS must resolve to 127.0.0.1 SPECIFICALLY. Resolving
// somewhere else is worse than not resolving at all -- the installer would
// hand traffic to a stranger's box while every symptom points at the cluster.
//
// Hermetic: `curl` and `getent` are stubbed on a PATH prefix and driven by
// small map files, so no packet leaves the machine and no resolver on the
// runner is consulted.
package install

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fdEnvelope struct {
	OK         bool            `json:"ok"`
	Capability string          `json:"capability"`
	Changed    bool            `json:"changed"`
	Result     json.RawMessage `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// fdCheck is one reported check. The whole point of the capability is that
// this struct is populated per check rather than collapsed into a single bool.
type fdCheck struct {
	Name   string `json:"name"`
	Host   string `json:"host"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type fdResult struct {
	Hosts      string    `json:"hosts"`
	AllPassed  bool      `json:"allPassed"`
	Checks     []fdCheck `json:"checks"`
	ReportOnly bool      `json:"reportOnly"`
	Failed     int       `json:"failedCount"`
	Passed     int       `json:"passedCount"`
}

func fdScript(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "verify-frontdoor.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("verify-frontdoor.sh not found at %s: %v", p, err)
	}
	return p
}

func fdRun(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", append([]string{fdScript(t)}, args...)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = nil
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v", err)
		}
	}
	return out.String(), errb.String(), code
}

func fdParse(t *testing.T, stdout string) (fdEnvelope, fdResult) {
	t.Helper()
	line := strings.TrimSpace(stdout)
	if line == "" {
		t.Fatal("no envelope on stdout")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("stdout carried more than one line -- human logs belong on stderr:\n%s", line)
	}
	var env fdEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, line)
	}
	if env.Capability != "install.verifyFrontDoor" {
		t.Errorf("capability = %q, want install.verifyFrontDoor", env.Capability)
	}
	if env.Changed {
		t.Error("changed=true for a read-only verification")
	}
	var res fdResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		t.Fatalf("result is not the expected object: %v\n%s", err, env.Result)
	}
	return env, res
}

// fdFind returns the check with the given name+host, failing if it is absent.
func fdFind(t *testing.T, res fdResult, name, host string) fdCheck {
	t.Helper()
	for _, c := range res.Checks {
		if c.Name == name && c.Host == host {
			return c
		}
	}
	t.Fatalf("no %q check reported for host %q; checks: %+v", name, host, res.Checks)
	return fdCheck{}
}

// -----------------------------------------------------------------------
// Stub world: a resolver and an HTTPS client the test fully controls
// -----------------------------------------------------------------------

// fdWorld installs stub `getent` and `curl` on a PATH prefix.
//
//	dns:  host -> comma-separated addresses ("" means NXDOMAIN)
//	http: host -> "rc|httpVersion|httpCode"; rc != 0 makes curl fail with that
//	      exit code (60 = certificate problem, 7 = connection refused)
func fdWorld(t *testing.T, dns map[string]string, http map[string]string) []string {
	t.Helper()
	dir := t.TempDir()
	maps := t.TempDir()

	dnsMap := filepath.Join(maps, "dns")
	var b strings.Builder
	for h, addrs := range dns {
		b.WriteString(h + "|" + addrs + "\n")
	}
	if err := os.WriteFile(dnsMap, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write dns map: %v", err)
	}

	httpMap := filepath.Join(maps, "http")
	b.Reset()
	for h, spec := range http {
		b.WriteString(h + "|" + spec + "\n")
	}
	if err := os.WriteFile(httpMap, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write http map: %v", err)
	}

	getentStub := `#!/usr/bin/env bash
host="${2:-}"
addrs="$(awk -F'|' -v h="$host" '$1==h {print $2; exit}' "$STUB_DNS_MAP")"
[[ -z "$addrs" ]] && exit 2
IFS=',' read -ra list <<< "$addrs"
for a in "${list[@]}"; do printf '%s STREAM %s\n' "$a" "$host"; done
exit 0
`
	curlStub := `#!/usr/bin/env bash
url=""; w=""; prev=""
for a in "$@"; do
  case "$a" in
    https://*|http://*) url="$a" ;;
  esac
  case "$prev" in
    -w|--write-out) w="$a" ;;
  esac
  prev="$a"
done
host="${url#*://}"; host="${host%%/*}"; host="${host%%:*}"
spec="$(awk -F'|' -v h="$host" '$1==h {printf "%s|%s|%s", $2, $3, $4; exit}' "$STUB_HTTP_MAP")"
if [[ -z "$spec" ]]; then echo "stub curl: no route for $host" >&2; exit 7; fi
rc="${spec%%|*}"; rest="${spec#*|}"; ver="${rest%%|*}"; code="${rest#*|}"
if [[ "$rc" != "0" ]]; then echo "stub curl: failure rc=$rc for $host" >&2; exit "$rc"; fi
out="${w//%\{http_version\}/$ver}"
out="${out//%\{http_code\}/$code}"
printf '%s' "$out"
`
	for name, body := range map[string]string{"getent": getentStub, "curl": curlStub} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	return []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_DNS_MAP=" + dnsMap,
		"STUB_HTTP_MAP=" + httpMap,
	}
}

// fdHealthy is the world where everything works.
func fdHealthy(t *testing.T, hosts ...string) []string {
	t.Helper()
	dns := map[string]string{}
	http := map[string]string{}
	for _, h := range hosts {
		dns[h] = "127.0.0.1"
		http[h] = "0|2|200"
	}
	return fdWorld(t, dns, http)
}

const (
	fdCockpit  = "cockpit.local.znas.io"
	fdIdentity = "identity.local.znas.io"
)

// -----------------------------------------------------------------------
// Surface
// -----------------------------------------------------------------------

func TestVerifyFrontDoorPrintSpec(t *testing.T) {
	stdout, _, code := fdRun(t, nil, "--print-spec")
	if code != 0 {
		t.Fatalf("--print-spec exited %d\n%s", code, stdout)
	}
	var spec struct {
		Capability string `json:"capability"`
		Params     []struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &spec); err != nil {
		t.Fatalf("spec is not JSON: %v\n%s", err, stdout)
	}
	if spec.Capability != "install.verifyFrontDoor" {
		t.Errorf("capability = %q, want install.verifyFrontDoor", spec.Capability)
	}
	names := map[string]bool{}
	for _, p := range spec.Params {
		names[p.Name] = true
	}
	for _, want := range []string{"hosts", "report-only"} {
		if !names[want] {
			t.Errorf("spec is missing the %q param; got %v", want, names)
		}
	}
}

// TestVerifyFrontDoorDefaultsToTheLocalFrontDoor: run with no --hosts and the
// capability must check the two hostnames the local overlay actually serves.
// An installer verification that has to be told what to verify verifies
// nothing by default.
func TestVerifyFrontDoorDefaultsToTheLocalFrontDoor(t *testing.T) {
	env := fdHealthy(t, fdCockpit, fdIdentity)
	stdout, stderr, code := fdRun(t, env)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	_, res := fdParse(t, stdout)
	for _, h := range []string{fdCockpit, fdIdentity} {
		fdFind(t, res, "dns", h)
		fdFind(t, res, "tls", h)
	}
}

// TestVerifyFrontDoorAllPass is the happy path and the per-check-shape
// assertion: every reported check carries a name, a host, and a non-empty
// detail, and allPassed is the single boolean the graph verifies on.
func TestVerifyFrontDoorAllPass(t *testing.T) {
	env := fdHealthy(t, fdCockpit, fdIdentity)
	stdout, stderr, code := fdRun(t, env, "--hosts="+fdCockpit+","+fdIdentity)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	_, res := fdParse(t, stdout)
	if !res.AllPassed {
		t.Errorf("allPassed=false in a healthy world: %+v", res.Checks)
	}
	if res.Failed != 0 {
		t.Errorf("failedCount = %d, want 0", res.Failed)
	}
	// dns + tls per host, plus the gRPC reachability probe.
	if len(res.Checks) != 5 {
		t.Errorf("got %d checks, want 5 (dns+tls per host, plus grpc): %+v", len(res.Checks), res.Checks)
	}
	names := map[string]int{}
	for _, c := range res.Checks {
		names[c.Name]++
		if c.Host == "" {
			t.Errorf("check %q reports no host -- unactionable: %+v", c.Name, c)
		}
		if c.Detail == "" {
			t.Errorf("check %q/%s reports no detail -- unactionable", c.Name, c.Host)
		}
		if !c.Passed {
			t.Errorf("check %q/%s failed in a healthy world: %s", c.Name, c.Host, c.Detail)
		}
	}
	if names["grpc"] != 1 {
		t.Errorf("want exactly one grpc reachability check, got %d", names["grpc"])
	}
}

// -----------------------------------------------------------------------
// THE assertion: DNS must land on 127.0.0.1, and one bad check does not
// blind the others
// -----------------------------------------------------------------------

// TestVerifyFrontDoorDnsMustResolveToLoopback: a hostname resolving somewhere
// else is a WORSE failure than not resolving -- the installer would be talking
// to a stranger's box. The check must fail and the detail must name the
// address it actually saw.
func TestVerifyFrontDoorDnsMustResolveToLoopback(t *testing.T) {
	env := fdWorld(t,
		map[string]string{fdCockpit: "10.0.0.5", fdIdentity: "127.0.0.1"},
		map[string]string{fdCockpit: "0|2|200", fdIdentity: "0|2|200"},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdCockpit+","+fdIdentity)
	if code != 5 {
		t.Fatalf("exit %d, want 5 (strict by default)\nstdout: %s", code, stdout)
	}
	env2, res := fdParse(t, stdout)
	if env2.Error == nil || env2.Error.Code != 5 {
		t.Errorf("want error.code=5, got: %s", stdout)
	}
	if res.AllPassed {
		t.Error("allPassed=true while a host resolves off-loopback")
	}

	bad := fdFind(t, res, "dns", fdCockpit)
	if bad.Passed {
		t.Errorf("dns/%s passed while resolving to 10.0.0.5", fdCockpit)
	}
	if !strings.Contains(bad.Detail, "10.0.0.5") {
		t.Errorf("dns detail must name the address actually seen; got %q", bad.Detail)
	}

	// The other host is untouched: a single failure must not blind the report.
	good := fdFind(t, res, "dns", fdIdentity)
	if !good.Passed {
		t.Errorf("dns/%s should still pass; detail: %s", fdIdentity, good.Detail)
	}
}

func TestVerifyFrontDoorDnsNotResolvingFails(t *testing.T) {
	env := fdWorld(t,
		map[string]string{fdCockpit: ""}, // NXDOMAIN
		map[string]string{fdCockpit: "0|2|200"},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdCockpit)
	if code != 5 {
		t.Fatalf("exit %d, want 5\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	c := fdFind(t, res, "dns", fdCockpit)
	if c.Passed {
		t.Error("dns check passed for a hostname that does not resolve")
	}
	if c.Detail == "" {
		t.Error("dns failure carries no detail")
	}
}

// TestVerifyFrontDoorTlsFailureIsIsolated: a certificate problem must be
// reported as the TLS check failing, with DNS still reported as passing. That
// separation is what tells the operator to re-run `mkcert -install` instead of
// editing /etc/hosts.
func TestVerifyFrontDoorTlsFailureIsIsolated(t *testing.T) {
	env := fdWorld(t,
		map[string]string{fdCockpit: "127.0.0.1"},
		map[string]string{fdCockpit: "60|0|000"}, // curl 60: certificate problem
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdCockpit)
	if code != 5 {
		t.Fatalf("exit %d, want 5\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	if dns := fdFind(t, res, "dns", fdCockpit); !dns.Passed {
		t.Errorf("dns check should still pass when only TLS is broken: %s", dns.Detail)
	}
	tls := fdFind(t, res, "tls", fdCockpit)
	if tls.Passed {
		t.Error("tls check passed against a certificate error")
	}
	if !strings.Contains(tls.Detail, "60") {
		t.Errorf("tls detail should name what curl reported; got %q", tls.Detail)
	}
}

// TestVerifyFrontDoorGrpcProbeRequiresH2: gRPC cannot run over HTTP/1.1, so a
// front door that answers but will not negotiate h2 is a broken front door
// even though DNS and TLS are perfect.
func TestVerifyFrontDoorGrpcProbeRequiresH2(t *testing.T) {
	env := fdWorld(t,
		map[string]string{fdCockpit: "127.0.0.1"},
		map[string]string{fdCockpit: "0|1.1|200"},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdCockpit)
	if code != 5 {
		t.Fatalf("exit %d, want 5\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	g := fdFind(t, res, "grpc", fdCockpit)
	if g.Passed {
		t.Error("grpc reachability passed without HTTP/2")
	}
	if !strings.Contains(g.Detail, "1.1") {
		t.Errorf("grpc detail should name the protocol actually negotiated; got %q", g.Detail)
	}
	// DNS and TLS were fine and must say so.
	if c := fdFind(t, res, "tls", fdCockpit); !c.Passed {
		t.Errorf("tls should pass; detail: %s", c.Detail)
	}
}

// -----------------------------------------------------------------------
// Strict by default; --report-only for the diagnostic pass
// -----------------------------------------------------------------------

func TestVerifyFrontDoorReportOnlyExitsZero(t *testing.T) {
	env := fdWorld(t,
		map[string]string{fdCockpit: "10.0.0.5"},
		map[string]string{fdCockpit: "60|0|000"},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdCockpit, "--report-only")
	if code != 0 {
		t.Fatalf("--report-only exited %d, want 0\nstdout: %s", code, stdout)
	}
	e, res := fdParse(t, stdout)
	if !e.OK {
		t.Error("--report-only must still emit ok=true; the run succeeded, the front door did not")
	}
	if res.AllPassed {
		t.Error("allPassed=true in a broken world -- --report-only changes the exit code, not the truth")
	}
	if !res.ReportOnly {
		t.Error("result.reportOnly should record that strictness was waived")
	}
	if res.Failed == 0 {
		t.Error("failedCount = 0 in a broken world")
	}
}

// TestVerifyFrontDoorGrpcHostIsParameterisable lets a product front door on a
// different hostname be probed without editing the script.
func TestVerifyFrontDoorGrpcHostIsParameterisable(t *testing.T) {
	const custom = "app.local.znas.io"
	env := fdHealthy(t, fdCockpit, custom)
	stdout, _, code := fdRun(t, env, "--hosts="+fdCockpit, "--grpc-host="+custom)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	fdFind(t, res, "grpc", custom)
}

func TestVerifyFrontDoorMissingCurlIsPrerequisite(t *testing.T) {
	dir := fdSanitizedBin(t, []string{"getent"})
	stdout, _, code := fdRun(t, []string{"PATH=" + dir}, "--hosts="+fdCockpit)
	if code != 4 {
		t.Fatalf("exit %d, want 4 (prerequisite missing)\nstdout: %s", code, stdout)
	}
}

// fdSanitizedBin builds a bin directory holding ONLY the shell utilities the
// capability library needs plus the named extras, so a test can prove what
// happens when a tool is genuinely absent (the runner's own PATH has curl).
func fdSanitizedBin(t *testing.T, extras []string) string {
	t.Helper()
	dir := t.TempDir()
	base := []string{"bash", "tr", "grep", "sed", "cat", "head", "tail", "mktemp",
		"chmod", "rm", "awk", "cut", "sort", "uniq", "printf", "mkdir", "dirname"}
	for _, name := range append(base, extras...) {
		src, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if err := os.Symlink(src, filepath.Join(dir, name)); err != nil && !os.IsExist(err) {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	return dir
}

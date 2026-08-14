// Tests for scripts/install/verify-frontdoor.sh (capability
// install.verifyFrontDoor, znasllc-io/memql#3365).
//
// The front door is the ONE connection path clients use in every environment
// (env parity: ingress -> TLS -> gRPC -> bff, dialed as https://api.<domain>).
// Locally that means an /etc/hosts entry pointing the hostname at 127.0.0.1, a
// mkcert-signed wildcard cert traefik serves on 443, and h2 on the wire because
// gRPC cannot exist without it. Any one of those three can be broken while the
// other two look fine.
//
// The assertion that matters, and the shape of every test here: EACH CHECK
// REPORTS ITSELF -- name, host, passed, detail. "The front door is broken" is
// not actionable; "dns for identity.memql.localhost resolves to 10.0.0.5" is. So
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
//
// Status carries the THIRD state: "passed" | "failed" | "inconclusive". Passed
// stays a plain boolean and is false for an inconclusive check -- it never
// claims a pass that was not measured -- while only "failed" counts against
// the rollup. See the script's record_check_status.
type fdCheck struct {
	Name   string `json:"name"`
	Host   string `json:"host"`
	Passed bool   `json:"passed"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type fdResult struct {
	Hosts             string    `json:"hosts"`
	AllPassed         bool      `json:"allPassed"`
	Checks            []fdCheck `json:"checks"`
	ReportOnly        bool      `json:"reportOnly"`
	Failed            int       `json:"failedCount"`
	Passed            int       `json:"passedCount"`
	Inconclusive      int       `json:"inconclusiveCount"`
	WildcardProbeHost string    `json:"wildcardProbeHost"`
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
//	http: host -> "rc|httpVersion|httpCode" or "rc|httpVersion|httpCode|body";
//	      rc != 0 makes curl fail with that exit code (60 = certificate problem,
//	      7 = connection refused). A host absent from the map is unroutable.
//
// The stub models two things the precedence probe depends on and the earlier
// probes did not: a RESPONSE BODY (the /healthz body is the only thing that
// says WHICH backend answered), and curl's own resolution -- a host with no dns
// entry fails with exit 6 unless the caller pinned it with --resolve, which is
// how a real machine behaves and what the synthetic wildcard-probe host needs,
// because a hosts file has no wildcard.
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
url=""; w=""; prev=""; sink=""; pinned=""
for a in "$@"; do
  case "$a" in
    https://*|http://*) url="$a" ;;
  esac
  case "$prev" in
    -w|--write-out) w="$a" ;;
    -o|--output)    sink="$a" ;;
    --resolve)      pinned="$a" ;;
  esac
  prev="$a"
done
host="${url#*://}"; host="${host%%/*}"; host="${host%%:*}"
# Every dialled host is recorded, which is the only way a test can assert what
# was NOT dialled -- a host needing no probe must not get one. Guarded so a
# hand-built world without the log variable fails in fdDialled (which says so)
# rather than here as an ambiguous redirect.
[[ -n "${STUB_CALL_LOG:-}" ]] && printf '%s\n' "$host" >> "$STUB_CALL_LOG"
# Resolution, the way curl does it: --resolve pins an address and skips the
# resolver; without it a name absent from the hosts map cannot be dialled.
if [[ "${pinned%%:*}" != "$host" ]]; then
  if [[ -z "$(awk -F'|' -v h="$host" '$1==h {print $2; exit}' "$STUB_DNS_MAP")" ]]; then
    echo "stub curl: could not resolve host: $host" >&2; exit 6
  fi
fi
spec="$(awk -F'|' -v h="$host" '$1==h {print substr($0, index($0,"|")+1); exit}' "$STUB_HTTP_MAP")"
if [[ -z "$spec" ]]; then echo "stub curl: no route for $host" >&2; exit 7; fi
rc="${spec%%|*}"; rest="${spec#*|}"
ver="${rest%%|*}"; rest="${rest#*|}"
code="${rest%%|*}"
body=""
[[ "$rest" == *"|"* ]] && body="${rest#*|}"
if [[ "$rc" != "0" ]]; then echo "stub curl: failure rc=$rc for $host" >&2; exit "$rc"; fi
out="${w//\\n/$'\n'}"
out="${out//%\{http_version\}/$ver}"
out="${out//%\{http_code\}/$code}"
# Real curl writes the body to stdout unless it was told to put it somewhere
# else, then the --write-out string after the transfer.
[[ -z "$sink" ]] && printf '%s' "$body"
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
		"STUB_CALL_LOG=" + filepath.Join(maps, "calls"),
	}
}

// fdDialled returns every hostname the stub curl was asked for, in order. Used
// to assert a NEGATIVE -- that a host with nothing to establish was not dialled.
func fdDialled(t *testing.T, env []string) []string {
	t.Helper()
	var path string
	for _, kv := range env {
		if strings.HasPrefix(kv, "STUB_CALL_LOG=") {
			path = strings.TrimPrefix(kv, "STUB_CALL_LOG=")
		}
	}
	if path == "" {
		t.Fatal("this world has no call log")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing was dialled at all
		}
		t.Fatalf("read call log: %v", err)
	}
	return strings.Fields(string(body))
}

// fdHealth is the /healthz body a memQL node returns. It is the discriminator
// the precedence check reads: the node that served the request names itself, and
// nothing weaker can separate the site edge from the ingress controller's own
// default backend (both 404s are Go's http.NotFound, byte for byte).
func fdHealth(nodeType, nodeID string) string {
	return `{"status":"ok","activeStreams":0,"nodeId":"` + nodeID + `","nodeType":"` + nodeType + `"}`
}

// fdTraefik404 is the ingress controller's own answer when no router matches --
// which is also, byte for byte, what the edge returns for a hostname it has no
// site row for. Reproduced here because the whole point of the discriminator is
// that this response names nobody.
const fdTraefik404 = "404 page not found"

// fdHealthy is the world where everything works: every named host resolves to
// loopback, answers h2 over a trusted certificate, and identifies itself on
// /healthz -- PLUS the synthetic wildcard-probe host, answered by the edge,
// which is what makes the precedence check testable instead of vacuous.
//
// fdProbe is deliberately absent from the dns map: a hosts file has no wildcard
// entry and never will, so the probe has to pin the address with --resolve. The
// stub models that (exit 6 without a pin), which is what keeps the pin from
// being dropped by a later edit.
func fdHealthy(t *testing.T, hosts ...string) []string {
	t.Helper()
	dns := map[string]string{}
	http := map[string]string{}
	for _, h := range hosts {
		dns[h] = "127.0.0.1"
		nodeType := fdNodeTypeFor(h)
		http[h] = "0|2|200|" + fdHealth(nodeType, nodeType+"-6d4f9c8b7a-2xk9p")
	}
	http[fdProbe] = "0|2|200|" + fdHealth(fdEdgeNodeType, fdEdgeNodeType+"-5c7b9d4f6e-tq4m2")
	return fdWorld(t, dns, http)
}

// fdNodeTypeFor names the node type behind a front-door host, as /healthz would
// report it: the api. door fronts the bff, and every other host in these tests
// is served by a node named after its own first label.
func fdNodeTypeFor(host string) string {
	if host == fdAPI {
		return "bff"
	}
	return strings.SplitN(host, ".", 2)[0]
}

const (
	fdAPI      = "api.memql.localhost"
	fdIdentity = "identity.memql.localhost"
	// The synthetic name PROBE 4 dials to find out whether a wildcard router is
	// loaded at all. Must match WILDCARD_PROBE_LABEL in the script.
	fdProbe = "frontdoor-precedence-probe.memql.localhost"
	// The node type serving `*.<domain>` and the apex (deploy/k8s/overlays/
	// local/edge-front-door.yaml -> svc/edge).
	fdEdgeNodeType = "edge"
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
	// wildcard-probe-host is in the list because --print-spec IS the accepted
	// flag surface: a param the script reads and does not declare is rejected by
	// its own library before it can be used.
	for _, want := range []string{"hosts", "report-only", "wildcard-probe-host"} {
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
	env := fdHealthy(t, fdAPI, fdIdentity)
	stdout, stderr, code := fdRun(t, env)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	_, res := fdParse(t, stdout)
	for _, h := range []string{fdAPI, fdIdentity} {
		fdFind(t, res, "dns", h)
		fdFind(t, res, "tls", h)
	}
}

// TestVerifyFrontDoorAllPass is the happy path and the per-check-shape
// assertion: every reported check carries a name, a host, and a non-empty
// detail, and allPassed is the single boolean the graph verifies on.
func TestVerifyFrontDoorAllPass(t *testing.T) {
	env := fdHealthy(t, fdAPI, fdIdentity)
	stdout, stderr, code := fdRun(t, env, "--hosts="+fdAPI+","+fdIdentity)
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
	// dns + tls + precedence per host, plus the gRPC reachability probe.
	if len(res.Checks) != 7 {
		t.Errorf("got %d checks, want 7 (dns+tls+precedence per host, plus grpc): %+v", len(res.Checks), res.Checks)
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
		if !c.Passed || c.Status != "passed" {
			t.Errorf("check %q/%s did not pass in a healthy world (status %q): %s",
				c.Name, c.Host, c.Status, c.Detail)
		}
	}
	if names["grpc"] != 1 {
		t.Errorf("want exactly one grpc reachability check, got %d", names["grpc"])
	}
	if names["precedence"] != 2 {
		t.Errorf("want a precedence check per host (2), got %d", names["precedence"])
	}
	if res.Inconclusive != 0 {
		t.Errorf("inconclusiveCount = %d in a world where every property is measurable", res.Inconclusive)
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
		map[string]string{fdAPI: "10.0.0.5", fdIdentity: "127.0.0.1"},
		map[string]string{fdAPI: "0|2|200", fdIdentity: "0|2|200"},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI+","+fdIdentity)
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

	bad := fdFind(t, res, "dns", fdAPI)
	if bad.Passed {
		t.Errorf("dns/%s passed while resolving to 10.0.0.5", fdAPI)
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
		map[string]string{fdAPI: ""}, // NXDOMAIN
		map[string]string{fdAPI: "0|2|200"},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI)
	if code != 5 {
		t.Fatalf("exit %d, want 5\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	c := fdFind(t, res, "dns", fdAPI)
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
		map[string]string{fdAPI: "127.0.0.1"},
		map[string]string{fdAPI: "60|0|000"}, // curl 60: certificate problem
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI)
	if code != 5 {
		t.Fatalf("exit %d, want 5\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	if dns := fdFind(t, res, "dns", fdAPI); !dns.Passed {
		t.Errorf("dns check should still pass when only TLS is broken: %s", dns.Detail)
	}
	tls := fdFind(t, res, "tls", fdAPI)
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
		map[string]string{fdAPI: "127.0.0.1"},
		map[string]string{fdAPI: "0|1.1|200"},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI)
	if code != 5 {
		t.Fatalf("exit %d, want 5\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	g := fdFind(t, res, "grpc", fdAPI)
	if g.Passed {
		t.Error("grpc reachability passed without HTTP/2")
	}
	if !strings.Contains(g.Detail, "1.1") {
		t.Errorf("grpc detail should name the protocol actually negotiated; got %q", g.Detail)
	}
	// DNS and TLS were fine and must say so.
	if c := fdFind(t, res, "tls", fdAPI); !c.Passed {
		t.Errorf("tls should pass; detail: %s", c.Detail)
	}
}

// -----------------------------------------------------------------------
// Wildcard versus exact host precedence, and the vacuous case
//
// The five-host front door (memql#3700) serves `*.<domain>` from the site edge
// beside the exact api. / identity. / mcp. names, and the wildcard MATCHES
// those exact names too -- so the design rests on an exact host outranking a
// wildcard (D3). The trap is that the check is trivially satisfiable: with the
// wildcard's backend Service absent, the ingress controller drops the whole
// wildcard router, and "api. reached the bff" is then true with no competing
// route in existence. These tests pin BOTH halves: the check must be able to
// fail, and it must refuse to claim a pass it did not measure.
// -----------------------------------------------------------------------

// TestVerifyFrontDoorPrecedenceIsInconclusiveWhileTheWildcardIsUnrouted is the
// vacuous case, and the state of a real cluster before the edge Deployment
// lands. The wildcard name does not answer at all, so precedence is untestable
// -- and reporting that as a PASS would be the false assurance the check exists
// to remove, while reporting it as a FAILURE would fail an install over a
// property the cluster is not yet in a position to have.
func TestVerifyFrontDoorPrecedenceIsInconclusiveWhileTheWildcardIsUnrouted(t *testing.T) {
	// Note what is NOT in this world: the wildcard probe host. Everything the
	// capability asserts is up, is up.
	env := fdWorld(t,
		map[string]string{fdAPI: "127.0.0.1", fdIdentity: "127.0.0.1"},
		map[string]string{
			fdAPI:      "0|2|200|" + fdHealth("bff", "bff-1"),
			fdIdentity: "0|2|200|" + fdHealth("identity", "identity-1"),
		},
	)
	stdout, stderr, code := fdRun(t, env)
	if code != 0 {
		t.Fatalf("exit %d, want 0 -- an unprovable property is not a broken front door\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	_, res := fdParse(t, stdout)
	if !res.AllPassed {
		t.Errorf("allPassed=false: an inconclusive check must not count as a failure: %+v", res.Checks)
	}
	if res.Failed != 0 {
		t.Errorf("failedCount = %d, want 0", res.Failed)
	}
	// THE assertion of this file, in its sharpest form: the two inconclusive
	// checks are not in the passed tally either.
	if res.Passed != 5 {
		t.Errorf("passedCount = %d, want 5 (dns+tls per host, plus grpc) -- an inconclusive check must not be counted as a pass: %+v",
			res.Passed, res.Checks)
	}
	if res.Inconclusive != 2 {
		t.Errorf("inconclusiveCount = %d, want 2 (one per host)", res.Inconclusive)
	}
	for _, h := range []string{fdAPI, fdIdentity} {
		c := fdFind(t, res, "precedence", h)
		if c.Passed {
			t.Errorf("precedence/%s reports passed=true with no wildcard route in existence -- that is the vacuous pass: %s", h, c.Detail)
		}
		if c.Status != "inconclusive" {
			t.Errorf("precedence/%s status = %q, want inconclusive", h, c.Status)
		}
		// The detail has to say WHY, or the operator reading it cannot tell an
		// unprovable property from a broken one.
		if !strings.Contains(c.Detail, "wildcard router is not loaded") ||
			!strings.Contains(c.Detail, "nothing for an exact host to take precedence over") {
			t.Errorf("precedence/%s must name the reason it could not be established; got %q", h, c.Detail)
		}
	}
	if !strings.Contains(stderr, "INCONCLUSIVE") {
		t.Errorf("an unproven check must be visible in the log, not silent:\n%s", stderr)
	}
}

// TestVerifyFrontDoorPrecedenceIsInconclusiveOnADefaultBackend404 is the crux
// of the discriminator. When the wildcard router is dropped, the front door
// answers the wildcard name with the ingress controller's own 404 -- which is
// the SAME status code, and the same body, as the edge's answer for a hostname
// it has no site row for. A probe that reads "it answered" as "the edge is
// live" would then run its precedence assertion against a route that does not
// exist and pass.
func TestVerifyFrontDoorPrecedenceIsInconclusiveOnADefaultBackend404(t *testing.T) {
	env := fdWorld(t,
		map[string]string{fdAPI: "127.0.0.1"},
		map[string]string{
			fdAPI:   "0|2|200|" + fdHealth("bff", "bff-1"),
			fdProbe: "0|2|404|" + fdTraefik404,
		},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	c := fdFind(t, res, "precedence", fdAPI)
	if c.Passed || c.Status != "inconclusive" {
		t.Errorf("a response that names no node was read as proof the wildcard is live (status %q): %s", c.Status, c.Detail)
	}
	// Naming the code is what lets an operator recognise the state from the
	// report alone.
	if !strings.Contains(c.Detail, "404") {
		t.Errorf("detail should name what the wildcard host actually answered; got %q", c.Detail)
	}
}

// TestVerifyFrontDoorPrecedenceFailsWhenTheWildcardSwallowsAnExactHost is the
// failure this check exists for: the wildcard router is live AND it is the
// thing answering api., which means the exact rule lost and every api. client
// is talking to the site edge.
//
// The two edge pods carry DIFFERENT ids on purpose. The comparison is on node
// TYPE, because the edge may run more than one replica -- an id comparison
// would call this a pass.
func TestVerifyFrontDoorPrecedenceFailsWhenTheWildcardSwallowsAnExactHost(t *testing.T) {
	env := fdWorld(t,
		map[string]string{fdAPI: "127.0.0.1", fdIdentity: "127.0.0.1"},
		map[string]string{
			fdAPI:      "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-5c7b9d4f6e-abcde"),
			fdIdentity: "0|2|200|" + fdHealth("identity", "identity-1"),
			fdProbe:    "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-5c7b9d4f6e-zyxwv"),
		},
	)
	stdout, _, code := fdRun(t, env)
	if code != 5 {
		t.Fatalf("exit %d, want 5 (strict by default)\nstdout: %s", code, stdout)
	}
	e, res := fdParse(t, stdout)
	if e.Error == nil || e.Error.Code != 5 {
		t.Errorf("want error.code=5, got: %s", stdout)
	}
	if res.AllPassed {
		t.Error("allPassed=true while the wildcard rule is answering an exact host")
	}
	bad := fdFind(t, res, "precedence", fdAPI)
	if bad.Passed || bad.Status != "failed" {
		t.Errorf("precedence/%s did not fail while the edge answered it (status %q): %s", fdAPI, bad.Status, bad.Detail)
	}
	if !strings.Contains(bad.Detail, fdEdgeNodeType) {
		t.Errorf("detail must name the node type that answered; got %q", bad.Detail)
	}

	// One broken door does not blind the others, and DNS/TLS were fine: the
	// separation is what tells an operator to look at the Ingress rules rather
	// than at the hosts file or the certificate.
	if good := fdFind(t, res, "precedence", fdIdentity); !good.Passed {
		t.Errorf("precedence/%s should still pass; detail: %s", fdIdentity, good.Detail)
	}
	if c := fdFind(t, res, "dns", fdAPI); !c.Passed {
		t.Errorf("dns/%s should still pass; detail: %s", fdAPI, c.Detail)
	}
	if c := fdFind(t, res, "tls", fdAPI); !c.Passed {
		t.Errorf("tls/%s should still pass; detail: %s", fdAPI, c.Detail)
	}
}

// TestVerifyFrontDoorPrecedenceFailsForAHostThatIsNotTheFirst: the failure path
// must not depend on position either.
//
// Every other failure test swallows `api.`, which is `host_list[0]` -- so a
// regression that only ever compared the first host would keep them all green.
// That is not hypothetical in this file: resolving the apex from `host_list[0]`
// alone is precisely the bug that made the apex short-circuit
// position-dependent, and this is the same assumption one function over.
func TestVerifyFrontDoorPrecedenceFailsForAHostThatIsNotTheFirst(t *testing.T) {
	env := fdWorld(t,
		map[string]string{fdAPI: "127.0.0.1", fdIdentity: "127.0.0.1"},
		map[string]string{
			fdAPI:      "0|2|200|" + fdHealth("bff", "bff-1"),
			fdIdentity: "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-9"),
			fdProbe:    "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-2"),
		},
	)
	stdout, _, code := fdRun(t, env)
	if code != 5 {
		t.Fatalf("exit %d, want 5 -- a swallowed SECOND host must fail the run\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	if c := fdFind(t, res, "precedence", fdIdentity); c.Status != "failed" {
		t.Errorf("precedence/%s status = %q, want failed: %s", fdIdentity, c.Status, c.Detail)
	}
	if c := fdFind(t, res, "precedence", fdAPI); !c.Passed {
		t.Errorf("precedence/%s should still pass: %s", fdAPI, c.Detail)
	}
}

// TestVerifyFrontDoorPrecedencePassesOnPositiveIdentification pins the shape of
// a pass: the wildcard is live, and each exact host is answered by a DIFFERENT
// node type, named in the detail. This is the by-hand measurement the epic
// recorded -- "the bff's own pod id in the body is what proves it".
func TestVerifyFrontDoorPrecedencePassesOnPositiveIdentification(t *testing.T) {
	env := fdHealthy(t, fdAPI, fdIdentity)
	stdout, _, code := fdRun(t, env)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	if res.WildcardProbeHost != fdProbe {
		t.Errorf("wildcardProbeHost = %q, want %q (derived from the probed hosts' apex)", res.WildcardProbeHost, fdProbe)
	}
	for host, want := range map[string]string{fdAPI: "bff", fdIdentity: "identity"} {
		c := fdFind(t, res, "precedence", host)
		if !c.Passed || c.Status != "passed" {
			t.Errorf("precedence/%s should pass (status %q): %s", host, c.Status, c.Detail)
		}
		if !strings.Contains(c.Detail, want) {
			t.Errorf("precedence/%s should name the node type that answered (%q); got %q", host, want, c.Detail)
		}
		if !strings.Contains(c.Detail, fdEdgeNodeType) {
			t.Errorf("precedence/%s should name what it was compared against; got %q", host, c.Detail)
		}
	}
}

// TestVerifyFrontDoorPrecedenceComparesTheMeasuredTypeNotALiteral: the check
// must compare each host against WHAT IT MEASURED on the wildcard name, never
// against a hardcoded "edge".
//
// The world is deliberately shaped so a literal cannot fake it: the wildcard is
// served by nodeType "sites", and the swallowed host answers "sites" too. A
// hardcoded `wildcard_type="edge"` would find "sites" != "edge" and report a
// PASS -- which is why the swallowed host here must NOT be the type the rest of
// the suite uses. A test where the swallow is "edge" cannot tell a measurement
// from a literal, and would stay green through the regression.
func TestVerifyFrontDoorPrecedenceComparesTheMeasuredTypeNotALiteral(t *testing.T) {
	const renamed = "sites" // the edge node type, renamed by some later epic
	env := fdWorld(t,
		map[string]string{fdAPI: "127.0.0.1", fdIdentity: "127.0.0.1"},
		map[string]string{
			fdAPI:      "0|2|200|" + fdHealth(renamed, renamed+"-1"),
			fdIdentity: "0|2|200|" + fdHealth("identity", "identity-1"),
			fdProbe:    "0|2|200|" + fdHealth(renamed, renamed+"-2"),
		},
	)
	stdout, _, code := fdRun(t, env)
	if code != 5 {
		t.Fatalf("exit %d, want 5 -- the swallowed host was not caught, so the comparison is against a literal rather than the measurement\nstdout: %s",
			code, stdout)
	}
	_, res := fdParse(t, stdout)
	bad := fdFind(t, res, "precedence", fdAPI)
	if bad.Status != "failed" {
		t.Errorf("precedence/%s status = %q, want failed: %s", fdAPI, bad.Status, bad.Detail)
	}
	if !strings.Contains(bad.Detail, renamed) {
		t.Errorf("detail must name the measured node type %q; got %q", renamed, bad.Detail)
	}
	// The measured type still discriminates in the other direction.
	if good := fdFind(t, res, "precedence", fdIdentity); !good.Passed {
		t.Errorf("precedence/%s should pass: %s", fdIdentity, good.Detail)
	}
	// The remedy travels with the failure -- curl_tls_hint's precedent -- and
	// BOTH branches are named, because the response cannot distinguish "the
	// wildcard outranked an exact rule" from "this name is wildcard-served on
	// purpose", and only one of those is repaired with a priority annotation.
	for _, want := range []string{"router.priority", "does not belong in --hosts"} {
		if !strings.Contains(bad.Detail, want) {
			t.Errorf("the failure detail should carry %q so the operator is not sent to the wrong repair: %q", want, bad.Detail)
		}
	}
}

// TestVerifyFrontDoorPrecedenceApexIsEdgeServedByDesign: the apex is answered by
// the edge on purpose -- its own rule points at the same Service the wildcard
// does -- so there is no precedence to establish for it. Reporting it as FAILED
// would be a detail that is exactly backwards ("the wildcard swallowed this
// host"), and it is reachable: frontDoorFor().hostnames includes the apex, so a
// human passing the whole hosts-block list hits it immediately.
func TestVerifyFrontDoorPrecedenceApexIsEdgeServedByDesign(t *testing.T) {
	const apex = "memql.localhost"
	env := fdWorld(t,
		map[string]string{fdAPI: "127.0.0.1", apex: "127.0.0.1"},
		map[string]string{
			fdAPI:   "0|2|200|" + fdHealth("bff", "bff-1"),
			apex:    "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-1"),
			fdProbe: "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-1"),
		},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI+","+apex)
	if code != 0 {
		t.Fatalf("exit %d, want 0 -- the apex being edge-served is the design, not a defect\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	c := fdFind(t, res, "precedence", apex)
	if c.Passed || c.Status != "inconclusive" {
		t.Errorf("precedence/%s status = %q, want inconclusive: %s", apex, c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "BY DESIGN") {
		t.Errorf("the detail has to say the edge answering here is intended; got %q", c.Detail)
	}
	// The host that does carry its own rule is unaffected.
	if good := fdFind(t, res, "precedence", fdAPI); !good.Passed {
		t.Errorf("precedence/%s should still pass: %s", fdAPI, good.Detail)
	}
}

// TestVerifyFrontDoorPrecedenceDialsNothingWhenThereIsNothingToEstablish: with
// the apex as the only probed host, the wildcard-liveness probe answers a
// question nobody asked. Not dialling it is the difference between a check that
// knows what it is for and one that just runs.
//
// IT ASSERTS THE REASON, NOT JUST THE STATUS, and that is the point of the
// assertion rather than pedantry. `status == inconclusive` plus "nothing was
// dialled" is ALSO true when the apex was never recognised and the run fell into
// the older "cannot derive the wildcard apex" branch -- which is exactly what a
// bug in the apex derivation produced while this test stayed green, claiming in
// its own comment to demonstrate a mechanism it was not reaching. A test that
// cannot tell which branch answered it proves nothing about either.
func TestVerifyFrontDoorPrecedenceDialsNothingWhenThereIsNothingToEstablish(t *testing.T) {
	const apex = "memql.localhost"
	env := fdWorld(t,
		map[string]string{apex: "127.0.0.1"},
		map[string]string{
			apex:    "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-1"),
			fdProbe: "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-1"),
		},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+apex)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	c := fdFind(t, res, "precedence", apex)
	if c.Status != "inconclusive" {
		t.Errorf("precedence/%s status = %q, want inconclusive", apex, c.Status)
	}
	if !strings.Contains(c.Detail, "BY DESIGN") {
		t.Errorf("precedence/%s must be inconclusive BECAUSE it is the apex; got %q", apex, c.Detail)
	}
	if strings.Contains(c.Detail, "cannot derive the wildcard apex") {
		t.Errorf("the apex was not recognised as the apex -- this answered from the "+
			"derivation-failure branch, which happens to satisfy the status and "+
			"not-dialled assertions while the mechanism under test never ran: %q", c.Detail)
	}
	for _, host := range fdDialled(t, env) {
		if host == fdProbe {
			t.Errorf("dialled %s with no host to establish precedence for; calls: %v", fdProbe, fdDialled(t, env))
			break
		}
	}
}

// TestVerifyFrontDoorPrecedenceApexIsRecognisedInAnyPosition: the apex is a
// property of the host SET, so which host was typed first must not decide
// anything.
//
// Deriving it from host_list[0] alone made the apex-FIRST ordering yield no apex
// at all (the apex has no label to strip), so no wildcard probe host was built
// and every host in the list -- `api.` and `identity.` included, both fully
// testable -- was reported "cannot derive the wildcard apex". A run that silently
// establishes nothing for the two hosts the check exists to cover is the failure
// mode; the ordering that triggers it is the one a human hits by pasting the
// whole hosts-block list.
func TestVerifyFrontDoorPrecedenceApexIsRecognisedInAnyPosition(t *testing.T) {
	const apex = "memql.localhost"
	world := func(t *testing.T) []string {
		return fdWorld(t,
			map[string]string{fdAPI: "127.0.0.1", fdIdentity: "127.0.0.1", apex: "127.0.0.1"},
			map[string]string{
				fdAPI:      "0|2|200|" + fdHealth("bff", "bff-1"),
				fdIdentity: "0|2|200|" + fdHealth("identity", "identity-1"),
				apex:       "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-1"),
				fdProbe:    "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-2"),
			},
		)
	}
	for _, hosts := range []string{
		apex + "," + fdAPI + "," + fdIdentity, // apex first -- the broken ordering
		fdAPI + "," + apex + "," + fdIdentity, // apex in the middle
		fdAPI + "," + fdIdentity + "," + apex, // apex last -- frontDoorFor()'s order
	} {
		t.Run(hosts, func(t *testing.T) {
			env := world(t)
			stdout, _, code := fdRun(t, env, "--hosts="+hosts)
			if code != 0 {
				t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
			}
			_, res := fdParse(t, stdout)

			ap := fdFind(t, res, "precedence", apex)
			if !strings.Contains(ap.Detail, "BY DESIGN") {
				t.Errorf("apex not recognised as the apex in this ordering: %q", ap.Detail)
			}
			// The load-bearing half: the testable hosts stay testable.
			for _, h := range []string{fdAPI, fdIdentity} {
				c := fdFind(t, res, "precedence", h)
				if !c.Passed || c.Status != "passed" {
					t.Errorf("precedence/%s is %q in this ordering -- a testable host was not tested: %s",
						h, c.Status, c.Detail)
				}
			}
		})
	}
}

// TestVerifyFrontDoorPrecedenceIsInconclusiveWhenAnExactHostNamesNoNode: the
// wildcard is live, so precedence IS testable, but this host's backend does not
// answer /healthz with a memQL identity (an mcp-style 401 to an unauthenticated
// probe). Which backend served it cannot be established, and a check that
// cannot see the answer must not guess it.
func TestVerifyFrontDoorPrecedenceIsInconclusiveWhenAnExactHostNamesNoNode(t *testing.T) {
	env := fdWorld(t,
		map[string]string{fdAPI: "127.0.0.1"},
		map[string]string{
			fdAPI:   "0|2|401|Unauthorized",
			fdProbe: "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-1"),
		},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	c := fdFind(t, res, "precedence", fdAPI)
	if c.Passed || c.Status != "inconclusive" {
		t.Errorf("status = %q, want inconclusive: %s", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "401") {
		t.Errorf("detail should name what the host answered; got %q", c.Detail)
	}
}

// TestVerifyFrontDoorPrecedenceProbeHostIsOverridable: a cluster whose wildcard
// apex is not the probed hosts' parent domain can name the probe host itself,
// without editing the script.
func TestVerifyFrontDoorPrecedenceProbeHostIsOverridable(t *testing.T) {
	const custom = "nothing-claims-this.example.test"
	env := fdWorld(t,
		map[string]string{fdAPI: "127.0.0.1"},
		map[string]string{
			fdAPI:  "0|2|200|" + fdHealth("bff", "bff-1"),
			custom: "0|2|200|" + fdHealth(fdEdgeNodeType, "edge-1"),
		},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI, "--wildcard-probe-host="+custom)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	if res.WildcardProbeHost != custom {
		t.Errorf("wildcardProbeHost = %q, want the override %q", res.WildcardProbeHost, custom)
	}
	if c := fdFind(t, res, "precedence", fdAPI); !c.Passed {
		t.Errorf("precedence should pass against the overridden probe host: %s", c.Detail)
	}
}

// TestVerifyFrontDoorPrecedenceIsInconclusiveWithNoDerivableApex: a single-label
// host has no parent domain a `*.<apex>` rule could cover, so there is no name
// to dial. Guessing one would mean dialling something the operator never named.
func TestVerifyFrontDoorPrecedenceIsInconclusiveWithNoDerivableApex(t *testing.T) {
	const bare = "localhost"
	env := fdWorld(t,
		map[string]string{bare: "127.0.0.1"},
		map[string]string{bare: "0|2|200|" + fdHealth("bff", "bff-1")},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+bare)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	c := fdFind(t, res, "precedence", bare)
	if c.Passed || c.Status != "inconclusive" {
		t.Errorf("status = %q, want inconclusive: %s", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "--wildcard-probe-host") {
		t.Errorf("detail should name the way out; got %q", c.Detail)
	}
}

// -----------------------------------------------------------------------
// Strict by default; --report-only for the diagnostic pass
// -----------------------------------------------------------------------

func TestVerifyFrontDoorReportOnlyExitsZero(t *testing.T) {
	env := fdWorld(t,
		map[string]string{fdAPI: "10.0.0.5"},
		map[string]string{fdAPI: "60|0|000"},
	)
	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI, "--report-only")
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
	const custom = "app.memql.localhost"
	env := fdHealthy(t, fdAPI, custom)
	stdout, _, code := fdRun(t, env, "--hosts="+fdAPI, "--grpc-host="+custom)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	_, res := fdParse(t, stdout)
	fdFind(t, res, "grpc", custom)
}

func TestVerifyFrontDoorMissingCurlIsPrerequisite(t *testing.T) {
	dir := fdSanitizedBin(t, []string{"getent"})
	stdout, _, code := fdRun(t, []string{"PATH=" + dir}, "--hosts="+fdAPI)
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

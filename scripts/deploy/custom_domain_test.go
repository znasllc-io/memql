// Tests for scripts/deploy/bind-custom-domain.sh (capability domain.bind) and
// scripts/deploy/unbind-custom-domain.sh (capability domain.unbind), epic
// memql#4805.
//
// These drive the scripts as PROGRAMS -- argv in, one JSON envelope out, an
// exit code -- because that is the whole of their contract. The engine-side
// half (envelope -> typed failureReason on the row) is proved in
// integrations/customdomain; what is proved here is that the envelope those
// tests assume is the envelope these scripts actually emit.
package deploy

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/integrations/customdomain"
	"gopkg.in/yaml.v3"
)

const (
	bindScript   = "bind-custom-domain.sh"
	unbindScript = "unbind-custom-domain.sh"
)

// envelopeFrom runs a script and parses its stdout through the SAME parser the
// engine uses. Parsing with deploycontrol.ParseCapabilityResult rather than
// encoding/json is the point: a script whose envelope this parser cannot read
// is a script the engine cannot read either, however valid its JSON.
func envelopeFrom(t *testing.T, script string, args ...string) (deploycontrol.CapabilityResult, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", append([]string{aksScript(t, script)}, args...)...)
	// STDOUT ONLY. The contract puts human logs on stderr and exactly one JSON
	// envelope on stdout, and CombinedOutput would mix them -- which is
	// precisely the mistake the split exists to make impossible.
	stdout, err := cmd.Output()
	code := 0
	var exitErr *exec.ExitError
	if err != nil {
		if !asExitError(err, &exitErr) {
			t.Fatalf("running %s: %v", script, err)
		}
		code = exitErr.ExitCode()
	}
	env, perr := deploycontrol.ParseCapabilityResult(stdout)
	if perr != nil {
		t.Fatalf("%s emitted no readable envelope on stdout (exit %d): %v\nstdout was:\n%s",
			script, code, perr, string(stdout))
	}
	return env, code
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// ===========================================================================
// The refusal that is not a failure (design D7, task memql#4803)
// ===========================================================================

// NO ISSUER -> EXIT 3 WITH THE TYPED REASON IN THE ENVELOPE. This is the answer
// a local k3d cluster gets on every pass, forever, and it is correct there: a
// Certificate with an empty issuerRef is ACCEPTED by the API server and then
// sits Pending with a condition nobody reads, which is a pretend success.
//
// The exit code says "refused" and not WHICH refusal, so the reason is on the
// RESULT as well -- that is the value the row records and the Domains panel
// renders.
func TestBindRefusesWithNoIssuer(t *testing.T) {
	env, code := envelopeFrom(t, bindScript, "--hostname=www.acme.com", "--domainId=d1")
	if code != 3 {
		t.Errorf("exit = %d, want 3 (refused)", code)
	}
	if env.OK {
		t.Error("the envelope reports ok on a refusal")
	}
	if env.Error == nil || env.Error.Code != 3 {
		t.Errorf("envelope error = %+v, want code 3", env.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("result: %v", err)
	}
	if result["reason"] != "no_acme_issuer" {
		t.Errorf("result.reason = %v, want no_acme_issuer -- the exit code alone does not say WHICH refusal, "+
			"and the panel keys on the typed value", result["reason"])
	}
	if detail, _ := result["detail"].(string); !strings.Contains(detail, "www.acme.com") {
		t.Errorf("result.detail = %q, does not name the hostname", detail)
	}
}

// ===========================================================================
// Bad parameters (exit 2)
// ===========================================================================

func TestBindRefusesBadParameters(t *testing.T) {
	cases := map[string][]string{
		"no hostname":  {"--domainId=d1", "--issuer=le"},
		"no domainId":  {"--hostname=www.acme.com", "--issuer=le"},
		"single label": {"--hostname=acme", "--domainId=d1", "--issuer=le"},
		// A wildcard is refused here as well as in the engine's own guard,
		// and that is not redundancy: the script is also an operator's manual
		// path, and ACME failing the WHOLE order on one wildcard dnsName
		// (memql#4224) is not something to discover from a rate limit.
		"wildcard":         {"--hostname=*.acme.com", "--domainId=d1", "--issuer=le"},
		"non-numeric port": {"--hostname=www.acme.com", "--domainId=d1", "--issuer=le", "--port=eighty"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			env, code := envelopeFrom(t, bindScript, args...)
			if code != 2 {
				t.Errorf("exit = %d, want 2 (bad param)", code)
			}
			if env.Error == nil || env.Error.Code != 2 {
				t.Errorf("envelope error = %+v, want code 2", env.Error)
			}
			if env.Error != nil && strings.TrimSpace(env.Error.Message) == "" {
				t.Error("a refusal with an empty message tells a caller nothing")
			}
		})
	}
}

func TestUnbindRequiresTheDomainId(t *testing.T) {
	env, code := envelopeFrom(t, unbindScript, "--hostname=www.acme.com")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "domainId") {
		t.Errorf("the refusal does not name the missing parameter: %+v", env.Error)
	}
}

// ===========================================================================
// The spec surface
// ===========================================================================

// --print-spec is the contract surface a caller reads WITHOUT running
// anything, which is how a wizard discovers a required input rather than
// finding out from an exit 2 partway through (memql#3568).
func TestBothScriptsDeclareTheirRequiredParameters(t *testing.T) {
	for script, wantRequired := range map[string][]string{
		bindScript:   {"hostname", "domainId"},
		unbindScript: {"domainId"},
	} {
		out, err := runAks(t, script, "--print-spec")
		if err != nil {
			t.Fatalf("%s --print-spec: %v\n%s", script, err, out)
		}
		var spec struct {
			Capability string `json:"capability"`
			Params     []struct {
				Name     string `json:"name"`
				Required bool   `json:"required"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &spec); err != nil {
			t.Fatalf("%s spec is not JSON: %v\n%s", script, err, out)
		}
		required := map[string]bool{}
		for _, p := range spec.Params {
			if p.Required {
				required[p.Name] = true
			}
		}
		for _, name := range wantRequired {
			if !required[name] {
				t.Errorf("%s does not declare %q required, so a caller cannot discover it without running the script", script, name)
			}
		}
	}
}

// The capability ids the scripts declare must be the ids the engine's
// allowlist resolves. A mismatch is silently inert on the engine path -- the
// script runs fine from a shell and the runner rejects its id before exec --
// which is exactly the failure the allowlist's own comment warns about.
func TestScriptCapabilityIdsMatchTheAllowlist(t *testing.T) {
	for script, want := range map[string]string{
		bindScript:   "domain.bind",
		unbindScript: "domain.unbind",
	} {
		out, err := runAks(t, script, "--print-spec")
		if err != nil {
			t.Fatalf("%s --print-spec: %v", script, err)
		}
		var spec struct {
			Capability string `json:"capability"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &spec); err != nil {
			t.Fatalf("%s spec is not JSON: %v", script, err)
		}
		if spec.Capability != want {
			t.Errorf("%s declares capability %q, want %q", script, spec.Capability, want)
		}
	}
}

// ===========================================================================
// The rendered objects
// ===========================================================================

// A dry run renders and checks WITHOUT touching a cluster, and without kubectl.
//
// It used to shell out to `kubectl apply --dry-run=client`, which passed here
// (a k3d cluster was up) and failed on every CI runner: a "client" dry run
// still fetches the API server's OpenAPI schema, and `--validate=false` does
// not help because `apply` needs discovery to map a kind to a resource either
// way. So the test was measuring the developer's cluster rather than the
// script. The check is now the one every machine can make -- the render
// carries both documents and the hostname -- and this test needs no
// prerequisite at all.
func TestBindDryRunRendersValidObjects(t *testing.T) {
	env, code := envelopeFrom(t, bindScript,
		"--hostname=www.acme.com", "--domainId=d1", "--siteId=site1",
		"--issuer=letsencrypt-prod", "--dryRun=true")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; error = %+v", code, env.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("result: %v", err)
	}
	// The object name is keyed on the ROW ID, and it must agree with
	// objectName() in integrations/customdomain/provision.go -- the two
	// substrates apply the SAME two objects, so a disagreement would have them
	// creating one each and neither cleaning up the other's.
	if result["objectName"] != "custom-domain-d1" {
		t.Errorf("objectName = %v, want custom-domain-d1 (must match integrations/customdomain's objectName)", result["objectName"])
	}
	if result["certificateReady"] != false {
		t.Errorf("certificateReady = %v on a dry run; nothing was requested, so it cannot be ready", result["certificateReady"])
	}
}

// ===========================================================================
// The error pages (after the 2026-10-11 outage)
// ===========================================================================
//
// The engine's own substrate writes these objects through
// integrations/customdomain, and this script is the operator's path to the SAME
// objects (provision.go: "both write the same two objects ... so a domain
// bound by one and re-reconciled by the other converges rather than fighting").
// So the annotation that sends an Ingress to `memql error-pages` when its
// Service has no ready endpoints has to be here too, under the same rule: only
// an Ingress whose proxy speaks plain HTTP carries it.
//
// The key and the Service name are literals below, ingress-nginx's contract and
// the base manifest's name, and not read from the code under test; the equality
// with the engine's constants is TestBothBindScriptsUseTheEnginesErrorPages.

const (
	scriptDefaultBackendKey  = "nginx.ingress.kubernetes.io/default-backend"
	scriptBackendProtocolKey = "nginx.ingress.kubernetes.io/backend-protocol"
	scriptCustomErrorsKey    = "nginx.ingress.kubernetes.io/custom-http-errors"
	scriptErrorPagesService  = "error-pages"
)

// scriptIngress is the slice of a rendered Ingress these tests read.
type scriptIngress struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
}

// ingressesOfManifest decodes every Ingress of a multi-document manifest. A
// decode error is fatal: a truncated prefix would leave every assertion passing
// over a fraction of the objects.
func ingressesOfManifest(t *testing.T, manifest string) []scriptIngress {
	t.Helper()
	var out []scriptIngress
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for i := 1; ; i++ {
		var d scriptIngress
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("decoding document %d of the rendered manifest: %v", i, err)
		}
		if d.Kind == "Ingress" {
			out = append(out, d)
		}
	}
}

// appliedByTheBindScript runs the REAL bind script with a stub kubectl that
// records what is piped to `apply`, so the test reads the objects the script
// would have applied without reaching a cluster (the stub certificate_owner_test
// uses, for the same reason: bind-custom-domain.sh has no --renderTo).
func appliedByTheBindScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	captured := filepath.Join(dir, "applied.yaml")
	stub := "#!/bin/bash\nfunction main() {\n  if [[ \"$1\" == apply ]]; then cat > \"$ERROR_PAGES_TEST_CAPTURE\"; echo configured; fi\n}\nmain \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ERROR_PAGES_TEST_CAPTURE", captured)

	env, code := envelopeFrom(t, bindScript,
		"--hostname=www.acme.com", "--domainId=d1", "--siteId=site1", "--issuer=letsencrypt-prod")
	if code != 0 || !env.OK {
		t.Fatalf("bind failed: exit %d, error %+v", code, env.Error)
	}
	raw, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("the script applied nothing: %v", err)
	}
	return string(raw)
}

// A client's own domain reaches the edge over plain HTTP, so its Ingress falls
// back to the error pages -- and it is the SAME annotation set the engine's
// IngressObject writes, so a domain bound from a shell and one bound by the
// reconciler are one object.
func TestBindFallsBackToTheErrorPages(t *testing.T) {
	ingresses := ingressesOfManifest(t, appliedByTheBindScript(t))
	if len(ingresses) != 1 {
		t.Fatalf("the script applied %d Ingress(es), want exactly 1", len(ingresses))
	}
	annotations := ingresses[0].Metadata.Annotations

	if got := annotations[scriptDefaultBackendKey]; got != scriptErrorPagesService {
		t.Errorf("the Ingress carries %s=%q, want %q: with no ready edge pod its visitors would get "+
			"ingress-nginx's bare 503", scriptDefaultBackendKey, got, scriptErrorPagesService)
	}
	if _, ok := annotations[scriptCustomErrorsKey]; ok {
		t.Errorf("the Ingress sets %s, which would replace the application's own error responses with the maintenance page", scriptCustomErrorsKey)
	}
	if v, ok := annotations[scriptBackendProtocolKey]; ok {
		t.Errorf("the Ingress sets %s=%q; the edge is plain HTTP, and the error pages only stand behind plain HTTP", scriptBackendProtocolKey, v)
	}

	// The engine's builder, same inputs (the script's defaults are the engine's).
	goIngress := customdomain.IngressObject(customdomain.BindRequest{
		Hostname: "www.acme.com", DomainID: "d1", SiteID: "site1", Namespace: "memql",
		Issuer: "letsencrypt-prod", IngressClass: "nginx", Service: "edge", Port: 8085,
	})
	asStrings := map[string]string{}
	for k, v := range goIngress["metadata"].(map[string]any)["annotations"].(map[string]any) {
		asStrings[k], _ = v.(string)
	}
	if !reflect.DeepEqual(asStrings, annotations) {
		t.Errorf("the script's Ingress annotations %v differ from integrations/customdomain.IngressObject's %v: "+
			"the two substrates must write the same object", annotations, asStrings)
	}
}

// The script's literal is a second spelling of the engine's two constants, held
// to them here the way the three host labels and the worker-stream prefix are
// held to component/frontdoor: read out of the shell source, not restated.
func TestBothBindScriptsUseTheEnginesErrorPages(t *testing.T) {
	want := "readonly ERROR_PAGES_BACKEND='" + customdomain.ErrorPagesBackendAnnotation + ": " + customdomain.ErrorPagesServiceName + "'"
	for _, script := range []string{bindScript, bindDoorScript} {
		raw, err := os.ReadFile(aksScript(t, script))
		if err != nil {
			t.Fatalf("reading %s: %v", script, err)
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s does not declare %q; integrations/customdomain.ErrorPagesBackendAnnotation and "+
				"ErrorPagesServiceName are the one spelling the engine writes, and a domain bound from a "+
				"shell must fall back to the same Service", script, want)
		}
	}
}

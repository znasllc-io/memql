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
	"os/exec"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/deploycontrol"
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

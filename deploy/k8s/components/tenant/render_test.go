package tenant

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// capabilityEnvelope is the one JSON object every capability script writes to
// stdout (docs/internal/design/capability-script-contract.md).
type capabilityEnvelope struct {
	OK         bool           `json:"ok"`
	Capability string         `json:"capability"`
	Changed    bool           `json:"changed"`
	Result     map[string]any `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// runScript executes one of the fleet capability scripts and parses its
// envelope. Stdin is closed, as the action executor closes it.
func runScript(t *testing.T, script string, args ...string) (capabilityEnvelope, int) {
	t.Helper()
	path := filepath.Join(repoRoot(t), "scripts", "fleet", script)
	cmd := exec.Command(path, args...)
	cmd.Stdin = nil
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()

	code := 0
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s: %v", script, err)
	}

	var env capabilityEnvelope
	if uerr := json.Unmarshal(out, &env); uerr != nil {
		t.Fatalf("%s did not write one JSON envelope to stdout (%v):\n%s", script, uerr, out)
	}
	return env, code
}

// placeholder matches the __TOKEN__ form the templates use.
var placeholder = regexp.MustCompile(`__[A-Z0-9_]+__`)

// TestProvisionSubstitutesEveryPlaceholder is the drift gate between
// template/ and tenant-provision.sh.
//
// The two are a pair: the templates declare the tokens, the script supplies the
// values. Adding a token to a template and forgetting the substitution does not
// fail the render -- it produces an overlay containing a literal `__FOO__`,
// which kustomize will happily accept as a namespace, an image tag or a domain.
// The failure then lands as a tenant that reconciles green at a hostname
// nothing resolves.
//
// So this test does not enumerate the tokens. It reads whatever the templates
// currently declare and asserts none of them survive into the render, which
// stays true as tokens are added.
func TestProvisionSubstitutesEveryPlaceholder(t *testing.T) {
	tmpl := filepath.Join(repoRoot(t), "deploy", "k8s", "components", "tenant", "template")
	entries, err := os.ReadDir(tmpl)
	if err != nil {
		t.Fatalf("read template dir: %v", err)
	}
	declared := map[string]bool{}
	for _, e := range entries {
		b, rerr := os.ReadFile(filepath.Join(tmpl, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		for _, tok := range placeholder.FindAllString(string(b), -1) {
			declared[tok] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("the tenant templates declare no placeholders -- either they stopped being templates or the pattern stopped matching, and either way this gate is no longer watching anything")
	}

	out := t.TempDir()
	env, code := runScript(t, "tenant-provision.sh",
		"--tenant=acme", "--domain=acme.memql.cloud",
		"--profile=solo", "--dbPreset=entry",
		"--engineTag=v1.2.3", "--dbImageTag=16.4-ts",
		"--outputRoot="+out)
	if code != 0 || !env.OK {
		t.Fatalf("provision failed: exit %d, %+v", code, env.Error)
	}

	var checked int
	err = filepath.Walk(out, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return werr
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		checked++
		if found := placeholder.FindAllString(string(b), -1); len(found) > 0 {
			rel, _ := filepath.Rel(out, path)
			t.Errorf("%s still contains %v after rendering -- tenant-provision.sh has no substitution for it, so this overlay would reconcile with a literal token where a value belongs", rel, found)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk rendered tree: %v", err)
	}
	if checked == 0 {
		t.Fatal("the render produced no files")
	}
}

// TestProvisionIsIdempotent pins the property the caller depends on: the
// provisioning automation has at-least-once delivery, so a redelivered event
// must not produce a second tenant.
//
// `changed` is the signal, and it has to be honest in both directions -- a
// script that always reports true makes every replay look like work, and one
// that always reports false hides a real re-render.
func TestProvisionIsIdempotent(t *testing.T) {
	out := t.TempDir()
	args := []string{
		"--tenant=acme", "--domain=acme.memql.cloud",
		"--profile=standard", "--dbPreset=mid",
		"--outputRoot=" + out,
	}

	first, code := runScript(t, "tenant-provision.sh", args...)
	if code != 0 || !first.OK {
		t.Fatalf("first provision failed: exit %d, %+v", code, first.Error)
	}
	if !first.Changed {
		t.Error("the first provision reported changed=false; it rendered a tenant that did not exist")
	}

	second, code := runScript(t, "tenant-provision.sh", args...)
	if code != 0 || !second.OK {
		t.Fatalf("second provision failed: exit %d, %+v", code, second.Error)
	}
	if second.Changed {
		t.Error("re-provisioning unchanged inputs reported changed=true -- a redelivered event would read as new work")
	}
}

// TestHaComposesOnlyOntoSolo pins the one branch in tenant-provision.sh.
//
// It is a MECHANICAL branch, which is what the capability-script contract
// permits: whether the tenant has HA was decided by the caller from the tier's
// haIncluded and the subscription's haAddOn. What the script decides is only
// whether composing the component would be a no-op.
func TestHaComposesOnlyOntoSolo(t *testing.T) {
	const haLine = "components/tenant/optional/ha"

	for _, tc := range []struct {
		profile string
		ha      string
		want    bool
	}{
		{profile: "solo", ha: "true", want: true},
		{profile: "solo", ha: "false", want: false},
		// From Graph up HA is in the price and the profile preset carries it;
		// composing the add-on there would be a no-op implying otherwise.
		{profile: "standard", ha: "true", want: false},
		{profile: "dedicated", ha: "true", want: false},
	} {
		t.Run(tc.profile+"/ha="+tc.ha, func(t *testing.T) {
			out := t.TempDir()
			env, code := runScript(t, "tenant-provision.sh",
				"--tenant=acme", "--domain=acme.memql.cloud",
				"--profile="+tc.profile, "--dbPreset=entry",
				"--ha="+tc.ha, "--outputRoot="+out)
			if code != 0 || !env.OK {
				t.Fatalf("provision failed: exit %d, %+v", code, env.Error)
			}

			b, err := os.ReadFile(filepath.Join(out, "deploy", "k8s", "tenants", "acme", "kustomization.yaml"))
			if err != nil {
				t.Fatalf("read rendered overlay: %v", err)
			}
			got := strings.Contains(string(b), haLine)
			if got != tc.want {
				t.Errorf("rendered overlay composes the HA component = %v, want %v", got, tc.want)
			}
			if env.Result["haComposed"] != tc.want {
				t.Errorf("envelope reports haComposed=%v, want %v -- the caller reads this to stamp the instance row", env.Result["haComposed"], tc.want)
			}
		})
	}
}

// TestProvisionRefusesUnusableTenantNames.
//
// The slug is a Kubernetes namespace, an ArgoCD Application name AND a DNS
// label at once, so it is validated as the strictest of the three, up front.
// A slug that is a valid namespace but an invalid DNS label produces a tenant
// that reconciles green and is unreachable, with nothing in the cluster to say
// why -- and by then it has a customer on it.
func TestProvisionRefusesUnusableTenantNames(t *testing.T) {
	for _, name := range []string{
		"Acme",        // uppercase: not a DNS label
		"9lives",      // leading digit
		"acme_corp",   // underscore
		"acme-",       // trailing hyphen
		"memql-prod",  // a real namespace on the cluster it would land in
		"argocd",      // ditto
		"kube-system", // ditto
	} {
		t.Run(name, func(t *testing.T) {
			env, code := runScript(t, "tenant-provision.sh",
				"--tenant="+name, "--domain=x.memql.cloud",
				"--profile=solo", "--dbPreset=entry",
				"--outputRoot="+t.TempDir())
			if code != 2 {
				t.Errorf("exit %d for tenant %q, want 2 (bad param)", code, name)
			}
			if env.OK {
				t.Errorf("tenant %q was accepted", name)
			}
		})
	}
}

// TestProvisionRefusesUnknownProfilesAndPresets. Both values select a directory
// that has to exist; an unknown one renders an overlay whose `components:` names
// a path that is not there, which fails at reconcile rather than at render --
// far from the operator who typed it.
func TestProvisionRefusesUnknownProfilesAndPresets(t *testing.T) {
	for _, tc := range []struct{ profile, preset string }{
		{profile: "tiny", preset: "entry"},
		{profile: "solo", preset: "enormous"},
	} {
		_, code := runScript(t, "tenant-provision.sh",
			"--tenant=acme", "--domain=x.memql.cloud",
			"--profile="+tc.profile, "--dbPreset="+tc.preset,
			"--outputRoot="+t.TempDir())
		if code != 2 {
			t.Errorf("profile=%s preset=%s: exit %d, want 2", tc.profile, tc.preset, code)
		}
	}
}

// TestTeardownRefusesWithoutTheExactPhrase.
//
// Teardown is the only irreversible operation in the fleet. The phrase includes
// the tenant name deliberately: a confirmation copy-pasted from the last
// teardown does not authorise this one. Exit 3 (refused) rather than 2 (bad
// param), because the parameters are well-formed and the operation was declined.
func TestTeardownRefusesWithoutTheExactPhrase(t *testing.T) {
	for _, confirm := range []string{"", "yes", "teardown", "teardown other-tenant", "TEARDOWN acme"} {
		env, code := runScript(t, "tenant-teardown.sh", "--tenant=acme", "--confirm="+confirm)
		if code != 3 {
			t.Errorf("confirm=%q: exit %d, want 3 (refused)", confirm, code)
		}
		if env.OK {
			t.Errorf("confirm=%q was accepted", confirm)
		}
	}

	// The exact phrase gets through to the dry run, which contacts nothing.
	env, code := runScript(t, "tenant-teardown.sh", "--tenant=acme", "--confirm=teardown acme")
	if code != 0 || !env.OK {
		t.Fatalf("the exact phrase was refused: exit %d, %+v", code, env.Error)
	}
	if env.Result["dryRun"] != true {
		t.Error("teardown ran for real without --dryRun=false; the default must be safe")
	}
}

// TestLifecycleScriptsDefaultToDryRun.
//
// Every one of these touches a live cluster, and three of the four are reached
// from an automation. A default that acts is a default that acts on the first
// mis-wired trigger, so the safe value is the one you get by omitting the flag.
func TestLifecycleScriptsDefaultToDryRun(t *testing.T) {
	for _, tc := range []struct {
		script string
		args   []string
	}{
		{script: "tenant-suspend.sh", args: []string{"--tenant=acme"}},
		{script: "tenant-resume.sh", args: []string{"--tenant=acme"}},
		{script: "tenant-teardown.sh", args: []string{"--tenant=acme", "--confirm=teardown acme"}},
	} {
		t.Run(tc.script, func(t *testing.T) {
			env, code := runScript(t, tc.script, tc.args...)
			if code != 0 || !env.OK {
				t.Fatalf("exit %d, %+v", code, env.Error)
			}
			if env.Result["dryRun"] != true {
				t.Errorf("%s did not default to a dry run", tc.script)
			}
		})
	}
}

// TestResumeRefusesZeroInstances. "Resume to zero instances" is a suspend, and
// a resume that quietly performs one leaves the fleet believing the tenant is
// up: the instance row moves to `running` on a zero exit, and the customer's
// mesh crash-loops against a database that is not there.
func TestResumeRefusesZeroInstances(t *testing.T) {
	for _, v := range []string{"0", "-1", "two", "1.5"} {
		_, code := runScript(t, "tenant-resume.sh", "--tenant=acme", "--dbInstances="+v)
		if code != 2 {
			t.Errorf("dbInstances=%q: exit %d, want 2", v, code)
		}
	}
}

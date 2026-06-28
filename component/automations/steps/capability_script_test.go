package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/actions"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql/callgraph"
)

// writeFixtureScript drops a tiny capability-style script into dir that echoes
// a single JSON result envelope on stdout, reflecting the --foo flag it was
// handed. It proves the runner dispatch + envelope parse without any real
// deploy tooling.
func writeFixtureScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	// The runner resolves the root by finding scripts/lib/capability.sh; make
	// the temp dir look like a repo root so resolveRoot is satisfied too.
	if err := os.MkdirAll(filepath.Join(dir, "scripts", "lib"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "lib", "capability.sh"), []byte("# marker\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir script: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return p
}

const echoEnvelopeScript = `#!/usr/bin/env bash
set -euo pipefail
foo=""
for a in "$@"; do
  case "$a" in --foo=*) foo="${a#*=}";; esac
done
# human log on stderr; the one JSON envelope on stdout
echo "running echo fixture" >&2
printf '{"ok":true,"capability":"test.echo","changed":true,"result":{"foo":"%s"},"error":null}\n' "$foo"
`

const failEnvelopeScript = `#!/usr/bin/env bash
set -euo pipefail
echo "boom" >&2
printf '{"ok":false,"capability":"test.fail","changed":false,"result":{},"error":{"code":5,"message":"op failed"}}\n'
exit 5
`

// TestCapabilityScriptRunnerDispatch is the acceptance case for the runner: a
// shell.script action's rendered args dispatch to a NAMED allowlisted script,
// pass params as argv flags, and the JSON envelope is parsed back.
func TestCapabilityScriptRunnerDispatch(t *testing.T) {
	dir := t.TempDir()
	writeFixtureScript(t, dir, "fixture/echo.sh", echoEnvelopeScript)

	r := &capabilityScriptRunner{root: dir, allow: map[string]string{"test.echo": "fixture/echo.sh"}}
	out, err := r.Invoke(context.Background(), map[string]any{
		"script": "test.echo",
		"foo":    "bar",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result type %T", out)
	}
	if m["ok"] != true || m["changed"] != true {
		t.Errorf("envelope flags = ok:%v changed:%v", m["ok"], m["changed"])
	}
	if m["capability"] != "test.echo" {
		t.Errorf("capability = %v", m["capability"])
	}
	res, ok := m["result"].(map[string]any)
	if !ok || res["foo"] != "bar" {
		t.Errorf("result = %#v (param did not flow as a flag)", m["result"])
	}
}

// TestCapabilityScriptRunnerRejectsUnlisted proves the security boundary: an id
// that is not in the allowlist is refused -- a shell action can only run a
// registered capability script, never an arbitrary path.
func TestCapabilityScriptRunnerRejectsUnlisted(t *testing.T) {
	dir := t.TempDir()
	writeFixtureScript(t, dir, "fixture/echo.sh", echoEnvelopeScript)
	r := &capabilityScriptRunner{root: dir, allow: map[string]string{"test.echo": "fixture/echo.sh"}}

	if _, err := r.Invoke(context.Background(), map[string]any{"script": "../../etc/evil"}); err == nil {
		t.Fatal("expected an allowlist rejection for an unregistered script id")
	}
	if _, err := r.Invoke(context.Background(), map[string]any{"foo": "bar"}); err == nil {
		t.Fatal("expected an error when the reserved \"script\" arg is missing")
	}
}

// TestCapabilityScriptRunnerSurfacesFailureEnvelope asserts a script that emits
// ok=false (and a non-zero exit) surfaces the structured error, not a parse
// failure.
func TestCapabilityScriptRunnerSurfacesFailureEnvelope(t *testing.T) {
	dir := t.TempDir()
	writeFixtureScript(t, dir, "fixture/fail.sh", failEnvelopeScript)
	r := &capabilityScriptRunner{root: dir, allow: map[string]string{"test.fail": "fixture/fail.sh"}}

	_, err := r.Invoke(context.Background(), map[string]any{"script": "test.fail"})
	if err == nil {
		t.Fatal("expected the failure envelope to surface as an error")
	}
	if !strings.Contains(err.Error(), "op failed") || !strings.Contains(err.Error(), "exit 5") {
		t.Errorf("error did not carry the structured failure: %v", err)
	}
}

// TestCapabilityScriptRunnerInjectionInert proves a param value with shell
// metacharacters is inert -- it flows as literal data through the argv flag,
// never re-parsed by a shell.
func TestCapabilityScriptRunnerInjectionInert(t *testing.T) {
	dir := t.TempDir()
	writeFixtureScript(t, dir, "fixture/echo.sh", echoEnvelopeScript)
	r := &capabilityScriptRunner{root: dir, allow: map[string]string{"test.echo": "fixture/echo.sh"}}

	out, err := r.Invoke(context.Background(), map[string]any{
		"script": "test.echo",
		"foo":    "x; echo PWNED",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	res := out.(map[string]any)["result"].(map[string]any)
	if res["foo"] != "x; echo PWNED" {
		t.Errorf("metacharacter value was not literal: %#v", res["foo"])
	}
}

// --- authored deploy actions: load/validate + replay via the real runner ----

// deployActionsSource reads the authored deploy actions file from the repo.
func deployActionsSource(t *testing.T) (string, string) {
	t.Helper()
	root := repoRootFromTest(t)
	p := filepath.Join(root, "dsl", "deployment", "actions.memql")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read actions.memql: %v", err)
	}
	return string(b), "dsl/deployment/actions.memql"
}

// repoRootFromTest walks up from the working directory for the repo root.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "lib", "capability.sh")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root from test")
		}
		dir = parent
	}
}

// TestDeployActionsLoadAndValidate is the action load/validate acceptance: every
// authored deploy action parses, loads into the registry (so @sideEffect
// reconciles against the capability class), and passes the I7 action-rules
// validator (exactly one external capability; no construct calls; no graph; no
// @trigger).
func TestDeployActionsLoadAndValidate(t *testing.T) {
	src, path := deployActionsSource(t)

	acts, err := actions.LoadSource(src, path)
	if err != nil {
		t.Fatalf("LoadSource (load/validate failed): %v", err)
	}
	want := []string{
		"cloneRepoAtVersion", "buildEngineImage", "importImageToK3d",
		"pinOverlayDigests", "argoSync", "runDeployGate", "revertOverlay",
		"notifyDeploy", "tagRelease",
	}
	got := map[string]*actions.Action{}
	for _, a := range acts {
		got[a.Name] = a
	}
	for _, n := range want {
		if got[n] == nil {
			t.Errorf("deploy action %q did not load", n)
		}
	}

	// The capability-script runner backs every shell action by an ALLOWLISTED
	// script. A shell.script action whose `script` arg is not registered would
	// never dispatch -- assert each one's id is in the allowlist.
	for _, a := range acts {
		if a.Capability != "shell.script" {
			continue
		}
		var scriptID string
		for _, e := range a.ArgTemplate {
			if e.Key == "script" {
				scriptID = strings.Trim(e.Template, `"`)
			}
		}
		if scriptID == "" {
			t.Errorf("shell action %q renders no \"script\" arg", a.Name)
			continue
		}
		if _, ok := capabilityScriptAllowlist[scriptID]; !ok {
			t.Errorf("shell action %q names capability-script %q which is not in the allowlist", a.Name, scriptID)
		}
	}

	// I7 action-rules validator: no findings on the authored file.
	if fs := callgraph.CheckFile(path, src, nil); len(fs) != 0 {
		msgs := make([]string, 0, len(fs))
		for _, f := range fs {
			msgs = append(msgs, f.Rule+": "+f.Message)
		}
		t.Fatalf("deploy actions must pass the action-rules validator; got:\n%s", strings.Join(msgs, "\n"))
	}
}

// TestDeployActionRunsAndReplays is the cockpit-surface acceptance: an authored
// deploy action runs via an automation action() step through the REAL
// capability-script runner against a no-op/dry-run target, and an identical
// input replays with a matching result fingerprint (token-free).
func TestDeployActionRunsAndReplays(t *testing.T) {
	src, path := deployActionsSource(t)
	acts, err := actions.LoadSource(src, path)
	if err != nil {
		t.Fatalf("LoadSource: %v", err)
	}
	reg := actions.NewRegistry()
	for _, a := range acts {
		// Origin under deployment/ so the policy-default surface (cockpit/runner)
		// is inherited exactly as in the embedded tree.
		a.Origin = path + ":" + a.Name
		if err := reg.Register(a); err != nil {
			t.Fatalf("Register %q: %v", a.Name, err)
		}
	}

	// No injected Dispatcher -> the executor builds the default dispatcher with
	// the real capability-script runner (root resolved from the working dir).
	exec := &ActionExecutor{Registry: reg}

	run := func() *automations.StepResult {
		step := &automations.Step{
			ID:   "clone",
			Type: automations.StepTypeAction,
			Action: &automations.ActionStepConfig{
				Ref:  "cloneRepoAtVersion@1",
				Args: map[string]any{"workdir": "/work/memql", "ref": "v9.9.9"},
			},
		}
		res, rerr := exec.Execute(context.Background(), step, &Context{})
		if rerr != nil {
			t.Fatalf("Execute: %v", rerr)
		}
		return res
	}

	r1 := run()
	if r1.Status != "success" {
		t.Fatalf("status = %q (err=%q)", r1.Status, r1.Error)
	}
	out := r1.Result.(map[string]any)
	if out["capability"] != "shell.script" {
		t.Errorf("capability = %v", out["capability"])
	}
	if out["surface"] != "cockpit/runner" {
		t.Errorf("surface = %v (deploy actions must resolve to cockpit/runner)", out["surface"])
	}
	// The dry-run clone is a deterministic no-op envelope.
	envelope, ok := out["result"].(map[string]any)
	if !ok || envelope["ok"] != true {
		t.Fatalf("capability result = %#v", out["result"])
	}
	inner, _ := envelope["result"].(map[string]any)
	if inner["ref"] != "v9.9.9" || inner["dryRun"] != true {
		t.Errorf("dry-run clone result = %#v", envelope["result"])
	}

	// Replay: identical input -> identical fingerprint, no LLM, no token.
	r2 := run()
	if r1.Result.(map[string]any)["resultFingerprint"] != r2.Result.(map[string]any)["resultFingerprint"] {
		t.Errorf("result fingerprints differ across replay: %v vs %v",
			r1.Result.(map[string]any)["resultFingerprint"],
			r2.Result.(map[string]any)["resultFingerprint"])
	}
}

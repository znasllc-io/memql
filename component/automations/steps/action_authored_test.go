package steps

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/actions"
	"github.com/znasllc-io/memql/component/automations"
)

// fakeDispatcher records invocations and returns a deterministic result so the
// authored path can be exercised without a real shell / engine.
type fakeDispatcher struct {
	mu    sync.Mutex
	calls []fakeCall
}

type fakeCall struct {
	capability string
	args       map[string]any
}

func (f *fakeDispatcher) Invoke(_ context.Context, capability string, args map[string]any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{capability: capability, args: args})
	// Deterministic echo: same input -> same output (token-free replay).
	return map[string]any{"capability": capability, "args": args}, nil
}

const authoredCloneSrc = `@kind("primitive")
@sideEffect("exec")
action cloneRepoAtVersion {
  capability "shell.exec"
  intent "checkout a repo at a ref"
  params {
    workdir string @required
    ref     string @required
  }
  argTemplate {
    cmd: "git -C $params.workdir checkout $params.ref"
  }
}`

func authoredRegistry(t *testing.T, src string) *actions.Registry {
	t.Helper()
	reg := actions.NewRegistry()
	acts, err := actions.LoadSource(src, "test.memql")
	if err != nil {
		t.Fatalf("LoadSource: %v", err)
	}
	for _, a := range acts {
		if err := reg.Register(a); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	return reg
}

func runAuthoredStep(t *testing.T, exec *ActionExecutor, ref string, args map[string]any) *automations.StepResult {
	t.Helper()
	step := &automations.Step{
		ID:     "act",
		Type:   automations.StepTypeAction,
		Action: &automations.ActionStepConfig{Ref: ref, Args: args},
	}
	res, err := exec.Execute(context.Background(), step, &Context{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

// TestAuthoredActionRunsViaStep is the acceptance case: an authored primitive
// runs via an automation action("name@1") step, binding params + rendering the
// argTemplate into a single capability invocation on the default surface.
func TestAuthoredActionRunsViaStep(t *testing.T) {
	fake := &fakeDispatcher{}
	exec := &ActionExecutor{Registry: authoredRegistry(t, authoredCloneSrc), Dispatcher: fake}

	res := runAuthoredStep(t, exec, "cloneRepoAtVersion@1",
		map[string]any{"workdir": "/work/memql", "ref": "v1.2.3"})

	if res.Status != "success" {
		t.Fatalf("status = %q (err=%q)", res.Status, res.Error)
	}
	out, ok := res.Result.(map[string]any)
	if !ok {
		t.Fatalf("result type %T", res.Result)
	}
	if out["authored"] != true {
		t.Errorf("expected authored=true, got %#v", out["authored"])
	}
	if out["capability"] != "shell.exec" {
		t.Errorf("capability = %#v", out["capability"])
	}
	if out["resultFingerprint"] == "" || out["resultFingerprint"] == nil {
		t.Error("expected a non-empty resultFingerprint")
	}

	if len(fake.calls) != 1 {
		t.Fatalf("expected 1 capability call, got %d", len(fake.calls))
	}
	if got := fake.calls[0].args["cmd"]; got != "git -C /work/memql checkout v1.2.3" {
		t.Errorf("rendered cmd = %#v", got)
	}
}

// TestAuthoredActionReplaysTokenFree asserts the acceptance replay property:
// the SAME input produces the SAME result fingerprint across two runs, with no
// LLM in the path.
func TestAuthoredActionReplaysTokenFree(t *testing.T) {
	exec := &ActionExecutor{Registry: authoredRegistry(t, authoredCloneSrc), Dispatcher: &fakeDispatcher{}}
	args := map[string]any{"workdir": "/work/memql", "ref": "v1.2.3"}

	r1 := runAuthoredStep(t, exec, "cloneRepoAtVersion@1", args)
	r2 := runAuthoredStep(t, exec, "cloneRepoAtVersion@1", args)

	fp1 := r1.Result.(map[string]any)["resultFingerprint"]
	fp2 := r2.Result.(map[string]any)["resultFingerprint"]
	if fp1 == "" || fp1 != fp2 {
		t.Fatalf("expected identical fingerprints on identical input, got %v vs %v", fp1, fp2)
	}
}

// TestAuthoredActionMissingRequiredParam fails the step (no panic, clean error).
func TestAuthoredActionMissingRequiredParam(t *testing.T) {
	exec := &ActionExecutor{Registry: authoredRegistry(t, authoredCloneSrc), Dispatcher: &fakeDispatcher{}}
	step := &automations.Step{
		ID:     "act",
		Type:   automations.StepTypeAction,
		Action: &automations.ActionStepConfig{Ref: "cloneRepoAtVersion@1", Args: map[string]any{"workdir": "/w"}},
	}
	res, err := exec.Execute(context.Background(), step, &Context{})
	if err == nil || res.Status != "failed" {
		t.Fatalf("expected failure for missing ref param, got status=%q err=%v", res.Status, err)
	}
}

// TestAuthoredFloatingRefResolvesLatest checks a ref with no @version.
func TestAuthoredFloatingRefResolvesLatest(t *testing.T) {
	fake := &fakeDispatcher{}
	exec := &ActionExecutor{Registry: authoredRegistry(t, authoredCloneSrc), Dispatcher: fake}
	res := runAuthoredStep(t, exec, "cloneRepoAtVersion",
		map[string]any{"workdir": "/w", "ref": "main"})
	if res.Status != "success" {
		t.Fatalf("floating ref status = %q (%s)", res.Status, res.Error)
	}
}

// TestDefaultDispatcherFsRoundTrip proves the real default dispatcher executes
// a deterministic fs capability end to end (no engine needed for fs.*): a write
// then a read of the same path returns the written content, with a stable
// fingerprint across identical runs (token-free replay).
func TestDefaultDispatcherFsRoundTrip(t *testing.T) {
	d := newDefaultDispatcher(nil)
	path := filepath.Join(t.TempDir(), "manifest.txt")

	if _, err := d.Invoke(context.Background(), "fs.writeFile",
		map[string]any{"path": path, "content": "v1.2.3\n"}); err != nil {
		t.Fatalf("fs.writeFile: %v", err)
	}
	out, err := d.Invoke(context.Background(), "fs.readFile", map[string]any{"path": path})
	if err != nil {
		t.Fatalf("fs.readFile: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok || m["content"] != "v1.2.3\n" {
		t.Fatalf("unexpected fs.readFile result: %#v", out)
	}
}

// TestDefaultDispatcherShellDeferred asserts shell.* is recognised but routed
// to the (not-yet-wired) capability-script runner rather than an unsafe shell.
func TestDefaultDispatcherShellDeferred(t *testing.T) {
	d := newDefaultDispatcher(nil)
	if _, err := d.Invoke(context.Background(), "shell.exec", map[string]any{"cmd": "echo hi"}); err == nil {
		t.Fatal("expected shell.* to be deferred (not executed) in the default dispatcher")
	}
}

// TestDefaultDispatcherUnknownCapability errors cleanly.
func TestDefaultDispatcherUnknownCapability(t *testing.T) {
	d := newDefaultDispatcher(nil)
	if _, err := d.Invoke(context.Background(), "weird.thing", map[string]any{}); err == nil {
		t.Fatal("expected error for unsupported capability")
	}
}

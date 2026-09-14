package steps

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
)

// memql#2235 -- arg-VALUE coverage for the authored forEach sweep pattern.
//
// The forEach feature (memql#2246) ships with execution coverage that proves
// the inner call fires once per item (count), but it dispatches a
// sub-automation and asserts only the fire count -- not the per-row ARGUMENT
// VALUES, and not the conditional-gating shape. The #2235 sweep migration
// turns each impure per-row sweep logic into a PURE logic (returns the rows)
// + an automation that does the per-row WRITE in a `for`:
//
//	decide := logic <pure>(event: event)
//	for item in decide.nodes() { mutation <write>(id: item.id, ...) }
//
// For the destructive sweeps (accountDeletionSweep -> deleteUserHard) and the
// safety sweep (killSwitchSuspendsRunningPlans -> updatePlanStatus) it is not
// enough to know the loop fires N times -- it must fire with the RIGHT per-row
// args (so the right user is deleted / the right plan is suspended), and a
// conditionally-gated write must fire ONLY on matching rows. These tests pin
// that, driven through the REAL loader (parser -> compiler -> IR) and the REAL
// executor, a trigger's event and all: runSweep fires the whole automation.

// argRecorder is a function-step executor that records the fully-resolved
// per-call args via the SAME evaluation the real FunctionExecutor runs
// (Evaluator.ResolveV1Map), so the captured values are exactly what would
// reach the engine -- no live DB needed.
type argRecorder struct {
	name  string           // the last recorded call's name
	args  []map[string]any // each recorded call's args, in order
	calls []string         // each recorded call's name, in order
	// answers are the calls answered with a value and not recorded: a
	// sweep's pure decide.
	answers map[string]any
}

func (r *argRecorder) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	if step.Function != nil {
		if answer, ok := r.answers[step.Function.Name]; ok {
			return &automations.StepResult{StepId: step.ID, Status: "success", StartedAt: time.Now(), CompletedAt: time.Now(), Result: answer}, nil
		}
		r.name = step.Function.Name
		r.calls = append(r.calls, step.Function.Name)
		resolved, err := stepCtx.Evaluator.ResolveV1Map(ctx, step.Function.Args)
		if err != nil {
			return nil, err
		}
		if obj, ok := resolved["0"].(map[string]any); ok {
			r.args = append(r.args, obj)
		} else {
			r.args = append(r.args, resolved)
		}
	}
	return &automations.StepResult{
		StepId:      step.ID,
		Status:      "success",
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
		Result:      "ok",
	}, nil
}

func sweepRow(id, label string) any {
	return map[string]any{"id": id, "payload": map[string]any{"label": label}}
}

// runSweep compiles a sweep automation and fires it through the real
// executor on a system.startup event carrying payload (bound into the
// automation's args): `decideRows` answers the given rows, and every other
// construct call is recorded with its resolved arguments.
func runSweep(t *testing.T, src string, rows []any, payload map[string]any) *argRecorder {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	auto, err := automations.NewLoader(automations.LoaderOptions{Logger: logger}).CompileSource(src, "test:sweep-argvalue")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	var loops int
	for _, s := range auto.Steps {
		if s != nil && s.Type == automations.StepTypeForEach {
			loops++
		}
	}
	if loops == 0 {
		t.Fatalf("compiled automation has NO forEach step -- the compile dropped the loop. Steps: %+v", auto.Steps)
	}

	rec := &argRecorder{answers: map[string]any{"decideRows": stepResultFor("query", rows)}}
	reg := NewRegistry()
	reg.Register(automations.StepTypeFunction, rec)
	ev := events.NewEvent("system.startup", events.KindMessage, payload)
	exec, err := automations.NewExecutor(automations.ExecutorOptions{Logger: logger, StepRegistry: reg}).ExecuteWithEvent(context.Background(), auto, "test", &ev)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if exec.Status != "completed" {
		t.Fatalf("run status = %q, want completed", exec.Status)
	}
	return rec
}

// called reports how many times the recorder saw name.
func (r *argRecorder) called(name string) int {
	n := 0
	for _, c := range r.calls {
		if c == name {
			n++
		}
	}
	return n
}

// TestForEachSweep_BareWrite_PerRowArgs: an unconditional per-row write fires
// once per row, with item.id and a nested item.payload.* both resolved to the
// row's own values (the magicLinkExpirySweep / revokeExpiredDelegations shape).
func TestForEachSweep_BareWrite_PerRowArgs(t *testing.T) {
	const src = `@description("bare per-row write")
@trigger(event="system.startup")
automation sweepBare {
  decide := logic decideRows(event: event)
  for item in decide.nodes() {
    mutation markRow(rowId: item.id, label: item.label)
  }
}`
	rows := []any{
		sweepRow("v1:identity:magiclink:a", "alpha"),
		sweepRow("v1:identity:magiclink:b", "bravo"),
		sweepRow("v1:identity:magiclink:c", "charlie"),
	}
	rec := runSweep(t, src, rows, nil)

	if rec.name != "markRow" {
		t.Errorf("write name = %q, want markRow", rec.name)
	}
	if len(rec.args) != 3 {
		t.Fatalf("per-row write fired %d times, want 3", len(rec.args))
	}
	wantID := []string{"v1:identity:magiclink:a", "v1:identity:magiclink:b", "v1:identity:magiclink:c"}
	wantLabel := []string{"alpha", "bravo", "charlie"}
	for i, got := range rec.args {
		if got["rowId"] != wantID[i] {
			t.Errorf("call %d rowId = %#v, want %q", i, got["rowId"], wantID[i])
		}
		if got["label"] != wantLabel[i] {
			t.Errorf("call %d label (nested item.payload.label) = %#v, want %q", i, got["label"], wantLabel[i])
		}
	}
}

// TestForEachSweep_ConditionalWrite_GatesPerRow: a conditionally-gated per-row
// write fires ONLY on matching rows, with correct per-row args (the
// accountDeletionSweep / killSwitchSuspendsRunningPlans / expiry-sweep shape).
// This is the load-bearing guarantee for the destructive deleteUserHard sweep:
// the wrong rows must NOT be written.
func TestForEachSweep_ConditionalWrite_GatesPerRow(t *testing.T) {
	const src = `@description("conditional per-row write")
@trigger(event="system.startup")
automation sweepConditional {
  decide := logic decideRows(event: event)
  for item in decide.nodes() {
    if item.label == "expired" {
      mutation retireRow(rowId: item.id)
    }
  }
}`
	rows := []any{
		sweepRow("u-1", "expired"),
		sweepRow("u-2", "active"),
		sweepRow("u-3", "expired"),
		sweepRow("u-4", "active"),
	}
	rec := runSweep(t, src, rows, nil)

	if len(rec.args) != 2 {
		t.Fatalf("conditional write fired %d times, want 2 (only 'expired' rows)", len(rec.args))
	}
	for i, got := range rec.args {
		want := []string{"u-1", "u-3"}[i]
		if got["rowId"] != want {
			t.Errorf("call %d rowId = %#v, want %q (only expired rows must be written)", i, got["rowId"], want)
		}
	}
}

// TestForEachSweep_KillSwitch_EventGate pins the SAFETY-CRITICAL gating of the
// migrated killSwitchSuspendsRunningPlans sweep (#2235): the per-row suspend
// fires for a running plan ONLY when (a) the plan has a computerUseScope AND
// (b) the user's computerUseEnabled preference is explicitly false (the kill
// switch is engaged). The scope test is `item.computerUseScope != nil`, which
// an empty scope does not pass, so a non-computer-use plan is never
// suspended. The event gate reproduces the original
// !coalesce(computerUseEnabled, true) for the realistic false/true/absent
// cases (absent -> not engaged -> no suspend).
//
// THE GATE IS READ FROM AN ARGS FIELD, and the seeded envelope is the one the
// CDC publisher actually builds (memql#3610). This fixture used to write
// `event.node.payload.preferences...` and hand-seed `{"node": {...}}` to match
// -- an envelope no publisher produces. So the test passed while the real
// automation, on the real event, never fired once: `event.node.*` resolved to
// nothing and the filter decided false forever. A fixture that constructs the
// shape its subject needs is not evidence about production.
func TestForEachSweep_KillSwitch_EventGate(t *testing.T) {
	const src = `@description("kill switch sweep")
@trigger(event="system.startup")
automation killSwitch {
  args {
    preferences any
  }
  decide := logic decideRows(event: event)
  for item in decide.nodes() {
    if item.computerUseScope != nil && args.preferences.computerUseEnabled == false {
      mutation updatePlanStatus(planId: item.id, status: "awaitingFeedback", feedbackReason: "kill_switch_engaged")
    }
  }
}`
	rows := []any{
		map[string]any{"id": "plan-scoped", "payload": map[string]any{"computerUseScope": "full"}},
		map[string]any{"id": "plan-empty", "payload": map[string]any{"computerUseScope": ""}},
		map[string]any{"id": "plan-none", "payload": map[string]any{}},
	}

	run := func(enabledPresent bool, enabledVal any) []string {
		prefs := map[string]any{}
		if enabledPresent {
			prefs["computerUseEnabled"] = enabledVal
		}
		// The payload binds into the args block, as the CDC envelope's does.
		// There is no `node` key, which is the whole point.
		rec := runSweep(t, src, rows, map[string]any{"id": "u1", "preferences": prefs})
		ids := make([]string, 0, len(rec.args))
		for _, a := range rec.args {
			ids = append(ids, a["planId"].(string))
		}
		return ids
	}

	if got := run(true, false); len(got) != 1 || got[0] != "plan-scoped" {
		t.Errorf("engaged: suspended %v, want [plan-scoped] only (empty/no-scope plans must NOT be suspended)", got)
	}
	if got := run(true, true); len(got) != 0 {
		t.Errorf("enabled=true: suspended %v, want none", got)
	}
	if got := run(false, nil); len(got) != 0 {
		t.Errorf("preference absent: suspended %v, want none (default is not-engaged)", got)
	}
}

// TestForEachSweep_ReleaseWorkspace pins the migrated releaseWorkspaceOnPlanTerminal
// sweep (#2235), which has three moving parts the original logic crammed inline:
//   - the loop: release each still-`provisioned` workspace, gated on the plan
//     having reached a terminal status;
//   - teardown (an `if`): call the workbenchTeardownDirectory builtin on
//     terminal status -- even when the plan has ZERO workspace rows (the MVP
//     integration provisions on-disk without writing the concept row).
func TestForEachSweep_ReleaseWorkspace(t *testing.T) {
	const src = `@description("release workspace on plan terminal")
@trigger(event="system.startup")
automation rw {
  args {
    id any
    status any
  }
  decide := logic decideRows(event: event)
  for item in decide.nodes() {
    if item.status == "provisioned" && (args.status == "succeeded" || args.status == "failed" || args.status == "cancelled") {
      mutation releaseWorkspace(workspaceId: item.id, reason: "plan_terminal")
    }
  }
  if args.id != nil && (args.status == "succeeded" || args.status == "failed" || args.status == "cancelled") {
    teardown := builtin workbenchTeardownDirectory(runId: args.id)
  }
}`
	scenario := func(status string, workspaces []any) (released []string, teardown bool) {
		// The payload binds into the args block -- see the kill-switch
		// fixture above for why a hand-built `{"node": ...}` envelope was
		// misleading (memql#3610).
		rec := runSweep(t, src, workspaces, map[string]any{"id": "run-1", "status": status})
		for i, a := range rec.args {
			if rec.calls[i] == "releaseWorkspace" {
				released = append(released, a["workspaceId"].(string))
			}
		}
		return released, rec.called("workbenchTeardownDirectory") == 1
	}

	prov := map[string]any{"id": "ws-prov", "payload": map[string]any{"status": "provisioned"}}
	rel := map[string]any{"id": "ws-rel", "payload": map[string]any{"status": "released"}}

	// Terminal + a provisioned and a released workspace: release ONLY the
	// provisioned one; teardown fires.
	if r, td := scenario("succeeded", []any{prov, rel}); len(r) != 1 || r[0] != "ws-prov" || !td {
		t.Errorf("succeeded: released=%v teardown=%v, want [ws-prov], true", r, td)
	}
	// Non-terminal: no release, no teardown.
	if r, td := scenario("running", []any{prov}); len(r) != 0 || td {
		t.Errorf("running: released=%v teardown=%v, want [], false", r, td)
	}
	// Terminal + zero workspace rows: teardown STILL fires.
	if r, td := scenario("cancelled", []any{}); len(r) != 0 || !td {
		t.Errorf("cancelled+no-rows: released=%v teardown=%v, want [], true", r, td)
	}
}

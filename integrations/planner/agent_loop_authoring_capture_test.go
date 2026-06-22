package planner

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// fakeCaptureEngine satisfies BOTH the planner Engine interface (via the
// embedded fakeEngine) AND the captureEngine seam surface (CompileBundle /
// CatalogNearMatches / RunBundleDryRun), so a single value can back the whole
// capture orchestrator the way the live CognitionEngineAdapter does in
// production.
type fakeCaptureEngine struct {
	*fakeEngine
	sandbox  *fakeSandbox
	dryRun   memql.BundleDryRunReport
	dryRunMu sync.Mutex
	dryRunN  int
}

func (f *fakeCaptureEngine) CompileBundle(constructs []memql.SandboxConstruct) memql.SandboxReport {
	return f.sandbox.CompileBundle(constructs)
}

func (f *fakeCaptureEngine) CatalogNearMatches(_ context.Context, _ string, _ int) ([]memql.CatalogNearMatch, error) {
	return nil, nil
}

func (f *fakeCaptureEngine) RunBundleDryRun(_ context.Context, _ memql.DryRunRequest) (memql.BundleDryRunReport, error) {
	f.dryRunMu.Lock()
	f.dryRunN++
	f.dryRunMu.Unlock()
	return f.dryRun, nil
}

func (f *fakeCaptureEngine) dryRunCalls() int {
	f.dryRunMu.Lock()
	defer f.dryRunMu.Unlock()
	return f.dryRunN
}

// capturePlanRow is the queryPlanById result for a completed task plan.
func capturePlanRow(planId, owner, goal string) map[string]any {
	return map[string]any{
		"id":          planId,
		"status":      "succeeded",
		"requestedBy": owner,
		"goal":        goal,
	}
}

// newCaptureFixture builds a capture engine + dispatcher whose:
//   - queryPlanById returns the given plan row,
//   - queryAuthoringBundleForPlan returns existingBundle rows (nil = none),
//   - authoringDesign yields a one-author-dep design (the digest spec),
//   - authoringEmit / authoringRepair yield the automation + spec constructs,
//   - Gate-1 returns the scripted reports, Gate-2 returns dryRun.
func newCaptureFixture(t *testing.T, plan map[string]any, existingBundle []any, reports []memql.SandboxReport, dryRun memql.BundleDryRunReport) (*fakeCaptureEngine, *AuthoringCaptureDispatcher) {
	t.Helper()
	deps := []designDependency{
		{Kind: "spec", Name: "specDigestItemActive", Purpose: "active items", CandidateSource: specCandidateSource},
	}
	emitOut := emitJSON(t, []memql.SandboxConstruct{automationCon, specCon})
	repairOut := emitJSON(t, []memql.SandboxConstruct{automationCon, specCon})
	designOut := designJSON(t, deps)

	fe := &fakeEngine{
		aiResponder: func(templateId string, _ map[string]any) (any, error) {
			switch templateId {
			case "authoringDesign":
				return designOut, nil
			case "authoringEmit":
				return emitOut, nil
			case "authoringRepair":
				return repairOut, nil
			}
			return nil, nil
		},
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "queryAuthoringBundleForPlan"):
				return map[string]any{"output": existingBundle}, nil
			case strings.Contains(query, "queryPlanById"):
				return map[string]any{"output": []any{plan}}, nil
			case strings.Contains(query, "queryCataloguedConstructsForOwner"):
				return map[string]any{"output": []any{}}, nil
			}
			// mutations + anything else
			return nil, nil
		},
	}
	ce := &fakeCaptureEngine{fakeEngine: fe, sandbox: &fakeSandbox{reports: reports}, dryRun: dryRun}
	loop := &PlannerAgentLoop{engine: ce, logger: authoringTestLogger()}
	d := NewAuthoringCaptureDispatcher(loop, ce, authoringTestLogger())
	return ce, d
}

// TestRunCapture_CleanPath: a completed produceArtifact plan whose bundle
// compiles clean and dry-runs clean is persisted as a bundle + its constructs +
// a validated Gate-1 record + a dryRunPassed Gate-2 record, stamping
// sourcePlanId so the capture is reproducible + idempotent.
func TestRunCapture_CleanPath(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_CAPTURE_ENABLED", "1")
	plan := capturePlanRow("p-capture", "user-7", "Make me a CSV of last week's signups")
	ce, d := newCaptureFixture(t, plan, nil,
		[]memql.SandboxReport{okReport(automationCon, specCon)},
		memql.BundleDryRunReport{OK: true})

	if err := d.runCapture(context.Background(), "p-capture", "produceArtifact"); err != nil {
		t.Fatalf("runCapture: %v", err)
	}

	exec, _, _ := ce.snapshot()
	if n := countContains(exec, "mutationCreateAuthoringBundle"); n != 1 {
		t.Fatalf("want exactly 1 create-bundle, got %d (exec=%v)", n, exec)
	}
	if !anyCallContainsAll(exec, `mutationCreateAuthoringBundle`, `"sourcePlanId":"p-capture"`) {
		t.Fatalf("create-bundle must stamp sourcePlanId; exec=%v", exec)
	}
	if n := countContains(exec, "mutationCreateAuthoringConstruct"); n != 2 {
		t.Fatalf("want 2 construct rows (automation + spec), got %d", n)
	}
	if !anyCallContainsAll(exec, "mutationRecordBundleValidation", `"status":"validated"`) {
		t.Fatalf("clean Gate-1 must record status=validated; exec=%v", exec)
	}
	if !anyCallContainsAll(exec, "mutationRecordBundleDryRun", `"status":"dryRunPassed"`) {
		t.Fatalf("clean Gate-2 must record status=dryRunPassed; exec=%v", exec)
	}
	if got := ce.dryRunCalls(); got != 1 {
		t.Fatalf("Gate-2 dry-run should run exactly once, got %d", got)
	}
}

// TestRunCapture_Gate1FailMarksFailedNoDryRun: a bundle that never compiles
// clean (Gate-1 keeps failing through the repair budget) is recorded failed and
// NEVER handed to Gate-2 -- you cannot behaviorally dry-run a non-compiling
// bundle.
func TestRunCapture_Gate1FailMarksFailedNoDryRun(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_CAPTURE_ENABLED", "1")
	t.Setenv("MEMQL_AUTHORING_MAX_REPAIRS", "1") // bound the repair loop
	plan := capturePlanRow("p-fail", "user-7", "Do the impossible thing")
	fail := failReport("spec", "specDigestItemActive", "unknown field payload.nope", automationCon, specCon)
	ce, d := newCaptureFixture(t, plan, nil,
		[]memql.SandboxReport{fail}, // every CompileBundle call fails
		memql.BundleDryRunReport{OK: true})

	if err := d.runCapture(context.Background(), "p-fail", "produceArtifact"); err != nil {
		t.Fatalf("runCapture: %v", err)
	}

	exec, _, _ := ce.snapshot()
	if !anyCallContainsAll(exec, "mutationRecordBundleValidation", `"status":"failed"`) {
		t.Fatalf("unclean Gate-1 must record status=failed; exec=%v", exec)
	}
	if countContains(exec, "mutationRecordBundleDryRun") != 0 {
		t.Fatalf("a non-compiling bundle must NOT be dry-run; exec=%v", exec)
	}
	if got := ce.dryRunCalls(); got != 0 {
		t.Fatalf("Gate-2 must not run on an unclean bundle, got %d", got)
	}
}

// TestRunCapture_IdempotentSkip: a Plan already captured (a bundle exists for
// its sourcePlanId) authors nothing on a re-delivered terminal event.
func TestRunCapture_IdempotentSkip(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_CAPTURE_ENABLED", "1")
	plan := capturePlanRow("p-dup", "user-7", "Already captured task")
	existing := []any{map[string]any{"id": "bundle-existing"}}
	ce, d := newCaptureFixture(t, plan, existing,
		[]memql.SandboxReport{okReport(automationCon, specCon)},
		memql.BundleDryRunReport{OK: true})

	if err := d.runCapture(context.Background(), "p-dup", "produceArtifact"); err != nil {
		t.Fatalf("runCapture: %v", err)
	}

	exec, si, _ := ce.snapshot()
	if countContains(exec, "mutationCreateAuthoringBundle") != 0 {
		t.Fatalf("idempotent skip must author nothing; exec=%v", exec)
	}
	if countContains(si, "authoringDesign") != 0 {
		t.Fatalf("idempotent skip must not even run the design pass; si=%v", si)
	}
}

// TestHandlePlanUpdated_IgnoresNonCapturable: a non-capturable kind, an
// internal-machinery kind, or a non-succeeded status is filtered out BEFORE any
// claim/dispatch (these spawn no capture goroutine).
func TestHandlePlanUpdated_IgnoresNonCapturable(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_CAPTURE_ENABLED", "1")
	_, d := newCaptureFixture(t, capturePlanRow("x", "u", "g"), nil,
		[]memql.SandboxReport{okReport()}, memql.BundleDryRunReport{OK: true})

	cases := []struct{ name, kind, status string }{
		{"running produceArtifact", "produceArtifact", "running"},
		{"trainSpecialist succeeded", "trainSpecialist", "succeeded"},
		{"failed produceArtifact", "produceArtifact", "failed"},
		{"embedDomainItems succeeded", "embedDomainItems", "succeeded"},
	}
	for _, c := range cases {
		d.HandlePlanUpdated(events.Event{Payload: map[string]any{
			"id":      "plan-" + c.name,
			"payload": map[string]any{"kind": c.kind, "status": c.status},
		}})
		d.mu.Lock()
		_, claimed := d.claimed["plan-"+c.name]
		d.mu.Unlock()
		if claimed {
			t.Errorf("%s: must not be claimed/captured", c.name)
		}
	}
}

// TestHandlePlanUpdated_DispatchesOnSucceededTask: a succeeded capturable task
// is claimed and runs the full async capture (one goroutine), persisting a
// bundle stamped with the source plan id.
func TestHandlePlanUpdated_DispatchesOnSucceededTask(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_CAPTURE_ENABLED", "1")
	// This test exercises the LLM author path's async dispatch; the default is
	// now deterministic transcription (#1188), so pin the mode explicitly.
	t.Setenv("MEMQL_AUTHORING_CAPTURE_MODE", "author")
	plan := capturePlanRow("plan-async", "user-9", "Summarize the quarterly numbers")
	ce, d := newCaptureFixture(t, plan, nil,
		[]memql.SandboxReport{okReport(automationCon, specCon)},
		memql.BundleDryRunReport{OK: true})

	d.HandlePlanUpdated(events.Event{Payload: map[string]any{
		"id":      "plan-async",
		"payload": map[string]any{"kind": "produceArtifact", "status": "succeeded"},
	}})

	waitFor(t, func() bool {
		exec, _, _ := ce.snapshot()
		return countContains(exec, "mutationCreateAuthoringBundle") == 1
	})
	exec, _, _ := ce.snapshot()
	if !anyCallContainsAll(exec, "mutationCreateAuthoringBundle", `"sourcePlanId":"plan-async"`) {
		t.Fatalf("async capture must stamp sourcePlanId; exec=%v", exec)
	}
}

// TestHandlePlanUpdated_DisabledGate: with the kill-switch off, a matching
// succeeded task is NOT claimed (capture is globally suppressed).
func TestHandlePlanUpdated_DisabledGate(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_CAPTURE_ENABLED", "0")
	_, d := newCaptureFixture(t, capturePlanRow("x", "u", "g"), nil,
		[]memql.SandboxReport{okReport()}, memql.BundleDryRunReport{OK: true})

	d.HandlePlanUpdated(events.Event{Payload: map[string]any{
		"id":      "plan-disabled",
		"payload": map[string]any{"kind": "produceArtifact", "status": "succeeded"},
	}})
	d.mu.Lock()
	_, claimed := d.claimed["plan-disabled"]
	d.mu.Unlock()
	if claimed {
		t.Fatalf("disabled kill-switch must suppress capture (no claim)")
	}
}

// containsAll returns true when at least one call string contains every one of
// the given substrings (used to assert a single mutation carries several args).
func anyCallContainsAll(calls []string, subs ...string) bool {
	for _, c := range calls {
		all := true
		for _, s := range subs {
			if !strings.Contains(c, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

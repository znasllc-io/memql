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

// newRunCaptureFixture builds a dispatcher over a fake engine that answers the
// two reads transcription makes.
func newRunCaptureFixture(t *testing.T, obs []any) (*fakeEngine, *AuthoringCaptureDispatcher) {
	t.Helper()
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			switch {
			case strings.Contains(query, "authoringBundleForRun"):
				return map[string]any{"output": []any{}}, nil
			case strings.Contains(query, "workObservationsForOwnerRun"):
				return map[string]any{"output": obs}, nil
			}
			return nil, nil
		},
	}
	loop := &PlannerAgentLoop{engine: fe, logger: authoringTestLogger()}
	return fe, NewAuthoringCaptureDispatcher(loop, fe, authoringTestLogger())
}

// TestHandleRunUpdated_IgnoresNonTerminal: a run that has not SUCCEEDED is
// filtered out before any claim or dispatch.
//
// The kind allow-list this test used to carry is gone with the Plan kinds it
// named (memql#5050): a run's template already says what it is, and the
// internal-machinery kinds it excluded were Plan kinds that no longer exist.
func TestHandleRunUpdated_IgnoresNonTerminal(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_CAPTURE_ENABLED", "1")
	_, d := newRunCaptureFixture(t, nil)

	for _, status := range []string{"compiling", "running", "waiting", "failed", "cancelled", "abandoned"} {
		id := "run-" + status
		d.HandleRunUpdated(events.Event{Payload: map[string]any{
			"id":      id,
			"payload": map[string]any{"status": status, "ownerUserId": "u1", "automationName": "demo"},
		}})
		d.mu.Lock()
		_, claimed := d.claimed[id]
		d.mu.Unlock()
		if claimed {
			t.Errorf("a run at %q must not be claimed/captured", status)
		}
	}
}

// TestHandleRunUpdated_DispatchesOnSucceededRun: a succeeded run is claimed
// and transcribed asynchronously, persisting a bundle stamped with the source
// run id.
func TestHandleRunUpdated_DispatchesOnSucceededRun(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_CAPTURE_ENABLED", "1")
	obs := []any{observationRow("o1", "workbenchHost", 0, false, map[string]any{"path": "x.md"})}
	fe, d := newRunCaptureFixture(t, obs)

	d.HandleRunUpdated(events.Event{Payload: map[string]any{
		"id":      "run-async",
		"payload": map[string]any{"status": "succeeded", "ownerUserId": "user-9", "automationName": "analyzeFile"},
	}})

	waitFor(t, func() bool {
		exec, _, _ := fe.snapshot()
		return countContains(exec, "createAuthoringBundle") == 1
	})
	exec, _, _ := fe.snapshot()
	if !anyCallContainsAll(exec, "createAuthoringBundle", `sourceRunId: "run-async"`) {
		t.Fatalf("async capture must stamp sourceRunId; exec=%v", exec)
	}
}

// TestHandleRunUpdated_DisabledGate: with the kill-switch off, a succeeded run
// is NOT claimed (capture is globally suppressed).
func TestHandleRunUpdated_DisabledGate(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_CAPTURE_ENABLED", "0")
	_, d := newRunCaptureFixture(t, nil)

	d.HandleRunUpdated(events.Event{Payload: map[string]any{
		"id":      "run-disabled",
		"payload": map[string]any{"status": "succeeded", "ownerUserId": "u1", "automationName": "demo"},
	}})
	d.mu.Lock()
	_, claimed := d.claimed["run-disabled"]
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

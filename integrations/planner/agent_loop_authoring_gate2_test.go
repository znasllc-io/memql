package planner

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// fakeGate2 is the Gate-2 seam stand-in: it returns a canned dry-run report (or
// error) and records the request it was handed.
type fakeGate2 struct {
	report memql.BundleDryRunReport
	err    error
	reqs   []memql.DryRunRequest
}

func (f *fakeGate2) RunBundleDryRun(_ context.Context, req memql.DryRunRequest) (memql.BundleDryRunReport, error) {
	f.reqs = append(f.reqs, req)
	return f.report, f.err
}

func gate2Bundle() authoringBundle {
	return authoringBundle{
		AutomationName: "dailyDigest",
		Constructs: []memql.SandboxConstruct{
			{Kind: "automation", Name: "dailyDigest", Source: "automation dailyDigest { }"},
			{Kind: "spec", Name: "specActive", Source: "spec activeRowTrait specActive { return active == true }"},
		},
	}
}

// TestHandoffToGate2_RunsAndReportsClean: a validated bundle is handed to Gate
// 2, which runs the automation and reports OK -> Available + Clean, with the
// automation source threaded into the request.
func TestHandoffToGate2_RunsAndReportsClean(t *testing.T) {
	l := newDesignLoop(&fakeEngine{})
	g2 := &fakeGate2{report: memql.BundleDryRunReport{OK: true, AutomationName: "dailyDigest"}}

	out, err := l.handoffToGate2(context.Background(), "bundle-1", gate2Bundle(), nil, g2)
	if err != nil {
		t.Fatalf("handoffToGate2: %v", err)
	}
	if !out.Available || !out.Clean {
		t.Fatalf("clean dry-run expected; available=%v clean=%v", out.Available, out.Clean)
	}
	if len(g2.reqs) != 1 {
		t.Fatalf("Gate 2 should be invoked once, got %d", len(g2.reqs))
	}
	req := g2.reqs[0]
	if req.BundleId != "bundle-1" || req.AutomationName != "dailyDigest" {
		t.Fatalf("request not threaded: %+v", req)
	}
	if req.AutomationSource == "" {
		t.Fatalf("automation source must be resolved from the bundle")
	}
	if req.Mode != memql.DryRunModeIsolated {
		t.Fatalf("handoff must default to the isolated tier (zero prod side effects), got %q", req.Mode)
	}
}

// TestHandoffToGate2_ReportsNotClean: when the dry-run runs but the automation
// fails a step, the outcome is Available + NOT Clean (the report carries the
// failure for the user-review gate).
func TestHandoffToGate2_ReportsNotClean(t *testing.T) {
	l := newDesignLoop(&fakeEngine{})
	g2 := &fakeGate2{report: memql.BundleDryRunReport{OK: false, FailureReason: "step blew up"}}

	out, err := l.handoffToGate2(context.Background(), "b1", gate2Bundle(), nil, g2)
	if err != nil {
		t.Fatalf("handoffToGate2: %v", err)
	}
	if !out.Available {
		t.Fatalf("a completed-but-failed dry-run is still Available")
	}
	if out.Clean {
		t.Fatalf("a failed dry-run must not be reported Clean")
	}
}

// TestHandoffToGate2_NilSeamSkips: with no Gate-2 seam wired (an
// automations-free binary), the handoff is a non-fatal skip -- the Gate-1-clean
// bundle stands. Core #960 acceptance does not depend on Gate 2.
func TestHandoffToGate2_NilSeamSkips(t *testing.T) {
	l := newDesignLoop(&fakeEngine{})
	out, err := l.handoffToGate2(context.Background(), "b1", gate2Bundle(), nil, nil)
	if err != nil {
		t.Fatalf("nil seam must not error: %v", err)
	}
	if out.Available {
		t.Fatalf("nil seam must report Gate 2 unavailable")
	}
}

// TestHandoffToGate2_RunnerUnavailableErrorIsNonFatal: when the runner reports
// itself unavailable (no automations linked) via an error, the handoff records
// an unavailable outcome rather than failing the pipeline.
func TestHandoffToGate2_RunnerUnavailableErrorIsNonFatal(t *testing.T) {
	l := newDesignLoop(&fakeEngine{})
	g2 := &fakeGate2{err: fmt.Errorf("behavioral dry-run is unavailable: no runner")}

	out, err := l.handoffToGate2(context.Background(), "b1", gate2Bundle(), nil, g2)
	if err != nil {
		t.Fatalf("an unavailable runner must be non-fatal, got err: %v", err)
	}
	if out.Available || out.Clean {
		t.Fatalf("an unavailable runner must report not-available, not-clean")
	}
}

// TestHandoffToGate2_MissingAutomationErrors: a bundle with no automation
// construct can't be dry-run -- the handoff returns an error rather than send a
// blank source to Gate 2.
func TestHandoffToGate2_MissingAutomationErrors(t *testing.T) {
	l := newDesignLoop(&fakeEngine{})
	g2 := &fakeGate2{report: memql.BundleDryRunReport{OK: true}}
	noAuto := authoringBundle{
		AutomationName: "dailyDigest",
		Constructs:     []memql.SandboxConstruct{{Kind: "spec", Name: "specActive", Source: "spec activeRowTrait specActive { return active == true }"}},
	}
	if _, err := l.handoffToGate2(context.Background(), "b1", noAuto, nil, g2); err == nil {
		t.Fatalf("a bundle with no automation must error")
	}
	if len(g2.reqs) != 0 {
		t.Fatalf("Gate 2 must NOT be called when the automation is missing")
	}
}

// TestHandoffToGate2_ThreadsTriggerEvent: a supplied trigger event is passed
// through to the dry-run request.
func TestHandoffToGate2_ThreadsTriggerEvent(t *testing.T) {
	l := newDesignLoop(&fakeEngine{})
	g2 := &fakeGate2{report: memql.BundleDryRunReport{OK: true}}
	trig := &memql.DryRunTriggerEvent{Topic: "graph.node.created.x", Kind: "created", Payload: map[string]any{"id": "n1"}}

	if _, err := l.handoffToGate2(context.Background(), "b1", gate2Bundle(), trig, g2); err != nil {
		t.Fatalf("handoffToGate2: %v", err)
	}
	if g2.reqs[0].TriggerEvent == nil || g2.reqs[0].TriggerEvent.Topic != "graph.node.created.x" {
		t.Fatalf("trigger event not threaded: %+v", g2.reqs[0].TriggerEvent)
	}
}

// TestBundleAutomationSource_FallsBackToSoleAutomation: when the automation
// name doesn't match exactly but there's a single automation construct, the
// source resolves to it (a model name-slip is tolerated).
func TestBundleAutomationSource_FallsBackToSoleAutomation(t *testing.T) {
	bundle := authoringBundle{
		AutomationName: "expectedName",
		Constructs: []memql.SandboxConstruct{
			{Kind: "automation", Name: "actualName", Source: "automation actualName { }"},
		},
	}
	src, ok := bundleAutomationSource(bundle)
	if !ok || src != "automation actualName { }" {
		t.Fatalf("sole-automation fallback failed: ok=%v src=%q", ok, src)
	}
}

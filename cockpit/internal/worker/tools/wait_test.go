package tools

import (
	"context"
	"encoding/json"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/cockpit/internal/worker/consent"
)

// Untagged test file: workerComputer.wait is build-agnostic
// (memql-cockpit#166), so these run identically on the headless and
// computeruse builds -- a sleep drives no real input.

// TestDispatcher_WaitWorksOnEveryBuild is the routing contract: wait
// dispatches successfully on every build variant (the headless
// router never sees it -- it's routed alongside capabilities), still
// passes through the consent gate exactly once, and echoes the
// effective ms in the payload.
func TestDispatcher_WaitWorksOnEveryBuild(t *testing.T) {
	gate := &fakeGate{decision: consent.Decision{Allowed: true, Reason: "consent granted"}}
	d := NewDispatcher(quietLogger(), DefaultPolicy(), gate)

	success, failure := d.Dispatch(context.Background(), &memqlv1.ToolDispatch{
		Tool:     "workerComputer",
		Action:   "wait",
		ArgsJson: []byte(`{"ms": 5}`),
		CallId:   "wait-1",
	})
	if failure != nil {
		t.Fatalf("wait must dispatch on every build; got %s: %s", failure.GetErrorCode(), failure.GetErrorMessage())
	}
	if success == nil {
		t.Fatal("expected a success payload")
	}
	if gate.calls != 1 {
		t.Errorf("consent gate should be consulted exactly once; got %d", gate.calls)
	}
	var payload struct {
		Ms int `json:"ms"`
	}
	if err := json.Unmarshal(success.GetResultJson(), &payload); err != nil {
		t.Fatalf("decode payload: %v (raw: %s)", err, success.GetResultJson())
	}
	if payload.Ms != 5 {
		t.Errorf("payload ms = %d, want 5", payload.Ms)
	}
}

// TestDispatcher_WaitRespectsConsentGate: wait is observe-class but
// still consent-gated like every other dispatch.
func TestDispatcher_WaitRespectsConsentGate(t *testing.T) {
	if got := consent.Classify("workerComputer", "wait"); got != consent.ClassObserve {
		t.Errorf("Classify(workerComputer, wait) = %q, want %q", got, consent.ClassObserve)
	}

	gate := &fakeGate{decision: consent.Decision{Allowed: false, Reason: "no window open"}}
	d := NewDispatcher(quietLogger(), DefaultPolicy(), gate)
	_, failure := d.Dispatch(context.Background(), &memqlv1.ToolDispatch{
		Tool:     "workerComputer",
		Action:   "wait",
		ArgsJson: []byte(`{"ms": 1}`),
		CallId:   "wait-2",
	})
	if failure == nil || failure.GetErrorCode() != "consent_required" {
		t.Fatalf("a denying gate must gate wait; got %+v", failure)
	}
}

// TestWaitMsFromArgs pins the [1, 5000] clamp without sleeping out
// the ceiling.
func TestWaitMsFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want int
	}{
		{"absent clamps up to the floor", map[string]any{}, 1},
		{"zero clamps up to the floor", map[string]any{"ms": float64(0)}, 1},
		{"negative clamps up to the floor", map[string]any{"ms": float64(-50)}, 1},
		{"in-range passes through", map[string]any{"ms": float64(250)}, 250},
		{"ceiling passes through", map[string]any{"ms": float64(5000)}, 5000},
		{"over-ceiling clamps down", map[string]any{"ms": float64(60000)}, 5000},
		{"non-numeric clamps up to the floor", map[string]any{"ms": "soon"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := waitMsFromArgs(tc.args); got != tc.want {
				t.Errorf("waitMsFromArgs(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// TestRunComputerWait_HonorsContext: a canceled dispatch context
// unblocks the sleep immediately instead of holding the call slot
// for the full duration.
func TestRunComputerWait_HonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	success, failure := runComputerWait(ctx, map[string]any{"ms": float64(5000)})
	if success != nil {
		t.Fatalf("canceled wait must not succeed; got %+v", success)
	}
	if failure == nil || failure.GetErrorCode() != "canceled" {
		t.Fatalf("expected canceled failure; got %+v", failure)
	}
}

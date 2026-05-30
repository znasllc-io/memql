package steps

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// TestScheduleTriggeredEventArgRendersAsObject pins the fix for
// issue #418. Schedule-triggered automations whose step passes the
// conventional `logic xxx { event: event }` argument (e.g.
// expireGuestInvitations, magicLinkExpirySweep, rolloverDailySpace,
// feedbackTimeoutAutoPause) compile that argument to the runtime
// reference string "event". At dispatch time the executor seeds the
// evaluator with an `event` object for BOTH event-triggered and
// schedule-triggered runs (see executor.go ExecuteWithEvent).
//
// The regression this guards against: when `event` is NOT seeded
// (the old schedule path), the bare reference fails to resolve, the
// renderer emits the unresolved literal token `event=event`, the
// engine coerces it to a STRING, and the receiving logic function's
// `event: object` validation trips with
// `argument "event": expected object, got string` -- the exact
// failure observed on every cron tick.
//
// This test asserts the rendered function-call argument is an OBJECT
// literal (`{...}`), never the bare/string token, given the evaluator
// is seeded with the synthetic schedule envelope the executor now
// installs.
func TestScheduleTriggeredEventArgRendersAsObject(t *testing.T) {
	// Mirror exactly what executor.ExecuteWithEvent seeds for a
	// schedule-triggered (triggeringEvent == nil) run.
	eval := automations.NewEvaluator()
	eval.SetCustom("event", map[string]any{
		"topic":   "schedule",
		"kind":    "schedule",
		"payload": map[string]any{"triggeredBy": "schedule"},
	})

	// This is what the compiler emits for `logic xxx { event: event }`.
	args := map[string]any{"event": "event"}

	resolved, err := resolveArgsRefs(args, eval)
	if err != nil {
		t.Fatalf("resolveArgsRefs failed: %v", err)
	}

	// The resolved `event` value must be an object, not a string.
	if _, ok := resolved["event"].(map[string]any); !ok {
		t.Fatalf("expected resolved event arg to be an object (map), got %T (%#v)",
			resolved["event"], resolved["event"])
	}

	rendered := renderFunctionArgs(resolved)

	// Must render as an object literal: `event={...}`.
	if !strings.HasPrefix(rendered, "event={") || !strings.HasSuffix(rendered, "}") {
		t.Fatalf("expected event arg to render as an object literal, got %q", rendered)
	}
	// Must NOT render as the bare/string token that the engine coerces
	// to a string (the #418 failure mode).
	if rendered == "event=event" {
		t.Fatalf("event arg rendered as bare token %q -- engine would coerce to string (regression of #418)", rendered)
	}
	if strings.Contains(rendered, `"triggeredBy"`) == false {
		t.Fatalf("expected synthetic schedule payload to carry triggeredBy, got %q", rendered)
	}
}

// TestUnseededEventArgIsTheKnownFailureShape documents the buggy
// behaviour the fix eliminates: with no `event` seeded (the old
// schedule path) the same compiled argument renders as the bare token
// `event=event`, which the engine coerces to a string. This test pins
// the contract that the executor must NOT leave `event` unseeded for
// scheduled runs -- if it ever regresses, the assertion above starts
// failing while this one keeps documenting why.
func TestUnseededEventArgIsTheKnownFailureShape(t *testing.T) {
	eval := automations.NewEvaluator() // intentionally no `event` seeded
	args := map[string]any{"event": "event"}

	resolved, err := resolveArgsRefs(args, eval)
	if err != nil {
		t.Fatalf("resolveArgsRefs failed: %v", err)
	}
	rendered := renderFunctionArgs(resolved)

	if rendered != "event=event" {
		t.Fatalf("expected unseeded event arg to render as the bare token \"event=event\" (the #418 failure shape), got %q", rendered)
	}
}

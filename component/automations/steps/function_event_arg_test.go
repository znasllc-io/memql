package steps

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// eventArg is the compiled `logic xxx ( event: event )` argument map: the
// value leaf `event`, parsed.
func eventArg(t *testing.T) map[string]any {
	t.Helper()
	node, err := langparser.ParseV1Expression("event")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"event": &automations.ExprLeaf{Src: "event", Node: node}}
}

// TestScheduleTriggeredEventArgRendersAsObject pins the fix for
// issue #418. Schedule-triggered automations whose step passes the
// conventional `logic xxx ( event: event )` argument (e.g.
// expireGuestInvitations, magicLinkExpirySweep, feedbackTimeoutAutoPause)
// read the `event` root at dispatch time, and the executor seeds the
// evaluator with an `event` object for BOTH event-triggered and
// schedule-triggered runs (see executor.go ExecuteWithEvent).
//
// The regression this guards against: an unseeded `event` rendered as the
// bare token `event: event`, the engine coerced it to a STRING, and the
// receiving logic's `event: object` validation tripped with
// `argument "event": expected object, got string` on every cron tick.
func TestScheduleTriggeredEventArgRendersAsObject(t *testing.T) {
	// Mirror exactly what executor.ExecuteWithEvent seeds for a
	// schedule-triggered (triggeringEvent == nil) run.
	eval := automations.NewEvaluator()
	eval.SetCustom("event", map[string]any{
		"topic":   "schedule",
		"kind":    "schedule",
		"payload": map[string]any{"triggeredBy": "schedule"},
	})

	resolved, err := eval.ResolveV1Map(context.Background(), eventArg(t))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The resolved `event` value must be an object, not a string.
	if _, ok := resolved["event"].(map[string]any); !ok {
		t.Fatalf("expected resolved event arg to be an object (map), got %T (%#v)",
			resolved["event"], resolved["event"])
	}

	rendered := renderV1CallArgs(resolved)

	// Must render as an object literal: `event: {...}`.
	if !strings.HasPrefix(rendered, "event: {") || !strings.HasSuffix(rendered, "}") {
		t.Fatalf("expected event arg to render as an object literal, got %q", rendered)
	}
	if !strings.Contains(rendered, `"triggeredBy"`) {
		t.Fatalf("expected synthetic schedule payload to carry triggeredBy, got %q", rendered)
	}
}

// TestUnseededEventArgIsRefused: with no `event` seeded the argument does not
// resolve, and the step fails rather than rendering the bare token
// `event: event` that the engine coerced to a string (the #418 failure
// shape). `event` is a reserved root, so an unseeded one is absent, and an
// absent argument is omitted -- the callee then reports its @required
// argument missing, in its own words.
func TestUnseededEventArgIsRefused(t *testing.T) {
	eval := automations.NewEvaluator() // intentionally no `event` seeded
	resolved, err := eval.ResolveV1Map(context.Background(), eventArg(t))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, present := resolved["event"]; present {
		t.Fatalf("an unseeded event resolved to %#v; want it omitted, never a bare token", resolved["event"])
	}
	if rendered := renderV1CallArgs(resolved); rendered == "event: event" {
		t.Fatalf("event arg rendered as the bare token %q -- the #418 failure shape", rendered)
	}
}

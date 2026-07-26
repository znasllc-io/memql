package app

// automation_preflight_wiring_test.go -- guards the synchronous automation
// pre-flight in engineAndBus (memql#2830).
//
// WHY A SOURCE-LEVEL GUARD. The pre-flight is the load-bearing half of the
// strict automation gate: the scheduler loads from its own goroutine, where a
// load error can only be LOGGED, so without this call site the loader's error
// is inert and a node boots green with automations missing. Review round 2
// confirmed the gap empirically -- deleting the call left the entire suite,
// including all of component/automations, passing.
//
// It cannot be covered behaviourally in-process: a.fatal ends in os.Exit(1),
// nothing in the repo calls app.Build(), and standing up a real App needs a
// database. A subprocess harness would be the thorough answer; this guard is
// the cheap one that catches the actual regression risk -- someone deleting or
// reordering the call during a refactor. It asserts WIRING, not behaviour: it
// cannot tell you the gate works, only that it is still connected. The
// behavioural half lives in
// component/automations/strict_automation_boot_test.go, which proves LoadAll
// returns the error this call site consumes.

import (
	"os"
	"strings"
	"testing"
)

func TestAutomationPreflightIsWiredIntoBoot(t *testing.T) {
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	body := string(src)

	const preflight = "a.automationLoader.LoadAll()"
	preflightIdx := strings.Index(body, preflight)
	if preflightIdx < 0 {
		t.Fatal("the synchronous automation pre-flight is GONE from engineAndBus. Without it the strict automation gate (memql#2830) is inert: the scheduler loads from a goroutine that can only log a load error, so a malformed automation silently disappears and the node boots green. Restore the LoadAll pre-flight, or replace it with another synchronous gate that refuses the boot.")
	}

	// It must FATAL, not log. A logged error here is the same silent-drop
	// failure mode in a different costume.
	tail := body[preflightIdx:]
	if end := strings.Index(tail, "\n\n"); end > 0 {
		tail = tail[:end]
	}
	if !strings.Contains(tail, "a.fatal(") {
		t.Fatalf("the automation pre-flight must call a.fatal on error -- logging it would let the node boot with automations missing. Got:\n%s", tail)
	}

	// It must run BEFORE the scheduler is constructed, so a broken tree is
	// refused rather than half-scheduled.
	schedIdx := strings.Index(body, "automations.NewScheduler(")
	if schedIdx < 0 {
		t.Fatal("could not find automations.NewScheduler in engine.go; this guard needs updating")
	}
	if preflightIdx > schedIdx {
		t.Fatal("the automation pre-flight must run BEFORE automations.NewScheduler, so a broken automation tree refuses the boot instead of being half-scheduled")
	}
}

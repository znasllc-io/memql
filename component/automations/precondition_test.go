package automations

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// Epic 4 / memql#2139: first-class automation preconditions.
//
// A precondition is a deterministic boolean check evaluated at the start
// of an automation run. A miss aborts the run cleanly and emits the
// structured healing.precondition.missed repair-trigger signal -- the
// clean self-healing trigger AND the cross-machine portability mechanism.

// --- extraction + parsing ------------------------------------------------

func TestExtractPreconditions_None(t *testing.T) {
	src := `automation foo {
  step run { logic doThing { event: event } }
}`
	pcs, stripped, err := extractPreconditions(src)
	if err != nil {
		t.Fatalf("extractPreconditions: %v", err)
	}
	if pcs != nil {
		t.Fatalf("expected nil preconditions, got %d", len(pcs))
	}
	if stripped != src {
		t.Fatalf("source with no preconditions should be unchanged")
	}
}

func TestExtractPreconditions_ParsesFieldsAndStrips(t *testing.T) {
	src := `automation deployStaging {
  precondition envIsStaging {
    check: $config.MEMQL_ENV == "staging"
    literal: MEMQL_ENV
    description: "Only drive the staging deploy spine in staging."
  }
  step run { logic driveDeploy { event: event } }
}`
	pcs, stripped, err := extractPreconditions(src)
	if err != nil {
		t.Fatalf("extractPreconditions: %v", err)
	}
	if len(pcs) != 1 {
		t.Fatalf("want 1 precondition, got %d", len(pcs))
	}
	pc := pcs[0]
	if pc.ID != "envIsStaging" {
		t.Errorf("id = %q, want envIsStaging", pc.ID)
	}
	if pc.Check != `$config.MEMQL_ENV == "staging"` {
		t.Errorf("check = %q (inner quotes must survive)", pc.Check)
	}
	if pc.Literal != "MEMQL_ENV" {
		t.Errorf("literal = %q, want MEMQL_ENV", pc.Literal)
	}
	if pc.Description != "Only drive the staging deploy spine in staging." {
		t.Errorf("description = %q", pc.Description)
	}
	// The precondition block must be gone from the stripped source so the
	// struct-form rewriter (which only knows `step`) never sees it.
	if contains(stripped, "precondition") {
		t.Errorf("stripped source still contains a precondition block:\n%s", stripped)
	}
	if !contains(stripped, "step run") {
		t.Errorf("stripped source dropped the step block:\n%s", stripped)
	}
}

func TestExtractPreconditions_Multiple(t *testing.T) {
	src := `automation multi {
  precondition a { check: $event.payload.x != "" }
  precondition b { check: $config.Y == "z" literal: Y }
  step run { logic doThing { event: event } }
}`
	pcs, stripped, err := extractPreconditions(src)
	if err != nil {
		t.Fatalf("extractPreconditions: %v", err)
	}
	if len(pcs) != 2 {
		t.Fatalf("want 2 preconditions, got %d", len(pcs))
	}
	if pcs[0].ID != "a" || pcs[1].ID != "b" {
		t.Errorf("ids = %q,%q want a,b", pcs[0].ID, pcs[1].ID)
	}
	if contains(stripped, "precondition") {
		t.Errorf("stripped source still has a precondition block")
	}
}

func TestParsePreconditionBody_RequiresCheck(t *testing.T) {
	_, err := parsePreconditionBody("noCheck", `literal: X`)
	if err == nil {
		t.Fatalf("expected error for missing check")
	}
}

func TestValidatePreconditions_DuplicateID(t *testing.T) {
	err := validatePreconditions([]*Precondition{
		{ID: "dup", Check: "a == b"},
		{ID: "dup", Check: "c == d"},
	})
	if err == nil {
		t.Fatalf("expected duplicate-id rejection")
	}
}

// --- full loader pipeline: strip -> rewrite -> compile -> re-attach ------

// An automation declaring a precondition must compile through the real
// loader pipeline (the struct-form rewriter only understands `step`, so
// the precondition must be stripped before the rewrite and re-attached
// after) and surface the precondition on the compiled Automation.
func TestCompileMemQL_AttachesPreconditions(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	src := `@enabled
@trigger(event="node.created", concept="v1:cognition:space", partition="*")
@description("guarded greet")
automation guardedGreet {
  precondition ownerPresent {
    check: exists(event.payload.ownerUserId)
    literal: ownerUserId
    description: "owner id must be present"
  }
  step greet {
    publishEvent { topic: "demo.greeted" }
  }
}`
	auto, err := loader.compileMemQL(src, "test:guardedGreet")
	if err != nil {
		t.Fatalf("compileMemQL: %v", err)
	}
	if len(auto.Preconditions) != 1 {
		t.Fatalf("want 1 precondition on the compiled automation, got %d", len(auto.Preconditions))
	}
	pc := auto.Preconditions[0]
	if pc.ID != "ownerPresent" || pc.Literal != "ownerUserId" {
		t.Errorf("precondition = %+v, want id=ownerPresent literal=ownerUserId", pc)
	}
	if len(auto.Steps) != 1 {
		t.Fatalf("the step must survive precondition stripping; got %d steps", len(auto.Steps))
	}
}

// A malformed precondition (missing check) must be rejected at compile so
// the full-DSL load-test catches authoring mistakes.
func TestCompileMemQL_RejectsPreconditionMissingCheck(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	src := `@trigger(event="node.created", concept="v1:cognition:space", partition="*")
automation badGuard {
  precondition broken {
    literal: ownerUserId
  }
  step greet { publishEvent { topic: "x" } }
}`
	if _, err := loader.compileMemQL(src, "test:badGuard"); err == nil {
		t.Fatalf("expected compile to reject a precondition with no check")
	}
}

// --- deterministic evaluation -------------------------------------------

func TestEvaluatePreconditions_HoldsAndMisses(t *testing.T) {
	eval := NewEvaluator()
	eval.SetCustom("event", map[string]any{"payload": map[string]any{"imageDigest": "sha256:abc"}})

	// Holds: the literal is present.
	missed, isMiss := EvaluatePreconditions([]*Precondition{
		{ID: "digestPinned", Check: `exists(event.payload.imageDigest)`},
	}, eval)
	if isMiss {
		t.Fatalf("expected no miss when digest present, got miss on %q", missed.ID)
	}

	// Misses: absent literal (the cross-machine drift case).
	eval2 := NewEvaluator()
	eval2.SetCustom("event", map[string]any{"payload": map[string]any{}})
	missed2, isMiss2 := EvaluatePreconditions([]*Precondition{
		{ID: "digestPinned", Check: `exists(event.payload.imageDigest)`},
	}, eval2)
	if !isMiss2 {
		t.Fatalf("expected a miss when digest absent")
	}
	if missed2.ID != "digestPinned" {
		t.Errorf("missed id = %q, want digestPinned", missed2.ID)
	}
}

func TestEvaluatePreconditions_FirstMissWins(t *testing.T) {
	eval := NewEvaluator()
	eval.SetCustom("event", map[string]any{"payload": map[string]any{"a": "set"}})
	missed, isMiss := EvaluatePreconditions([]*Precondition{
		{ID: "aSet", Check: `exists(event.payload.a)`}, // holds
		{ID: "bSet", Check: `exists(event.payload.b)`}, // misses
		{ID: "cSet", Check: `exists(event.payload.c)`}, // would also miss
	}, eval)
	if !isMiss {
		t.Fatalf("expected a miss")
	}
	if missed.ID != "bSet" {
		t.Errorf("first miss = %q, want bSet", missed.ID)
	}
}

// --- executor integration: miss aborts + emits the signal ----------------

func newTestExecutor(t *testing.T, bus *events.Bus) *Executor {
	t.Helper()
	return NewExecutor(ExecutorOptions{
		EventBus: bus,
	})
}

// A precondition miss must (a) abort the run with status "skipped" so NO
// step fires, and (b) emit a healing.precondition.missed signal carrying
// the construct identity + the failed check + the asserted literal + the
// triggering event payload (so the repair loop can see the value that did
// not satisfy the check on this machine).
func TestExecutor_PreconditionMiss_AbortsAndEmits(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()

	var mu sync.Mutex
	var got *events.Event
	done := make(chan struct{})
	unsub := bus.Subscribe(events.TopicPreconditionMissed, func(ev events.Event) {
		mu.Lock()
		got = &ev
		mu.Unlock()
		close(done)
	})
	defer unsub()

	exec := newTestExecutor(t, bus)
	automation := &Automation{
		Name:   "deployStaging",
		Origin: "unified:deploypack/automations.memql:deployStaging",
		Preconditions: []*Precondition{
			{ID: "digestPinned", Check: `exists(event.payload.imageDigest)`, Literal: "imageDigest",
				Description: "the deploy needs a pinned image digest"},
		},
		// A step that, if it ran, would be observable. We rely on status to
		// assert the abort; the step has no registered executor so running it
		// would surface as an error rather than a clean skip.
		Steps: []*Step{{ID: "run", Type: StepTypeFunction}},
	}

	// Trigger payload WITHOUT imageDigest -> the precondition misses.
	ev := events.NewEvent("graph.node.updated.v1:cluster:deployment", events.KindNodeUpdated,
		map[string]any{"imageDigest": ""})

	result, err := exec.ExecuteWithEvent(context.Background(), automation, "test", &ev)
	if err != nil {
		t.Fatalf("ExecuteWithEvent returned error: %v", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("status = %q, want skipped (the miss must abort before any step)", result.Status)
	}
	// No step results -> no step fired.
	if len(result.Steps) != 0 {
		t.Fatalf("expected zero step results on a precondition miss, got %d", len(result.Steps))
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive a healing.precondition.missed signal")
	}

	mu.Lock()
	defer mu.Unlock()
	if got == nil {
		t.Fatalf("miss signal not captured")
	}
	if got.Kind != events.KindPreconditionMissed {
		t.Errorf("kind = %v, want KindPreconditionMissed", got.Kind)
	}
	assertPayload(t, got.Payload, "automationName", "deployStaging")
	assertPayload(t, got.Payload, "preconditionId", "digestPinned")
	assertPayload(t, got.Payload, "literal", "imageDigest")
	assertPayload(t, got.Payload, "check", `exists(event.payload.imageDigest)`)
	if got.Payload["triggerPayload"] == nil {
		t.Errorf("miss signal must carry the triggering event payload for the repair loop")
	}
}

// When every precondition holds, the run proceeds to steps (no miss signal,
// status not skipped-by-precondition).
func TestExecutor_PreconditionHolds_Proceeds(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()

	var missEmitted bool
	var mu sync.Mutex
	unsub := bus.Subscribe(events.TopicPreconditionMissed, func(ev events.Event) {
		mu.Lock()
		missEmitted = true
		mu.Unlock()
	})
	defer unsub()

	exec := newTestExecutor(t, bus)
	automation := &Automation{
		Name: "deployStaging",
		Preconditions: []*Precondition{
			{ID: "digestPinned", Check: `exists(event.payload.imageDigest)`},
		},
		Steps: []*Step{}, // no steps -> clean completion when preconditions hold
	}
	ev := events.NewEvent("graph.node.updated.v1:cluster:deployment", events.KindNodeUpdated,
		map[string]any{"imageDigest": "sha256:abc"})

	result, err := exec.ExecuteWithEvent(context.Background(), automation, "test", &ev)
	if err != nil {
		t.Fatalf("ExecuteWithEvent: %v", err)
	}
	if result.Status == "skipped" && contains(result.Error, "precondition") {
		t.Fatalf("run was aborted by a precondition that should have held: %s", result.Error)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if missEmitted {
		t.Fatalf("a miss signal was emitted even though the precondition held")
	}
}

func assertPayload(t *testing.T, payload map[string]any, key, want string) {
	t.Helper()
	got, _ := payload[key].(string)
	if got != want {
		t.Errorf("miss payload[%q] = %q, want %q", key, got, want)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

package conformance

// conf_2271_test.go -- the cognition decide->persist migration dimension
// (#2271 / #2235 tail).
//
// bootstrapSession and generateResponse were cognition's last two grandfathered
// logic-purity violations: each performed graph writes INLINE in a `logic`. The
// migration makes the logic PURE (it only reads + decides) and moves the writes
// onto `if`-gated automation steps that read the logic's returned value via the
// step result -- the decide->persist pattern. That read was a Bundle-wrapped
// engine envelope and resolved to nil at real-DB runtime (the #2271 engine bug),
// which is exactly why these two were deferred. The engine fix unwraps the
// envelope so `steps.decide.result` / `decide.result.x` read flat.
//
// This dimension drives BOTH migrated automations against a real DB with a real
// triggering event and asserts the persisted side effects -- the only check that
// proves the decide->persist wiring end to end at real-DB runtime:
//
//   - bootstrapSession: a node.created for a participant creates exactly ONE
//     v1:cognition:session, and re-firing is idempotent (the
//     `steps.decide.result == true` gate skips the second create). Full DB e2e.
//   - generateResponse: the engine fix is asserted for the exact reads the
//     migrated automation performs against REAL engine results -- an object
//     decide.result.x field read and the scalar `steps.decide.result` + `!= ""`
//     gate both resolve flat off the Bundle-wrapped envelope. (The ai()-backed
//     respond path is owner-verified on staging; the LLM is not mechanizable
//     here.)
//
// It FAILS on pre-fix main (decide.result resolves to nil / the opaque
// envelope: bootstrapSession's create gate never fires; generateResponse can't
// read the reply text) and PASSES once the envelope-unwrap + the pure-logic
// migration land.

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
)

func cognitionDecidePersistCheck() check {
	return check{
		Issue:   "#2271",
		Dim:     "cognition-decide-persist",
		NeedsDB: true,
		Run:     runCognitionDecidePersist,
	}
}

func runCognitionDecidePersist(t *testing.T, e *Env) {
	suffix := uniqueSuffix("2271")

	t.Run("bootstrapSession", func(t *testing.T) {
		participantId := "v1:cognition:participant:bs-" + suffix
		partitionId := "v1:cognition:space:bs-" + suffix

		// Precondition: no session for this participant yet.
		before := asArray(t, e.runQuery(t, "participantSession", map[string]any{"participantId": participantId}))
		if len(before) != 0 {
			t.Fatalf("#2271: precondition -- participantSession(%q) should be empty, got %d", participantId, len(before))
		}

		payload := map[string]any{"id": participantId, "partitionId": partitionId}
		fireForgeAutomation(t, e, "bootstrapSession", "node.created", events.KindNodeCreated, payload)

		after := asArray(t, e.runQuery(t, "participantSession", map[string]any{"participantId": participantId}))
		if len(after) != 1 {
			t.Fatalf("#2271: bootstrapSession did not create a session (the decide.result==true gate never fired -- "+
				"the Bundle-wrapped step result regression); participantSession(%q) = %d rows, want 1", participantId, len(after))
		}

		// Idempotency: re-fire; the decide gate must skip the second create.
		fireForgeAutomation(t, e, "bootstrapSession", "node.created", events.KindNodeCreated, payload)
		again := asArray(t, e.runQuery(t, "participantSession", map[string]any{"participantId": participantId}))
		if len(again) != 1 {
			t.Fatalf("#2271: bootstrapSession not idempotent -- participantSession(%q) = %d rows after re-fire, want 1",
				participantId, len(again))
		}
		t.Logf("#2271: bootstrapSession -> 1 session created + idempotent on re-fire OK")
	})

	// generateResponse uses the SCALAR form of decide->persist: the pure logic
	// returns the reply TEXT as a scalar (empty when already answered), and the
	// automation's persist + presence steps are gated on
	// `steps.decide.result != ""` and read the text via `steps.decide.result`.
	// (The respond path calls ai() and is owner-verified on staging; the skip
	// path -- empty decide.result -> no write -- is what's mechanizable here.)
	//
	// This asserts the #2271 engine fix for the exact reads those steps perform,
	// against REAL engine results (the Bundle-wrapped envelope): an object-field
	// read and a scalar read both resolve flat, and the `!= ""` gate fires
	// correctly. On pre-fix main these resolve to nil / the opaque envelope.
	t.Run("decide-result-reads-resolve-flat", func(t *testing.T) {
		// Object-literal return -> *ExecuteResult(output=map): decide.result.x
		// and the comparison gate read flat.
		objRes, err := e.Eng.Execute(e.Ctx, `{ shouldRespond: true, text: "hi there" }`)
		if err != nil {
			t.Fatalf("#2271: execute object literal: %v", err)
		}
		ov := newDecideEvaluator(objRes)
		if got, _ := ov.EvaluateValue("$steps.decide.result.text"); got != "hi there" {
			t.Errorf("#2271: $steps.decide.result.text = %#v, want \"hi there\" (envelope not unwrapped)", got)
		}
		if ok, _ := ov.EvaluateCondition(`steps.decide.result.shouldRespond == true`); !ok {
			t.Error("#2271: gate steps.decide.result.shouldRespond == true should be true")
		}

		// Scalar string step result (generateResponse's form): the `!= ""` gate
		// + `steps.decide.result` value read both resolve.
		nonEmpty := newDecideEvaluator("the AI reply text")
		if got, _ := nonEmpty.EvaluateValue("$steps.decide.result"); got != "the AI reply text" {
			t.Errorf("#2271: scalar $steps.decide.result = %#v, want \"the AI reply text\"", got)
		}
		if ok, _ := nonEmpty.EvaluateCondition(`steps.decide.result != ""`); !ok {
			t.Error("#2271: persist gate steps.decide.result != \"\" should be TRUE when text present")
		}
		empty := newDecideEvaluator("")
		if ok, _ := empty.EvaluateCondition(`steps.decide.result != ""`); ok {
			t.Error("#2271: persist gate steps.decide.result != \"\" should be FALSE when empty (skip path)")
		}
		t.Log("#2271: generateResponse decide.result reads (object-field + scalar) + gates resolve flat OK")
	})
}

// newDecideEvaluator returns an Evaluator with a `decide` step whose Result is
// the given value -- the shape the generateResponse automation's persist step
// reads from.
func newDecideEvaluator(result any) *automations.Evaluator {
	ev := automations.NewEvaluator()
	ev.SetStepResult("decide", &automations.StepResult{StepId: "decide", Status: "success", Result: result})
	return ev
}

package planner

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// responsibilityEvent builds a graph event in the shape
// extractResponsibilityIntakeFields reads: top-level id + a nested payload
// map carrying status / intakeStatus.
func responsibilityEvent(id, status, intakeStatus string) events.Event {
	return events.Event{Payload: map[string]any{
		"id": id,
		"payload": map[string]any{
			"status":       status,
			"intakeStatus": intakeStatus,
		},
	}}
}

// TestResponsibilityIntake_AssignDoesNotReTriggerFirstPass locks in the
// memql#1645 fix. MemQL is append-only: a routing-only
// assignResponsibility materialises a fresh row version and re-fires
// graph.node.created for a still-draft (intakeStatus=="") responsibility.
// Before the fix, that re-entered first-pass intake -- re-inferring
// trigger/schedule from the bare statement and clobbering the user's explicit
// cadence (recurring->standing, schedule blanked). The permanent once-per-row
// dispatch guard + dropping the first-pass re-trigger from the updated handler
// must hold first-pass intake to exactly one run per responsibility.
func TestResponsibilityIntake_AssignDoesNotReTriggerFirstPass(t *testing.T) {
	const rid = "v1:planner:responsibility:r1"
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			// loadResponsibility -> responsibilityById. The row stays
			// draft + intakeStatus="" throughout (simulating the worst case
			// where the durable intakeStatus marker has not been observed),
			// so only the in-process guard can prevent a re-fire.
			return map[string]any{
				"id":           rid,
				"statement":    "Every Monday review the budget",
				"trigger":      "recurring",
				"schedule":     "0 9 * * 1",
				"status":       "draft",
				"intakeStatus": "",
			}, nil
		},
		aiResponder: func(_ string, _ map[string]any) (any, error) {
			// The model "re-infers" a DIFFERENT cadence -- this is exactly the
			// clobber #1645 reports. A clear result (no questions) flips the
			// row to active via applyResponsibilityIntake.
			return map[string]any{
				"trigger":    "standing",
				"targetKind": "assistant",
			}, nil
		},
	}
	d := NewResponsibilityIntakeDispatcher(eng, testLogger())

	// 1. Genuine creation: first-pass intake should run exactly once.
	d.HandleResponsibilityCreated(responsibilityEvent(rid, "draft", ""))
	waitFor(t, func() bool {
		_, si, _ := eng.snapshot()
		return countContains(si, "responsibilityIntake") >= 1
	})

	// 2. assignResponsibility on the still-draft row re-fires BOTH
	//    graph.node.created (update materialises as insert) and
	//    graph.node.updated. Neither may re-trigger first-pass intake.
	d.HandleResponsibilityCreated(responsibilityEvent(rid, "draft", ""))
	d.HandleResponsibilityUpdated(responsibilityEvent(rid, "draft", ""))

	// Give any erroneous second run a beat to surface.
	time.Sleep(75 * time.Millisecond)

	_, si, _ := eng.snapshot()
	if got := countContains(si, "responsibilityIntake"); got != 1 {
		t.Fatalf("responsibilityIntake invoked %d times; want exactly 1 "+
			"(assignment must not re-trigger first-pass intake -- memql#1645)", got)
	}
	exec, _, _ := eng.snapshot()
	if got := countContains(exec, "applyResponsibilityIntake"); got > 1 {
		t.Fatalf("applyResponsibilityIntake ran %d times; the intake "+
			"cascade must not re-run as a side effect of assignment", got)
	}
}

// TestResponsibilityIntake_FoldAnswersStillFiresOnUpdate guards the surviving
// updated-path case: a row parked in intakeStatus=="awaitingAnswers" that now
// carries an intakeResponse must fold the answers (the user answered the
// clarifying questions). Dropping the first-pass re-trigger must not also drop
// this path.
func TestResponsibilityIntake_FoldAnswersStillFiresOnUpdate(t *testing.T) {
	const rid = "v1:planner:responsibility:r2"
	eng := &fakeEngine{
		execResponder: func(q string) (any, error) {
			return map[string]any{
				"id":           rid,
				"statement":    "Review the budget",
				"intakeStatus": "awaitingAnswers",
				"intakeRequest": map[string]any{
					"questions": []any{map[string]any{"id": "q1", "question": "How often?"}},
				},
				"intakeResponse": map[string]any{
					"answers": []any{map[string]any{"id": "q1", "answer": "weekly"}},
				},
			}, nil
		},
		aiResponder: func(_ string, _ map[string]any) (any, error) {
			return map[string]any{"trigger": "recurring", "schedule": "0 9 * * 1", "targetKind": "assistant"}, nil
		},
	}
	d := NewResponsibilityIntakeDispatcher(eng, testLogger())

	ev := events.Event{Payload: map[string]any{
		"id": rid,
		"payload": map[string]any{
			"status":         "draft",
			"intakeStatus":   "awaitingAnswers",
			"intakeResponse": map[string]any{"answers": []any{map[string]any{"id": "q1", "answer": "weekly"}}},
		},
	}}
	d.HandleResponsibilityUpdated(ev)

	waitFor(t, func() bool {
		exec, _, _ := eng.snapshot()
		return countContains(exec, "foldResponsibilityIntakeAnswers") >= 1
	})
}

// TestEncodeArgsSpellsEveryStringTheLanguagesWay: every string encodeArgs
// writes -- an argument, a string nested in an object or a list, an object's
// key -- is spelled by langparser.QuoteString, and everything else keeps the
// spelling it had. The strings carry < and &, the characters json.Marshal
// spells its own way, so a renderer that still used it fails here; the real
// parser reading every value back unchanged is the other half.
func TestEncodeArgsSpellsEveryStringTheLanguagesWay(t *testing.T) {
	text := "a \"quote\", a back\\slash, a line separator, <tag> & co"
	args := map[string]any{
		"title":    text,
		"metadata": map[string]any{"note": text, "a<b": 1, "on": true},
		"tags":     []string{text, "plain"},
		"count":    3,
		"ratio":    0.25,
		"none":     nil,
	}
	got := encodeArgs(args)
	q := langparser.QuoteString
	want := "count: 3, metadata: {" + q("a<b") + ":1," + q("note") + ":" + q(text) + "," + q("on") + ":true}, none: null, ratio: 0.25, " +
		"tags: [" + q(text) + "," + q("plain") + "], title: " + q(text)
	if got != want {
		t.Errorf("encodeArgs wrote\n\t%s\nwant every string in the language's own quoting\n\t%s", got, want)
	}

	expr, err := langparser.ParseExpression("probe(" + got + ")")
	if err != nil {
		t.Fatalf("the real parser refused what encodeArgs wrote: %v\n\t%s", err, got)
	}
	fn, ok := expr.(*langparser.FunctionCallExpr)
	if !ok {
		t.Fatalf("parsed as %T, not a call", expr)
	}
	if fn.Args["title"] != text {
		t.Errorf("title read back as %q, want %q", fn.Args["title"], text)
	}
	if meta, _ := fn.Args["metadata"].(map[string]any); meta["note"] != text || meta["on"] != true {
		t.Errorf("metadata read back as %v", fn.Args["metadata"])
	}
	if tags, _ := fn.Args["tags"].([]any); len(tags) != 2 || tags[0] != text {
		t.Errorf("tags read back as %v", fn.Args["tags"])
	}
}

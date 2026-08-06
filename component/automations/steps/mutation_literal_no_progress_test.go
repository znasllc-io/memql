package steps

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/literalparity"

	"github.com/znasllc-io/memql/component/automations"
)

// parserDeadline is the shared watchdog window; see
// literalparity.ParserDeadline for the value and the measurements behind
// it. Declared once there rather than three times here.
const parserDeadline = literalparity.ParserDeadline

// leaked latches once a case abandons a non-terminating goroutine. It is
// package-level on purpose: a per-loop `break` bounds one loop, but the NEXT
// group or the next Test function in the package still starts, so each would
// leak its own spinner. Tests in a package run sequentially (nothing here
// calls t.Parallel) and t.Run blocks until the subtest returns, so a plain
// bool needs no synchronisation.
var leaked bool

// mustTerminate runs fn and reports whether it returned in time.
//
// It returns a bool rather than failing the test itself, and every caller
// STOPS its loop on false. That is load-bearing, not style: a leaked spinner
// here is an unbounded `append` growing about a gigabyte a second, and
// `t.Fatalf` inside a `t.Run` subtest aborts only that SUBTEST -- the parent's
// range loop starts the next case immediately. A first cut did exactly that
// and measured 8 concurrent spinners at 23 GiB peak RSS, so on a normal runner
// a regressed guard produced an OOM-killed job with a truncated log instead of
// a readable red test.
//
// Between the `leaked` latch and the callers' `break`, a regressed guard
// leaves at most ONE live spinner in this package: the first case to blow the
// deadline reports the failure, and every later case skips instead of adding
// another goroutine to race the runner's memory limit.
//
// What that does and does NOT buy, stated exactly. The spinner is not
// cancellable -- these parsers take no context -- so it keeps growing until
// the process exits, and its peak therefore tracks how much memory the box
// will hand out, NOT any fixed bound. The latch makes the actionable
// `--- FAIL` line the FIRST thing printed in the common case, which is the
// difference between a run that says which guard regressed and one that does
// not.
//
// It is NOT a guarantee, and an earlier version of this comment claimed it was
// (memql#2835). Measured with the array guard neutralised under
// `ulimit -v 6000000`: the process died of OOM at 1.89s with ZERO `--- FAIL`
// lines -- before the old 2s deadline. #2828 saw the FAIL print first only
// because it measured under a 10 GiB cap; at a tighter cap, or on a
// memory-limited CI runner, the race inverts.
//
// The deadline is 500ms, and the margin is MEASURED rather than guessed: mean
// parse is 757ns / 821ns / 2.15us across the three copies, so 500ms is
// ~240,000-660,000x, and the worst full round trip observed under `-race` with
// a 5% CPU quota on one core -- an absurd configuration -- was 195ms. So the
// window an allocation storm has to beat is far smaller than at 2s, but "far
// smaller" is the honest claim, not "cannot happen".
func mustTerminate(t *testing.T, name string, fn func()) bool {
	t.Helper()
	if leaked {
		t.Skipf("skipped: an earlier case left a non-terminating goroutine running; "+
			"fix that one first (%s)", name)
		return false
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(parserDeadline):
		leaked = true
		t.Errorf("%s did not terminate within %s -- the parser loop is not making progress", name, parserDeadline)
		return false
	}
}

// TestRuntimeLiteralParsersTerminate locks in the memql#2785 defect in the
// RUNTIME copy of the literal parser.
//
// This is the highest-stakes of the three copies. The other two run at load,
// where a hang is a node that fails to boot; this one is dispatched per value
// inside evaluateValue at EVENT-DISPATCH time, so a malformed literal spins an
// automation worker on a live node with unbounded memory growth.
func TestRuntimeLiteralParsersTerminate(t *testing.T) {
	e := &MutationExecutor{}
	ev := automations.NewEvaluator()

	t.Run("array", func(t *testing.T) {
		for _, src := range []string{"[}{]", "[}]", "[)(]", "[]]", "[{]", `["unterminated]`} {
			if !t.Run(src, func(t *testing.T) {
				mustTerminate(t, "parseAndEvaluateArrayLiteral("+src+")", func() {
					_, _ = e.parseAndEvaluateArrayLiteral(ev, src)
				})
			}) {
				break // one live spinner at a time -- see mustTerminate
			}
		}
	})

	t.Run("object", func(t *testing.T) {
		for _, src := range []string{"{ k: [}{] }", "{ k: [)(] }", "{ a: 1, b: [}{] }"} {
			if !t.Run(src, func(t *testing.T) {
				mustTerminate(t, "parseAndEvaluateObjectLiteral("+src+")", func() {
					_, _ = e.parseAndEvaluateObjectLiteral(ev, src)
				})
			}) {
				break // one live spinner at a time -- see mustTerminate
			}
		}
	})
}

// TestRuntimeMalformedLiteralErrors -- terminating is necessary but not
// sufficient: the dispatch must FAIL rather than quietly write a partial value
// into the row.
//
// Every input here is malformed, so all of them go through mustTerminate.
// Calling a parser directly with malformed input would bypass the `leaked`
// latch and, with a guard regressed, leak a live spinner -- the exact
// unbounded growth the latch exists to bound.
//
// (`["abc]` and `[[1, 2]` are NOT among the hang test's inputs; the rest
// overlap it. Two earlier versions of this sentence were wrong -- first
// claiming all of them were fed by the hang test, then miscounting the
// overlap -- so it no longer states a count. If you need the overlap, read
// the two lists.)
//
// The list covers BOTH halves of the array loop's `!ok || newPos <= pos`
// guard. It originally held only the first three, which are all NO-PROGRESS
// cases the position half already catches -- so `!ok` could be deleted and
// this package stayed green, even though dropping it silently turns a
// malformed element into an empty one:
//
//	["abc]   with !ok -> error       without -> []any{""}, err=nil
//	[{]      with !ok -> error       without -> []any{""}, err=nil
//	[[1, 2]  with !ok -> error       without -> []any{""}, err=nil
//
// Those report ok=false HAVING ADVANCED to len(s), which is exactly what the
// position half cannot see.
//
// The three copies are NOT mirror images of each other -- the load-bearing
// sets are nested, and the position half carries every one of them:
//
//	component/memql              {position}          `!ok` is dead
//	component/language/compiler  {position, !ok}     both live
//	this copy                    {position, !ok}     both live
//
// So `!ok` needs pinning HERE and in the compiler, while memql needs the
// opposite -- TestArrayOkImpliesNoProgress pins that its `!ok` stays dead. A
// test in one copy is not evidence about another; an earlier version of this
// comment called the relationship an "inversion" with "opposite halves",
// which is wrong in both directions.
func TestRuntimeMalformedLiteralErrors(t *testing.T) {
	e := &MutationExecutor{}
	ev := automations.NewEvaluator()
	for _, src := range []string{
		// no-progress -- caught by the position half
		"[}{]", "[}]", "[]]",
		// advanced-then-failed -- caught ONLY by `!ok`
		`["abc]`, "[{]", "[[1, 2]",
	} {
		if !mustTerminate(t, "parseAndEvaluateArrayLiteral("+src+")", func() {
			if _, err := e.parseAndEvaluateArrayLiteral(ev, src); err == nil {
				t.Errorf("parseAndEvaluateArrayLiteral(%q) = nil error, want a malformed-literal error", src)
			}
		}) {
			break
		}
	}
}

// TestRuntimeCrossCopyParity pins the RUNTIME copy's half of the shared
// accept/reject table.
//
// The corpus itself lives in component/memql/literalparity (memql#2835). It
// used to be duplicated verbatim in all three files under a comment reading
// "keep the three tables identical", with nothing enforcing it -- protection
// against drift that was itself three copies kept in step by a comment.
//
// This copy is the one that diverged on `{ k: "abc }`: parseValueFromString
// returned the partial text on an unterminated string, so a literal that
// FAILED to compile still DISPATCHED, as {"k":"abc"}.
func TestRuntimeCrossCopyParity(t *testing.T) {
	e := &MutationExecutor{}
	ev := automations.NewEvaluator()
	for _, tc := range literalparity.Cases {
		if !mustTerminate(t, "parseAndEvaluateObjectLiteral("+tc.Src+")", func() {
			v, err := e.parseAndEvaluateObjectLiteral(ev, tc.Src)
			got := "ERR"
			if err == nil {
				b, mErr := json.Marshal(v)
				if mErr != nil {
					t.Fatalf("marshal %q: %v", tc.Src, mErr)
				}
				got = string(b)
			}
			if got != tc.Steps {
				t.Errorf("parseAndEvaluateObjectLiteral(%q):\n  got  %s\n  want %s\n\n"+
					"The corpus records what each of the three copies produces, MEASURED. A "+
					"mismatch means this copy drifted -- or that a divergence was fixed, in "+
					"which case update component/memql/literalparity and check whether the row "+
					"is now agreed (memql#2835).", tc.Src, got, tc.Steps)
			}
		}) {
			break
		}
	}
}

// TestRuntimeValidLiteralsUnchanged guards against over-eager bailing on real
// automation payloads.
func TestRuntimeValidLiteralsUnchanged(t *testing.T) {
	e := &MutationExecutor{}
	ev := automations.NewEvaluator()

	arr, err := e.parseAndEvaluateArrayLiteral(ev, `["a", "b"]`)
	if err != nil {
		t.Fatalf("valid array errored: %v", err)
	}
	if len(arr) != 2 {
		t.Errorf("array = %#v, want 2 elements", arr)
	}

	obj, err := e.parseAndEvaluateObjectLiteral(ev, `{ k: "v", n: 1 }`)
	if err != nil {
		t.Fatalf("valid object errored: %v", err)
	}
	if len(obj) != 2 {
		t.Errorf("object = %#v, want 2 keys", obj)
	}
}

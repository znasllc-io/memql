package steps

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

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
// will hand out, NOT any fixed bound. What the latch guarantees is that the
// actionable `--- FAIL` line is printed at the 2s deadline, long before any
// later OOM can truncate the log. That ordering is the whole point: a run that
// dies at 6 GiB having already said which guard regressed is debuggable, and
// one that dies without saying is not.
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
	case <-time.After(2 * time.Second):
		leaked = true
		t.Errorf("%s did not terminate within 2s -- the parser loop is not making progress", name)
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
// (Three of the six are also fed by the hang test; `["abc]` and `[[1, 2]`
// are not. An earlier version of this comment said all of them were, which
// stopped being true the moment those two were added.)
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
// position half cannot see. Note the inversion against the sibling: in
// component/memql `!ok` is DEAD and TestArrayOkImpliesNoProgress pins that it
// stays dead; here it is LIVE and these rows pin that it stays live. Same
// expression, opposite halves load-bearing -- a test in one copy is not
// evidence about the other.
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
// accept/reject table. The list is duplicated verbatim in
// component/language/compiler and component/memql -- see the comment on
// crossCopyParityCases there for why the three must agree.
//
// This copy is the one that diverged on `{ k: "abc }`: parseValueFromString
// returned the partial text on an unterminated string, so a literal that
// FAILED to compile still DISPATCHED, as {"k":"abc"}.
func TestRuntimeCrossCopyParity(t *testing.T) {
	e := &MutationExecutor{}
	ev := automations.NewEvaluator()
	for _, tc := range []struct {
		src    string
		accept bool
	}{
		{`{ k: "ok" }`, true},
		{`{ k: }`, true},
		{`{k:}`, true},
		{`{ k: [,] }`, true},
		{`{ k: ["a", "b"] }`, true},
		{`{ punct: "commas, braces } brackets ]" }`, true},
		{`{ a: { b: "}" } }`, true},
		{`{ a: { b: "a}b" }, c: 1 }`, true},
		{`{ k: "abc }`, false},
		{`{ k: [}{] }`, false},

		// Unbalanced NESTED literals -- see the compiler-side table for why
		// the third case matters (an unterminated string one level down,
		// swallowed by the enclosing arm's runoff).
		{`{ a: { b: 1 }`, false},
		{`{ a: [ 1, 2 }`, false},
		{`{ a: { b: """ } }`, false},

		{`{ a: { b: 1 } }`, true},
		{`{ a: [ 1, 2 ] }`, true},
		{`{ name: "x", nested: { deep: { deeper: 1 } } }`, true},
	} {
		if !mustTerminate(t, "parseAndEvaluateObjectLiteral("+tc.src+")", func() {
			_, err := e.parseAndEvaluateObjectLiteral(ev, tc.src)
			if got := err == nil; got != tc.accept {
				t.Errorf("parseAndEvaluateObjectLiteral(%q): accepted=%v, want %v (err=%v)",
					tc.src, got, tc.accept, err)
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

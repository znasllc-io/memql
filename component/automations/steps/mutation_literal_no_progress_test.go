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
// These inputs are the same MALFORMED literals the hang test feeds, so they go
// through mustTerminate as well. Calling the parser directly would bypass the
// `leaked` latch and, with the guard regressed, leak a second live spinner
// next to the one the hang test already stopped on -- the exact unbounded
// growth the latch exists to bound.
func TestRuntimeMalformedLiteralErrors(t *testing.T) {
	e := &MutationExecutor{}
	ev := automations.NewEvaluator()
	for _, src := range []string{"[}{]", "[}]", "[]]"} {
		if !mustTerminate(t, "parseAndEvaluateArrayLiteral("+src+")", func() {
			if _, err := e.parseAndEvaluateArrayLiteral(ev, src); err == nil {
				t.Errorf("parseAndEvaluateArrayLiteral(%q) = nil error, want a malformed-literal error", src)
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

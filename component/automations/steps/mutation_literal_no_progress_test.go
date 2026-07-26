package steps

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// mustTerminate runs fn and reports whether it returned in time.
//
// It returns a bool rather than failing the test itself, and every caller
// STOPS its loop on false. That is load-bearing, not style: a leaked spinner
// here is an unbounded `append` growing about a gigabyte a second, and
// `t.Fatalf` inside a `t.Run` subtest aborts only that SUBTEST -- the parent's
// range loop starts the next case immediately. A first cut did exactly that
// and measured 8 concurrent spinners at 23 GiB peak RSS, so on a normal runner
// a regressed guard produced an OOM-killed job with a truncated log instead of
// a readable red test. Stopping the loop keeps at most one spinner alive.
func mustTerminate(t *testing.T, name string, fn func()) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(2 * time.Second):
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
func TestRuntimeMalformedLiteralErrors(t *testing.T) {
	e := &MutationExecutor{}
	ev := automations.NewEvaluator()
	for _, src := range []string{"[}{]", "[}]", "[]]"} {
		if _, err := e.parseAndEvaluateArrayLiteral(ev, src); err == nil {
			t.Errorf("parseAndEvaluateArrayLiteral(%q) = nil error, want a malformed-literal error", src)
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

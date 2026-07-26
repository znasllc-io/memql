package memql

import (
	"testing"
	"time"
)

// leaked latches once a case abandons a non-terminating goroutine. It is
// package-level on purpose: a per-loop `break` bounds one loop, but the NEXT
// Test function in the package still starts, so each would leak its own
// spinner. Tests in a package run sequentially (nothing here calls
// t.Parallel) and t.Run blocks until the subtest returns, so a plain bool
// needs no synchronisation.
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

// TestParseArrayLiteralTerminatesOnUnbalancedInput locks in memql#2785.
//
// parseArrayLiteral appended whatever parseLiteralOrExpr returned and assigned
// `pos = next` with no check that `next` advanced. On an unbalanced literal --
// or on a stray depth-0 closer -- parseLiteralOrExpr returns its input `pos`
// unchanged, so the loop appended nil forever: an unbounded memory grow plus a
// hang, not a panic.
//
// It matters at LOAD (parsePayloadRawToTemplate runs while the tree is read),
// so a malformed payload literal in an authored .memql file would hang the
// node at boot instead of failing the strict-boot gate.
//
// Every case here was verified to hang on origin/main. `[)(]` is deliberately
// NOT in this list: scanBalanced only tracks the bracket kind it was asked
// for, so a paren mismatch inside `[...]` advances where a brace mismatch does
// not -- it terminated before the fix and asserts nothing about it. That
// asymmetry is pinned in TestParenMismatchInsideArrayIsNotAHang instead.
func TestParseArrayLiteralTerminatesOnUnbalancedInput(t *testing.T) {
	for _, src := range []string{
		"[}{]",
		"[}]",
		"[ } { ]",
		"[a, }{]",
		"[[}{]]",
		`["ok", }{]`,
		"[]]",
		"[{]",
		`["unterminated]`,
		"[[)]",
	} {
		if !t.Run(src, func(t *testing.T) {
			mustTerminate(t, "parseArrayLiteral("+src+")", func() { parseArrayLiteral(src) })
		}) {
			break // one live spinner at a time -- see mustTerminate
		}
	}
}

// TestParseObjectLiteralTerminatesOnUnbalancedInput -- `{ k: [}{] }` is the
// issue's own repro. It is saved by the ARRAY guard (the object loop cannot
// spin: after a non-advancing assignment the next iteration's leading skip
// consumes the comma, and otherwise parseObjectKey either advances or bails),
// so these cases pin the entry point rather than a second spin site.
func TestParseObjectLiteralTerminatesOnUnbalancedInput(t *testing.T) {
	for _, src := range []string{
		"{ k: [}{] }",
		"{ a: 1, b: [}{] }",
		"{ k: [[}{]] }",
		`{ k: ["unterminated] }`,
	} {
		if !t.Run(src, func(t *testing.T) {
			mustTerminate(t, "parseObjectLiteral("+src+")", func() { parseObjectLiteral(src) })
		}) {
			break // one live spinner at a time -- see mustTerminate
		}
	}
}

// TestMalformedPayloadFailsLoudly is the half a bare no-progress guard got
// WRONG. Returning nil from the array parser stopped the hang but left the
// nil indistinguishable from a legitimate null, so `{ k: [}{] }` loaded
// `k: null` with err=nil -- a silent wrong value at boot, which is worse than
// the hang it replaced. parseLiteralOrExpr now reports well-formedness
// explicitly so the failure reaches the caller's error path.
//
// Every case here feeds a MALFORMED array to the same loop the hang tests
// exercise, so it must go through mustTerminate too. Calling the parser
// directly would sidestep the `leaked` latch entirely: with the guard
// regressed, this test spun up a SECOND live spinner alongside the one the
// hang test had already stopped on, and the pair reached 25.9 GiB before the
// package timeout fired. Only tests whose inputs are all well-formed may call
// a parser directly -- those cannot spin no matter what a guard does.
func TestMalformedPayloadFailsLoudly(t *testing.T) {
	for _, src := range []string{
		"{ k: [}{] }",
		"{ a: 1, b: [}{] }",
		`{ k: ["a", }{] }`,
		"{ k: [}] }",
	} {
		if !mustTerminate(t, "parsePayloadRawToTemplate("+src+")", func() {
			tpl, err := parsePayloadRawToTemplate(src)
			if err == nil {
				t.Errorf("parsePayloadRawToTemplate(%q) = %v, err=nil; want a malformed-payload error", src, tpl)
			}
		}) {
			break
		}
	}
}

// TestEmptyValueIsNullNotAnError guards the distinction the ok result exists
// to preserve: an empty value is a null, not a malformed literal. `{k:}` and
// `{ k: }` differ by one space and must not disagree -- a position-based guard
// in the object loop made exactly that mistake.
func TestEmptyValueIsNullNotAnError(t *testing.T) {
	cases := map[string]int{
		"{ k: }":        1,
		"{k:}":          1,
		"{k:,m:1}":      2,
		"{ k: , m: 1 }": 2,
		`{ k: null }`:   1,
		`{ k: "" }`:     1,
		`{ k: [] }`:     1,
		`{ k: {} }`:     1,
	}
	for src, wantLen := range cases {
		t.Run(src, func(t *testing.T) {
			tpl, err := parsePayloadRawToTemplate(src)
			if err != nil {
				t.Fatalf("parsePayloadRawToTemplate(%q) errored: %v", src, err)
			}
			if len(tpl) != wantLen {
				t.Errorf("parsePayloadRawToTemplate(%q) = %v (%d keys), want %d", src, tpl, len(tpl), wantLen)
			}
		})
	}
}

// TestObjectValueOkGuardIsLive covers the object loop's `!ok` check, which is
// the one guard in this change that is neither a hang-stopper nor dead code:
// it rejects a malformed value that the ARRAY guard cannot see because the
// value is not an array.
//
// `{k:{:}` is the minimal input that actually DEPENDS on the guard. Verified
// by neutralizing it (`if false && !ok`):
//
//	with the guard:    err = "invalid object literal payload"
//	without the guard: map[string]any{"k": nil, "{": nil}, err = nil
//
// The obvious-looking candidates do NOT work. An unterminated string value
// (`{ k: "unterminated }`) errors either way: the next iteration's
// parseObjectKey hits the same unterminated quote, returns "", and bails, so
// the guard is never the deciding step. An earlier version of this test used
// exactly those inputs and passed with the guard neutralized -- a vacuous test
// standing in for the untested guard it was written to rule out.
func TestObjectValueOkGuardIsLive(t *testing.T) {
	for _, src := range []string{
		`{k:{:}`,
		`{ a: 1, k:{:}`,
	} {
		if !mustTerminate(t, "parsePayloadRawToTemplate("+src+")", func() {
			tpl, err := parsePayloadRawToTemplate(src)
			if err == nil {
				t.Errorf("parsePayloadRawToTemplate(%q) = %#v, err=nil; want a malformed-payload error", src, tpl)
			}
		}) {
			break
		}
	}
}

// TestSeparatorOnlyArrayIsEmptyNotMalformed -- `[,]` appends nothing but is
// well-formed. parseArrayLiteral built its slice with `var out []any`, so a
// separator-only input returned nil, and the ok-propagation then read that nil
// as a parse failure: `{ k: [,] }` errored where it had yielded null.
func TestSeparatorOnlyArrayIsEmptyNotMalformed(t *testing.T) {
	for _, src := range []string{"[,]", "[ , ]", "[,,]", "[]", "[ ]"} {
		got := parseArrayLiteral(src)
		if got == nil {
			t.Errorf("parseArrayLiteral(%q) = nil (malformed); want an empty slice", src)
		}
	}
	for _, src := range []string{"{ k: [,] }", "{ k: [ , ] }"} {
		tpl, err := parsePayloadRawToTemplate(src)
		if err != nil {
			t.Errorf("parsePayloadRawToTemplate(%q) errored: %v", src, err)
		}
		if len(tpl) != 1 {
			t.Errorf("parsePayloadRawToTemplate(%q) = %v, want one key", src, tpl)
		}
	}
}

// TestLoadCrossCopyParity pins the LOAD copy's half of the shared
// accept/reject table. The list is duplicated verbatim in
// component/language/compiler and component/automations/steps -- see the
// comment on crossCopyParityCases there for why the three must agree.
func TestLoadCrossCopyParity(t *testing.T) {
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
		if !mustTerminate(t, "parsePayloadRawToTemplate("+tc.src+")", func() {
			_, err := parsePayloadRawToTemplate(tc.src)
			if got := err == nil; got != tc.accept {
				t.Errorf("parsePayloadRawToTemplate(%q): accepted=%v, want %v (err=%v)",
					tc.src, got, tc.accept, err)
			}
		}) {
			break
		}
	}
}

// TestArrayOkImpliesNoProgress pins the invariant that makes the `!ok` half of
// parseArrayLiteral's guard dead code: every ok=false return from
// parseLiteralOrExpr leaves the position at or below where it started (after
// the caller's own whitespace skip), so `next <= pos` alone already covers it.
//
// The guard keeps `!ok` anyway, as defence against a future parseLiteralOrExpr
// that reports failure AFTER advancing -- at which point the position check
// would silently stop covering the case and a malformed element would become a
// silent nil. This test is what turns that day into a red test. If it starts
// failing, do not delete it: the `!ok` guard has just become load-bearing.
func TestArrayOkImpliesNoProgress(t *testing.T) {
	// The invariant is scoped to the loop's PRECONDITION, and that scope is
	// the whole reason it holds. parseArrayLiteral skips whitespace and commas
	// before every call, so it never calls at such a position. Without that
	// filter the invariant is simply false -- parseLiteralOrExpr skips leading
	// whitespace internally and then fails, so `parseLiteralOrExpr(" {", 0)`
	// reports !ok having advanced 0 -> 1. Measured over the SAME enumeration
	// this test runs, minus the precondition filter: 947 such violations
	// across 43,210 (input, position) pairs, every one of them at a whitespace
	// or comma position the loop cannot reach. The filter leaves 34,568.
	//
	// (43,210 = sum over d in 1..4 of 10^d * d, the alphabet being 10 runes
	// and the position bound `pos < len`. An earlier note said 54,320, which
	// is the `pos <= len` bound -- it counts the end-of-input position, where
	// parseLiteralOrExpr trivially returns ok=true and nothing is at stake.)
	//
	// The sibling copy in component/language/compiler is the mirror image:
	// there `!ok` IS load-bearing, because its unterminated-string and
	// unclosed-`{` arms report failure HAVING ADVANCED, so a position check
	// alone would miss them.
	skippedByLoop := func(c byte) bool {
		return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ','
	}

	// Every reachable position of every string over a bracket-heavy alphabet,
	// which is where the ok=false arms live.
	alphabet := []rune(`{}[],:"a1 `)
	var gen func(prefix string, depth int)
	checked, violations := 0, 0
	gen = func(prefix string, depth int) {
		if depth == 0 {
			for pos := 0; pos < len(prefix); pos++ {
				if skippedByLoop(prefix[pos]) {
					continue // unreachable from parseArrayLiteral's call site
				}
				_, next, ok := parseLiteralOrExpr(prefix, pos)
				checked++
				if !ok && next > pos {
					violations++
					if violations <= 5 {
						t.Errorf("parseLiteralOrExpr(%q, %d) reported !ok but advanced %d -> %d; "+
							"the `next <= pos` check no longer covers !ok, so the `!ok` guard in "+
							"parseArrayLiteral is now load-bearing", prefix, pos, pos, next)
					}
				}
			}
			return
		}
		for _, r := range alphabet {
			gen(prefix+string(r), depth-1)
		}
	}
	for d := 1; d <= 4; d++ {
		gen("", d)
	}
	t.Logf("checked %d (input, position) pairs; %d violations", checked, violations)
}

// TestParenMismatchInsideArrayIsNotAHang records an asymmetry worth pinning so
// it is not rediscovered: scanBalanced tracks only the bracket kind it was
// asked for, so `[)(]` advances and parses as a scalar where `[}{]` does not
// advance at all.
func TestParenMismatchInsideArrayIsNotAHang(t *testing.T) {
	mustTerminate(t, `parseArrayLiteral("[)(]")`, func() { parseArrayLiteral("[)(]") })
	got := parseArrayLiteral("[)(]")
	if len(got) != 1 || got[0] != ")(" {
		t.Errorf(`parseArrayLiteral("[)(]") = %#v, want [")("]`, got)
	}
}

// TestParseLiteralsStillAcceptValidInput guards against a guard that bails too
// eagerly and silently breaks real payloads.
func TestParseLiteralsStillAcceptValidInput(t *testing.T) {
	arr := parseArrayLiteral(`["a", "b", 3, true, null]`)
	if len(arr) != 5 {
		t.Fatalf("parseArrayLiteral returned %d elements (%v), want 5", len(arr), arr)
	}
	if arr[0] != "a" || arr[2] != int64(3) || arr[3] != true || arr[4] != nil {
		t.Errorf("parseArrayLiteral element mismatch: %#v", arr)
	}
	if got := parseArrayLiteral(`[]`); got == nil || len(got) != 0 {
		t.Errorf("parseArrayLiteral(`[]`) = %#v, want empty non-nil slice", got)
	}
	if got := parseArrayLiteral(`["a",]`); len(got) != 1 {
		t.Errorf("trailing comma should parse: %#v", got)
	}

	obj := parseObjectLiteral(`{ name: "x", count: 2, tags: ["a", "b"], nested: { k: "v" } }`)
	if obj == nil {
		t.Fatal("parseObjectLiteral returned nil for a valid literal")
	}
	if obj["name"] != "x" || obj["count"] != int64(2) {
		t.Errorf("parseObjectLiteral scalar mismatch: %#v", obj)
	}
	if tags, ok := obj["tags"].([]any); !ok || len(tags) != 2 {
		t.Errorf("parseObjectLiteral nested array mismatch: %#v", obj["tags"])
	}
	if _, ok := obj["nested"].(map[string]any); !ok {
		t.Errorf("parseObjectLiteral nested object mismatch: %#v", obj["nested"])
	}
}

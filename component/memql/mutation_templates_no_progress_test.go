package memql

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql/literalparity"
)

// parserDeadline is the shared watchdog window; see
// literalparity.ParserDeadline for the value and the measurements behind
// it. Declared once there rather than three times here.
const parserDeadline = literalparity.ParserDeadline

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
// accept/reject table.
//
// The corpus itself lives in component/memql/literalparity (memql#2835). It
// used to be duplicated verbatim in all three files under a comment reading
// "keep the three tables identical", with nothing enforcing it -- protection
// against drift that was itself three copies kept in step by a comment.
func TestLoadCrossCopyParity(t *testing.T) {
	for _, tc := range literalparity.Cases {
		if !mustTerminate(t, "parsePayloadRawToTemplate("+tc.Src+")", func() {
			v, err := parsePayloadRawToTemplate(tc.Src)
			got := "ERR"
			if err == nil {
				b, mErr := json.Marshal(v)
				if mErr != nil {
					t.Fatalf("marshal %q: %v", tc.Src, mErr)
				}
				got = string(b)
			}
			if got != tc.MemQL {
				t.Errorf("parsePayloadRawToTemplate(%q):\n  got  %s\n  want %s\n\n"+
					"The corpus records what each of the three copies produces, MEASURED. A "+
					"mismatch means this copy drifted -- or that a divergence was fixed, in "+
					"which case update component/memql/literalparity and check whether the row "+
					"is now agreed (memql#2835).", tc.Src, got, tc.MemQL)
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
	// reports !ok having advanced 0 -> 1 -- a violation at a position the loop
	// cannot reach.
	//
	// The test therefore counts BOTH populations and logs them: unfiltered
	// (where violations are expected and non-zero) and array-legal (where the
	// invariant must hold exactly). No count is written down in prose here.
	// Earlier versions of this comment asserted four figures, and two of them
	// were wrong at different times -- a restated number drifts, a printed one
	// cannot. Run the test to see them.
	//
	// The other two copies are supersets, not mirror images -- the position
	// half is load-bearing in all three, and they differ only in whether
	// `!ok` is ALSO needed:
	//
	//	this copy                    {position}          `!ok` is dead
	//	component/language/compiler  {position, !ok}     both live
	//	component/automations/steps  {position, !ok}     both live
	//
	// In those two, the unterminated-string and unbalanced-bracket arms report
	// failure HAVING ADVANCED, so a position check alone would miss them.
	// ("Mirror image" was the earlier wording here and overstated it: nothing
	// is reversed, one term is added.)
	skippedByLoop := func(c byte) bool {
		return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ','
	}

	// Every reachable position of every string over a bracket-heavy alphabet,
	// which is where the ok=false arms live.
	alphabet := []rune(`{}[],:"a1 `)
	var gen func(prefix string, depth int)
	checked, violations := 0, 0
	allChecked, allViolations := 0, 0
	gen = func(prefix string, depth int) {
		if depth == 0 {
			for pos := 0; pos < len(prefix); pos++ {
				_, nextAll, okAll := parseLiteralOrExpr(prefix, pos)
				allChecked++
				if !okAll && nextAll > pos {
					allViolations++
				}
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
	// Both populations, measured rather than asserted. The array-legal count
	// is the invariant under test; the unfiltered one exists so the scope of
	// that invariant is visible instead of described.
	t.Logf("array-legal: checked %d (input, position) pairs; %d violations", checked, violations)
	t.Logf("unfiltered:  checked %d (input, position) pairs; %d violations", allChecked, allViolations)
	if allViolations == 0 {
		t.Errorf("unfiltered violations = 0, want > 0: the precondition filter is " +
			"supposed to be what makes this invariant hold, so if the unscoped " +
			"population is ALSO clean, either the enumeration stopped covering " +
			"whitespace positions or parseLiteralOrExpr changed. Either way the " +
			"scoping comment above is no longer describing reality.")
	}
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

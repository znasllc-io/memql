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
func TestMalformedPayloadFailsLoudly(t *testing.T) {
	for _, src := range []string{
		"{ k: [}{] }",
		"{ a: 1, b: [}{] }",
		`{ k: ["a", }{] }`,
		"{ k: [}] }",
	} {
		t.Run(src, func(t *testing.T) {
			tpl, err := parsePayloadRawToTemplate(src)
			if err == nil {
				t.Fatalf("parsePayloadRawToTemplate(%q) = %v, err=nil; want a malformed-payload error", src, tpl)
			}
		})
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
// value is not an array. Without a case behind it, it would be exactly the
// untested guard the review flagged elsewhere.
func TestObjectValueOkGuardIsLive(t *testing.T) {
	// An unterminated string value: parseLiteralOrExpr's scanQuotedString arm
	// reports !ok, and nothing else in the loop would notice.
	for _, src := range []string{
		`{ k: "unterminated }`,
		`{ a: 1, k: "unterminated }`,
	} {
		if _, err := parsePayloadRawToTemplate(src); err == nil {
			t.Errorf("parsePayloadRawToTemplate(%q) = nil error, want a malformed-payload error", src)
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

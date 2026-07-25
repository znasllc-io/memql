package memql

import (
	"testing"
	"time"
)

// mustTerminate runs fn and fails if it does not return in time.
//
// Two properties matter here, both learned the hard way:
//
//   - A plain call would HANG the package's test binary, so the defect would
//     present as a stuck CI job rather than a red test.
//   - The leaked goroutine on failure is NOT idle -- it is an unbounded
//     `append` growing at roughly a gigabyte per second. A first cut ran every
//     case in its own goroutine and let them all leak, which on a
//     memory-capped runner produced an OOM-killed job with a truncated log
//     instead of a readable failure. So this bails the whole test on the FIRST
//     timeout, keeping at most one spinner alive.
func mustTerminate(t *testing.T, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// FailNow on the parent: a leaked spinner allocates ~1 GB/s, so
		// letting sibling cases start would race the OOM killer.
		t.Fatalf("%s did not terminate within 2s -- the parser loop is not making progress", name)
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
		src := src
		t.Run(src, func(t *testing.T) {
			mustTerminate(t, "parseArrayLiteral("+src+")", func() { parseArrayLiteral(src) })
		})
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
		src := src
		t.Run(src, func(t *testing.T) {
			mustTerminate(t, "parseObjectLiteral("+src+")", func() { parseObjectLiteral(src) })
		})
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

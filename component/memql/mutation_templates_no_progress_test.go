package memql

import (
	"testing"
	"time"
)

// runWithDeadline runs fn and fails the test if it does not return in time.
//
// A plain call would HANG the whole package's test binary rather than fail, so
// the defect this file guards would look like a stuck CI job instead of a red
// test. The goroutine leaks if fn never returns -- acceptable in a test binary
// that is about to fail anyway, and the alternative (no timeout) is worse.
func runWithDeadline(t *testing.T, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not terminate within 2s -- the parser loop is not making progress", name)
	}
}

// TestParseArrayLiteralTerminatesOnUnbalancedInput locks in memql#2785.
//
// parseArrayLiteral appended whatever parseLiteralOrExpr returned and assigned
// `pos = next` with no check that `next` advanced. On an unbalanced literal
// parseLiteralOrExpr returns its input `pos` unchanged, so the loop appended
// nil forever: an unbounded memory grow plus a hang, not a panic.
//
// It matters at LOAD -- parsePayloadRawToTemplate runs while the tree is being
// read -- so a malformed payload literal in an authored .memql file would hang
// the node at boot instead of failing the strict-boot gate with a diagnostic.
func TestParseArrayLiteralTerminatesOnUnbalancedInput(t *testing.T) {
	cases := []string{
		"[}{]",
		"[}]",
		"[ } { ]",
		"[a, }{]",
		"[[}{]]",
		"[)(]",
		`["ok", }{]`,
	}
	for _, src := range cases {
		src := src
		t.Run(src, func(t *testing.T) {
			runWithDeadline(t, "parseArrayLiteral("+src+")", func() {
				parseArrayLiteral(src)
			})
		})
	}
}

// TestParseObjectLiteralTerminatesOnUnbalancedInput -- the sibling loop in
// parseObjectLiteral assigns `pos = next` from the same helper with the same
// missing guard, and it is the entry point the issue reproduces through.
func TestParseObjectLiteralTerminatesOnUnbalancedInput(t *testing.T) {
	cases := []string{
		"{ k: [}{] }",
		"{ k: }{ }",
		"{ k: )( }",
		"{ a: 1, b: [}{] }",
		"{ k: { nested: [}{] } }",
	}
	for _, src := range cases {
		src := src
		t.Run(src, func(t *testing.T) {
			runWithDeadline(t, "parseObjectLiteral("+src+")", func() {
				parseObjectLiteral(src)
			})
		})
	}
}

// TestParsePayloadRawToTemplateTerminatesOnUnbalancedInput exercises the
// load-time entry point named in the issue, so the guard is pinned at the
// altitude where a hang would actually cost a boot.
func TestParsePayloadRawToTemplateTerminatesOnUnbalancedInput(t *testing.T) {
	for _, src := range []string{"{ k: [}{] }", "{ k: )( }"} {
		src := src
		t.Run(src, func(t *testing.T) {
			runWithDeadline(t, "parsePayloadRawToTemplate("+src+")", func() {
				_, _ = parsePayloadRawToTemplate(src)
			})
		})
	}
}

// TestParseLiteralsStillAcceptValidInput guards against a no-progress guard
// that bails too eagerly and breaks legitimate payloads.
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

	obj := parseObjectLiteral(`{ name: "x", count: 2, tags: ["a", "b"], nested: { k: "v" } }`)
	if obj == nil {
		t.Fatal("parseObjectLiteral returned nil for a valid literal")
	}
	if obj["name"] != "x" || obj["count"] != int64(2) {
		t.Errorf("parseObjectLiteral scalar mismatch: %#v", obj)
	}
	tags, ok := obj["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Errorf("parseObjectLiteral nested array mismatch: %#v", obj["tags"])
	}
	if _, ok := obj["nested"].(map[string]any); !ok {
		t.Errorf("parseObjectLiteral nested object mismatch: %#v", obj["nested"])
	}
}

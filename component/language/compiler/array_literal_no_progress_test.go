package compiler

import (
	"testing"
	"time"
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

// TestCompilerParseArrayLiteralTerminates locks in the memql#2785 defect in
// THIS package's copy of the literal parser.
//
// The compiler carries a full duplicate of component/memql's payload parser
// and had the identical missing progress check. It sits on the automation LOAD
// path (component/automations/loader.go -> CompileFile -> compileMutationConfig
// -> parsePayloadRaw), so a malformed payload literal in an authored automation
// hung the node at boot -- exactly the stake the original fix claimed to close
// while only patching the memql-side copy.
func TestCompilerParseArrayLiteralTerminates(t *testing.T) {
	c := &Compiler{}
	for _, src := range []string{
		"[}{]", "[}]", "[)(]", "[]]", "[{]", `["unterminated]`, "[a, }{]",
	} {
		if !t.Run(src, func(t *testing.T) {
			mustTerminate(t, "parseArrayLiteral("+src+")", func() { c.parseArrayLiteral(src) })
		}) {
			break // one live spinner at a time -- see mustTerminate
		}
	}
}

// TestCompilerParseObjectLiteralTerminates -- the object entry point, which is
// what parsePayloadRaw actually calls, and the shape the issue reproduces.
func TestCompilerParseObjectLiteralTerminates(t *testing.T) {
	c := &Compiler{}
	for _, src := range []string{
		"{ k: [}{] }", "{ k: [)(] }", "{ a: 1, b: [}{] }", "{ k: [[}{]] }",
	} {
		if !t.Run(src, func(t *testing.T) {
			mustTerminate(t, "parseObjectLiteral("+src+")", func() { c.parseObjectLiteral(src) })
		}) {
			break // one live spinner at a time -- see mustTerminate
		}
	}
}

// TestCompilerParsesValidLiteralsUnchanged guards against a guard that bails
// too eagerly on real automation payloads.
func TestCompilerParsesValidLiteralsUnchanged(t *testing.T) {
	c := &Compiler{}
	if got := c.parseArrayLiteral(`["a", "b"]`); len(got) != 2 {
		t.Errorf(`parseArrayLiteral(["a","b"]) = %#v, want 2 elements`, got)
	}
	if got := c.parseArrayLiteral(`[]`); got == nil || len(got) != 0 {
		t.Errorf("parseArrayLiteral([]) = %#v, want empty non-nil", got)
	}
	obj := c.parseObjectLiteral(`{ k: "v", n: 1, list: ["a"] }`)
	if obj == nil || obj["k"] != "v" {
		t.Errorf("parseObjectLiteral returned %#v, want a populated map", obj)
	}
}

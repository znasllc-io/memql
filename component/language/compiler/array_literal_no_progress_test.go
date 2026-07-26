package compiler

import (
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/language/parser"
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
// another goroutine to race the runner's memory limit. Every test in this file
// that touches a parser goes through here -- a direct call would sidestep the
// latch and leak a second spinner.
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
			mustTerminate(t, "parseArrayLiteral("+src+")", func() { _, _ = c.parseArrayLiteral(src) })
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
			mustTerminate(t, "parseObjectLiteral("+src+")", func() { _, _ = c.parseObjectLiteral(src) })
		}) {
			break // one live spinner at a time -- see mustTerminate
		}
	}
}

// TestCompilerMalformedPayloadFailsLoudly is memql#2816.
//
// Terminating is necessary but not sufficient. The array guard originally
// stopped the hang with a bare `return nil`, but parseValue wraps a nil []any
// in a NON-NIL `any` and parseObjectLiteral stores it under the key, so the
// nil never reached parsePayloadRaw's own nil check: `{ k: [}{] }` compiled to
// `{"k": null}` with err=nil. The sole caller was `if parsed != nil { ... }`,
// which had no error channel at all -- so a malformed payload became a silent
// null in a compiled automation rather than a refused boot.
//
// The two sibling copies already surface this (component/memql's error reaches
// the strict-boot gate; component/automations/steps' reaches a failed step).
// This asserts the compiler copy now matches them.
func TestCompilerMalformedPayloadFailsLoudly(t *testing.T) {
	c := &Compiler{}
	for _, src := range []string{
		"{ k: [}{] }",
		"{ a: 1, b: [}{] }",
		"{ k: [[}{]] }",
		`{ k: ["a", }{] }`,
		"{ k: [}] }",
	} {
		if !mustTerminate(t, "parsePayloadRaw("+src+")", func() {
			got, err := c.parsePayloadRaw(src)
			if err == nil {
				t.Errorf("parsePayloadRaw(%q) = %#v, err=nil; want a malformed-payload error", src, got)
			}
		}) {
			break
		}
	}
}

// TestCompilerMalformedPayloadReachesCompileStep proves the error is not
// swallowed one frame up. A test that only checks parsePayloadRaw would pass
// even if compileMutationConfig dropped the error on the floor, which is
// exactly what the pre-fix `if parsed != nil` did.
func TestCompilerMalformedPayloadReachesCompileStep(t *testing.T) {
	c := &Compiler{}
	cfg, err := c.compileMutationConfig(&parser.MutationStmt{
		Concept:    "v1:test:thing",
		PayloadRaw: "{ k: [}{] }",
	})
	if err == nil {
		t.Fatalf("compileMutationConfig = %#v, err=nil; want the malformed payload to propagate", cfg)
	}
	if !strings.Contains(err.Error(), "malformed payload literal") {
		t.Errorf("error %q does not name the cause", err)
	}
	if !strings.Contains(err.Error(), "v1:test:thing") {
		t.Errorf("error %q does not name the mutation, so a red boot cannot be traced to a file", err)
	}
}

// TestCompilerParsesValidLiteralsUnchanged guards against a guard that bails
// too eagerly on real automation payloads.
func TestCompilerParsesValidLiteralsUnchanged(t *testing.T) {
	c := &Compiler{}
	if got, ok := c.parseArrayLiteral(`["a", "b"]`); !ok || len(got) != 2 {
		t.Errorf(`parseArrayLiteral(["a","b"]) = %#v ok=%v, want 2 elements`, got, ok)
	}
	if got, ok := c.parseArrayLiteral(`[]`); !ok || got == nil || len(got) != 0 {
		t.Errorf("parseArrayLiteral([]) = %#v ok=%v, want empty non-nil", got, ok)
	}
	obj, ok := c.parseObjectLiteral(`{ k: "v", n: 1, list: ["a"] }`)
	if !ok || obj == nil || obj["k"] != "v" {
		t.Errorf("parseObjectLiteral returned %#v ok=%v, want a populated map", obj, ok)
	}
}

// TestCompilerEmptyValueIsNullNotAnError guards the distinction the ok result
// exists to preserve: an EMPTY value is a null, not a malformed literal.
// `{k:}` and `{ k: }` differ by one space and must not disagree. The sibling
// copy in component/memql broke exactly this with a position-based guard.
func TestCompilerEmptyValueIsNullNotAnError(t *testing.T) {
	c := &Compiler{}
	for _, src := range []string{"{ k: }", "{k:}", "{k:,m:1}", "{ k: , m: 1 }", "{}", "{ k: [] }", "{ k: [,] }"} {
		got, err := c.parsePayloadRaw(src)
		if err != nil {
			t.Errorf("parsePayloadRaw(%q) errored: %v -- an empty value is a null", src, err)
			continue
		}
		if got == nil {
			t.Errorf("parsePayloadRaw(%q) = nil map, want a parsed payload", src)
		}
	}
}

// TestCompilerAcceptsRealisticPayloads is the regression net for the ok
// threading. Rejecting a VALID payload is the failure mode a fail-loudly
// change invites, and it would surface as a refused boot rather than a test
// failure, so the accept side needs at least as much cover as the reject side.
//
// This corpus is synthetic on purpose. Walking the real dsl/ tree finds
// exactly ZERO PayloadRaw sites across all 199 .memql files -- the compiler's
// payload parser is unreachable from authored DSL today, which is the "latent"
// caveat memql#2816 carries. A corpus probe therefore proves nothing here, and
// these hand-written shapes are the only thing standing between a future
// authored payload and a silent rejection.
func TestCompilerAcceptsRealisticPayloads(t *testing.T) {
	c := New(Config{})
	for _, src := range []string{
		`{ id: "x", name: "Alice" }`,
		`{ count: 1, ratio: 1.5, neg: -3, active: true, off: false, none: null }`,
		`{ tags: ["a", "b", "c"] }`,
		`{ nested: { inner: { deep: "v" } } }`,
		`{ mixed: [1, "two", true, null, { k: "v" }, ["nested"]] }`,
		`{ trailing: ["a", "b",] }`,
		`{ expr: concat("a", "b") }`,
		`{ ref: event.payload.id }`,
		`{ fallback: coalesce(event.payload.name, "unnamed") }`,
		`{ when: now }`,
		`{ punct: "commas, braces } brackets ] and colons: inside" }`,
		`{ escaped: "a \"quoted\" word" }`,
		`{ empty: "", list: [], obj: {} }`,
		`{ event.payload.partitionId, name: "explicit" }`,
		`{ multi: [ { a: 1 }, { b: 2 } ], flag: true }`,
	} {
		got, err := c.parsePayloadRaw(src)
		if err != nil {
			t.Errorf("parsePayloadRaw(%q) REJECTED a valid payload: %v", src, err)
			continue
		}
		if got == nil {
			t.Errorf("parsePayloadRaw(%q) = nil map, want a parsed payload", src)
		}
	}
}

// TestCompilerEmptyPayloadIsNotAnError -- an absent payload is not malformed,
// it is simply nothing to compile. parsePayloadRaw returns (nil, nil) there,
// and compileMutationConfig must not treat that as a failure.
func TestCompilerEmptyPayloadIsNotAnError(t *testing.T) {
	c := &Compiler{}
	got, err := c.parsePayloadRaw("")
	if err != nil || got != nil {
		t.Errorf(`parsePayloadRaw("") = %#v, %v; want nil, nil`, got, err)
	}
	cfg, err := c.compileMutationConfig(&parser.MutationStmt{Concept: "v1:test:thing"})
	if err != nil {
		t.Errorf("compileMutationConfig with no payload errored: %v", err)
	}
	if cfg["concept"] != "v1:test:thing" {
		t.Errorf("compileMutationConfig = %#v, want the concept carried through", cfg)
	}
}

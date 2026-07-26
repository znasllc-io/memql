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
// another goroutine to race the runner's memory limit.
//
// What that does and does NOT buy, stated exactly. The spinner is not
// cancellable -- these parsers take no context -- so it keeps growing until
// the process exits, and its peak therefore tracks how much memory the box
// will hand out, NOT any fixed bound. What the latch guarantees is that the
// actionable `--- FAIL` line is printed at the 2s deadline, long before any
// later OOM can truncate the log. That ordering is the whole point: a run that
// dies at 6 GiB having already said which guard regressed is debuggable, and
// one that dies without saying is not. Every test in this file
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
// null in a compiled automation.
//
// As of memql#2830 the error DOES refuse the boot: LoadFromUnifiedTree records
// the compile failure and returns it, and app/engine.go gates on that
// synchronously, so the node refuses to start rather than running with the
// automation silently absent. See the note on parsePayloadRaw for the exact
// frames. The progression was "silently wrong" -> "absent, with a WARN"
// (#2785/#2816) -> "red boot naming the automation" (#2830).
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
// swallowed one frame up. An earlier version of this test called
// compileMutationConfig directly, which is the very frame it claims to go
// beyond: deleting compileStep's `if err != nil { return nil, err }` left the
// package GREEN. It now drives the real compileStep entry point, so the
// propagation itself is covered.
//
// It also goes through mustTerminate. The input is a MALFORMED array, so a
// direct call would bypass the `leaked` latch: with the array guard regressed
// this test spun on the test goroutine and the package died at 6.2 GiB with a
// truncated OOM log and no FAIL line -- verbatim the outcome mustTerminate
// exists to prevent.
func TestCompilerMalformedPayloadReachesCompileStep(t *testing.T) {
	c := &Compiler{}
	mustTerminate(t, "compileStep(mutation with malformed payload)", func() {
		out, err := c.compileStep(&parser.StepDef{
			Name: "writeThing",
			Type: parser.StepTypeMutation,
			Config: &parser.MutationStepConfig{
				Mutation: &parser.MutationStmt{
					Concept:    "v1:test:thing",
					PayloadRaw: "{ k: [}{] }",
				},
			},
		})
		if err == nil {
			t.Fatalf("compileStep = %#v, err=nil; want the malformed payload to propagate", out)
		}
		if !strings.Contains(err.Error(), "malformed payload literal") {
			t.Errorf("error %q does not name the cause", err)
		}
		if !strings.Contains(err.Error(), "v1:test:thing") {
			t.Errorf("error %q does not name the mutation, so the failure cannot be traced to a file", err)
		}
	})
}

// TestCompilerValidStepStillCompiles is the accept-side counterpart: the new
// error return must not turn a healthy mutation step into a compile failure.
func TestCompilerValidStepStillCompiles(t *testing.T) {
	c := &Compiler{}
	out, err := c.compileStep(&parser.StepDef{
		Name: "writeThing",
		Type: parser.StepTypeMutation,
		Config: &parser.MutationStepConfig{
			Mutation: &parser.MutationStmt{
				Concept:    "v1:test:thing",
				PayloadRaw: `{ name: "x", tags: ["a"], nested: { k: "v" } }`,
			},
		},
	})
	if err != nil {
		t.Fatalf("compileStep on a valid payload errored: %v", err)
	}
	if out["mutation"] == nil {
		t.Errorf("compileStep = %#v, want a mutation config", out)
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

// crossCopyParityCases is the shared accept/reject table for the THREE
// near-duplicate payload parsers: this one, component/memql's
// parsePayloadRawToTemplate (load), and component/automations/steps'
// parseAndEvaluateObjectLiteral (runtime dispatch).
//
// They are copies, so they drift, and a drift means the same literal is
// accepted at compile time and rejected at dispatch (or the reverse) -- a bug
// class no single-copy test can see. Two live divergences were found this way:
// nested string-braces (only this copy rejected) and unterminated strings
// (only the runtime copy accepted). Keep the three tables identical; the
// package-local test in each of the other two packages carries the same list.
var crossCopyParityCases = []struct {
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

	// Unbalanced NESTED literals. The runtime copy's nested-object and
	// nested-array arms returned their runoff text as a VALUE, so a single
	// missing brace produced a string where an object belongs. The third case
	// is the sharp one: an unterminated string ONE LEVEL DOWN, whose own
	// ok=false the enclosing arm swallowed -- the fix for unterminated
	// strings did not hold under nesting.
	{`{ a: { b: 1 }`, false},
	{`{ a: [ 1, 2 }`, false},
	{`{ a: { b: """ } }`, false},

	// ...and the balanced counterparts, which must keep parsing.
	{`{ a: { b: 1 } }`, true},
	{`{ a: [ 1, 2 ] }`, true},
	{`{ name: "x", nested: { deep: { deeper: 1 } } }`, true},
}

// TestCompilerCrossCopyParity pins this copy's half of the shared table.
func TestCompilerCrossCopyParity(t *testing.T) {
	c := New(Config{})
	for _, tc := range crossCopyParityCases {
		if !mustTerminate(t, "parsePayloadRaw("+tc.src+")", func() {
			_, err := c.parsePayloadRaw(tc.src)
			if got := err == nil; got != tc.accept {
				t.Errorf("parsePayloadRaw(%q): accepted=%v, want %v (err=%v)", tc.src, got, tc.accept, err)
			}
		}) {
			break
		}
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
// change invites. Since memql#2830 it surfaces as a REFUSED BOOT naming the
// automation (LoadFromUnifiedTree returns the compile error and app/engine.go
// gates on it) rather than as a silent drop -- louder than before, but still
// not a failure of THIS test, so the accept side needs at least as much cover
// as the reject side. Finding F3
// of the round-4 review was exactly this: a valid nested payload newly
// rejected, caught only because the accept net was widened.
//
// This corpus is synthetic on purpose, and the reason is a ROUTING fact, not
// a count -- an earlier version of this comment stated it as a count and got
// the count wrong:
//
//	struct-form `insert {` / `update {` blocks   -> component/memql's copy
//	inline `mutation { ... }` automation steps   -> THIS copy
//	                                                (compileMutationConfig
//	                                                 -> parsePayloadRaw)
//
// The tree is full of the first kind and, at the time of writing, carries
// none of the second -- which is the "latent" caveat memql#2816 describes,
// and why a corpus probe over dsl/ proves nothing about THIS function. The
// live numbers are whatever `grep -c` says today; do not restate them here,
// because the routing is the durable part and the counts are not.
//
// These hand-written shapes are therefore the only thing standing between a
// future authored payload and a silent rejection. If inline mutation steps
// ever appear in the tree, this parser goes live and everything memql#2816
// calls latent stops being latent.
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

		// Braces and brackets inside a NESTED quoted string. The nested-object
		// arm of parseValue depth-counted `{`/`}` with no string awareness, so
		// the `}` inside the string closed the object early: parseObjectLiteral
		// received the truncated `{ b: "}` and returned `{"b":""}` silently.
		// Once malformation became an error these valid payloads turned into
		// hard rejections. The top-level variant above (`punct:`) always
		// passed -- only the NESTED case was broken, which is why the accept
		// net needs both.
		`{ a: { b: "}" } }`,
		`{ a: { b: "{" } }`,
		`{ a: { b: "a}b" }, c: 1 }`,
		`{ a: { b: "]" } }`,
		`{ a: { b: "x", c: { d: "}}" } } }`,
		`{ a: [ "}" ] }`,
		`{ a: { b: "a\"}" }, c: 2 }`,
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

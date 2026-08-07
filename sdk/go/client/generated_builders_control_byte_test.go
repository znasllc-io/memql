package client

// generated_builders_control_byte_test.go -- regression guard for memql#3192
// on the Go SDK's call-building path.
//
// Every generated builder rendered its string args with
// fmt.Sprintf("%q"), and so did renderMemQLValue in support.go. Go's %q
// escape set and the MemQL lexer's do not agree, and the disagreement is a
// hard error at tokenize time rather than a fallback: the lexer implements the
// JSON escapes and only those, while %q emits `\x00`, `\a` and `\v`. One
// control byte in a caller's arg made the whole call unparseable -- rejected
// at the engine with a syntax error naming a position in text the caller never
// wrote.
//
// The failure mode differs from the engine-side sites (a rejected query rather
// than a stuck row), but it is the same disagreement, and there are on the
// order of a thousand generated call sites carrying it.
//
// The fix is at the GENERATOR (sdk/gen/emit_go.go), which now emits
// quoteMemQL(...) -- a one-line alias for langparser.QuoteString, the single
// definition that lives beside the lexer. No generated file is hand-edited.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// sdkControlByteFixture carries one of each byte %q escapes in a form the
// lexer rejects: NUL, BEL and VT. Tab and newline ride along because both
// encoders handle those, so a regression cannot hide behind them.
const sdkControlByteFixture = "boom \x00 \a \v \t\n end"

// TestGeneratedBuilder_ControlByteStringArgParses drives a REAL generated
// builder with a control byte in a required string arg. Against %q this fails
// with `invalid escape character 'x'`.
func TestGeneratedBuilder_ControlByteStringArgParses(t *testing.T) {
	call := HarnessTraceBuild(HarnessTraceArgs{PlanId: sdkControlByteFixture})

	parsed, err := langparser.ParseExpression(call)
	if err != nil {
		t.Fatalf("generated call does not parse (the #3192 defect): %v\ncall: %#v", err, call)
	}
	fn, ok := parsed.(*langparser.FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T", parsed)
	}
	if fn.Args["planId"] != sdkControlByteFixture {
		t.Errorf("planId did not round-trip: %#v", fn.Args["planId"])
	}
}

// TestGeneratedBuilder_ControlByteAcrossArgKinds covers the other arg shapes
// the emitter renders as string literals: several required strings, plus the
// []string and object args that route through renderMemQLValue in support.go.
func TestGeneratedBuilder_ControlByteAcrossArgKinds(t *testing.T) {
	call := AddHarnessStepBuild(AddHarnessStepArgs{
		StepId:         "v1:harness:step:s1",
		PlanId:         "v1:harness:plan:p1",
		Title:          sdkControlByteFixture,
		IdempotencyKey: "k\x00ey",
		DependsOn:      []string{"a\vb"},
		Input:          map[string]any{"note": "r\ax"},
	})

	parsed, err := langparser.ParseExpression(call)
	if err != nil {
		t.Fatalf("generated call does not parse (the #3192 defect): %v\ncall: %#v", err, call)
	}
	fn, ok := parsed.(*langparser.FunctionCallExpr)
	if !ok {
		t.Fatalf("expected *FunctionCallExpr, got %T", parsed)
	}
	if fn.Args["title"] != sdkControlByteFixture {
		t.Errorf("title did not round-trip: %#v", fn.Args["title"])
	}
	if deps, ok := fn.Args["dependsOn"].([]any); !ok || len(deps) != 1 || deps[0] != "a\vb" {
		t.Errorf("dependsOn did not round-trip: %#v", fn.Args["dependsOn"])
	}
	if in, ok := fn.Args["input"].(map[string]any); !ok || in["note"] != "r\ax" {
		t.Errorf("input did not round-trip: %#v", fn.Args["input"])
	}
}

// TestRenderMemQLValue_MatchesQuoteString pins the single-definition rule on
// the SDK's own value renderer: it must not carry a second idea of the MemQL
// escape set.
func TestRenderMemQLValue_MatchesQuoteString(t *testing.T) {
	for _, s := range []string{
		sdkControlByteFixture,
		"",
		`quote " and backslash \ and slash /`,
		"<html> & \"amp\"",
		strings.Repeat("\x1b", 8),
	} {
		if got, want := renderMemQLValue(s), langparser.QuoteString(s); got != want {
			t.Errorf("renderMemQLValue(%#v):\n  got:  %#v\n  want: %#v", s, got, want)
		}
		if got, want := renderMemQLValue([]string{s}), "["+langparser.QuoteString(s)+"]"; got != want {
			t.Errorf("renderMemQLValue([]string{%#v}):\n  got:  %#v\n  want: %#v", s, got, want)
		}
	}
}

// TestGeneratedFilesCarryNoPercentQ pins the fix at the generator rather than
// at any one builder: no generated file may render a MemQL string literal with
// %q. This is the check that fails if the emitter regresses and the client is
// regenerated.
func TestGeneratedFilesCarryNoPercentQ(t *testing.T) {
	for _, name := range []string{
		"generated_queries.go",
		"generated_mutations.go",
		"generated_logics.go",
		"generated_builtins.go",
	} {
		src, err := readGeneratedFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if n := strings.Count(src, `fmt.Sprintf("%q"`); n != 0 {
			t.Errorf("%s still renders %d MemQL string literals with %%q; regenerate after fixing sdk/gen/emit_go.go", name, n)
		}
	}
}

// readGeneratedFile reads a generated source file from this package's own
// directory. Reading the source rather than reflecting over the builders is
// deliberate: the property under test is what the EMITTER wrote, and a
// per-builder behavioural check cannot see the ~1200 call sites it did not
// exercise.
func readGeneratedFile(name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(generatedFileDir(), name))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// generatedFileDir is this test file's own directory, derived from
// runtime.Caller so it does not depend on how the test was invoked.
func generatedFileDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Dir(thisFile)
}

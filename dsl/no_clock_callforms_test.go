package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// clockCallFormRe matches the retired author-surface clock call-forms `now()`
// and `timestamp()`. The `\b` before each name keeps `subtractTimestamps(...)`
// (which carries args, never an empty paren pair anyway) and identifiers like
// `myNow` from matching.
var clockCallFormRe = regexp.MustCompile(`\b(now|timestamp)\(\s*\)`)

// TestNoClockCallForms is the core-builtins ADR lock-in (epic #2298, Story
// #2301): authored .memql files must express "current time" as the bare
// reserved identifier `now`, never the call-forms `now()` / `timestamp()`.
//
// `now`, `now()`, and `timestamp()` are ONE clock primitive -- all three parse
// to TimestampExprFunc and evaluate via RuntimeEvaluator.EvaluateTimestamp()
// (a fresh time.Now()). The engine canonicalizes bare `now` -> `now()` ->
// `timestamp()` internally (mutation_templates.classifyScalarOrExpr,
// compiler/automation_generator) and re-parses those strings, so the PARSER
// must keep accepting the call-forms -- they are the engine's internal
// canonical form. This test enforces the retirement at the AUTHOR surface only:
// a human writes `createdAt: now`, and the engine does the rest. Keeping a
// single author-facing spelling matches the other reserved engine identifiers
// (`actor` / `partition` / `config`) and makes every clock dependency legible
// as a declared name rather than a hidden call.
//
// _reference/ skeletons are exempt (they document the grammar, including forms
// authors do not write).
func TestNoClockCallForms(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "_reference/") {
			continue
		}
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			// Strip line comments -- prose may legitimately mention the
			// retired forms (e.g. an ADR pointer); only authored code counts.
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = line[:idx]
			}
			if m := clockCallFormRe.FindString(line); m != "" {
				t.Errorf("%s:%d: retired clock call-form %q -- write the bare reserved identifier `now` instead (core-builtins ADR / #2301)", p, i+1, m)
			}
		}
	}
}

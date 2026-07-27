package parser

import (
	"strings"
	"testing"
)

// Author-written procedural form (`func (Receiver) name(ctx any) ...`)
// is retired by memql#303. The DSL-load entry point in
// component/memql/function_loader.go calls
// parser.RejectLegacyProceduralAuthorForm on every .memql slice; the
// rewriter's own struct-form lowering synthesises procedural source
// as an internal IR and is therefore unaffected.
//
// These tests pin the rejection's regex coverage. The
// component/memql package owns the end-to-end test that the loader
// fails fast on author-written procedural input.

func TestRejectLegacyProceduralAuthorForm_AllReceivers(t *testing.T) {
	for _, receiver := range []string{
		"Query", "Mutation", "Logic", "Spec", "Automation",
		"Builtin", "Prompt", "Provider", "Shape", "Tool", "Policy", "Seed",
	} {
		t.Run(receiver, func(t *testing.T) {
			src := "@description(\"legacy form\")\nfunc (" + receiver + ") legacyName(ctx any) (any, error) {\n  return ctx, nil\n}\n"
			err := RejectLegacyProceduralAuthorForm(src)
			if err == nil {
				t.Fatalf("expected rejection for author-written `func (%s) ...`", receiver)
			}
			if !strings.Contains(err.Error(), "retired") {
				t.Fatalf("error should mention retirement; got %q", err.Error())
			}
			if !strings.Contains(err.Error(), receiver) {
				t.Fatalf("error should name the receiver %q; got %q", receiver, err.Error())
			}
			if !strings.Contains(err.Error(), "memql#303") {
				t.Fatalf("error should cite the retirement issue; got %q", err.Error())
			}
		})
	}
}

// TestRejectLegacyProceduralAuthorForm_SpellingVariants pins the spellings that
// used to dodge the rejection entirely.
//
// The pattern was `^func \(Receiver\)` -- column 0, exactly one space after
// `func`, no space inside the parens. The slicer that actually EXTRACTS these
// declarations (component/memql's functionSliceHeader) is
// `func[ \t]*\([ \t]*(Query|...)[ \t]*\)`, which allows leading indentation and
// flexible spacing. Everything in that gap was rejected by nothing and
// registered by the loader: a retired author form kept loading, and a
// kind-prefixed construct written that way shipped with the whole suite green
// (memql#2853 round-3 review).
//
// A rejection gate must be at least as permissive as the matcher it guards.
func TestRejectLegacyProceduralAuthorForm_SpellingVariants(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"column 0, one space", "func (Query) queryFoo(ctx any) (any, error) {\n  return ctx, nil\n}\n"},
		{"indented with spaces", "  func (Query) queryFoo(ctx any) (any, error) {\n  return ctx, nil\n}\n"},
		{"indented with a tab", "\tfunc (Query) queryFoo(ctx any) (any, error) {\n  return ctx, nil\n}\n"},
		{"two spaces after func", "func  (Query) queryFoo(ctx any) (any, error) {\n  return ctx, nil\n}\n"},
		{"no space after func", "func(Query) queryFoo(ctx any) (any, error) {\n  return ctx, nil\n}\n"},
		{"spaces inside the parens", "func ( Query ) queryFoo(ctx any) (any, error) {\n  return ctx, nil\n}\n"},
		{"tab inside the parens", "func (\tQuery\t) queryFoo(ctx any) (any, error) {\n  return ctx, nil\n}\n"},
		{"indented logic", "  func (Logic) logicFoo(ctx any) (any, error) {\n  return ctx, nil\n}\n"},
		{"preceded by another line", "@description(\"x\")\n  func (Mutation) mutationFoo(ctx any) (any, error) {\n  return ctx, nil\n}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := RejectLegacyProceduralAuthorForm(tc.src); err == nil {
				t.Fatalf("spelling %q was NOT rejected -- the guard is narrower than the "+
					"slicer, so this form loads and registers.\nSource:\n%s", tc.name, tc.src)
			}
		})
	}
}

// TestRejectLegacyProceduralAuthorForm_NotTrippedByLookalikes keeps the widened
// pattern from over-matching: a Go func in a comment, or a receiver name that
// merely starts with a retired one, must still pass.
func TestRejectLegacyProceduralAuthorForm_NotTrippedByLookalikes(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"commented out", "// func (Query) queryFoo(ctx any) (any, error) {}\n"},
		{"block comment", "/*\nfunc (Query) queryFoo(ctx any) (any, error) {}\n*/\n"},
		{"receiver is a longer word", "func (QueryBuilder) build(ctx any) (any, error) {\n  return ctx, nil\n}\n"},
		{"not a receiver at all", "func (r *Runner) Run(ctx any) (any, error) {\n  return ctx, nil\n}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := RejectLegacyProceduralAuthorForm(tc.src); err != nil {
				t.Fatalf("spelling %q should NOT be rejected; got %v\nSource:\n%s",
					tc.name, err, tc.src)
			}
		})
	}
}

// TestRejectLegacyProceduralAuthorForm_StructFormPasses locks the
// happy path: struct-form input does NOT trip the rejection. Even
// though the rewriter will later synthesise a `func (Query) ...`
// string from this source, RejectLegacyProceduralAuthorForm is
// called on the AUTHOR source (pre-rewrite), where no `func` shape
// is present.
func TestRejectLegacyProceduralAuthorForm_StructFormPasses(t *testing.T) {
	src := `@description("Active participants in a space.")
query participant queryActiveParticipants {
  args {
    partitionId  string  @required
  }
  filter participant.partitionId == args.partitionId
  shape  participantFull
}`
	if err := RejectLegacyProceduralAuthorForm(src); err != nil {
		t.Fatalf("unexpected rejection for struct-form input: %v", err)
	}
}

// TestNormaliseAll_StructFormRoundTrips locks that the rewriter's
// internal procedural-IR output continues to round-trip cleanly --
// NormaliseAll itself is not gated by the retirement.
func TestNormaliseAll_StructFormRoundTrips(t *testing.T) {
	src := `@description("Active participants in a space.")
query participant queryActiveParticipants {
  args {
    partitionId  string  @required
  }
  filter participant.partitionId == args.partitionId
  shape  participantFull
}`
	out, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: unexpected error for struct-form input: %v", err)
	}
	if !strings.Contains(out, "func (Query) queryActiveParticipants") {
		t.Fatalf("expected rewriter to emit internal procedural IR; got:\n%s", out)
	}
}

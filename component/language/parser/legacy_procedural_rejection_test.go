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

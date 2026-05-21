package main

import (
	"testing"
)

// TestParseArgsBlock_BraceAwareForPatternQuantifiers pins the
// regression that closed the sdk-gen extractor over `@pattern("...{N,M}...")`
// annotations. Pre-fix the non-greedy regex captured everything
// between `args {` and the first `}` -- which was the brace INSIDE
// a regex string -- truncating the args list.
//
// The fix introduced a string-aware brace counter (extractArgsBlockBody)
// that skips `}` characters inside double-quoted strings. This test
// codifies that contract so a future regex-only rewrite can't
// silently reintroduce the truncation.
func TestParseArgsBlock_BraceAwareForPatternQuantifiers(t *testing.T) {
	body := `args {
    userId    string  @required @pattern("^v1:[a-z0-9]+:[a-z0-9_]+:[a-zA-Z0-9_-]{1,128}$")
    epoch     int     @required
  }
  update user {
    id: args.userId
  }`

	got := parseArgsBlock(body)
	if len(got) != 2 {
		t.Fatalf("expected 2 args fields (userId, epoch), got %d: %+v", len(got), got)
	}
	if got[0].Name != "userId" {
		t.Errorf("field[0].Name = %q, want \"userId\"", got[0].Name)
	}
	if got[1].Name != "epoch" {
		t.Errorf("field[1].Name = %q, want \"epoch\" -- pre-fix this field was truncated because the `}` in `{1,128}` ended the args block early", got[1].Name)
	}
}

// TestParseArgsBlock_NoArgsBlock confirms the extractor's null path:
// a construct body without an `args { ... }` block returns nil
// rather than panicking on the missing brace.
func TestParseArgsBlock_NoArgsBlock(t *testing.T) {
	body := `update user {
    id: "v1:identity:user:alice"
    revocationEpoch: 1
  }`
	if got := parseArgsBlock(body); got != nil {
		t.Errorf("expected nil args for body with no `args {...}`, got %+v", got)
	}
}

// TestParseArgsBlock_PreservesMixedAnnotations is the existing-shape
// regression: every annotation flavor we already shipped (@required,
// @enum, @default, @description) still threads through the
// brace-aware extractor unchanged.
func TestParseArgsBlock_PreservesMixedAnnotations(t *testing.T) {
	body := `args {
    name     string  @required @description("display name")
    role     string  @enum("owner", "admin", "writer", "reader") @default("reader")
    locale   string
  }
`

	got := parseArgsBlock(body)
	if len(got) != 3 {
		t.Fatalf("expected 3 args, got %d: %+v", len(got), got)
	}
	if !got[0].Required {
		t.Errorf("name should be @required")
	}
	if got[0].Description != "display name" {
		t.Errorf("description = %q, want \"display name\"", got[0].Description)
	}
	if len(got[1].Enum) != 4 {
		t.Errorf("role enum should have 4 values, got %d", len(got[1].Enum))
	}
	if got[1].Default != "reader" {
		t.Errorf("role default = %q, want \"reader\"", got[1].Default)
	}
}

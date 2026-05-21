package parser

import (
	"testing"
)

// TestParseArgsBlockField_MaxLength exercises the `@maxLength(N)`
// annotation contract end-to-end against the parser. The number
// literal lands on ArgsField.MaxLength; missing parens fail; non-
// number literals fail; negative caps fail.
func TestParseArgsBlockField_MaxLength(t *testing.T) {
	t.Run("accepts non-negative integer", func(t *testing.T) {
		schema := parseArgsForTest(t, "args { name string @maxLength(64) }")
		if schema.Fields[0].MaxLength != 64 {
			t.Errorf("MaxLength = %d, want 64", schema.Fields[0].MaxLength)
		}
	})

	t.Run("rejects negative", func(t *testing.T) {
		if _, err := parseArgsSafe("args { name string @maxLength(-1) }"); err == nil {
			t.Error("expected error for @maxLength(-1), got nil")
		}
	})

	t.Run("rejects non-number", func(t *testing.T) {
		if _, err := parseArgsSafe(`args { name string @maxLength("64") }`); err == nil {
			t.Error("expected error for string literal in @maxLength, got nil")
		}
	})

	t.Run("rejects missing parens", func(t *testing.T) {
		if _, err := parseArgsSafe("args { name string @maxLength 64 }"); err == nil {
			t.Error("expected error for @maxLength without parens, got nil")
		}
	})
}

// TestParseArgsBlockField_Pattern exercises the `@pattern("regex")`
// annotation. The regex source-string lands on ArgsField.Pattern
// without compile-checking (that happens in the function loader).
// Missing parens, non-string literals all fail at parse time.
func TestParseArgsBlockField_Pattern(t *testing.T) {
	t.Run("accepts string literal", func(t *testing.T) {
		schema := parseArgsForTest(t, `args { id string @pattern("^v1:[a-z0-9]+$") }`)
		if schema.Fields[0].Pattern != "^v1:[a-z0-9]+$" {
			t.Errorf("Pattern = %q, want \"^v1:[a-z0-9]+$\"", schema.Fields[0].Pattern)
		}
	})

	t.Run("rejects non-string", func(t *testing.T) {
		if _, err := parseArgsSafe("args { id string @pattern(64) }"); err == nil {
			t.Error("expected error for number in @pattern, got nil")
		}
	})

	t.Run("rejects missing parens", func(t *testing.T) {
		if _, err := parseArgsSafe(`args { id string @pattern "foo" }`); err == nil {
			t.Error("expected error for @pattern without parens, got nil")
		}
	})
}

// TestParseArgsBlockField_CombinedAnnotations confirms that multiple
// annotations on the same field compose -- @required + @maxLength +
// @pattern all parse cleanly.
func TestParseArgsBlockField_CombinedAnnotations(t *testing.T) {
	schema := parseArgsForTest(t, `args { spaceId string @required @maxLength(128) @pattern("^v1:") }`)
	f := schema.Fields[0]
	if f.Optional {
		t.Errorf("@required not applied (Optional still true)")
	}
	if f.MaxLength != 128 {
		t.Errorf("MaxLength = %d, want 128", f.MaxLength)
	}
	if f.Pattern != "^v1:" {
		t.Errorf("Pattern = %q, want \"^v1:\"", f.Pattern)
	}
}

// parseArgsForTest fails the test on parse error; parseArgsSafe
// returns the error for cases that should reject.
func parseArgsForTest(t *testing.T, src string) *ArgsSchema {
	t.Helper()
	schema, err := parseArgsSafe(src)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	return schema
}

func parseArgsSafe(src string) (*ArgsSchema, error) {
	tokens, err := NewLexer(src).Tokenize()
	if err != nil {
		return nil, err
	}
	p := &Parser{tokens: tokens}
	if len(tokens) > 0 {
		p.current = tokens[0]
	}
	return p.parseFileTopArgsBlock()
}

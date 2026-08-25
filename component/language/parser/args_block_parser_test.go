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
	schema := parseArgsForTest(t, `args { partitionId string @required @maxLength(128) @pattern("^v1:") }`)
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

// TestParseArgsBlockField_RejectsDefault locks in the #991 de-overload: an
// `@default` on an args-block field is rejected (it was silently discarded and
// never applied; authors use the `??` shorthand in the body instead. A
// concept-field @default is NOT the alternative -- it is emitted into the
// schema and never applied on insert either (memql#2960).
func TestParseArgsBlockField_RejectsDefault(t *testing.T) {
	_, err := parseArgsSafe(`args { kind string @default("a") }`)
	if err == nil {
		t.Fatal("expected @default on an args field to be rejected, got nil")
	}
	// The message must name the `??` shorthand, not `coalesce(...)`:
	// test/dslconformance/no_coalesce_longhand_test.go gates the corpus on the shorthand, so
	// an author following the longhand advice writes a construct the tree
	// rejects (memql#2909 review).
	if !contains(err.Error(), "retired") || !contains(err.Error(), "??") {
		t.Fatalf("error should explain the migration to the `??` shorthand; got %q", err.Error())
	}
	if contains(err.Error(), "coalesce(") {
		t.Errorf("the message must not send authors to the longhand the corpus gate rejects; got %q", err.Error())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestParseArgsBlockField_NumericBounds covers @minimum / @maximum (memql#4522).
//
// The fields, the runtime check, the tool JSON-schema emitter and the MCP
// promoter all carried Minimum/Maximum already; the modern `args { }` block was
// the one surface that could not SET them, so a bound an author wanted had to
// become a Go seam or go unenforced.
func TestParseArgsBlockField_NumericBounds(t *testing.T) {
	t.Run("parses both bounds", func(t *testing.T) {
		schema := parseArgsForTest(t, `args { cursorTweenMs int @minimum(250) @maximum(2500) }`)
		f := schema.Fields[0]
		if f.Minimum == nil || *f.Minimum != 250 {
			t.Errorf("Minimum = %v, want 250", f.Minimum)
		}
		if f.Maximum == nil || *f.Maximum != 2500 {
			t.Errorf("Maximum = %v, want 2500", f.Maximum)
		}
	})

	t.Run("composes with the other annotations", func(t *testing.T) {
		schema := parseArgsForTest(t, `args { n int @required @minimum(1) @maximum(10) }`)
		f := schema.Fields[0]
		if f.Optional {
			t.Error("@required not applied alongside the bounds")
		}
		if f.Minimum == nil || *f.Minimum != 1 || f.Maximum == nil || *f.Maximum != 10 {
			t.Errorf("bounds = (%v, %v), want (1, 10)", f.Minimum, f.Maximum)
		}
	})

	// The lexer scans a number from its first DIGIT, so a negative bound
	// arrives as an operator token followed by the magnitude. Reading only
	// TokenNumber would reject this while naming the wrong problem.
	t.Run("accepts a negative bound", func(t *testing.T) {
		schema := parseArgsForTest(t, `args { offset int @minimum(-5) }`)
		f := schema.Fields[0]
		if f.Minimum == nil || *f.Minimum != -5 {
			t.Errorf("Minimum = %v, want -5", f.Minimum)
		}
	})

	t.Run("accepts a fractional bound", func(t *testing.T) {
		schema := parseArgsForTest(t, `args { score float @minimum(0.5) @maximum(1.0) }`)
		f := schema.Fields[0]
		if f.Minimum == nil || *f.Minimum != 0.5 {
			t.Errorf("Minimum = %v, want 0.5", f.Minimum)
		}
		if f.Maximum == nil || *f.Maximum != 1.0 {
			t.Errorf("Maximum = %v, want 1.0", f.Maximum)
		}
	})

	t.Run("rejects missing parens", func(t *testing.T) {
		if _, err := parseArgsSafe(`args { n int @minimum 5 }`); err == nil {
			t.Error("expected error for @minimum without parens, got nil")
		}
	})

	t.Run("rejects a non-numeric bound", func(t *testing.T) {
		if _, err := parseArgsSafe(`args { n int @maximum("ten") }`); err == nil {
			t.Error("expected error for a string @maximum, got nil")
		}
	})

	// Absent annotations must leave the bounds NIL rather than zero: a zero
	// minimum is a real constraint, and defaulting to it would silently refuse
	// every negative value on fields that never asked for a bound.
	t.Run("absent bounds stay nil", func(t *testing.T) {
		schema := parseArgsForTest(t, `args { n int }`)
		f := schema.Fields[0]
		if f.Minimum != nil || f.Maximum != nil {
			t.Errorf("bounds = (%v, %v), want (nil, nil)", f.Minimum, f.Maximum)
		}
	})
}

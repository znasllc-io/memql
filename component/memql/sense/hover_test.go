package sense

import (
	"strings"
	"testing"
)

// hoverAt is a thin t.Helper wrapper so a failing hover case reports at the
// call site.
func hoverAt(t *testing.T, s *Service, source string, line, col int) *HoverResult {
	t.Helper()
	return s.Hover(source, line, col)
}

func TestHover_Keyword(t *testing.T) {
	s := New(&stubRegistry{})
	// "    return 1" -- 'return' spans cols 5..10; hover mid-token.
	res := hoverAt(t, s, "logic x {\n  body {\n    return 1\n  }\n}", 3, 7)
	if res == nil || !strings.Contains(res.Contents, "(keyword)") {
		t.Fatalf("expected a keyword hover for 'return', got %+v", res)
	}
	if !strings.Contains(res.Contents, "return") {
		t.Errorf("keyword hover should name the keyword: %q", res.Contents)
	}
}

func TestHover_Annotation(t *testing.T) {
	s := New(&stubRegistry{})
	// "@description(\"x\")" -- hover on 'description' (cols 2..12).
	res := hoverAt(t, s, "@description(\"x\")\nquery Thing q { }", 1, 6)
	if res == nil || !strings.Contains(res.Contents, "(annotation)") {
		t.Fatalf("expected an annotation hover for '@description', got %+v", res)
	}
}

func TestHover_Concept(t *testing.T) {
	s := New(&stubRegistry{concepts: map[string]*ConceptInfo{
		"v1:ns:thing": {
			Name:        "v1:ns:thing",
			Description: "A thing concept.",
			Fields:      []FieldInfo{{Name: "id", Type: "string", Required: true}},
		},
	}})
	// A concept id lexes as one identifier token (contains ':').
	res := hoverAt(t, s, "ref v1:ns:thing", 1, 8)
	if res == nil {
		t.Fatal("expected a concept hover")
	}
	if !strings.Contains(res.Contents, "v1:ns:thing") || !strings.Contains(res.Contents, "A thing concept.") {
		t.Errorf("concept hover missing name/description: %q", res.Contents)
	}
}

func TestHover_UserFunction(t *testing.T) {
	s := New(&stubRegistry{functions: map[string]*FunctionInfo{
		"createSpace": {Name: "createSpace", Description: "Create a cognition space.", Kind: "mutation"},
	}})
	res := hoverAt(t, s, "    return createSpace", 1, 14)
	if res == nil {
		t.Fatal("expected a function hover")
	}
	if !strings.Contains(res.Contents, "createSpace") {
		t.Errorf("function hover should name the function: %q", res.Contents)
	}
}

func TestHover_NilWithoutRegistry(t *testing.T) {
	if res := New(nil).Hover("return 1", 1, 2); res != nil {
		t.Errorf("hover without a registry should be nil, got %+v", res)
	}
}

func TestHover_EmptyOnBlankToken(t *testing.T) {
	s := New(&stubRegistry{})
	if res := s.Hover("   ", 1, 2); res != nil {
		t.Errorf("hover over whitespace should be nil, got %+v", res)
	}
}

package sense

import (
	"strings"
	"testing"
)

// hoverAt is a thin t.Helper wrapper so a failing hover case reports at the
// call site. The document path is empty -- see hoverAtFile for the cases
// that need the ambient domain.
func hoverAt(t *testing.T, s *Service, source string, line, col int) *HoverResult {
	t.Helper()
	return s.Hover(source, line, col, "")
}

// hoverAtFile is hoverAt with a document path, which supplies the ambient
// domain used to disambiguate a bare concept name.
func hoverAtFile(t *testing.T, s *Service, source string, line, col int, filePath string) *HoverResult {
	t.Helper()
	return s.Hover(source, line, col, filePath)
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
	if res := New(nil).Hover("return 1", 1, 2, ""); res != nil {
		t.Errorf("hover without a registry should be nil, got %+v", res)
	}
}

func TestHover_EmptyOnBlankToken(t *testing.T) {
	s := New(&stubRegistry{})
	if res := s.Hover("   ", 1, 2, ""); res != nil {
		t.Errorf("hover over whitespace should be nil, got %+v", res)
	}
}

// bareConceptRegistry is a stubRegistry preloaded with the concept ids a
// bare-name hover case needs.
func bareConceptRegistry(ids ...string) *stubRegistry {
	concepts := make(map[string]*ConceptInfo, len(ids))
	for _, id := range ids {
		concepts[id] = &ConceptInfo{
			Name:        id,
			Description: "Doc for " + id + ".",
			Fields:      []FieldInfo{{Name: "id", Type: "string", Required: true}},
		}
	}
	return &stubRegistry{concepts: concepts}
}

// TestHover_BareConceptShortName is the #2753 regression net. Same-domain
// constructs are ambient -- referenced with no `use` import (authoring
// rule 25, #2617) -- so `shape candidate candidateFull` names a concept
// the registry only knows as v1:actions:candidate. Pre-fix the concept
// branch was gated on the token containing a colon, so hovering the bare
// short name returned nothing at all.
func TestHover_BareConceptShortName(t *testing.T) {
	s := New(bareConceptRegistry("v1:actions:candidate"))
	// `shape candidate candidateFull {` -- 'candidate' spans cols 7..16.
	res := hoverAtFile(t, s, "shape candidate candidateFull {\n  id\n}", 1, 10, "dsl/actions/shapes.memql")
	if res == nil {
		t.Fatal("expected a concept hover for the bare short name 'candidate'")
	}
	if !strings.Contains(res.Contents, "v1:actions:candidate") {
		t.Errorf("hover should report the canonical id: %q", res.Contents)
	}
	if !strings.Contains(res.Contents, "Doc for v1:actions:candidate.") {
		t.Errorf("hover should carry the concept's doc comment: %q", res.Contents)
	}
}

// TestHover_BareConceptResolvesWithoutFilePath: an unambiguous short name
// needs no document path -- the ambient domain only breaks ties. This is
// the gRPC/Cockpit path, which carries no file_path today.
func TestHover_BareConceptResolvesWithoutFilePath(t *testing.T) {
	s := New(bareConceptRegistry("v1:actions:candidate"))
	res := hoverAt(t, s, "shape candidate candidateFull {\n  id\n}", 1, 10)
	if res == nil || !strings.Contains(res.Contents, "v1:actions:candidate") {
		t.Fatalf("unambiguous bare name should resolve without a file path, got %+v", res)
	}
}

// TestHover_BareConceptAmbiguity pins the rule that matters most: when a
// trailing segment collides across namespaces (`plan` is both
// v1:planner:plan and v1:harness:plan, and 46 bare names collide
// tree-wide), the file's own domain decides -- and where it cannot,
// hover stays silent rather than naming the wrong concept.
func TestHover_BareConceptAmbiguity(t *testing.T) {
	const src = "query plan plansForSpace {\n  id\n}"
	cases := []struct {
		name     string
		filePath string
		want     string // canonical id expected in the hover, or "" for no hover
	}{
		{"domain picks planner", "dsl/planner/queries.memql", "v1:planner:plan"},
		{"domain picks harness", "dsl/harness/queries.memql", "v1:harness:plan"},
		{"unrelated domain stays silent", "dsl/cognition/queries.memql", ""},
		{"no file path stays silent", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(bareConceptRegistry("v1:planner:plan", "v1:harness:plan"))
			res := hoverAtFile(t, s, src, 1, 8, tc.filePath)
			if tc.want == "" {
				if res != nil {
					t.Fatalf("ambiguous bare name must not resolve, got %q", res.Contents)
				}
				return
			}
			if res == nil {
				t.Fatalf("expected %s, got no hover", tc.want)
			}
			if !strings.Contains(res.Contents, tc.want) {
				t.Errorf("hover = %q, want it to name %s", res.Contents, tc.want)
			}
		})
	}
}

// TestHover_ExactNameWinsOverBareConcept pins precedence: the bare-name
// resolution runs last, so a shape/function/provider whose name happens
// to match a concept's trailing segment still hovers as itself.
func TestHover_ExactNameWinsOverBareConcept(t *testing.T) {
	reg := bareConceptRegistry("v1:actions:candidate")
	reg.functions = map[string]*FunctionInfo{
		"candidate": {Name: "candidate", Description: "A function named candidate.", Kind: "query"},
	}
	s := New(reg)
	res := hoverAtFile(t, s, "shape candidate candidateFull {\n  id\n}", 1, 10, "dsl/actions/shapes.memql")
	if res == nil {
		t.Fatal("expected a hover")
	}
	if !strings.Contains(res.Contents, "A function named candidate.") {
		t.Errorf("exact function-name match must win over bare concept resolution: %q", res.Contents)
	}
}

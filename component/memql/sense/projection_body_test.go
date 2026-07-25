package sense

import (
	"strings"
	"testing"
)

// agentRegistry is a registry carrying one concept with a couple of fields,
// enough to prove a body offers THOSE and not the global vocabulary.
func agentRegistry() *stubRegistry {
	return &stubRegistry{concepts: map[string]*ConceptInfo{
		"v1:agents:agent": {
			Name:        "v1:agents:agent",
			Description: "An agent.",
			Fields: []FieldInfo{
				{Name: "name", Type: "string", Required: true, Description: "Display name."},
				{Name: "role", Type: "string", Description: "The agent's role."},
			},
		},
	}}
}

func containsLabel(items []CompletionItem, want string) bool {
	for _, it := range items {
		if it.Label == want {
			return true
		}
	}
	return false
}

// TestComplete_ShapeBodyOffersOnlyBoundConceptFields is the #2762 defect-B
// pin. A shape body projects its bound concept, so the only names legal there
// are that concept's fields plus the accessor roots the signature annotations
// enable. Before this, the body fell through to generic function-body
// completion and offered the entire global vocabulary -- every control
// keyword, builtin and function name -- none of which is legal in a
// projection list.
func TestComplete_ShapeBodyOffersOnlyBoundConceptFields(t *testing.T) {
	s := New(agentRegistry())
	src := "@row\nshape agent agentDisplayBasicInfo {\n    \n}"
	items := s.Complete(src, 3, 5, "dsl/agents/shapes.memql")

	for _, want := range []string{"name", "role"} {
		if !containsLabel(items, want) {
			t.Errorf("shape body must offer the bound concept's field %q; got %v", want, labelsOfItems(items))
		}
	}
	// The global vocabulary must be gone. `return` and `if` are control
	// keywords; a projection list can hold neither.
	for _, unwanted := range []string{"return", "if", "for", "coalesce"} {
		if containsLabel(items, unwanted) {
			t.Errorf("shape body must not offer %q -- a projection list holds fields, not statements; got %v", unwanted, labelsOfItems(items))
		}
	}
}

// TestComplete_ShapeBodyAccessorsFollowAnnotations: @row puts `row` in scope
// and @actor puts `actor` in scope. Offering a root the shape did not declare
// would teach a body the loader then rejects.
func TestComplete_ShapeBodyAccessorsFollowAnnotations(t *testing.T) {
	cases := []struct {
		name        string
		annotations string
		wantRoots   []string
		absentRoots []string
	}{
		{"row only", "@row\n", []string{"row"}, []string{"actor"}},
		{"actor only", "@actor\n", []string{"actor"}, []string{"row"}},
		{"both", "@row\n@actor\n", []string{"row", "actor"}, nil},
		{"neither", "", nil, []string{"row", "actor"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(agentRegistry())
			src := tc.annotations + "shape agent agentDisplayBasicInfo {\n    \n}"
			line := strings.Count(tc.annotations, "\n") + 2
			items := s.Complete(src, line, 5, "dsl/agents/shapes.memql")
			for _, want := range tc.wantRoots {
				if !containsLabel(items, want) {
					t.Errorf("want accessor %q offered; got %v", want, labelsOfItems(items))
				}
			}
			for _, absent := range tc.absentRoots {
				if containsLabel(items, absent) {
					t.Errorf("accessor %q must not be offered without its annotation; got %v", absent, labelsOfItems(items))
				}
			}
			// The concept's fields are offered regardless of accessor kind.
			if !containsLabel(items, "name") {
				t.Errorf("bound concept fields must be offered in every case; got %v", labelsOfItems(items))
			}
		})
	}
}

// TestComplete_RowMemberCompletion is the #2762 defect-C pin. `@row` puts the
// row envelope in scope, but `row.` offered nothing -- `actor.` and `event.`
// had member tables and `row` had none, leaving the intrinsics undiscoverable.
func TestComplete_RowMemberCompletion(t *testing.T) {
	s := New(agentRegistry())
	src := "@row\nshape agent agentDisplayBasicInfo {\n    row.\n}"
	items := s.Complete(src, 3, 9, "dsl/agents/shapes.memql")

	for _, want := range []string{"id", "createdAt", "createdBy", "concept", "type", "schema"} {
		if !containsLabel(items, want) {
			t.Errorf("row. must offer the intrinsic %q; got %v", want, labelsOfItems(items))
		}
	}
	// Payload fields are referenced BARE, never through row (#2292), so the
	// concept's own fields must not appear here.
	if containsLabel(items, "name") {
		t.Errorf("row. must not offer payload fields -- those are bare names; got %v", labelsOfItems(items))
	}
}

// TestComplete_RowMemberPrefixFilters: a partial member filters the set.
func TestComplete_RowMemberPrefixFilters(t *testing.T) {
	s := New(agentRegistry())
	src := "@row\nshape agent agentDisplayBasicInfo {\n    row.created\n}"
	items := s.Complete(src, 3, 16, "dsl/agents/shapes.memql")
	got := labelsOfItems(items)
	if len(got) != 2 || !containsLabel(items, "createdAt") || !containsLabel(items, "createdBy") {
		t.Errorf("row.created must narrow to createdAt/createdBy; got %v", got)
	}
}

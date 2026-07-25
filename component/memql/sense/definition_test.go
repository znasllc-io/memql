package sense

import "testing"

// definitionGraph is a fakeGraph carrying only declaration sites.
func definitionGraph(sites map[string][]DeclSite) fakeGraph {
	return fakeGraph{declSites: sites}
}

// TestDefinition_BareSameDomainConcept is the #2754 headline case. Same-domain
// constructs are ambient -- referenced with no `use` import (rule 25, #2617) --
// so there is no import line to read and F12 is the only way to reach the
// declaration.
func TestDefinition_BareSameDomainConcept(t *testing.T) {
	g := definitionGraph(map[string][]DeclSite{
		"candidate": {{File: "actions/concepts.memql", Name: "candidate", Kind: "concept", Line: 31, Column: 9}},
	})
	s := NewWithWorkspace(nil, g)
	// `shape candidate candidateFull {` -- 'candidate' spans cols 7..16.
	got := s.Definition("shape candidate candidateFull {\n  id\n}", 1, 10, "actions/shapes.memql")
	if len(got) != 1 {
		t.Fatalf("want 1 definition, got %d (%+v)", len(got), got)
	}
	if got[0].File != "actions/concepts.memql" || got[0].Kind != "concept" {
		t.Errorf("target = %+v, want actions/concepts.memql concept", got[0])
	}
	if got[0].Range.Start.Line != 31 || got[0].Range.Start.Column != 9 {
		t.Errorf("start = %+v, want line 31 col 9", got[0].Range.Start)
	}
	// The range must cover the declared name so the editor highlights it.
	if want := 9 + len("candidate"); got[0].Range.End.Column != want {
		t.Errorf("end column = %d, want %d", got[0].Range.End.Column, want)
	}
}

// TestDefinition_AmbiguityNarrowsByDomain pins the rule that keeps F12 honest:
// a colliding name resolves via the referencing file's own domain, and where
// that cannot decide it returns nothing. A wrong jump is worse than no jump.
func TestDefinition_AmbiguityNarrowsByDomain(t *testing.T) {
	sites := map[string][]DeclSite{
		"plan": {
			{File: "harness/concepts.memql", Name: "plan", Kind: "concept", Line: 5, Column: 9},
			{File: "planner/concepts.memql", Name: "plan", Kind: "concept", Line: 12, Column: 9},
		},
	}
	cases := []struct {
		name     string
		filePath string
		wantFile string // "" == no definition
	}{
		{"planner file picks planner", "planner/queries.memql", "planner/concepts.memql"},
		{"harness file picks harness", "harness/queries.memql", "harness/concepts.memql"},
		{"unrelated domain stays silent", "cognition/queries.memql", ""},
		{"no file path stays silent", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewWithWorkspace(nil, definitionGraph(sites))
			got := s.Definition("query plan plansForSpace {\n  id\n}", 1, 8, tc.filePath)
			if tc.wantFile == "" {
				if len(got) != 0 {
					t.Fatalf("ambiguous reference must not resolve, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].File != tc.wantFile {
				t.Fatalf("got %+v, want single target %s", got, tc.wantFile)
			}
		})
	}
}

// TestDefinition_CanonicalIdResolvesByTrailingSegment: a fully-qualified id
// lexes as one token, and declarations are keyed by the short name.
func TestDefinition_CanonicalIdResolvesByTrailingSegment(t *testing.T) {
	g := definitionGraph(map[string][]DeclSite{
		"candidate": {{File: "actions/concepts.memql", Name: "candidate", Kind: "concept", Line: 31, Column: 9}},
	})
	s := NewWithWorkspace(nil, g)
	got := s.Definition("ref v1:actions:candidate", 1, 8, "actions/shapes.memql")
	if len(got) != 1 || got[0].File != "actions/concepts.memql" {
		t.Fatalf("canonical id should resolve by trailing segment, got %+v", got)
	}
}

// TestDefinition_SilentCases covers everything that must NOT produce a jump.
func TestDefinition_SilentCases(t *testing.T) {
	g := definitionGraph(map[string][]DeclSite{
		"candidate": {{File: "actions/concepts.memql", Name: "candidate", Kind: "concept", Line: 31, Column: 9}},
	})
	cases := []struct {
		name   string
		svc    *Service
		source string
		line   int
		col    int
	}{
		{"no workspace graph", New(nil), "shape candidate candidateFull {\n}", 1, 10},
		{"unknown name", NewWithWorkspace(nil, g), "shape mystery mysteryFull {\n}", 1, 10},
		{"empty source", NewWithWorkspace(nil, g), "", 1, 1},
		{"dotted accessor is not a construct", NewWithWorkspace(nil, g), "shape x y {\n  row.id\n}", 2, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.svc.Definition(tc.source, tc.line, tc.col, "actions/shapes.memql"); len(got) != 0 {
				t.Errorf("expected no definition, got %+v", got)
			}
		})
	}
}

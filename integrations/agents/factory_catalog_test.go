package agents

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// catalogEngine is a fake engine that answers Execute with a POPULATED result,
// built the way MemQLEngine.Execute builds one.
//
// The distinction from integration_test.go's recordingEngine is the entire
// point of this file. recordingEngine returns &memql.ExecuteResult{} -- an
// empty result -- so a parser that reads NOTHING out of a result passes against
// it exactly as a correct parser does. That is how memql#4689 shipped: the
// package had fake-engine coverage, and the coverage could not distinguish the
// two behaviours it was there to tell apart.
type catalogEngine struct {
	memql.IntegrationEngineAccess
	rows []any
}

func (e *catalogEngine) Execute(_ context.Context, _ string) (*memql.ExecuteResult, error) {
	return memql.NewResultWithOutput(e.rows), nil
}

// TestRoleCatalogReadsWhatExecuteReturns is the memql#4689 regression.
//
// RED before the typed arm in extractRowsFromExecuteResult: json.Marshal of an
// *ExecuteResult drops the unexported output field, the loose walk finds no
// top-level "rows"/"nodes", and the catalog comes back empty -- which is what
// the live cluster did on every goal while 97 role rows sat in the database.
func TestRoleCatalogReadsWhatExecuteReturns(t *testing.T) {
	eng := &catalogEngine{rows: []any{
		map[string]any{
			"id":      "v1:agents:agentRole:fiction-writer",
			"payload": map[string]any{"slug": "fiction-writer", "name": "Fiction Writer"},
		},
		map[string]any{
			"id":      "v1:agents:agentRole:creative-companion",
			"payload": map[string]any{"slug": "creative-companion", "name": "Creative Companion"},
		},
	}}
	i := New(memql.NewAgentRegistry(), eng)

	roles := i.loadRoleCatalog(context.Background())
	if len(roles) != 2 {
		t.Fatalf("loadRoleCatalog returned %d roles, want 2.\n\n"+
			"The catalog is what agentFactoryAnalyze picks a roleSlug FROM. Empty, the model "+
			"is asked to choose from nothing, invents a slug, and createAgent fails the whole "+
			"goal with `roleSlug \"X\" not in catalog` (memql#4689).", len(roles))
	}
	if roles[0].Slug != "fiction-writer" {
		t.Errorf("first role slug = %q, want %q -- the payload is not being read", roles[0].Slug, "fiction-writer")
	}
}

// TestSkillCatalogReadsWhatExecuteReturns pins the second catalog on the same
// parser. Both were empty for the same reason; fixing one and not the other
// leaves the model choosing skills from an empty list.
func TestSkillCatalogReadsWhatExecuteReturns(t *testing.T) {
	eng := &catalogEngine{rows: []any{
		map[string]any{
			"id":      "v1:skills:skill:research",
			"payload": map[string]any{"slug": "research", "name": "Research"},
		},
	}}
	i := New(memql.NewAgentRegistry(), eng)

	skills := i.loadSkillCatalog(context.Background())
	if len(skills) != 1 {
		t.Fatalf("loadSkillCatalog returned %d skills, want 1 (memql#4689)", len(skills))
	}
}

// TestExtractRowsStillReadsLooseShapes guards the fallback. The typed arm is an
// ADDITION -- the map/slice callers must keep working, or fixing the catalog
// breaks whatever else walks a loose payload.
func TestExtractRowsStillReadsLooseShapes(t *testing.T) {
	cases := map[string]any{
		"top-level rows":  map[string]any{"rows": []any{map[string]any{"id": "a"}}},
		"nested rows":     map[string]any{"result": map[string]any{"rows": []any{map[string]any{"id": "a"}}}},
		"top-level nodes": map[string]any{"nodes": []any{map[string]any{"id": "a"}}},
		"bare slice":      []any{map[string]any{"id": "a"}},
	}
	for name, in := range cases {
		if got := extractRowsFromExecuteResult(in); len(got) != 1 {
			t.Errorf("%s: got %d rows, want 1", name, len(got))
		}
	}
}

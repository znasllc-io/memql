package planner

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

type workDependencyEngine struct {
	fakeEngine
	catalog map[string][]map[string]any
	bundles map[string]map[string]any
	members map[string][]map[string]any
}

func (e *workDependencyEngine) Execute(ctx context.Context, query string) (any, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.UserId != "alice" {
		return nil, fmt.Errorf("dependency lookup lost its owner")
	}
	for name, rows := range e.catalog {
		if strings.Contains(query, `name=="`+name+`"`) {
			return rowsEnvelope(rows...), nil
		}
	}
	for id, row := range e.bundles {
		if strings.Contains(query, `"`+id+`"`) {
			if strings.Contains(query, "authoringConstructsForBundle(") {
				return rowsEnvelope(e.members[id]...), nil
			}
			return rowsEnvelope(row), nil
		}
	}
	return rowsEnvelope(), nil
}

func dependencyFixture() (*PlannerAgentLoop, *workDependencyEngine, authoringBundle) {
	row := func(id, bundle, kind, name, source string) map[string]any {
		return map[string]any{"id": id, "ownerUserId": "v1:identity:user:alice", "bundleId": bundle, "kind": kind, "name": name, "source": source, "status": "active", "catalogued": true, "targetNamespace": "authored"}
	}
	query := row("q", "b", "query", "reused", "use identity.concepts.{ user }\n@actor\nquery user reused {\n  filter row => row.id == actor.userId\n  shape reusedShape\n}")
	shape := row("s", "b", "shape", "reusedShape", "use identity.concepts.{ user }\n@row\nshape user reusedShape { row.id }")
	logic := row("l", "c", "logic", "transitive", "logic transitive { body { return 42 } }")
	e := &workDependencyEngine{
		catalog: map[string][]map[string]any{"reused": {query}, "transitive": {logic}},
		bundles: map[string]map[string]any{
			"b": {"id": "b", "ownerUserId": "alice", "reusedConstructRefs": []any{map[string]any{"kind": "logic", "name": "transitive"}}},
			"c": {"id": "c", "ownerUserId": "alice"},
		},
		members: map[string][]map[string]any{"b": {query, shape}, "c": {logic}},
	}
	bundle := authoringBundle{AutomationName: "runFile", Constructs: []memql.SandboxConstruct{{Kind: "automation", Name: "runFile", Source: "@template\nautomation runFile { step get { query reused() } }"}}, ReuseEdges: []reuseEdge{{Kind: "query", Name: "reused", Namespace: "authored"}}}
	return &PlannerAgentLoop{engine: e}, e, bundle
}

func TestSealWorkDraftCapturesTransitiveSources(t *testing.T) {
	l, engine, bundle := dependencyFixture()
	sealed, err := l.sealWorkDraft(context.Background(), "alice", bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed.Constructs) != 4 {
		t.Fatalf("missing catalog, sibling or transitive source: %+v", sealed.Constructs)
	}
	if len(bundle.Constructs) != 1 {
		t.Fatal("sealing mutated the original draft")
	}
	version := memql.WorkBundleVersion(sealed.Constructs)
	engine.catalog["reused"][0]["source"] = "changed after acceptance"
	if version != memql.WorkBundleVersion(sealed.Constructs) {
		t.Fatal("saved dependency changed with its catalog row")
	}
	var captured string
	for _, c := range sealed.Constructs {
		if c.Name == "reused" {
			captured = c.Source
		}
	}
	if !strings.Contains(captured, "use identity.concepts") {
		t.Fatal("sealing discarded the shipped dependency import")
	}
}

func TestSealWorkDraftRefusesUnstableOrUnreadableDependencies(t *testing.T) {
	for _, name := range []string{"missing", "ambiguous", "other owner", "retired", "namespace mismatch", "source conflict", "missing source bundle"} {
		t.Run(name, func(t *testing.T) {
			l, e, bundle := dependencyFixture()
			switch name {
			case "missing":
				delete(e.catalog, "reused")
			case "ambiguous":
				e.catalog["reused"] = append(e.catalog["reused"], maps.Clone(e.catalog["reused"][0]))
			case "other owner":
				e.catalog["reused"][0]["ownerUserId"] = "bob"
			case "retired":
				e.catalog["reused"][0]["status"] = "retired"
			case "namespace mismatch":
				e.catalog["reused"][0]["targetNamespace"] = "elsewhere"
			case "source conflict":
				bundle.Constructs = append(bundle.Constructs, memql.SandboxConstruct{Kind: "query", Name: "reused", Source: "different source"})
			case "missing source bundle":
				delete(e.bundles, "b")
			}
			if _, err := l.sealWorkDraft(context.Background(), "alice", bundle); err == nil {
				t.Fatal("accepted an unsealed dependency")
			}
		})
	}
}

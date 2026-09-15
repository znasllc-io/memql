package automations

// loop_graph_source_test.go -- the function-registry adapter the static loop
// graph walks (memql#5381), pinned over REAL constructs: each is built from
// its .memql source by the unified loader's own per-construct entry point
// (memql.BuildFunctionConstruct), so the leaf encoding asserted here is the
// one boot produces -- parsed v1 nodes in the mutation template, not text.

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/baseregistry"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// newTestFunctionRegistry is an empty function registry, keyed as the
// engine's is (Upsert qualifies each name with its origin's namespace).
func newTestFunctionRegistry() *memql.FunctionRegistry {
	return &memql.FunctionRegistry{Registry: baseregistry.New[memql.Function]("function",
		func(f *memql.Function) *memql.Function { c := *f; return &c }, nil)}
}

// functionsFromTree builds the named constructs of the embedded tree, file by
// file, into a function registry, against the loaded concept registry.
func functionsFromTree(t *testing.T, want map[string][]string) *memql.FunctionRegistry {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("load concepts: %v", err)
	}
	concepts := memorynodes.DefaultRegistry()
	tree := memqldsl.Tree()
	lines, _ := memql.ResolveLanguageLines(tree)
	fns := newTestFunctionRegistry()
	for path, names := range want {
		data, err := fs.ReadFile(tree, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if data, err = lines.Prepare(path, data); err != nil {
			t.Fatalf("prepare %s: %v", path, err)
		}
		for _, name := range names {
			fn, err := memql.BuildFunctionConstruct(string(data), name, "unified:"+path, concepts)
			if err != nil {
				t.Fatalf("build %s %s: %v", path, name, err)
			}
			if err := fns.Upsert(fn); err != nil {
				t.Fatalf("register %s: %v", name, err)
			}
		}
	}
	return fns
}

// functionsFromSource builds constructs from a source written here.
func functionsFromSource(t *testing.T, origin, src string, names ...string) *memql.FunctionRegistry {
	t.Helper()
	fns := newTestFunctionRegistry()
	for _, name := range names {
		fn, err := memql.BuildFunctionConstruct(src, name, origin, nil)
		if err != nil {
			t.Fatalf("build %s: %v", name, err)
		}
		if err := fns.Upsert(fn); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	return fns
}

// treeAutomation compiles one automation of the embedded tree, as the tree
// loader compiles it, with the origin the loader stamps.
func treeAutomation(t *testing.T, path, name string) *Automation {
	t.Helper()
	origin := "unified:" + path + ":" + name
	slice, ok := treeAutomationSlices(t, memqldsl.Tree(), false)[origin]
	if !ok {
		t.Fatalf("the tree has no automation %s", origin)
	}
	a, err := NewLoader(LoaderOptions{}).compileUnifiedSlice(slice.authored, slice.automationSlice, origin)
	if err != nil {
		t.Fatalf("compile %s: %v", origin, err)
	}
	a.Origin = origin
	return a
}

func TestFunctionSource_AMutationIsItsWrite(t *testing.T) {
	reg := NewFunctionSource(functionsFromTree(t, map[string][]string{
		"forge/mutations.memql":   {"advanceRequest", "recordRequestEvent"},
		"library/mutations.memql": {"createArtifact"},
	})).Registry()

	adv, ok := reg["advanceRequest"]
	if !ok {
		t.Fatal("advanceRequest is not reachable by its bare name")
	}
	if _, ok := reg["forge.advanceRequest"]; !ok {
		t.Error("advanceRequest is not reachable by its qualified name")
	}
	want := work.Target{ConstructKind: work.ConstructMutation, Concept: "v1:forge:request", Write: &work.WriteSpec{
		Kind: "update",
		Fields: map[string]work.FieldValue{
			// accept { ... } is `field: args.field`: exactly an argument.
			"status":            {Arg: "status"},
			"validatedByUserId": {Arg: "validatedByUserId"},
			"approvedByUserId":  {Arg: "approvedByUserId"},
			"resolution":        {Arg: "resolution"},
			// stamp { ... } a literal.
			"provenanceMutation": {Literal: "advanceRequest", HasLiteral: true},
		},
	}}
	if !reflect.DeepEqual(adv, want) {
		t.Errorf("advanceRequest =\n  %+v %+v\nwant\n  %+v %+v", adv, adv.Write, want, want.Write)
	}

	rec := reg["recordRequestEvent"]
	if rec.Concept != "v1:forge:requestEvent" || rec.Write == nil || rec.Write.Kind != "insert" || rec.Write.NewRow {
		t.Fatalf("recordRequestEvent = %+v %+v, want an insert of v1:forge:requestEvent that names its id", rec, rec.Write)
	}
	// A call or an actor read is a value only the run knows.
	for _, f := range []string{"requestId", "actorUserId", "actorRole"} {
		fv, set := rec.Write.Fields[f]
		if !set || fv.HasLiteral || fv.Arg != "" {
			t.Errorf("recordRequestEvent's %s = %+v (set %v), want an unknown value", f, fv, set)
		}
	}

	art := reg["createArtifact"]
	if art.Concept != "v1:library:artifact" || art.Write.NewRow {
		t.Errorf("createArtifact = %+v %+v: it derives its id, so it is not a new row", art, art.Write)
	}
	if fv := art.Write.Fields["archived"]; fv.HasLiteral || fv.Arg != "" {
		t.Errorf("createArtifact's archived is `args.archived ?? false`, not exactly an argument: %+v", fv)
	}
	if fv := art.Write.Fields["kind"]; fv.Arg != "kind" {
		t.Errorf("createArtifact's kind = %+v, want args.kind", fv)
	}
}

func TestFunctionSource_NewRowsAndSplats(t *testing.T) {
	const src = `/// A new row, field by field.
mutate thing createPlainThing {
  args {
    name string!
  }
  insert {
    name: args.name
    status: "new"
    count: 3
    note: nil
  }
}

/// A new row whose payload is the caller's object.
@actor
mutate thing createSplatThing {
  args {
    payload object!
  }
  insert {
    payload: args.payload
    ownerUserId: actor.userId
  }
}
`
	reg := NewFunctionSource(functionsFromSource(t, "unified:things/mutations.memql", src, "createPlainThing", "createSplatThing")).Registry()
	plain := reg["createPlainThing"]
	want := &work.WriteSpec{Kind: "insert", NewRow: true, Fields: map[string]work.FieldValue{
		"name":   {Arg: "name"},
		"status": {Literal: "new", HasLiteral: true},
		"count":  {Literal: int64(3), HasLiteral: true},
		"note":   {HasLiteral: true},
	}}
	if !reflect.DeepEqual(plain.Write, want) {
		t.Errorf("createPlainThing writes %+v, want %+v", plain.Write, want)
	}
	splat := reg["createSplatThing"]
	if splat.Write == nil || splat.Write.NewRow {
		t.Fatalf("a splat can write any field, so it is not a new row whose unset fields are absent: %+v", splat.Write)
	}
	if fv, set := splat.Write.Fields["ownerUserId"]; !set || fv.HasLiteral || fv.Arg != "" {
		t.Errorf("the overlay's ownerUserId = %+v (set %v), want an unknown value", fv, set)
	}
}

func TestFunctionSource_LogicCallsWhatItsBodyCalls(t *testing.T) {
	reg := NewFunctionSource(functionsFromTree(t, map[string][]string{
		"deployment/logic.memql": {"nextDeploymentVersion"},
		"forge/logic.memql":      {"requestRouteStatus"},
	})).Registry()
	// `return builtin suggestNextVersion(...)`: the return's call.
	if got := reg["nextDeploymentVersion"]; got.ConstructKind != work.ConstructLogic || strings.Join(got.Calls, ",") != "suggestNextVersion" {
		t.Errorf("nextDeploymentVersion = %+v, want a logic calling suggestNextVersion", got)
	}
	// A body of expressions calls nothing.
	if got := reg["requestRouteStatus"]; got.ConstructKind != work.ConstructLogic || len(got.Calls) != 0 {
		t.Errorf("requestRouteStatus = %+v, want a logic calling nothing", got)
	}

	const src = `/// Statements that call constructs, one inside a loop and one behind a condition.
logic advanceAll {
  args {
    ids []string!
  }
  body {
    rows := query findThings(ids: args.ids)
    for item := range rows.nodes() {
      moved := mutation advanceThing(id: item.id, s: "open")
    }
    noted := if rows.count() > 0 {
      mutation recordThing(count: rows.count())
    }
    return logic summarize(count: rows.count())
  }
}
`
	reg = NewFunctionSource(functionsFromSource(t, "unified:things/logic.memql", src, "advanceAll")).Registry()
	if got := reg["advanceAll"]; strings.Join(got.Calls, ",") != "advanceThing,findThings,recordThing,summarize" {
		t.Errorf("advanceAll calls %v, want advanceThing, findThings, recordThing and summarize", got.Calls)
	}
}

// A field the engine rewrites before it stores the row is read as unknown:
// the row does not carry what the template (or the call site) wrote.
func TestFunctionSource_FieldsTheEngineRewrites(t *testing.T) {
	fns := functionsFromTree(t, map[string][]string{
		"forge/mutations.memql":    {"advanceRequest"},
		"platform/mutations.memql": {"stageOutboundRequest"},
	})
	with := newFunctionSource(fns, memorynodes.DefaultRegistry()).Registry()
	without := NewFunctionSource(fns).Registry()

	// approvedByUserId is an outgoing @relationship of v1:forge:request, so
	// the id written there is stored in canonical form.
	if fv := without["advanceRequest"].Write.Fields["approvedByUserId"]; fv.Arg != "approvedByUserId" {
		t.Fatalf("without concepts approvedByUserId = %+v, want args.approvedByUserId", fv)
	}
	if fv := with["advanceRequest"].Write.Fields["approvedByUserId"]; fv.Arg != "" || fv.HasLiteral {
		t.Errorf("approvedByUserId is canonicalized on write, yet read as %+v", fv)
	}
	if fv := with["advanceRequest"].Write.Fields["status"]; fv.Arg != "status" {
		t.Errorf("status is no relationship, yet read as %+v", fv)
	}

	// stageOutboundRequest names its id, so @createOnly drops status and
	// attempts over a row that already exists.
	st := with["stageOutboundRequest"].Write
	for _, f := range []string{"status", "attempts"} {
		if fv := st.Fields[f]; fv.HasLiteral || fv.Arg != "" {
			t.Errorf("@createOnly %s is not written over a stored row, yet read as %+v", f, fv)
		}
	}
	if fv := st.Fields["medium"]; fv.Arg != "medium" {
		t.Errorf("medium = %+v, want args.medium", fv)
	}

	const src = `/// Read-merge annotations on an update.
@noUnset("note", "label", "marker")
@appendFields("tags")
mutate thing updateNoted {
  args {
    id    string!
    label string
    tags  []string
  }
  update {
    id: args.id
    note: ""
    label: args.label
    marker: "set"
    tags: args.tags
    kind: "k"
  }
}

/// The same annotation on a new row, which no read-merge touches.
@noUnset("note")
mutate thing createNoted {
  insert {
    note: ""
  }
}

/// A scrub, whose PII fields the concept names.
@scrubPii
mutate thing scrubThing {
  args {
    id string!
  }
  update {
    id: args.id
    status: "deleted"
  }
}
`
	reg := NewFunctionSource(functionsFromSource(t, "unified:things/mutations.memql", src, "updateNoted", "createNoted", "scrubThing")).Registry()
	upd := reg["updateNoted"].Write.Fields
	for _, f := range []string{"note", "label", "tags"} {
		if fv := upd[f]; fv.HasLiteral || fv.Arg != "" {
			t.Errorf("updateNoted's %s may not be written as the template says, yet read as %+v", f, fv)
		}
	}
	for f, want := range map[string]any{"marker": "set", "kind": "k"} {
		if fv := upd[f]; !fv.HasLiteral || fv.Literal != want {
			t.Errorf("updateNoted's %s = %+v, want the literal %v", f, fv, want)
		}
	}
	if fv := reg["createNoted"].Write.Fields["note"]; !fv.HasLiteral || fv.Literal != "" {
		t.Errorf("a new row writes its @noUnset field as written: %+v", fv)
	}
	if fv := reg["scrubThing"].Write.Fields["status"]; fv.HasLiteral {
		t.Errorf("with no concept to name its PII fields, a scrub's every field is unknown: %+v", fv)
	}
}

func TestFunctionSource_KindsAndNil(t *testing.T) {
	fns := newTestFunctionRegistry()
	for _, fn := range []*memql.Function{
		{Name: "sendThing", Type: memql.FunctionTypeBuiltin, FunctionKind: "builtin", Origin: "unified:things/builtins.memql"},
		{Name: "findThing", FunctionKind: "query", Origin: "unified:things/queries.memql"},
		{Name: "legacyThing", Origin: "unified:things/queries.memql"},
	} {
		if err := fns.Upsert(fn); err != nil {
			t.Fatal(err)
		}
	}
	reg := NewFunctionSource(fns).Registry()
	for name, want := range map[string]string{"sendThing": work.ConstructBuiltin, "findThing": work.ConstructQuery, "legacyThing": work.ConstructQuery} {
		if got := reg[name].ConstructKind; got != want {
			t.Errorf("%s is a %q, want %q", name, got, want)
		}
	}
	if reg := NewFunctionSource(nil).Registry(); len(reg) != 0 {
		t.Errorf("no function registry is an empty call graph, got %v", reg)
	}
}

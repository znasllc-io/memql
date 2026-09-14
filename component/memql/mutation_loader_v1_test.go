package memql

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// mutation_loader_v1_test.go -- the function loader building a mutation's
// template from its edition-2026 parse (epic memql#5363, memql#5367).
//
// The loader builds every mutation's template with newMutationTemplateV1 from
// the parsed nodes (MutationStmt.PayloadExpr and the four slots); nothing
// reads PayloadRaw's text. The fixture tree below covers every write-block
// shape, and mutation_tree_v1_db_test.go runs it against a real engine.

const v1TreeFixtureDomain = "mutvone"

// v1TreeFixtureConcepts is the fixture's one concept: scalars, a list, and a
// nested object, with no row-authz tier so the writes need no owner match.
const v1TreeFixtureConcepts = `
/// The row the edition-2026 mutation tests write.
concept probe {
  ownerUserId  string!   @description("The caller, stamped from the actor.")
  title        string!   @description("A required field.")
  status       string    @description("An optional field.")
  note         string    @description("A field with a ?? default.")
  count        int       @description("A number with a ?? default.")
  done         bool      @description("A literal boolean.")
  tags         []string  @description("A list built from optional args.")
  ref          string    @description("A composed string.")
  label        string    @description("A conditional value.")
  details {
    source  string  @description("A nested value with a ?? default.")
    depth   int     @description("A nested literal.")
  }
}
`

// v1TreeFixtureMutations covers every write-block shape: a longhand insert
// with bare mirrors, `??`, a composite hashed id, a list and a nested object;
// accept/stamp nested in a write block and in the bare top-level form; an
// update read-merging the fields it names; and a payload splat re-stamped by
// its overlay. It was written in the pre-flip spelling -- concat, cond --
// and the fixture migration moved it to `+` and `? :`.
const v1TreeFixtureMutations = `
/// A longhand insert.
@actor
mutate probe createProbe {
  args {
    probeId  string!
    title    string!
    status   string
    note     string
    count    int
    tagA     string
    tagB     string
    source   string
  }
  insert {
    id: "p-" + hash(args.probeId + ":" + args.title)
    args.title
    args.status
    note: args.note ?? "none"
    count: args.count ?? 0
    done: false
    tags: [args.tagA, args.tagB, "fixed"]
    details: { source: args.source ?? "import", depth: 1 }
    label: args.status == "active" ? "LIVE" : "IDLE"
    ref: "ref-" + args.probeId
    ownerUserId: actor.userId
  }
}

/// accept / stamp inside the write block.
@actor
mutate probe createProbeAccepted {
  args {
    probeId  string!
    title    string!
    status   string
    count    int
  }
  insert {
    accept { title, status, count }
    stamp {
      id: args.probeId
      ownerUserId: actor.userId
      note: args.status ?? "draft"
      done: true
    }
  }
}

/// The bare accept / stamp form: no write block, so an insert.
@actor
mutate probe createProbeBare {
  args {
    probeId  string!
    title    string!
  }
  accept { title }
  stamp {
    id: args.probeId
    ownerUserId: actor.userId
    status: "bare"
  }
}

/// An update: a read-merge write of the fields the call names.
@actor
@mergeFields("details")
mutate probe updateProbe {
  args {
    probeId  string!
    note     string
    status   string
  }
  update {
    id: args.probeId
    args.note
    status: args.status ?? "updated"
    ownerUserId: actor.userId
  }
}

/// A payload splat, re-stamped by its overlay (memql#401).
@actor
mutate probe replaceProbe {
  args {
    probeId  string!
    payload  object!
  }
  update {
    id: args.probeId
    args.payload
    ownerUserId: actor.userId
  }
}
`

const v1TreeFixtureOrigin = "unified:" + v1TreeFixtureDomain + "/mutations.memql"

// mountV1TreeFixture registers the fixture domain's concept and restores the
// process-wide concept registry afterwards (see mountTraversalFixture for why
// both halves are needed). Only concepts.memql is mounted: the mutations are
// loaded by the test itself, once per grammar.
func mountV1TreeFixture(t *testing.T) {
	t.Helper()
	before := memorynodes.All()
	memqldsl.RegisterTree(v1TreeFixtureDomain, fstest.MapFS{
		"memql.toml":     languageLineFile(),
		"concepts.memql": {Data: []byte(v1TreeFixtureConcepts)},
	})
	t.Cleanup(func() {
		memqldsl.UnregisterTree(v1TreeFixtureDomain)
		memorynodes.ReplaceAll(before)
	})
}

// loadV1TreeMutations loads every mutation of src through the loader's own
// entry point (dispatchPerConstructParser, as LoadUnifiedFunctions does).
func loadV1TreeMutations(t *testing.T, src string, registry memorynodes.Registry) map[string]*Function {
	t.Helper()
	out := map[string]*Function{}
	for _, slice := range ExtractFunctionSlices(src) {
		if slice.Kind != languageParser.FunctionTypeMutation {
			continue
		}
		fn, err := dispatchPerConstructParser(slice, v1TreeFixtureOrigin, registry)
		require.NoErrorf(t, err, "load %s", slice.Name)
		require.NotNil(t, fn.MutationTemplate)
		out[slice.Name] = fn
	}
	require.Len(t, out, 5, "the fixture declares five mutations")
	return out
}

// TestV1TreeFixtureParses: the fixture parses -- each mutation's payload a v1
// map literal of explicit entries (the bare mirrors and accept names expanded
// by the rewriter) and its id a v1 node.
func TestV1TreeFixtureParses(t *testing.T) {
	for _, slice := range ExtractFunctionSlices(v1TreeFixtureMutations) {
		normalised, err := languageParser.NormaliseAll(slice.Source)
		require.NoError(t, err)
		file, err := languageParser.ParseFile(normalised)
		require.NoErrorf(t, err, "%s", slice.Name)
		fn := file.Definitions[0].(*languageParser.FunctionDef)
		stmt := fn.Body.(*languageParser.MutationStmt)
		payload, ok := stmt.PayloadExpr.(*ast.MapExpr)
		require.Truef(t, ok, "%s: payload is %T", slice.Name, stmt.PayloadExpr)
		for _, en := range payload.Entries {
			require.NotContainsf(t, en.Key, ".", "%s: a key-less mirror reached the map", slice.Name)
		}
		_, isNode := stmt.IDTemplate.(ast.ExpressionNode)
		require.Truef(t, isNode, "%s: the id slot is a v1 node", slice.Name)
	}
}

// TestLoaderBuildsTheTemplateFromItsV1Parse: the loader builds every
// mutation's template from its parse -- the concept and write kind the
// statement names, the annotation fields riding on it, and the layout the
// gates read -- and the pre-flip spelling no longer loads, refused naming the
// migration that rewrites it.
func TestLoaderBuildsTheTemplateFromItsV1Parse(t *testing.T) {
	mountV1TreeFixture(t)
	_, err := LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	registry := memorynodes.DefaultRegistry()

	v1 := loadV1TreeMutations(t, v1TreeFixtureMutations, registry)
	for name, tc := range map[string]struct {
		kind ast.MutationKind
		id   string
	}{
		"createProbe":         {ast.MutationKindInsert, `"p-" + hash(args.probeId + ":" + args.title)`},
		"createProbeAccepted": {ast.MutationKindInsert, `args.probeId`},
		"createProbeBare":     {ast.MutationKindInsert, `args.probeId`},
		"updateProbe":         {ast.MutationKindUpdate, `args.probeId`},
		"replaceProbe":        {ast.MutationKindUpdate, `args.probeId`},
	} {
		tmpl := v1[name].MutationTemplate
		require.Equalf(t, v1TreeFixtureConcept, tmpl.Concept, "%s binds the fixture concept", name)
		require.Equalf(t, tc.kind, tmpl.Kind, "%s is a %s", name, tc.kind)
		id, ok := tmpl.IDTemplate.(ast.ExpressionNode)
		require.Truef(t, ok, "%s: the id is a parsed node, got %T", name, tmpl.IDTemplate)
		require.Equalf(t, tc.id, ast.FormatExpr(id), "%s: the id slot holds the parsed id", name)
	}
	require.Equal(t, []string{"details"}, v1["updateProbe"].MutationTemplate.MergeFields, "@mergeFields rides on the template")
	require.True(t, isPayloadSplat(v1["replaceProbe"].MutationTemplate.PayloadTemplate), "the payload splat is laid out as the loader always did")
	require.Contains(t, v1["replaceProbe"].MutationTemplate.PayloadOverlayTemplate, "ownerUserId", "with its explicit fields as the overlay")

	// The pre-flip spelling does not load, and the refusal names the
	// migration that rewrites it.
	// memqlmigrate:keep -- the legacy spelling is the subject.
	const legacy = `mutate probe legacyProbe {
  args {
    probeId  string!
  }
  insert {
    id: args.probeId
    ref: concat("ref-", args.probeId)
  }
}`
	_, err = tryParseNewFunctionSyntax("legacyProbe", "mutation", legacy, v1TreeFixtureOrigin, registry)
	require.Error(t, err, "the pre-flip spelling (concat) must not load")
	require.Contains(t, err.Error(), "memqlmigrate --rewrite=expressions", "the refusal names the migration")
}

// TestLoaderRunsC5OnAV1Template: C5 (memql#2035) refuses, at load, a mutation
// writing an @serverSet field from caller args.
func TestLoaderRunsC5OnAV1Template(t *testing.T) {
	concept := mustConcept(t, `
concept Space {
  name    string  @required
  status  string  @serverSet
}
`, "v1/cognition/space")
	registry := conceptRegistry(concept)
	src := `mutate space mutBad {
  args {
    name    string!
    status  string
  }
  insert {
    args.name
    args.status
  }
}`
	_, err := tryParseNewFunctionSyntax("mutBad", "mutation", src, "unified:cognition/mutations.memql", registry)
	require.Error(t, err)
	require.Contains(t, err.Error(), "@serverSet")
}

// TestLoaderRefusesAV1ValueAtLoad: a v1 mutation value the position cannot
// evaluate is a load error (a baseloader Skip strict boot refuses), not a
// failure on the first call.
func TestLoaderRefusesAV1ValueAtLoad(t *testing.T) {
	_, err := tryParseNewFunctionSyntax("mutUnbound", "mutation", `mutate space mutUnbound {
  args {
    name  string!
  }
  insert {
    name: args.name
    status: active
  }
}`, "unified:cognition/mutations.memql", conceptRegistry(mustConcept(t, "concept Space {\n  name string\n  status string\n}\n", "v1/cognition/space")))
	require.Error(t, err)
	require.Contains(t, err.Error(), "reads `active`, which it does not bind")
}

// TestCanonicalIdQuotedShortNameResolves: `canonicalId(x, "campaign")` -- the
// spelling the codemod writes, because an edition-2026 expression resolves no
// bare name -- resolves at load exactly as the bare `canonicalId(x, campaign)`
// does; a quoted canonical id is untouched; and a quoted short name the file
// cannot bind is the same load error the bare one is. The quoted short name
// never worked unresolved: the engine knows a concept by its canonical id.
func TestCanonicalIdQuotedShortNameResolves(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memorynodes.Concept{
		"v1:campaigns:campaign": {Name: "v1:campaigns:campaign"},
	})
	r := NewConceptResolver(registry)
	header := "use campaigns.concepts.{ campaign }\n"
	for _, tc := range []struct{ in, want string }{
		{`id: hash(canonicalId(args.c, campaign))`, `id: hash(canonicalId(args.c, "v1:campaigns:campaign"))`},
		{`id: hash(canonicalId(args.c, "campaign"))`, `id: hash(canonicalId(args.c, "v1:campaigns:campaign"))`},
		{`id: hash(canonicalId(args.c, "v1:campaigns:campaign"))`, `id: hash(canonicalId(args.c, "v1:campaigns:campaign"))`},
	} {
		got, err := r.ResolveCanonicalIdConceptRefsInNamespace(header+tc.in, "other", "other")
		require.NoError(t, err, tc.in)
		require.Equal(t, header+tc.want, got)
	}
	_, err := r.ResolveCanonicalIdConceptRefsInNamespace(header+`id: canonicalId(args.c, "campaing")`, "other", "other")
	require.ErrorContains(t, err, `concept "campaing" is neither imported nor a same-domain concept`)
}

// ---------------------------------------------------------------------------
// the tree's tools, through the codemod
// ---------------------------------------------------------------------------

// corpusFiles reads the tree the engine loads, as it is or as the codemod
// leaves it -- CollectPredicates over every file, then RewriteExpressions per
// file, which is what memqlmigrate --rewrite=expressions runs. The tree is
// migrated, so the rewrite is its idempotence check as much as anything.
func corpusFiles(t *testing.T, migrated bool) []baseloader.RawFile {
	t.Helper()
	files := baseloader.ReadAll(nil)
	require.NotEmpty(t, files, "the tree reads no .memql file")
	if !migrated {
		return files
	}
	byPath := make(map[string][]byte, len(files))
	for _, f := range files {
		byPath[f.Path] = []byte(f.Content)
	}
	preds, err := languageParser.CollectPredicates(byPath)
	require.NoError(t, err)
	for i, f := range files {
		out, err := languageParser.RewriteExpressions([]byte(f.Content), preds)
		require.NoErrorf(t, err, "the codemod refuses %s", f.Path)
		files[i].Content = string(out)
	}
	return files
}

// TestV1CorpusToolHandlersResolve: every tool of the migrated tree loads, each
// query handler -- its `$args.` placeholders rewritten by the codemod -- parsed
// at load as an edition-2026 handler, and each handler's target resolves
// against the registered functions and builtins, as it must at boot
// (validateToolHandlerTargets refuses a strict boot otherwise).
func TestV1CorpusToolHandlersResolve(t *testing.T) {
	tools := newToolRegistry()
	queryHandlers := 0
	for _, f := range corpusFiles(t, true) {
		for _, s := range ExtractKeywordSlices(f.Content, "tool") {
			decl, err := languageParser.ParseToolDecl(s.Source)
			require.NoErrorf(t, err, "%s: tool %s", f.Path, s.Name)
			if decl.Disabled {
				continue
			}
			loaded, err := toolDeclToTool(decl, "unified:"+f.Path)
			require.NoErrorf(t, err, "%s: tool %s", f.Path, decl.Name)
			for _, tool := range loaded {
				require.NoError(t, ValidateTool(tool))
				if strings.EqualFold(tool.Handler.Type, "query") {
					queryHandlers++
					require.NotContainsf(t, tool.Handler.Query, "$args.", "%s: the codemod leaves no placeholder", tool.Name)
					require.NotNilf(t, tool.Handler.queryV1, "%s: a migrated handler is parsed at load", tool.Name)
				}
				require.NoError(t, tools.Upsert(tool))
			}
		}
	}
	require.Equal(t, len(toolHandlerCorpus), queryHandlers, "the migrated tree carries the tree's query handlers")

	functions := newFunctionRegistry()
	_, err := LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	_, _, err = LoadUnifiedFunctions(nil, functions, memorynodes.DefaultRegistry())
	require.NoError(t, err)
	_, err = LoadUnifiedBuiltins(nil, functions)
	require.NoError(t, err)
	require.Empty(t, validateToolHandlerTargets(tools, functions), "every migrated handler names a registered target")
}

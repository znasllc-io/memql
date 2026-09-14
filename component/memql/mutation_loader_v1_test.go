package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// mutation_loader_v1_test.go -- the function loader building a mutation's
// template from the edition-2026 parse (epic memql#5363, memql#5367).
//
// A mutation parsed with parser.Options.ExpressionsV1 carries FunctionDef.
// ExpressionsV1, and the loader builds its template with newMutationTemplateV1
// from the parsed nodes (MutationStmt.PayloadExpr and the four slots) instead
// of reading PayloadRaw's text. The fixture tree below is written in TODAY's
// spelling; its edition-2026 twin is what memqlmigrate --rewrite=expressions
// writes for it, generated here by the codemod's own entry point, so every
// comparison in this file and in mutation_tree_v1_db_test.go is between the
// tree as it is and the tree as the migration will leave it.

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
// its overlay. Written in today's spelling -- concat, cond -- which the
// codemod moves to `+` and `? :`.
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
    id: concat("p-", hash(concat(args.probeId, ":", args.title)))
    args.title
    args.status
    note: args.note ?? "none"
    count: args.count ?? 0
    done: false
    tags: [args.tagA, args.tagB, "fixed"]
    details: { source: args.source ?? "import", depth: 1 }
    label: cond(args.status == "active", "LIVE", "IDLE")
    ref: concat("ref-", args.probeId)
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
		"concepts.memql": {Data: []byte(v1TreeFixtureConcepts)},
	})
	t.Cleanup(func() {
		memqldsl.UnregisterTree(v1TreeFixtureDomain)
		memorynodes.ReplaceAll(before)
	})
}

// migratedV1TreeFixture is the fixture as memqlmigrate --rewrite=expressions
// leaves it.
func migratedV1TreeFixture(t *testing.T) string {
	t.Helper()
	src := []byte(v1TreeFixtureMutations)
	preds, err := languageParser.CollectPredicates(map[string][]byte{v1TreeFixtureDomain + "/mutations.memql": src})
	require.NoError(t, err)
	out, err := languageParser.RewriteExpressions(src, preds)
	require.NoError(t, err)
	require.NotEqual(t, v1TreeFixtureMutations, string(out),
		"the codemod must change the fixture (concat, cond), or the two grammars are compared over one spelling")
	return string(out)
}

// loadV1TreeMutations loads every mutation of src through the loader's own
// entry point (dispatchPerConstructParser, as LoadUnifiedFunctions does), with
// the edition-2026 grammar on or off. DefaultOptions is what the engine's
// loaders parse with, so the swap is exactly the one-line flip, scoped to this
// call; no test in this package runs in parallel.
func loadV1TreeMutations(t *testing.T, src string, v1 bool, registry memorynodes.Registry) map[string]*Function {
	t.Helper()
	saved := languageParser.DefaultOptions
	languageParser.DefaultOptions = languageParser.Options{ExpressionsV1: v1}
	defer func() { languageParser.DefaultOptions = saved }()

	out := map[string]*Function{}
	for _, slice := range ExtractFunctionSlices(src) {
		if slice.Kind != languageParser.FunctionTypeMutation {
			continue
		}
		fn, err := dispatchPerConstructParser(slice, v1TreeFixtureOrigin, registry)
		require.NoErrorf(t, err, "load %s (ExpressionsV1=%v)", slice.Name, v1)
		require.NotNil(t, fn.MutationTemplate)
		require.Equalf(t, v1, fn.MutationTemplate.ValuesV1, "%s: the template follows the grammar it was parsed with", slice.Name)
		out[slice.Name] = fn
	}
	require.Len(t, out, 5, "the fixture declares five mutations")
	return out
}

// TestV1TreeFixtureParsesUnderTheOption: the migrated fixture parses with
// ParseFileWithOptions(ExpressionsV1) -- each mutation a definition marked
// ExpressionsV1 whose payload is a v1 map literal of explicit entries (the
// bare mirrors and accept names expanded by the rewriter) and whose id is a v1
// node.
func TestV1TreeFixtureParsesUnderTheOption(t *testing.T) {
	migrated := migratedV1TreeFixture(t)
	for _, slice := range ExtractFunctionSlices(migrated) {
		normalised, err := languageParser.NormaliseAll(slice.Source)
		require.NoError(t, err)
		file, err := languageParser.ParseFileWithOptions(normalised, languageParser.Options{ExpressionsV1: true})
		require.NoErrorf(t, err, "%s", slice.Name)
		fn := file.Definitions[0].(*languageParser.FunctionDef)
		require.True(t, fn.ExpressionsV1, slice.Name)
		stmt := fn.Body.(*languageParser.MutationStmt)
		payload, ok := stmt.PayloadExpr.(*ast.MapExpr)
		require.Truef(t, ok, "%s: payload is %T", slice.Name, stmt.PayloadExpr)
		for _, en := range payload.Entries {
			require.NotContainsf(t, en.Key, ".", "%s: a key-less mirror reached the map", slice.Name)
		}
		_, isNode := stmt.IDTemplate.(ast.ExpressionNode)
		require.Truef(t, isNode, "%s: the id slot is a v1 node", slice.Name)
	}
	require.Contains(t, migrated, `hash(args.probeId + ":" + args.title)`, "the composite id is `+` after the migration")
	require.Contains(t, migrated, `args.status == "active" ? "LIVE" : "IDLE"`, "cond is `? :` after the migration")
}

// TestLoaderBuildsTheTemplateFromTheGrammar: the loader builds a v1 template
// for a definition parsed with ExpressionsV1 and the string half's for one
// parsed without, and the annotation fields ride on either.
func TestLoaderBuildsTheTemplateFromTheGrammar(t *testing.T) {
	mountV1TreeFixture(t)
	_, err := LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	registry := memorynodes.DefaultRegistry()

	legacy := loadV1TreeMutations(t, v1TreeFixtureMutations, false, registry)
	v1 := loadV1TreeMutations(t, migratedV1TreeFixture(t), true, registry)

	for name, fn := range v1 {
		require.Equalf(t, legacy[name].MutationTemplate.Concept, fn.MutationTemplate.Concept, "%s binds the same concept", name)
		require.Equalf(t, legacy[name].MutationTemplate.Kind, fn.MutationTemplate.Kind, "%s is the same write kind", name)
	}
	require.Equal(t, []string{"details"}, v1["updateProbe"].MutationTemplate.MergeFields, "@mergeFields rides on the v1 template")
	require.True(t, isPayloadSplat(v1["replaceProbe"].MutationTemplate.PayloadTemplate), "the payload splat is laid out as the loader always did")
	require.Contains(t, v1["replaceProbe"].MutationTemplate.PayloadOverlayTemplate, "ownerUserId", "with its explicit fields as the overlay")
}

// TestLoaderRunsC5OnAV1Template: C5 (memql#2035) refuses, at load, a v1
// mutation writing an @serverSet field from caller args, exactly as it
// refuses the string half's.
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
	saved := languageParser.DefaultOptions
	t.Cleanup(func() { languageParser.DefaultOptions = saved })
	for _, v1 := range []bool{false, true} {
		languageParser.DefaultOptions = languageParser.Options{ExpressionsV1: v1}
		_, err := tryParseNewFunctionSyntax("mutBad", "mutation", src, "unified:cognition/mutations.memql", registry)
		require.Errorf(t, err, "ExpressionsV1=%v", v1)
		require.Containsf(t, err.Error(), "@serverSet", "ExpressionsV1=%v", v1)
	}
}

// TestLoaderRefusesAV1ValueAtLoad: a v1 mutation value the position cannot
// evaluate is a load error (a baseloader Skip strict boot refuses), not a
// failure on the first call.
func TestLoaderRefusesAV1ValueAtLoad(t *testing.T) {
	saved := languageParser.DefaultOptions
	t.Cleanup(func() { languageParser.DefaultOptions = saved })
	languageParser.DefaultOptions = languageParser.Options{ExpressionsV1: true}
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
// the tree, migrated in process
// ---------------------------------------------------------------------------
//
// The two tests below migrate the tree the engine loads -- every .memql file
// baseloader.ReadAll returns -- with memqlmigrate --rewrite=expressions' own
// entry points (CollectPredicates, then RewriteExpressions per file), and load
// every mutate construct of the result with the edition-2026 grammar. They are
// the flip's evidence for mutation values ahead of the flip: that every
// mutation the migration writes loads, and renders the row today's template
// renders. After the flip, TestUnifiedTreeLoadsClean covers the first half
// directly; the second is the migration's no-change claim and stays true by
// construction once there is one grammar.

// corpusFiles reads the tree the engine loads, as it is or as the codemod
// migrates it -- CollectPredicates over every file, then RewriteExpressions
// per file, which is what memqlmigrate --rewrite=expressions runs.
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

// corpusMutations loads every mutate construct of the tree, migrated or as it
// is, with the matching grammar, keyed by path and name. It fails the test on
// any construct that does not load.
func corpusMutations(t *testing.T, migrated bool) map[string]*Function {
	t.Helper()
	files := corpusFiles(t, migrated)

	saved := languageParser.DefaultOptions
	languageParser.DefaultOptions = languageParser.Options{ExpressionsV1: migrated}
	defer func() { languageParser.DefaultOptions = saved }()

	out := map[string]*Function{}
	var failures []string
	for _, f := range files {
		for _, slice := range ExtractFunctionSlices(f.Content) {
			if slice.Kind != languageParser.FunctionTypeMutation {
				continue
			}
			fn, err := dispatchPerConstructParser(slice, "unified:"+f.Path, memorynodes.DefaultRegistry())
			switch {
			case err != nil:
				failures = append(failures, f.Path+" "+slice.Name+": "+err.Error())
			case fn == nil || fn.MutationTemplate == nil || fn.MutationTemplate.ValuesV1 != migrated:
				failures = append(failures, f.Path+" "+slice.Name+": the template does not follow the grammar")
			default:
				out[f.Path+" "+slice.Name] = fn
			}
		}
	}
	require.Emptyf(t, failures, "%d mutations do not load (migrated=%v):\n%s", len(failures), migrated, strings.Join(failures, "\n"))
	return out
}

// TestV1CorpusMutationsBuild: every mutate construct in the tree, migrated by
// the codemod, loads with the edition-2026 grammar into a v1 template -- the
// same set that loads today.
func TestV1CorpusMutationsBuild(t *testing.T) {
	_, err := LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	legacy := corpusMutations(t, false)
	v1 := corpusMutations(t, true)
	require.Greater(t, len(legacy), 300, "the tree has hundreds of mutations; a small count means the walk went blind")
	require.Equal(t, len(legacy), len(v1), "the migrated tree loads the same mutations")
	for key := range legacy {
		require.Containsf(t, v1, key, "%s loads today and not after the migration", key)
	}
}

// TestV1CorpusMutationsRenderTheLegacyRows: every mutation's v1 template
// renders the row its template renders today -- every argument filled, and
// only the required ones -- so the migration changes no row a mutation of the
// tree writes. A render that depends on the clock (an id that hashes `now`
// when its timestamp argument is absent) is recognised by the legacy render
// differing from itself, and is not a difference.
func TestV1CorpusMutationsRenderTheLegacyRows(t *testing.T) {
	_, err := LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	legacy := corpusMutations(t, false)
	v1 := corpusMutations(t, true)

	e := &MemQLEngine{concepts: memorynodes.DefaultRegistry()}
	ctx := auth.ContextWithUserActor(context.Background(), "user-corpus")
	keys := make([]string, 0, len(legacy))
	for k := range legacy {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	compared, clock := 0, 0
	var diffs []string
	for _, key := range keys {
		for _, requiredOnly := range []bool{false, true} {
			args := corpusArgs(legacy[key].ArgsSchema, requiredOnly)
			want := corpusRender(e, ctx, legacy[key], args)
			got := corpusRender(e, ctx, v1[key], args)
			if want == got {
				compared++
				continue
			}
			if again := corpusRender(e, ctx, legacy[key], args); again != want {
				clock++
				continue
			}
			diffs = append(diffs, fmt.Sprintf("%s (required args only: %v)\n    today: %s\n    v1:    %s", key, requiredOnly, want, got))
		}
	}
	require.Emptyf(t, diffs, "%d renders differ:\n%s", len(diffs), strings.Join(diffs, "\n"))
	require.Greater(t, compared, 2*300-clock-1, "most renders must compare equal; a low count means the renders went blind")
	t.Logf("%d renders identical, %d clock-dependent, of %d mutations", compared, clock, len(keys))
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

// corpusArgs builds a call's arguments from a mutation's args schema: a value
// of the declared type for every field, or for the required ones only.
func corpusArgs(schema *ArgsSchemaConfig, requiredOnly bool) map[string]any {
	out := map[string]any{}
	if schema == nil {
		return out
	}
	for _, f := range schema.Fields {
		if f == nil || (requiredOnly && f.Optional) {
			continue
		}
		out[f.Name] = corpusValue(f)
	}
	return out
}

func corpusValue(f *FunctionArgsField) any {
	if len(f.Enum) > 0 {
		return f.Enum[0]
	}
	typ := strings.ToLower(f.Type)
	switch {
	case typ == "int" || typ == "integer" || typ == "number" || typ == "float":
		return float64(3)
	case typ == "bool" || typ == "boolean":
		return true
	case typ == "object":
		m := map[string]any{}
		for _, n := range f.Nested {
			if n != nil {
				m[n.Name] = corpusValue(n)
			}
		}
		if len(m) == 0 {
			m["k"] = "v"
		}
		return m
	case typ == "array" || strings.HasPrefix(typ, "[]"):
		return []any{"a"}
	case typ == "datetime" || typ == "date":
		return "2026-09-13T00:00:00Z"
	}
	return "s-" + f.Name
}

// corpusTimestamp matches a rendered timestamp, which two renders a moment
// apart legitimately disagree on.
var corpusTimestamp = regexp.MustCompile(`"20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z"`)

// corpusRender renders a mutation for args, as a comparable string: the
// node's id, slots and kind and its payload, timestamps masked. The args are
// copied per render, so neither renderer can see what the other did to them.
func corpusRender(e *MemQLEngine, ctx context.Context, fn *Function, args map[string]any) string {
	raw, _ := json.Marshal(args)
	var call map[string]any
	_ = json.Unmarshal(raw, &call)
	node, err := e.renderMutationTemplate(ctx, fn.MutationTemplate, call)
	if err != nil {
		return "refused: " + err.Error()
	}
	var payload any
	_ = json.Unmarshal([]byte(node.PayloadRaw), &payload)
	body, _ := json.Marshal(payload)
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	s := fmt.Sprintf("kind=%s id=%s createdAt=%v parent=%s aliasOf=%s payload=%s",
		node.Kind, node.ID, node.CreatedAt != nil, deref(node.ParentRef), deref(node.AliasOfRef), body)
	return corpusTimestamp.ReplaceAllString(s, `"<ts>"`)
}

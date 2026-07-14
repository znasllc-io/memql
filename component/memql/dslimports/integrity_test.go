package dslimports

// Tests for VerifyReferentialIntegrity (znasllc-io/memql#2509): the four
// lint lanes that make memqllint catch what engine boot would reject --
// Form B use-decl module + symbol resolution, signature-concept existence,
// insert/update fields vs concept schemas, and stranded (unused) imports.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// loadTree is a test helper: Load must succeed (the integrity lanes are the
// object under test, not the parse layer).
func loadTree(t *testing.T, root fstest.MapFS) *Tree {
	t.Helper()
	tree, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return tree
}

func file(content string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(content)}
}

// demoConcepts is a minimal concepts module used across the fixtures.
const demoConcepts = `@version("1.0.0")
@namespace("demo")
@description("A demo item.")
concept item {
  name    string  @required @description("Item name.")
  status  string  @description("Item status.")
}`

// assertFindings asserts that the verifier returns exactly len(want)
// diagnostics and that each want[i] substring appears in some diagnostic.
func assertFindings(t *testing.T, tree *Tree, want ...string) {
	t.Helper()
	errs := tree.VerifyReferentialIntegrity()
	if len(errs) != len(want) {
		t.Fatalf("got %d diagnostics, want %d:\n%s", len(errs), len(want), joinErrs(errs))
	}
	for _, w := range want {
		found := false
		for _, e := range errs {
			if strings.Contains(e.Error(), w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no diagnostic contains %q; got:\n%s", w, joinErrs(errs))
		}
	}
}

func joinErrs(errs []error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString("  - " + e.Error() + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Lane 1: Form B use-decl resolution
// ---------------------------------------------------------------------------

func TestVerify_UseModuleMissing(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/queries.memql": file(`use demo.nonexistentfile.{ ghost }

@enabled
@description("References a module that does not exist.")
query ghost queryGhosts {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
	})
	assertFindings(t, tree,
		`demo/queries.memql: use demo.nonexistentfile: module does not resolve to a file in the DSL root (expected demo/nonexistentfile.memql)`)
}

func TestVerify_UseSymbolMissing(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/queries.memql": file(`use demo.concepts.{ item, deletedConcept }

@enabled
@description("Imports a concept that was deleted from concepts.memql. deletedConcept is referenced here so only the missing-symbol lane fires.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
	})
	assertFindings(t, tree,
		`demo/queries.memql: use demo.concepts: "deletedConcept" is not declared in demo/concepts.memql`)
}

func TestVerify_ExternalNamespaceSkipped(t *testing.T) {
	// A product bundle linting standalone imports engine namespaces that do
	// not exist in its root -- those must be treated as external, not broken.
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/queries.memql": file(`use cognition.concepts.{ space }
use demo.concepts.{ item }

@enabled
@description("space is engine-side; item is local. space is referenced in the description... no -- referenced here: space.")
query item queryBySpace {
  args {
    spaceId  string  @required
  }
  filter  name == args.spaceId
}`),
	})
	assertFindings(t, tree)
}

func TestVerify_VersionPrefixedUsePath(t *testing.T) {
	// A leading version segment is stripped, matching the boot loaders'
	// tolerance: v1.demo.concepts resolves to demo/concepts.memql.
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/queries.memql": file(`use v1.demo.concepts.{ item }

@enabled
@description("Version-prefixed module path.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
	})
	assertFindings(t, tree)
}

func TestVerify_ConsolidatedCapabilityModule(t *testing.T) {
	// `use capabilities.integration.github.{ tagRelease }` resolves against
	// the namespace-consolidated capabilities/capabilities.memql file, whose
	// capability declarations carry dotted names.
	caps := `@sideEffect("write")
@description("Create a git tag + GitHub release for a version.")
capability integration.github.tagRelease {
  args {
    repo string @required
    tag  string @required
  }
}

@sideEffect("exec")
@description("Run an allowlisted capability script.")
capability shell.script {
  args {
    script string @required
  }
}`
	actions := `use capabilities.shell.{ script }
use capabilities.integration.github.{ tagRelease }

@description("Tag a release.")
action tagIt {
  args {
    tag string @required
  }
  capability tagRelease(repo: "znasllc-io/memql", tag: args.tag)
}

@description("Run a script.")
action runIt {
  args {
    name string @required
  }
  capability script(script: args.name)
}`
	tree := loadTree(t, fstest.MapFS{
		"capabilities/capabilities.memql": file(caps),
		"deployment/actions.memql":        file(actions),
	})
	assertFindings(t, tree)

	// A typo'd verb in a CLOSED capability namespace is reported.
	tree = loadTree(t, fstest.MapFS{
		"capabilities/capabilities.memql": file(caps),
		"deployment/actions.memql": file(`use capabilities.integration.github.{ tagReleaze }

@description("Typo in the imported capability verb; referenced below.")
action tagIt {
  args {
    tag string @required
  }
  capability tagReleaze(repo: "znasllc-io/memql", tag: args.tag)
}`),
	})
	assertFindings(t, tree,
		`deployment/actions.memql: use capabilities.integration.github: "integration.github.tagReleaze" is not declared in capabilities/capabilities.memql`)
}

func TestVerify_OpenCapabilityNamespaceSkipped(t *testing.T) {
	// mcp.* capability verbs are runtime-dynamic (the MCP server's own tool
	// list) and never declared in the catalog; boot binds them textually, so
	// the symbol check must skip them.
	tree := loadTree(t, fstest.MapFS{
		"capabilities/capabilities.memql": file(`@sideEffect("exec")
@description("Run an allowlisted capability script.")
capability shell.script {
  args {
    script string @required
  }
}`),
		"deployment/actions.memql": file(`use capabilities.mcp.github.{ searchIssues }

@description("Calls a runtime-dynamic MCP verb.")
action findIssues {
  args {
    query string @required
  }
  capability searchIssues(query: args.query)
}`),
	})
	assertFindings(t, tree)
}

func TestVerify_ImportsOnlyTargetSkipped(t *testing.T) {
	// A comment-only module lands in the tree as the imports-only parse
	// projection; the verifier must not use its (empty) declaration set as
	// evidence that a symbol is missing.
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/builtins.memql": file("// comment-only module: declarations not visible to the generic parser\n"),
		"demo/queries.memql": file(`use demo.builtins.{ someBuiltin }

@enabled
@description("Imports from an imports-only file; someBuiltin used below.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name && someBuiltin == true
}`),
	})
	if !tree.ImportsOnly["demo/builtins.memql"] {
		t.Fatalf("expected demo/builtins.memql to be tracked as imports-only; got %v", tree.ImportsOnly)
	}
	assertFindings(t, tree)
}

// ---------------------------------------------------------------------------
// Lane 2: signature bindings
// ---------------------------------------------------------------------------

func TestVerify_SignatureConceptMissing(t *testing.T) {
	// The file authors in-root Form B imports (positive evidence), the tree
	// has no imports-only files, and no import is external -- so a signature
	// concept absent from the whole tree is provably missing.
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/queries.memql": file(`use demo.concepts.{ item }

@enabled
@description("A healthy import-first query.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}

@enabled
@description("Binds a concept that exists nowhere in the tree.")
query phantom queryPhantoms {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
	})
	assertFindings(t, tree,
		`demo/queries.memql: signature concept "phantom" is not declared anywhere in the DSL root`)
}

func TestVerify_ZeroImportFileNeverProvablyMissing(t *testing.T) {
	// Boot resolves signature concepts via the global registry with no
	// import required, and the registry may include runtime-mounted domains
	// outside the linted root. A file with zero Form B imports therefore
	// offers no evidence the author works in-root -- stay silent.
	tree := loadTree(t, fstest.MapFS{
		"myapp/mutations.memql": file(`@enabled
@description("Binds an engine concept without any import; boots green when the bundle mounts alongside the engine tree.")
mutate space productCreateSpace {
  args {
    spaceId string @required
  }
  insert {
    id: args.spaceId
  }
}`),
	})
	assertFindings(t, tree)
}

func TestVerify_BrokenSiblingSuppressesMissing(t *testing.T) {
	// When ANY file fell back to the imports-only projection, its
	// declarations are invisible -- a "not declared anywhere" verdict is no
	// longer provable, and only the parse diagnostic should surface.
	root := fstest.MapFS{
		"demo/concepts.memql": file(`@version("1.0.0")
@namespace("demo")
@description("Broken on purpose.")
concept thing {
  name string @required @@@ this does not parse
}`),
		"demo/mutations.memql": file(`use demo.mutationshelpers.{ nothing }

@enabled
@description("Valid file binding the concept declared in the broken sibling.")
mutate thing createThing {
  args {
    thingId string @required
  }
  insert {
    id: args.thingId
  }
}`),
		"demo/mutationshelpers.memql": file(demoConcepts),
	}
	tree, err := Load(root)
	if err == nil {
		t.Fatalf("expected a parse diagnostic from the broken concepts file")
	}
	if len(tree.ImportsOnly) == 0 {
		t.Skipf("fixture did not produce an imports-only fallback; adjust the fixture")
	}
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "not declared anywhere") {
			t.Errorf("false positive on a partial tree: %v", e)
		}
	}
}

func TestVerify_SignatureConceptViaImportAndGlobal(t *testing.T) {
	// Resolves via the file's own import; and, in a second file, via the
	// tree-global unique-name fallback (no import at all).
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/queries.memql": file(`use demo.concepts.{ item }

@enabled
@description("Signature concept resolved through the file import.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
		"other/queries.memql": file(`@enabled
@description("Signature concept resolved through the global fallback.")
query item queryOtherItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
	})
	assertFindings(t, tree)
}

func TestVerify_SignatureConceptSkippedWithExternalImports(t *testing.T) {
	// The file imports an external namespace -- the missing concept may live
	// there, so the verifier stays silent.
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/queries.memql": file(`use cognition.concepts.{ space }

@enabled
@description("space is external; the signature binds it. Referenced: space.")
query space querySpaces {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
	})
	assertFindings(t, tree)
}

func TestVerify_SignatureConceptResolvesToNonConcept(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/logic.memql": file(`@enabled
@description("A logic construct sharing a name the query below binds.")
logic widget {
  args {
    event object @required
  }
  body {
    return true
  }
}`),
		"demo/queries.memql": file(`use demo.logic.{ widget }

@enabled
@description("Signature binds an imported name that is a logic, not a concept. Referenced: widget.")
query widget queryWidgets {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
	})
	assertFindings(t, tree,
		`demo/queries.memql: signature concept "widget" resolves to a logic, not a concept`)
}

func TestVerify_NonConceptImportDoesNotShadowConcept(t *testing.T) {
	// Boot resolves signature concepts through the concept registry only: a
	// logic imported under the same bare name does not shadow a concept that
	// exists elsewhere in the tree. No finding.
	tree := loadTree(t, fstest.MapFS{
		"other/concepts.memql": file(`@version("1.0.0")
@namespace("other")
@description("The real concept.")
concept widget {
  label string @required @description("Label.")
}`),
		"demo/logic.memql": file(`@enabled
@description("Same-named logic.")
logic widget {
  args {
    event object @required
  }
  body {
    return true
  }
}`),
		"demo/queries.memql": file(`use demo.logic.{ widget }

@enabled
@description("The signature concept resolves globally to other/concepts.memql despite the same-named logic import. Referenced: widget.")
query widget queryWidgets {
  args {
    label  string  @required
  }
  filter  label == args.label
}`),
	})
	assertFindings(t, tree)
}

func TestVerify_AmbiguousUnimportedConceptReported(t *testing.T) {
	// An unimported signature concept declared in two namespaces is
	// boot-fatal (the registry match has no namespace hint), so the lane
	// reports it. Importing the name resolves the ambiguity.
	otherConcepts := `@version("1.0.0")
@namespace("other")
@description("Another item.")
concept item {
  label string @required @description("Label.")
}`
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql":  file(demoConcepts),
		"other/concepts.memql": file(otherConcepts),
		"third/queries.memql": file(`use demo.concepts.{ item }

@enabled
@description("Imported: unambiguous. Referenced: item.")
query item queryImported {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
		"fourth/queries.memql": file(`use fourth.helpers.{ helperItemQueryDoc }

@enabled
@description("NOT imported: ambiguous across demo and other. helperItemQueryDoc referenced here.")
query item queryUnimported {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
		"fourth/helpers.memql": file(`@description("Helper shape so the fourth file authors an in-root import.")
@row
shape item helperItemQueryDoc {
  row.id
  name
}`),
	})
	// Note: fourth/helpers.memql binds `item` via its shape signature too,
	// and it has no imports of its own, so it is not provably missing there
	// (zero-import rule) -- but it IS ambiguous... it has no Form B imports,
	// so the ambiguity report applies only to fourth/queries.memql, whose
	// import list proves in-root authorship.
	errs := tree.VerifyReferentialIntegrity()
	var ambiguous []string
	for _, e := range errs {
		if strings.Contains(e.Error(), "cannot disambiguate") {
			ambiguous = append(ambiguous, e.Error())
		} else {
			t.Errorf("unexpected diagnostic: %v", e)
		}
	}
	if len(ambiguous) == 0 {
		t.Errorf("expected an ambiguity diagnostic for fourth/queries.memql; got:\n%s", joinErrs(errs))
	}
	for _, a := range ambiguous {
		if !strings.Contains(a, `signature concept "item" is not imported and is declared in 2 places`) {
			t.Errorf("ambiguity diagnostic malformed: %s", a)
		}
	}
}

func TestVerify_SpecBoundNameMissing(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/specs.memql": file(`use demo.concepts.{ item }

@enabled
@description("A healthy concept-bound spec.")
spec item specIsActive {
  return status == "active"
}

@enabled
@description("Binds a shape/concept that exists nowhere.")
spec ghostShape specIsGhost {
  return status == "ghost"
}`),
	})
	assertFindings(t, tree,
		`demo/specs.memql: spec "specIsGhost" binds "ghostShape", which is not declared as a shape or concept anywhere in the DSL root`)
}

// ---------------------------------------------------------------------------
// Lane 3: insert/update fields vs concept schema
// ---------------------------------------------------------------------------

func TestVerify_InsertFieldNotOnSchema(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/mutations.memql": file(`use demo.concepts.{ item }

@enabled
@description("Writes a field the concept does not declare.")
mutate item createItem {
  args {
    itemId  string  @required
    name    string  @required
  }
  insert {
    id: args.itemId
    args.name
    status: "active"
    bogusField: "not-on-schema"
  }
}`),
	})
	assertFindings(t, tree,
		`demo/mutations.memql: mutation "createItem": insert writes field "bogusField", which concept "item" does not declare`)
}

func TestVerify_InsertCleanFormsPass(t *testing.T) {
	// Keyed fields, bare args shorthand, row intrinsics, and the literal
	// `payload` name (the boot loader's whole-payload splat wrapper, in both
	// bare and keyed form) must all pass.
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/mutations.memql": file(`use demo.concepts.{ item }

@enabled
@description("Every write form that must lint clean.")
mutate item upsertItem {
  args {
    itemId  string  @required
    name    string  @required
    payload object  @required
  }
  insert {
    id: args.itemId
    args.name
    status: coalesce(args.name, "active")
    createdAt: now
    createdBy: actor.userId
    args.payload
  }
}`),
	})
	assertFindings(t, tree)
}

func TestVerify_ObjectArgContributesField(t *testing.T) {
	// The splat exception is keyed on the literal name `payload` (mirroring
	// function_loader.go), NOT on the arg's declared type: a bare
	// object-typed args.config is a single-field write of `config` at boot,
	// so an undeclared `config` field is a real finding.
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/mutations.memql": file(`use demo.concepts.{ item }

@enabled
@description("Object-typed bare arg that is NOT the payload splat.")
mutate item configureItem {
  args {
    itemId    string  @required
    settings  object  @required
  }
  insert {
    id: args.itemId
    args.settings
  }
}`),
	})
	assertFindings(t, tree,
		`demo/mutations.memql: mutation "configureItem": insert writes field "settings", which concept "item" does not declare`)
}

func TestVerify_UpdateFieldNotOnSchema(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/mutations.memql": file(`use demo.concepts.{ item }

@enabled
@description("Partial update writing an undeclared field.")
mutate item renameItem {
  args {
    itemId  string  @required
    title   string  @required
  }
  update {
    id: args.itemId
    title: args.title
  }
}`),
	})
	assertFindings(t, tree,
		`demo/mutations.memql: mutation "renameItem": update writes field "title", which concept "item" does not declare`)
}

// ---------------------------------------------------------------------------
// Lane 4: unused imports
// ---------------------------------------------------------------------------

func TestVerify_StrandedImportAfterCallRename(t *testing.T) {
	// The issue's verified repro: an automation step's logic call renamed to
	// a nonexistent construct lints clean pre-#2509. The call site cannot be
	// resolved tree-globally (intrinsics / runtime-mounted product constructs
	// are legitimately absent), but the rename strands the original import.
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/logic.memql": file(`@enabled
@description("Decides something about an item event.")
logic decideThing {
  args {
    event object @required
  }
  body {
    return true
  }
}`),
		"demo/automations.memql": file(`use demo.logic.{ decideThing }

@enabled
@trigger(event="graph.node.created.v1:demo:item")
@description("The step call was renamed to a nonexistent construct, stranding the file-top logic import.")
automation onItemCreated {
  step decide {
    logic decideThingX ( event: event )
  }
}`),
	})
	assertFindings(t, tree,
		`demo/automations.memql: use demo.logic: imported symbol "decideThing" is never referenced in the file body`)
}

func TestVerify_MultiLineUseDeclBlankedAsOneSpan(t *testing.T) {
	// A brace list spanning lines is one use-decl span: names on
	// continuation lines must not count as body usage of themselves.
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts + `

@description("A second demo concept.")
concept other {
  label string @required @description("Label.")
}`),
		"demo/queries.memql": file(`use demo.concepts.{
  item,
  other
}

@enabled
@description("Only the first imported symbol is used below; the second is stranded on a continuation line.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
	})
	assertFindings(t, tree,
		`demo/queries.memql: use demo.concepts: imported symbol "other" is never referenced in the file body`)
}

func TestVerify_UnusedPrefixComposedImport(t *testing.T) {
	// Lane 4 fires for prefix-composed (consolidated capability) imports too.
	tree := loadTree(t, fstest.MapFS{
		"capabilities/capabilities.memql": file(`@sideEffect("write")
@description("Create a git tag + GitHub release for a version.")
capability integration.github.tagRelease {
  args {
    repo string @required
    tag  string @required
  }
}`),
		"deployment/actions.memql": file(`use capabilities.integration.github.{ tagRelease }

@description("Imports a capability verb but never calls it.")
action noopAction {
  args {
    tag string @required
  }
  capability somethingElse(tag: args.tag)
}`),
	})
	assertFindings(t, tree,
		`deployment/actions.memql: use capabilities.integration.github: imported symbol "tagRelease" is never referenced in the file body`)
}

func TestVerify_MissingSymbolNotDoubleReportedAsUnused(t *testing.T) {
	// A symbol that is both missing from its module AND unreferenced gets
	// exactly one diagnostic (the missing-symbol one).
	tree := loadTree(t, fstest.MapFS{
		"demo/concepts.memql": file(demoConcepts),
		"demo/queries.memql": file(`use demo.concepts.{ item, deletedConcept }

@enabled
@description("deletedConcept neither exists nor is referenced.")
query item queryItems {
  args {
    name  string  @required
  }
  filter  name == args.name
}`),
	})
	assertFindings(t, tree,
		`demo/queries.memql: use demo.concepts: "deletedConcept" is not declared in demo/concepts.memql`)
}

// ---------------------------------------------------------------------------
// Real-tree regression: the engine's own DSL tree must stay clean
// ---------------------------------------------------------------------------

func TestVerifyReferentialIntegrity_RealDSLTree(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dslRoot := filepath.Clean(filepath.Join(wd, "..", "..", "..", "dsl"))
	if _, err := os.Stat(dslRoot); err != nil {
		t.Skipf("dsl tree not found at %s: %v", dslRoot, err)
	}
	tree, err := Load(os.DirFS(dslRoot))
	if err != nil {
		t.Fatalf("Load(dsl/): %v", err)
	}
	if errs := tree.VerifyReferentialIntegrity(); len(errs) != 0 {
		t.Errorf("engine dsl/ tree has %d referential-integrity findings:\n%s", len(errs), joinErrs(errs))
	}
	if errs := tree.VerifyAllSymbolReferences(); len(errs) != 0 {
		t.Errorf("engine dsl/ tree has %d legacy symbol-ref findings:\n%s", len(errs), joinErrs(errs))
	}
}

// TestSignatureConceptRegexMatchesBootLoader locks the lint-side signature
// extraction to the boot loader's: the exact regex literal in
// component/memql/function_loader.go must match integrity.go's. A drift means
// lint and boot disagree about what a signature binding is.
func TestSignatureConceptRegexMatchesBootLoader(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	loaderPath := filepath.Clean(filepath.Join(wd, "..", "function_loader.go"))
	b, err := os.ReadFile(loaderPath)
	if err != nil {
		t.Skipf("function_loader.go not readable at %s: %v", loaderPath, err)
	}
	if !strings.Contains(string(b), signatureConceptRe.String()) {
		t.Errorf("component/memql/function_loader.go no longer contains the signature-concept "+
			"regex used by dslimports integrity checking:\n  %s\nkeep the two in sync", signatureConceptRe.String())
	}
}

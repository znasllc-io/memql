package memql

// construct_catalog_test.go covers the acceptance properties of the registry-
// grain construct catalog (memql#3749):
//
//   - every kind enumerated, with counts that match what the engine loaded;
//   - origin correct across all three sources -- core, bundle, promoted;
//   - a promoted construct appears the moment it is promoted and disappears the
//     moment it is demoted, which is the case a file walk structurally cannot do
//     and therefore the case worth a test;
//   - the argument shape is the LANGUAGE SERVER's, not a second one derived
//     from the registry.

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/sense"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// catalogFixtureDomain is a product domain mounted the way MEMQL_DSL_PATH
// mounts one -- RegisterTree -- so a construct in it must read as `bundle`.
const catalogFixtureDomain = "catalogfixture"

const catalogFixtureConcepts = `/// A widget owned by one user.
@displayCard(primary="label")
@rowAuthz(owner="ownerUserId")
concept widget {
  ownerUserId string! @description("Owner.")
  label       string  @description("Display label.")
  status      string  @description("Lifecycle state.")
}
`

const catalogFixtureShapes = `use catalogfixture.concepts.{ widget }

@description("Widget summary")
@row
shape widget widgetFull {
  row.id
  label
  status
}
`

const catalogFixtureQueries = `use catalogfixture.concepts.{ widget }
use catalogfixture.shapes.{ widgetFull }

/// List the caller's widgets.
@actor
query widget catalogFixtureWidgets {
  args {
    /// Restrict to one widget.
    widgetId  string
    /// Which lifecycle state to read.
    state     string  @enum("live", "retired")
  }
  filter  ownerUserId==actor.userId && when(args.widgetId) { row.id==args.widgetId } && when(args.state) { status==args.state }
  sort    "row.createdAt", "desc"
  shape   widgetFull
}
`

// mountCatalogFixture registers the fixture product domain and returns an
// Init'd engine over core + fixture. Cleanup restores the embedded-only
// registry so the fixture concept does not leak into the rest of the package.
func mountCatalogFixture(t *testing.T) *MemQLEngine {
	t.Helper()

	memqldsl.RegisterTree(catalogFixtureDomain, fstest.MapFS{
		"concepts.memql": {Data: []byte(catalogFixtureConcepts)},
		"shapes.memql":   {Data: []byte(catalogFixtureShapes)},
		"queries.memql":  {Data: []byte(catalogFixtureQueries)},
	})
	t.Cleanup(func() {
		memqldsl.UnregisterTree(catalogFixtureDomain)
		concept.ReplaceAll(nil)
		_, _ = LoadUnifiedConcepts(nil)
	})

	concept.ReplaceAll(nil)
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts over core + fixture: %v", err)
	}
	registry := concept.DefaultRegistry()

	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine Init over core + fixture: %v", err)
	}
	return eng
}

// byKind groups a catalog by kind.
func byKind(entries []ConstructCatalogEntry) map[string][]ConstructCatalogEntry {
	out := map[string][]ConstructCatalogEntry{}
	for _, e := range entries {
		out[e.Kind] = append(out[e.Kind], e)
	}
	return out
}

// findEntry returns the entry for one (kind, name).
func findEntry(entries []ConstructCatalogEntry, kind, name string) (ConstructCatalogEntry, bool) {
	for _, e := range entries {
		if e.Kind == kind && e.Name == name {
			return e, true
		}
	}
	return ConstructCatalogEntry{}, false
}

// TestConstructCatalogEnumeratesEveryKind asserts the catalog reports every
// kind the engine holds a registry for, and that each kind's count is the
// registry's own count rather than a file count.
func TestConstructCatalogEnumeratesEveryKind(t *testing.T) {
	eng := mountCatalogFixture(t)
	groups := byKind(eng.ConstructCatalog())

	// Kinds that come out of a registry with no per-kind discrimination: count
	// parity against that registry is exact.
	if got, want := len(groups[ConstructKindConcept]), len(eng.concepts.List()); got != want {
		t.Errorf("concept count: catalog %d, concept registry %d", got, want)
	}
	if got, want := len(groups[ConstructKindShape]), eng.shapes.Count(); got != want {
		t.Errorf("shape count: catalog %d, shape registry %d", got, want)
	}
	if got, want := len(groups[ConstructKindTool]), eng.tools.Count(); got != want {
		t.Errorf("tool count: catalog %d, tool registry %d", got, want)
	}
	if got, want := len(groups[ConstructKindPrompt]), len(eng.prompts.List()); got != want {
		t.Errorf("prompt count: catalog %d, prompt registry %d", got, want)
	}
	if got, want := len(groups[ConstructKindProvider]), eng.providers.Count(); got != want {
		t.Errorf("provider count: catalog %d, provider registry %d", got, want)
	}
	if got, want := len(groups[ConstructKindPolicy]), eng.policies.Count(); got != want {
		t.Errorf("policy count: catalog %d, policy registry %d", got, want)
	}
	if got, want := len(groups[ConstructKindSeed]), len(eng.seeds.All()); got != want {
		t.Errorf("seed count: catalog %d, seed registry %d", got, want)
	}

	// The function + spec registries hold several kinds each, so parity is over
	// the sum: every entry is reported exactly once, under exactly one kind.
	fnKinds := len(groups[ConstructKindQuery]) + len(groups[ConstructKindMutation]) +
		len(groups[ConstructKindLogic]) + len(groups[ConstructKindBuiltin])
	if got, want := fnKinds, eng.functions.Count(); got != want {
		t.Errorf("function-family count: catalog %d, function registry %d", got, want)
	}
	specKinds := len(groups[ConstructKindSpec]) + len(groups[ConstructKindTrait])
	if got, want := specKinds, eng.specs.Count(); got != want {
		t.Errorf("spec-family count: catalog %d, spec registry %d", got, want)
	}

	// Every kind the tree actually carries must be non-empty, or the catalog is
	// silently dropping a whole category.
	for _, kind := range []string{
		ConstructKindConcept, ConstructKindQuery, ConstructKindMutation,
		ConstructKindLogic, ConstructKindTool, ConstructKindSpec,
		ConstructKindTrait, ConstructKindShape, ConstructKindPrompt,
		ConstructKindProvider, ConstructKindBuiltin, ConstructKindPolicy,
		ConstructKindSeed,
	} {
		if len(groups[kind]) == 0 {
			t.Errorf("kind %q enumerated nothing; the embedded tree carries some", kind)
		}
	}

	// Automations come through the injected cataloger, so with none wired the
	// truthful answer is none -- not a file walk's answer.
	if len(groups[ConstructKindAutomation]) != 0 {
		t.Errorf("automations reported with no cataloger wired: %d", len(groups[ConstructKindAutomation]))
	}
}

// fakeAutomationCataloger is the automations component's seam, without it.
type fakeAutomationCataloger struct{ entries []AutomationCatalogEntry }

func (f fakeAutomationCataloger) CatalogAutomations() []AutomationCatalogEntry { return f.entries }

// TestConstructCatalogReportsWiredAutomations: automations reach the catalog
// through the injected cataloger, and are runnable.
func TestConstructCatalogReportsWiredAutomations(t *testing.T) {
	eng := mountCatalogFixture(t)
	eng.SetAutomationCataloger(fakeAutomationCataloger{entries: []AutomationCatalogEntry{
		{Name: "autoJoinSI", Description: "auto join", Origin: "unified:cognition/automations.memql"},
	}})

	entry, ok := findEntry(eng.ConstructCatalog(), ConstructKindAutomation, "autoJoinSI")
	if !ok {
		t.Fatal("wired automation missing from the catalog")
	}
	if !entry.Runnable {
		t.Error("automation is one of the five runnable kinds and must report runnable")
	}
	if entry.Origin != ConstructOriginCore {
		t.Errorf("automation origin: got %q, want %q", entry.Origin, ConstructOriginCore)
	}
}

// TestConstructCatalogOriginAcrossSources covers the derivation over the two
// file-backed sources. `promoted` gets its own test below, because it is the
// one that cannot be produced by mounting anything.
func TestConstructCatalogOriginAcrossSources(t *testing.T) {
	entries := mountCatalogFixture(t).ConstructCatalog()

	// A core construct: from the embedded tree, with a path and a hash.
	core, ok := findEntry(entries, ConstructKindConcept, "v1:identity:user")
	if !ok {
		t.Fatal("v1:identity:user missing from the catalog")
	}
	if core.Origin != ConstructOriginCore {
		t.Errorf("embedded concept origin: got %q, want %q", core.Origin, ConstructOriginCore)
	}
	if core.OriginPath != "identity/concepts.memql" {
		t.Errorf("embedded concept path: got %q, want identity/concepts.memql", core.OriginPath)
	}
	if core.SourceHash == "" {
		t.Error("embedded concept has no source hash")
	}
	if core.Namespace != "identity" {
		t.Errorf("embedded concept namespace: got %q, want identity", core.Namespace)
	}

	// A bundle construct: from a RegisterTree'd product domain, which is what
	// MEMQL_DSL_PATH mounts.
	bundle, ok := findEntry(entries, ConstructKindQuery, "catalogFixtureWidgets")
	if !ok {
		t.Fatal("fixture query missing from the catalog")
	}
	if bundle.Origin != ConstructOriginBundle {
		t.Errorf("fixture query origin: got %q, want %q", bundle.Origin, ConstructOriginBundle)
	}
	if bundle.OriginPath != catalogFixtureDomain+"/queries.memql" {
		t.Errorf("fixture query path: got %q", bundle.OriginPath)
	}
	if bundle.Namespace != catalogFixtureDomain {
		t.Errorf("fixture query namespace: got %q, want %q", bundle.Namespace, catalogFixtureDomain)
	}
	if !bundle.Runnable {
		t.Error("a query is runnable")
	}
	if bundle.BoundConcept == "" {
		t.Error("a query reports its signature-bound concept")
	}
}

// TestConstructCatalogArgsComeFromTheLanguageServer pins the argument form to
// Sense's own analysis. If these ever diverge the generated argument form
// disagrees with the compiler about what the construct accepts, and the
// developer finds out by running the wrong thing.
func TestConstructCatalogArgsComeFromTheLanguageServer(t *testing.T) {
	entries := mountCatalogFixture(t).ConstructCatalog()
	entry, ok := findEntry(entries, ConstructKindQuery, "catalogFixtureWidgets")
	if !ok {
		t.Fatal("fixture query missing from the catalog")
	}

	var want []sense.RunnableArg
	for _, rc := range sense.AnalyzeRunnable(catalogFixtureQueries) {
		if rc.Name == "catalogFixtureWidgets" {
			want = rc.Args
		}
	}
	if len(want) == 0 {
		t.Fatal("the language server reported no args for the fixture query; the fixture is wrong")
	}
	if len(entry.Args) != len(want) {
		t.Fatalf("arg count: catalog %d, language server %d", len(entry.Args), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(entry.Args[i], want[i]) {
			t.Errorf("arg %d: catalog %+v, language server %+v", i, entry.Args[i], want[i])
		}
	}

	// And the argument metadata actually survives, rather than the two agreeing
	// on an empty answer.
	byName := map[string]sense.RunnableArg{}
	for _, a := range entry.Args {
		byName[a.Name] = a
	}
	if got := byName["state"].Enum; len(got) != 2 || got[0] != "live" || got[1] != "retired" {
		t.Errorf("@enum lost on the way to the catalog: %v", got)
	}
	if !strings.Contains(byName["widgetId"].Description, "Restrict to one widget") {
		t.Errorf("/// doc comment lost on the way to the catalog: %q", byName["widgetId"].Description)
	}
}

// TestConstructCatalogViewOnlyKindsCarryNoArgs: an argument form implies a run,
// and the runnable set is deliberately five. A view-only kind that reported an
// argument form would be offering an execution semantic the design defers.
func TestConstructCatalogViewOnlyKindsCarryNoArgs(t *testing.T) {
	for _, e := range mountCatalogFixture(t).ConstructCatalog() {
		if e.Runnable {
			continue
		}
		if len(e.Args) != 0 {
			t.Errorf("view-only %s %q reported %d args", e.Kind, e.Name, len(e.Args))
		}
		if e.Args == nil {
			t.Errorf("%s %q: Args is nil; it must marshal as an empty list", e.Kind, e.Name)
		}
	}
}

// TestConstructCatalogPromotedAppearsAndDisappears is the case a file walk
// cannot do: a construct that lives in the database and in no file.
func TestConstructCatalogPromotedAppearsAndDisappears(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	reg := NewAuthoredRuntimeRegistry()
	c := authorOneSpec(t, reg, "owner-1")

	if _, ok := findEntry(e.ConstructCatalog(), ConstructKindSpec, "mcpSessSpec"); ok {
		t.Fatal("the spec is in the catalog before it was promoted")
	}

	if err := e.PromoteAuthoredConstruct(context.Background(), c); err != nil {
		t.Fatalf("promote: %v", err)
	}

	entry, ok := findEntry(e.ConstructCatalog(), ConstructKindSpec, "mcpSessSpec")
	if !ok {
		t.Fatal("promoted spec absent from the catalog immediately after promote")
	}
	if entry.Origin != ConstructOriginPromoted {
		t.Errorf("promoted spec origin: got %q, want %q", entry.Origin, ConstructOriginPromoted)
	}
	if entry.OriginPath != "" {
		t.Errorf("a promoted construct has no file, but reported path %q", entry.OriginPath)
	}
	if want := sense.ConstructSourceHash(sessionSpecSrc); entry.SourceHash != want {
		t.Errorf("promoted spec hash: got %q, want the hash of the promoted source %q", entry.SourceHash, want)
	}
	// With no file, the catalog is the only thing that can serve the source --
	// so it does, and only here.
	if entry.Source != sessionSpecSrc {
		t.Errorf("promoted spec source: got %q, want the promoted source", entry.Source)
	}

	if err := e.DemoteAuthoredConstruct(context.Background(), "spec", "mcpSessSpec"); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if _, ok := findEntry(e.ConstructCatalog(), ConstructKindSpec, "mcpSessSpec"); ok {
		t.Error("demoted spec still in the catalog")
	}
}

// TestConstructCatalogCarriesSourceOnlyWithoutAFile: `source` is populated
// exactly when there is no file to read it from. Populating it for a
// file-backed construct too would duplicate what the pack browser already
// serves -- at the cost of every construct body on every catalog read.
func TestConstructCatalogCarriesSourceOnlyWithoutAFile(t *testing.T) {
	for _, e := range mountCatalogFixture(t).ConstructCatalog() {
		if e.OriginPath != "" && e.Source != "" {
			t.Errorf("%s %q has a file (%s) AND carries its source; read the file instead",
				e.Kind, e.Name, e.OriginPath)
		}
	}
}

// TestConstructCatalogPromotedRunnableCarriesItsArgs: a promoted construct is
// still runnable, so it still needs an argument form -- and it has no file to
// read one out of. Without the promoted source on the marker the extension
// would offer Run on a promoted mutation with no arguments to fill in, which
// fails at dispatch instead of at the form.
func TestConstructCatalogPromotedRunnableCarriesItsArgs(t *testing.T) {
	e := &MemQLEngine{functions: newFunctionRegistry()}
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", sessionConceptSrc+"\n\n"+sessionMutationSrc, ""); err != nil {
		t.Fatalf("author session bundle: %v", err)
	}
	c, ok := reg.Lookup("owner-1", "mutation", "mutationCreateMcpWidget")
	if !ok {
		t.Fatal("session mutation not registered")
	}
	if err := e.PromoteAuthoredConstruct(context.Background(), c); err != nil {
		t.Fatalf("promote: %v", err)
	}

	entry, found := findEntry(e.ConstructCatalog(), ConstructKindMutation, "mutationCreateMcpWidget")
	if !found {
		t.Fatal("promoted mutation absent from the catalog")
	}
	if entry.Origin != ConstructOriginPromoted {
		t.Errorf("origin: got %q, want %q", entry.Origin, ConstructOriginPromoted)
	}
	if !entry.Runnable {
		t.Fatal("a mutation is runnable, promoted or not")
	}
	if len(entry.Args) != 1 || entry.Args[0].Name != "widgetId" || !entry.Args[0].Required {
		t.Fatalf("promoted mutation lost its argument form: %+v", entry.Args)
	}
	if entry.SourceHash != sense.ConstructSourceHash(c.Source) {
		t.Errorf("source hash: got %q, want the hash of the promoted source", entry.SourceHash)
	}
}

// TestDeriveConstructOrigin covers the derivation directly, including the two
// cases the fixtures above cannot stage: a construct registered from Go rather
// than from a file, and a promoted construct that also has a path (it must not,
// but the derivation must not depend on the caller having cleared it).
func TestDeriveConstructOrigin(t *testing.T) {
	domains := map[string]string{
		"identity":           "embedded",
		catalogFixtureDomain: "pack:" + catalogFixtureDomain,
	}
	cases := []struct {
		name     string
		path     string
		promoted bool
		want     string
	}{
		{"embedded tree", "identity/queries.memql", false, ConstructOriginCore},
		{"mounted product domain", catalogFixtureDomain + "/queries.memql", false, ConstructOriginBundle},
		{"registered from Go, no file", "", false, ConstructOriginCore},
		{"promoted wins over any path", "identity/queries.memql", true, ConstructOriginPromoted},
		{"unknown domain is not a bundle", "somewhereelse/queries.memql", false, ConstructOriginCore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveConstructOrigin(tc.path, tc.promoted, domains); got != tc.want {
				t.Errorf("deriveConstructOrigin(%q, %v) = %q, want %q", tc.path, tc.promoted, got, tc.want)
			}
		})
	}
}

// TestConstructCatalogIsDeterministic: two calls against an unchanged engine
// return the same list in the same order, so a client can diff two reads.
func TestConstructCatalogIsDeterministic(t *testing.T) {
	eng := mountCatalogFixture(t)
	first := eng.ConstructCatalog()
	second := eng.ConstructCatalog()
	if len(first) != len(second) {
		t.Fatalf("two reads of an unchanged engine: %d then %d entries", len(first), len(second))
	}
	for i := range first {
		if first[i].Kind != second[i].Kind || first[i].Name != second[i].Name {
			t.Fatalf("entry %d differs between reads: %s %s vs %s %s",
				i, first[i].Kind, first[i].Name, second[i].Kind, second[i].Name)
		}
	}
}

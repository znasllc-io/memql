package memql

// authoring_promote_concept_test.go -- coverage for teaching a running cluster a
// new CONCEPT (memql#3746).
//
// Internal (package memql) test so it can drive the real promote path with the
// real engine registries and the unexported store seams, no live DB.
//
// The load-bearing tests here are the last two groups. Merging a concept into
// the registry is easy and a test that only checked THAT would pass against an
// implementation that merges and does nothing else -- which is precisely the
// implementation that looks right and is not. MemQLEngine.Init DERIVES state
// from the concept registry, and two of this issue's invariants live in that
// derivation: relationship normalization + validation (producing e.relationships,
// which drives id canonicalization and traversal) and the node-type invariants on
// the `type` column. So the relationship + node-type tests below assert those
// hold for a PROMOTED concept, and each of them fails against a merge-only
// implementation.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// --- fixtures -------------------------------------------------------------

const trainedWidgetSrc = `@version("1.0.0")
@namespace("trainingns")
@description("A concept taught to a running cluster")
concept trainedWidget {
  ownerUserId  string  @required
  label        string
}`

const trainedWidgetId = "v1:trainingns:trainedWidget"

// trainedWidgetMutation binds the promoted concept by its signature, which is
// the binding a row write goes through.
const trainedWidgetMutationSrc = `use trainingns.concepts.{ trainedWidget }

@description("Create a trained widget")
mutate trainedWidget mutationCreateTrainedWidget {
  args {
    widgetId  string  @required
  }
  insert {
    id:    canonicalId(args.widgetId, trainedWidget)
    label: "x"
  }
}`

// --- harness --------------------------------------------------------------

// promoteConceptEngine builds an engine whose concept registry is an ISOLATED
// clone of the loaded core tree, carrying exactly the concept-derived state Init
// leaves behind: e.relationships and e.schemaIdx, both produced by the same
// deriveConceptRegistryState the boot path uses. Isolated so a promote never
// leaks into the package-level default registry other tests read -- and so two
// engines in one test are genuinely two nodes.
func promoteConceptEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	return conceptEngineOver(t, loadedConceptTree(t).Clone())
}

// promoteConceptEngineOnTheDefaultRegistry builds an engine bound to the
// PACKAGE-LEVEL default registry, restoring its contents afterwards.
//
// It exists because the Gate-1 sandbox is engine-free: SandboxCompileBundle ->
// compileBundle takes no registry and clones the package default, so a construct
// binding a concept resolves it there and NOWHERE ELSE. That is fine in
// production -- one engine per process, bound to that same default registry
// (app/database.go), so a concept promoted into the engine IS in the registry
// Gate 1 clones -- but it means only an engine bound to the default can
// reproduce the production binding. A test using the isolated harness above
// would see the concept promoted and the mutation still failing to bind, which
// says something about the harness rather than about the system.
func promoteConceptEngineOnTheDefaultRegistry(t *testing.T) *MemQLEngine {
	t.Helper()
	registry := loadedConceptTree(t)
	before := memoryNodes.All()
	t.Cleanup(func() { memoryNodes.ReplaceAll(before) })
	return conceptEngineOver(t, registry)
}

// loadedConceptTree returns the package-level default registry with the core
// tree loaded into it.
func loadedConceptTree(t *testing.T) *memoryNodes.MemoryRegistry {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry, ok := memoryNodes.DefaultRegistry().(*memoryNodes.MemoryRegistry)
	if !ok {
		t.Fatalf("default concept registry is %T, not *memoryNodes.MemoryRegistry", memoryNodes.DefaultRegistry())
	}
	return registry
}

// conceptEngineOver wires an engine to a registry with the concept-derived state
// Init would have left on it.
func conceptEngineOver(t *testing.T, registry *memoryNodes.MemoryRegistry) *MemQLEngine {
	t.Helper()
	relationships, err := deriveConceptRegistryState(registry.List())
	if err != nil {
		t.Fatalf("derive core concept state: %v", err)
	}
	schemaIdx, err := buildSchemaIndex(registry)
	if err != nil {
		t.Fatalf("buildSchemaIndex: %v", err)
	}
	return &MemQLEngine{
		concepts:      registry,
		relationships: relationshipRegistry{ByConcept: relationships},
		schemaIdx:     schemaIdx,
		functions:     newFunctionRegistry(),
		specs:         newSpecRegistry(),
	}
}

// promoteConceptSource authors a concept bundle into a session registry and
// durably promotes it, returning the first error from either half so a test can
// assert on either outcome.
func promoteConceptSource(t *testing.T, e *MemQLEngine, source, name string) error {
	t.Helper()
	return promoteBundleAndLookup(t, e, source, "concept", name)
}

// promoteBundleAndLookup authors + durably promotes a single named construct,
// returning the first error from either half.
//
// A Gate-1 failure comes back from AuthorSessionBundle as a bare "N of M
// constructs did not compile" count, so the per-construct diagnostics are folded
// into the error here -- otherwise every validation test could only assert THAT
// a bundle was refused, never WHY, which is the half that catches a refusal
// firing for the wrong reason.
func promoteBundleAndLookup(t *testing.T, e *MemQLEngine, source, kind, name string) error {
	t.Helper()
	reg := NewAuthoredRuntimeRegistry()
	res, err := AuthorSessionBundle(reg, "owner-1", source)
	if err != nil {
		var detail []string
		for _, d := range res.Diagnostics {
			if !d.OK && strings.TrimSpace(d.Error) != "" {
				detail = append(detail, d.Error)
			}
		}
		if len(detail) > 0 {
			return fmt.Errorf("%w: %s", err, strings.Join(detail, "; "))
		}
		return err
	}
	c, ok := reg.Lookup("owner-1", kind, name)
	if !ok {
		t.Fatalf("session define did not register %s %q", kind, name)
	}
	return e.promoteConstructDurableWithStore(context.Background(), &fakePromoteStore{}, nil, "owner-1", c)
}

// --- the derivation itself ------------------------------------------------

// TestDeriveConceptRegistryState_IsIdempotent pins the defect that appeared the
// moment the derivation gained a second caller.
//
// It had only ever run once, at Init, so nothing had run it twice over the same
// concepts -- and it did not survive that. The first pass writes the DERIVED
// `fieldSource` back onto each Concept.Relationships element (which is what
// makes `concepts()` metadata report canonical values), and
// normalizeRelationshipDefinition refused any non-empty fieldSource as an
// authored one. So the second pass refused every core concept declaring a
// relationship -- the whole registry, on the first promote against a running
// engine.
//
// Idempotency is not a nicety here: the promote path derives over the CANDIDATE
// set before merging, which is what leaves the live registry untouched when a
// candidate is refused. Without it, the ordering is impossible and there is no
// remove on MemoryRegistry to roll back with instead.
//
// This asserts the value half. The write half -- that a second pass performs no
// redundant writes to live concepts a request goroutine may be reading -- is
// only observable under the race detector, and is why the assignments in
// deriveConceptRegistryState are guarded.
func TestDeriveConceptRegistryState_IsIdempotent(t *testing.T) {
	registry := loadedConceptTree(t).Clone()

	first, err := deriveConceptRegistryState(registry.List())
	if err != nil {
		t.Fatalf("first derivation over the core tree: %v", err)
	}
	second, err := deriveConceptRegistryState(registry.List())
	if err != nil {
		t.Fatalf("second derivation over the SAME concepts failed: %v -- the derivation must be re-runnable, "+
			"because a concept promote re-derives the whole registry (memql#3746)", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Error("the second derivation produced a different relationship index than the first")
	}
}

// --- session define -------------------------------------------------------

// TestAuthorSessionBundle_CompilesConceptOntoCompiled: a session-defined concept
// now carries its built *Concept on .Compiled.
//
// This is the gap the promote path fell through. sandboxCompileConcept has
// always built a real *Concept -- it merges one into the ISOLATED clone so later
// constructs in the same bundle bind against it -- but AuthorSessionBundle
// dropped that build on the floor and stored Compiled=nil, so promotion had
// nothing to register and refused the kind outright.
func TestAuthorSessionBundle_CompilesConceptOntoCompiled(t *testing.T) {
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", trainedWidgetSrc); err != nil {
		t.Fatalf("author concept: %v", err)
	}
	c, ok := reg.Lookup("owner-1", "concept", "trainedWidget")
	if !ok {
		t.Fatal("session concept not registered")
	}
	built, ok := c.Compiled.(*memoryNodes.Concept)
	if !ok || built == nil {
		t.Fatalf("session concept Compiled = %#v, want a *memoryNodes.Concept", c.Compiled)
	}
	if built.Name != trainedWidgetId {
		t.Errorf("compiled concept id = %q, want %q", built.Name, trainedWidgetId)
	}
}

// TestAuthorSessionBundle_ConceptDoesNotTouchAnyLiveRegistry: defining a concept
// on a stream validates it and registers NOTHING. The no-mutation guarantee the
// sandbox has always held has to survive .Compiled being populated -- otherwise
// "define" would silently become "promote".
func TestAuthorSessionBundle_ConceptDoesNotTouchAnyLiveRegistry(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	before := len(memoryNodes.List())

	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", trainedWidgetSrc); err != nil {
		t.Fatalf("author concept: %v", err)
	}

	if after := len(memoryNodes.List()); after != before {
		t.Fatalf("session define mutated the global concept registry: before=%d after=%d", before, after)
	}
	if _, err := memoryNodes.Get(trainedWidgetId); err == nil {
		t.Fatalf("session-defined concept %q leaked into the global default registry", trainedWidgetId)
	}
}

// --- promote --------------------------------------------------------------

// TestPromoteConcept_MergesIntoLiveRegistryAndPersists: the acceptance path. A
// concept authored in a bundle is validated, session-defined, and durably
// promoted -- landing in the engine's LIVE registry (no restart) with a
// reviewable persisted row pair behind it.
func TestPromoteConcept_MergesIntoLiveRegistryAndPersists(t *testing.T) {
	e := promoteConceptEngine(t)
	if _, err := e.concepts.Get(trainedWidgetId); err == nil {
		t.Fatalf("engine already knows %q before the promote", trainedWidgetId)
	}

	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", trainedWidgetSrc); err != nil {
		t.Fatalf("author concept: %v", err)
	}
	c, _ := reg.Lookup("owner-1", "concept", "trainedWidget")

	store := &fakePromoteStore{}
	if err := e.promoteConstructDurableWithStore(context.Background(), store, nil, "owner-1", c); err != nil {
		t.Fatalf("durable promote concept: %v", err)
	}

	// Live on this engine, immediately.
	got, err := e.concepts.Get(trainedWidgetId)
	if err != nil || got == nil {
		t.Fatalf("promoted concept not in the live registry: %v", err)
	}
	// The schema index is the other thing Init derives; a promoted concept
	// missing from it is invisible to every spec bound to it.
	idx := e.schemaIndex()
	if idx == nil {
		t.Fatal("schema index is nil after a concept promote")
	}
	if _, ok := idx.concepts[trainedWidgetId]; !ok {
		t.Errorf("promoted concept %q absent from the rebuilt schema index", trainedWidgetId)
	}

	// Reviewable + restart-durable: one bundle row, one construct row carrying
	// kind=concept and the source.
	if len(store.bundles) != 1 || len(store.constructs) != 1 {
		t.Fatalf("expected 1 bundle + 1 construct row, got %d + %d", len(store.bundles), len(store.constructs))
	}
	row := store.constructs[0]
	if row.Kind != "concept" || row.Name != "trainedWidget" || strings.TrimSpace(row.Source) == "" {
		t.Errorf("persisted construct row not captured correctly: %+v", row)
	}
}

// TestPromoteConcept_CatalogReportsItPromoted: the construct catalog keys a
// promotion marker by REGISTRY, and a concept's registry key is its canonical
// id, not its declaration name. Filed under the wrong key the catalog reports a
// promoted concept as `core` (memql#3749).
func TestPromoteConcept_CatalogReportsItPromoted(t *testing.T) {
	e := promoteConceptEngine(t)
	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("promote: %v", err)
	}

	promoted, marker := e.promotedConstruct(ConstructKindConcept, trainedWidgetId)
	if !promoted {
		t.Fatalf("catalog does not see %q as promoted; it would report it as core", trainedWidgetId)
	}
	if marker.Source == "" || marker.SourceHash == "" {
		t.Errorf("promotion marker carries no source/hash: %+v -- a promoted concept lives in no file, so this is the only copy", marker)
	}

	// And the declaration name is NOT the key: nothing should answer under it.
	if promoted, _ := e.promotedConstruct(ConstructKindConcept, "trainedWidget"); promoted {
		t.Error("promotion marker also answers under the declaration name; the registry key is the canonical id")
	}

	// The catalog entry itself: origin `promoted`, source carried (there is no
	// file to read it from), and the namespace KEPT. A concept's namespace comes
	// from its canonical id, not from a directory, so the promoted branch has no
	// business clearing it.
	var entry ConstructCatalogEntry
	found := false
	for _, got := range e.ConstructCatalog() {
		if got.Kind == ConstructKindConcept && got.Name == trainedWidgetId {
			entry, found = got, true
			break
		}
	}
	if !found {
		t.Fatalf("promoted concept %q is not in the construct catalog at all", trainedWidgetId)
	}
	if entry.Origin != ConstructOriginPromoted {
		t.Errorf("catalog origin = %q, want %q", entry.Origin, ConstructOriginPromoted)
	}
	if entry.Namespace != "trainingns" {
		t.Errorf("catalog namespace = %q, want %q -- a concept's namespace is derived from its id, not from a file path", entry.Namespace, "trainingns")
	}
	if entry.Source == "" || entry.OriginPath != "" {
		t.Errorf("catalog entry source/path = %q / %q; a promoted concept carries its source and no path", entry.Source, entry.OriginPath)
	}
}

// TestPromoteConcept_BindsAFunctionPromotedAfterIt: the reason to teach a
// cluster a noun is to bind constructs to it. A mutation promoted AFTER the
// concept compiles + binds against it, which is the binding a row write resolves
// through -- so this is how far "rows can be written under it immediately" can
// be carried without a live database.
//
// On the default-registry harness deliberately: see its doc comment. Gate 1
// resolves concept references against the package-level registry, which in
// production is the same one the engine holds.
func TestPromoteConcept_BindsAFunctionPromotedAfterIt(t *testing.T) {
	e := promoteConceptEngineOnTheDefaultRegistry(t)

	// The mutation alone cannot bind -- proving the concept promote, and not
	// some ambient registration, is what makes the positive case work.
	if err := promoteBundleAndLookup(t, e, trainedWidgetMutationSrc, "mutation", "mutationCreateTrainedWidget"); err == nil {
		t.Fatal("a mutation binding an unknown concept must not compile")
	}

	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("promote concept: %v", err)
	}
	if err := promoteBundleAndLookup(t, e, trainedWidgetMutationSrc, "mutation", "mutationCreateTrainedWidget"); err != nil {
		t.Fatalf("promote mutation bound to the promoted concept: %v", err)
	}
	if got, _ := e.functions.Get("mutationCreateTrainedWidget"); got == nil {
		t.Error("mutation bound to the promoted concept is not callable")
	}
}

// TestPromoteBundleDurable_ConceptAndItsMutationInOneBundle is the headline
// acceptance, at the entry point the owner-gated gRPC handler actually calls: a
// `.memql` bundle declaring a NEW concept AND a mutation bound to it is
// validated, session-defined and durably promoted in one go, against a running
// engine, with no restart.
//
// The ordering inside the bundle is not incidental. SplitBundleSource emits
// concepts first, and PromoteBundleDurable promotes in that order, so the
// concept is in the registry before the mutation's promote runs -- which is also
// the order the boot re-hydration replays them in.
func TestPromoteBundleDurable_ConceptAndItsMutationInOneBundle(t *testing.T) {
	e := promoteConceptEngineOnTheDefaultRegistry(t)

	bundle := trainedWidgetSrc + "\n\n" + trainedWidgetMutationSrc
	res, err := e.promoteBundleDurableWithStore(context.Background(), &fakePromoteStore{}, "owner-1", bundle, false)
	if err != nil {
		t.Fatalf("promote bundle: %v (diagnostics %+v)", err, res.Diagnostics)
	}
	if !res.OK {
		t.Fatalf("bundle promote reported not-OK: %+v", res)
	}

	promoted := map[string]string{}
	for _, d := range res.Promoted {
		promoted[d.Name] = d.Kind
	}
	if promoted["trainedWidget"] != "concept" {
		t.Errorf("bundle did not report the concept as promoted: %+v", res.Promoted)
	}
	if promoted["mutationCreateTrainedWidget"] != "mutation" {
		t.Errorf("bundle did not report the mutation as promoted: %+v", res.Promoted)
	}
	if _, err := e.concepts.Get(trainedWidgetId); err != nil {
		t.Errorf("concept not live after the bundle promote: %v", err)
	}
	if got, _ := e.functions.Get("mutationCreateTrainedWidget"); got == nil {
		t.Error("mutation not callable after the bundle promote")
	}
}

// --- core-first, never-shadow ---------------------------------------------

// TestPromoteConcept_CannotShadowACoreConcept: the same one-way guarantee
// PromoteAuthoredConstruct enforces for functions and specs, on the concept
// registry. A promote that would redefine a core concept is refused and the core
// definition is left exactly as it was.
func TestPromoteConcept_CannotShadowACoreConcept(t *testing.T) {
	e := promoteConceptEngine(t)

	coreId := "v1:identity:user"
	before, err := e.concepts.Get(coreId)
	if err != nil || before == nil {
		t.Fatalf("fixture: core concept %q is not loaded: %v", coreId, err)
	}

	shadow := `@version("1.0.0")
@namespace("identity")
@description("An impostor")
concept user {
  label  string
}`
	err = promoteConceptSource(t, e, shadow, "user")
	if err == nil {
		t.Fatal("promoting a concept whose canonical id a core concept owns must be refused")
	}
	if !strings.Contains(err.Error(), "core concept already owns") {
		t.Errorf("refusal message = %q, want the core-shadow refusal", err)
	}

	after, err := e.concepts.Get(coreId)
	if err != nil || after != before {
		t.Errorf("core concept %q was replaced by the refused promote", coreId)
	}
}

// TestPromoteConcept_RepromoteReplacesItsOwnPromotion: idempotent-friendly, the
// same as the function + spec branches. The marker under the canonical id is
// what distinguishes "mine to replace" from "core, hands off".
func TestPromoteConcept_RepromoteReplacesItsOwnPromotion(t *testing.T) {
	e := promoteConceptEngine(t)
	if err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget"); err != nil {
		t.Fatalf("first promote: %v", err)
	}

	revised := `@version("1.0.0")
@namespace("trainingns")
@description("A concept taught to a running cluster, revised")
concept trainedWidget {
  ownerUserId  string  @required
  label        string
  note         string
}`
	if err := promoteConceptSource(t, e, revised, "trainedWidget"); err != nil {
		t.Fatalf("re-promote of an already-promoted concept must be allowed: %v", err)
	}
	got, err := e.concepts.Get(trainedWidgetId)
	if err != nil {
		t.Fatalf("re-promoted concept missing: %v", err)
	}
	if !strings.Contains(got.Description, "revised") {
		t.Errorf("re-promote did not replace the prior definition: description = %q", got.Description)
	}
}

// --- reserved namespaces / reserved payload fields ------------------------

// TestPromoteConcept_RefusesReservedPayloadField pins the invariant the issue
// lists twice -- "reserved namespaces" and "reserved payload fields" -- which is
// ONE guard, already in place, and reached by the promote path for free.
//
// The two names describe one list. There is no separate registry of reserved
// concept NAMESPACES anywhere in the engine; what is reserved is the set of
// ENGINE NAMESPACE heads a filter resolves (`provenance` / `row` / `actor` /
// `args` / `now` / `config` / `trace` / `meta`, memql#3613) plus the row
// intrinsics, all of which a concept is refused for declaring as a top-level
// payload property. `reservedPayloadFields` in memory-nodes is that list.
//
// The guard lives at BuildConceptFromDecl (ensureReservedFieldsNotDeclared),
// which both the Gate-1 sandbox and the promote path reach through the single
// buildCandidateConcept. That sharing is the whole reason no promote-side check
// had to be written: such a concept never compiles, so it never reaches a
// registry to be merged into. The test exists so that stays true -- a promote
// path that built its concept some other way would fail here.
func TestPromoteConcept_RefusesReservedPayloadField(t *testing.T) {
	// One per class: an engine namespace (memql#3613) and a row intrinsic.
	for _, field := range []string{"provenance", "actor", "args", "row", "config", "id", "createdAt"} {
		t.Run(field, func(t *testing.T) {
			e := promoteConceptEngine(t)
			src := "@version(\"1.0.0\")\n@namespace(\"trainingns\")\nconcept trainedReserved {\n  " +
				field + "  object\n  label  string\n}"

			err := promoteConceptSource(t, e, src, "trainedReserved")
			if err == nil {
				t.Fatalf("promoting a concept declaring %q as a payload property must be refused", field)
			}
			if _, gerr := e.concepts.Get("v1:trainingns:trainedReserved"); gerr == nil {
				t.Fatalf("refused concept still landed in the live registry")
			}
		})
	}
}

// TestPromoteConcept_RefusesConceptWithoutANamespace: without @namespace there
// is no canonical id to register under, so there is nothing to promote. Refused
// at compile, before any registry is touched.
func TestPromoteConcept_RefusesConceptWithoutANamespace(t *testing.T) {
	e := promoteConceptEngine(t)
	src := `@description("no namespace")
concept trainedNoNamespace {
  label  string
}`
	err := promoteConceptSource(t, e, src, "trainedNoNamespace")
	if err == nil {
		t.Fatal("a concept with no @namespace must not promote")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("refusal message = %q, want a missing-@namespace refusal", err)
	}
}

// --- the derived state: relationships -------------------------------------

// TestPromoteConcept_RelationshipToACoreConceptIsDerived is the test the
// derivation refactor exists for.
//
// A concept merged into the registry and nothing else would write rows perfectly
// well while its @relationship contributed NOTHING: e.relationships is built
// once, at Init, from the registry as it stood then, and it is what drives id
// canonicalization on the write path and (concept, id) resolution on the filter
// path. The symptom would be no error anywhere -- ids persisting non-canonical
// and lookups quietly returning empty, which is exactly the failure memql#3653
// recorded.
//
// So: promote a concept carrying an @relationship to a CORE concept, and assert
// the edge is in e.relationships, normalized. This FAILS against a promote that
// only merges.
func TestPromoteConcept_RelationshipToACoreConceptIsDerived(t *testing.T) {
	e := promoteConceptEngine(t)

	src := `@version("1.0.0")
@namespace("trainingns")
@description("A trained concept pointing at a core one")
concept trainedNote {
  ownerUserId  string  @required
  body         string

  @relationship(type="parent", field="ownerUserId", target="v1:identity:user", direction="outgoing")
}`
	if err := promoteConceptSource(t, e, src, "trainedNote"); err != nil {
		t.Fatalf("promote concept with a core relationship: %v", err)
	}

	defs := e.relationshipDefinitionsForConcept("v1:trainingns:trainedNote")
	if len(defs) != 1 {
		t.Fatalf("promoted concept contributed %d relationship(s) to e.relationships, want 1 -- "+
			"a merge-only promote leaves the edge inert: no id canonicalization, no traversal", len(defs))
	}
	if defs[0].TargetConcept != "v1:identity:user" || defs[0].Field != "ownerUserId" {
		t.Errorf("derived relationship = %+v, want the declared parent edge on ownerUserId", defs[0])
	}
	// Normalized, not merely copied: the derivation fills FieldSource.
	if defs[0].FieldSource == "" {
		t.Error("derived relationship carries no FieldSource -- it was copied, not normalized")
	}
}

// TestPromoteConcept_RefusesUnresolvableRelationshipTarget: the other half of
// the same invariant. A relationship naming a target that is not a registered
// concept is REFUSED (memql#3653 made this fatal at boot rather than a skip),
// and a promote must be held to it too -- otherwise the one path that adds
// concepts at runtime is the one path that can install a broken edge.
//
// Nothing is merged: the derivation runs over the CANDIDATE set before the
// registry is touched, so a refusal leaves the live registry as it was.
func TestPromoteConcept_RefusesUnresolvableRelationshipTarget(t *testing.T) {
	e := promoteConceptEngine(t)

	src := `@version("1.0.0")
@namespace("trainingns")
concept trainedDangling {
  ownerUserId  string  @required

  @relationship(type="parent", field="ownerUserId", target="v1:trainingns:nobody", direction="outgoing")
}`
	err := promoteConceptSource(t, e, src, "trainedDangling")
	if err == nil {
		t.Fatal("a promoted concept whose relationship target is not registered must be refused")
	}
	if !strings.Contains(err.Error(), "not a registered concept") {
		t.Errorf("refusal message = %q, want the unresolvable-target refusal", err)
	}
	if _, gerr := e.concepts.Get("v1:trainingns:trainedDangling"); gerr == nil {
		t.Error("a refused promote left the concept in the live registry")
	}
	if defs := e.relationshipDefinitionsForConcept("v1:trainingns:trainedDangling"); len(defs) != 0 {
		t.Error("a refused promote left relationships behind")
	}
}

// TestPromoteConcept_RefusesRelationshipFieldNotDeclared: memql#3654's check,
// applied to a promoted concept. `field` must name a field the owning side
// declares -- an edge whose field does not exist half-works, which is worse than
// one that plainly does not.
//
// The fixture's `ownerUserld` is a deliberate lowercase-L-for-I homoglyph, not a
// slip: it is the shape of typo this check exists to catch, and the one a reader
// diffing two spellings by eye would miss.
func TestPromoteConcept_RefusesRelationshipFieldNotDeclared(t *testing.T) {
	e := promoteConceptEngine(t)

	src := `@version("1.0.0")
@namespace("trainingns")
concept trainedTypo {
  ownerUserId  string  @required

  @relationship(type="parent", field="ownerUserld", target="v1:identity:user", direction="outgoing")
}`
	err := promoteConceptSource(t, e, src, "trainedTypo")
	if err == nil {
		t.Fatal("a promoted concept whose relationship field is undeclared must be refused")
	}
	if !strings.Contains(err.Error(), "is not declared on") {
		t.Errorf("refusal message = %q, want the undeclared-field refusal", err)
	}
}

// --- the derived state: node-type invariants ------------------------------

// TestPromoteConcept_NodeTypeInvariantsApply: the collection / reference
// invariants on the `type` column are derived at Init too, so a merge-only
// promote would let a promoted `collection` with no `contains` edge -- or a
// `reference` that aliases nothing -- into the registry unchecked.
func TestPromoteConcept_NodeTypeInvariantsApply(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   string
	}{
		{
			name: "collection without contains",
			source: `@version("1.0.0")
@namespace("trainingns")
@type("collection")
concept trainedBadBasket {
  ownerUserId  string  @required
}`,
			want: "must declare contains relationship",
		},
		{
			name: "reference without alias or equals",
			source: `@version("1.0.0")
@namespace("trainingns")
@type("reference")
concept trainedBadPointer {
  ownerUserId  string  @required
}`,
			want: "must declare alias or equals relationship",
		},
		{
			name: "contains on a non-collection",
			source: `@version("1.0.0")
@namespace("trainingns")
concept trainedBadContainer {
  ownerUserId  string  @required
  itemId       string

  @relationship(type="contains", field="itemId", target="v1:identity:user", direction="outgoing")
}`,
			want: "must be type collection to define contains relationships",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := promoteConceptEngine(t)
			decl := declaredConceptName(t, tc.source)
			err := promoteConceptSource(t, e, tc.source, decl)
			if err == nil {
				t.Fatalf("promote must be refused: %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal message = %q, want it to contain %q", err, tc.want)
			}
			if _, gerr := e.concepts.Get("v1:trainingns:" + decl); gerr == nil {
				t.Error("a refused promote left the concept in the live registry")
			}
		})
	}
}

// TestPromoteConcept_CollectionWithContainsLands: the positive node-type case,
// and the two-step ordering that makes it possible. The contained concept is
// promoted first; the collection's `contains` edge then resolves against it,
// because the derivation runs over the registry as it stands at THAT promote.
func TestPromoteConcept_CollectionWithContainsLands(t *testing.T) {
	e := promoteConceptEngine(t)

	item := `@version("1.0.0")
@namespace("trainingns")
concept trainedItem {
  ownerUserId  string  @required
  label        string
}`
	basket := `@version("1.0.0")
@namespace("trainingns")
@type("collection")
concept trainedBasket {
  ownerUserId  string  @required
  itemId       string

  @relationship(type="contains", field="itemId", target="v1:trainingns:trainedItem", direction="outgoing")
}`

	// Order matters, and the failure is the proof: the collection cannot land
	// before the concept it contains exists.
	if err := promoteConceptSource(t, e, basket, "trainedBasket"); err == nil {
		t.Fatal("a collection whose contained concept is not registered must be refused")
	}

	if err := promoteConceptSource(t, e, item, "trainedItem"); err != nil {
		t.Fatalf("promote contained concept: %v", err)
	}
	if err := promoteConceptSource(t, e, basket, "trainedBasket"); err != nil {
		t.Fatalf("promote collection concept: %v", err)
	}

	got, err := e.concepts.Get("v1:trainingns:trainedBasket")
	if err != nil {
		t.Fatalf("collection concept missing after promote: %v", err)
	}
	if got.NodeType != memoryNodes.NodeTypeCollection {
		t.Errorf("promoted concept NodeType = %q, want %q", got.NodeType, memoryNodes.NodeTypeCollection)
	}
	if defs := e.relationshipDefinitionsForConcept("v1:trainingns:trainedBasket"); len(defs) != 1 {
		t.Errorf("collection contributed %d relationship(s), want 1", len(defs))
	}
}

// declaredConceptName pulls the concept name out of a fixture source so the
// table cases above do not have to repeat it.
func declaredConceptName(t *testing.T, source string) string {
	t.Helper()
	decls := ExtractConceptDecls(source)
	if len(decls) != 1 {
		t.Fatalf("fixture declares %d concepts, want exactly 1", len(decls))
	}
	return decls[0].Name
}

// --- registry binding -----------------------------------------------------

// stubConceptRegistry is a concept.Registry that is NOT a *MemoryRegistry, the
// shape several tests in this package already use.
type stubConceptRegistry struct{}

func (stubConceptRegistry) Get(string) (*memoryNodes.Concept, error) { return nil, nil }
func (stubConceptRegistry) List() []*memoryNodes.Concept             { return nil }

// TestPromoteConcept_RefusesAnImmutableRegistry: e.concepts is an INTERFACE.
// Production binds it to memoryNodes.DefaultRegistry() and BuildOfflineSense to
// a clone, but a caller can bind anything. A promote must mutate the registry
// THIS engine is bound to -- never the package-level default, which would reach
// past the engine and hit the global -- so a registry it cannot mutate is a
// refusal naming the type, not a silent no-op and not a write somewhere else.
func TestPromoteConcept_RefusesAnImmutableRegistry(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	globalBefore := len(memoryNodes.List())

	e := &MemQLEngine{concepts: stubConceptRegistry{}, functions: newFunctionRegistry(), specs: newSpecRegistry()}
	err := promoteConceptSource(t, e, trainedWidgetSrc, "trainedWidget")
	if err == nil {
		t.Fatal("promoting into a non-mutable concept registry must be refused")
	}
	if !strings.Contains(err.Error(), "MemoryRegistry") {
		t.Errorf("refusal message = %q, want it to name the registry type", err)
	}
	if after := len(memoryNodes.List()); after != globalBefore {
		t.Fatalf("the refused promote wrote to the package-level default registry: before=%d after=%d", globalBefore, after)
	}
}

// --- boot re-hydration ----------------------------------------------------

// TestRehydratePromotedConcept_SurvivesARestart: the persisted row pair is
// replayed on a FRESH engine and the concept is live again -- the "survives a
// restart" acceptance. The canonical id is re-derived from the stored source's
// @namespace, so the row keeps carrying the DECLARATION name like every other
// kind and there is no second place for the id to be stored wrong.
func TestRehydratePromotedConcept_SurvivesARestart(t *testing.T) {
	old := promoteConceptEngine(t)
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", trainedWidgetSrc); err != nil {
		t.Fatalf("author concept: %v", err)
	}
	c, _ := reg.Lookup("owner-1", "concept", "trainedWidget")

	persist := &fakePromoteStore{}
	if err := old.promoteConstructDurableWithStore(context.Background(), persist, nil, "owner-1", c); err != nil {
		t.Fatalf("durable promote: %v", err)
	}

	fresh := promoteConceptEngine(t)
	if _, err := fresh.concepts.Get(trainedWidgetId); err == nil {
		t.Fatal("fresh engine should not know the promoted concept before re-hydration")
	}

	row := persist.constructs[0]
	row.OwnerUserId = "owner-1"
	store := &fakeRehydrateStore{
		bundles:    []AuthoringBundleRow{{Id: persist.bundles[0].Id, OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{persist.bundles[0].Id: {row}},
	}

	res, err := fresh.rehydratePromotedNow(context.Background(), store)
	if err != nil {
		t.Fatalf("re-hydrate: %v", err)
	}
	if res.Seen != 1 || res.Rehydrated != 1 || len(res.Failed) != 0 {
		t.Fatalf("re-hydrate result = %+v, want seen=1 rehydrated=1 failed=0", res)
	}
	if _, err := fresh.concepts.Get(trainedWidgetId); err != nil {
		t.Fatalf("promoted concept not restored after restart re-hydration: %v", err)
	}

	// And idempotent: a second walk (or the live cross-node propagation
	// re-applying the same bundle) is a no-op-equivalent, not a shadow refusal.
	if _, err := fresh.rehydratePromotedNow(context.Background(), store); err != nil {
		t.Fatalf("second re-hydrate (should be idempotent): %v", err)
	}
	if _, err := fresh.concepts.Get(trainedWidgetId); err != nil {
		t.Fatalf("idempotent re-hydrate lost the concept: %v", err)
	}
}

// TestRehydratePromotedConcept_PropagatesAcrossNodes: the single-bundle
// re-hydration the authoring-promote BROADCAST drives, which is how a promote on
// node A becomes live on node B within seconds rather than at B's next restart.
// Multi-node is the default here; a concept that only exists on the node that
// promoted it is not a promoted concept.
func TestRehydratePromotedConcept_PropagatesAcrossNodes(t *testing.T) {
	nodeA := promoteConceptEngine(t)
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", trainedWidgetSrc); err != nil {
		t.Fatalf("author concept: %v", err)
	}
	c, _ := reg.Lookup("owner-1", "concept", "trainedWidget")

	persist := &fakePromoteStore{}
	if err := nodeA.promoteConstructDurableWithStore(context.Background(), persist, nil, "owner-1", c); err != nil {
		t.Fatalf("durable promote on node A: %v", err)
	}

	bundleId := persist.bundles[0].Id
	row := persist.constructs[0]
	row.OwnerUserId = "owner-1"
	store := &fakeRehydrateStore{
		bundles:    []AuthoringBundleRow{{Id: bundleId, OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{bundleId: {row}},
	}

	nodeB := promoteConceptEngine(t)
	res, err := nodeB.rehydratePromotedBundleNow(context.Background(), store, "owner-1", bundleId)
	if err != nil {
		t.Fatalf("bundle re-hydration on node B: %v", err)
	}
	if res.Rehydrated != 1 {
		t.Fatalf("node B re-hydrate result = %+v, want rehydrated=1", res)
	}
	if _, err := nodeB.concepts.Get(trainedWidgetId); err != nil {
		t.Fatalf("promoted concept did not become live on the peer node: %v", err)
	}
}

// --- demote ---------------------------------------------------------------
//
// Concept demote semantics -- RETIRE when rows exist, REMOVE only when there are
// none -- live in authoring_concept_retire_test.go alongside the code that owns
// them (memql#3756). Two tests here pinned the interim refusal ("a concept can be
// taught and cannot yet be un-taught") and were deleted with it, rather than
// weakened into asserting the new behaviour from the promote side.

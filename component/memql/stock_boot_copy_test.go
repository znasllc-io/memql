package memql

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/core/baseregistry"
	"github.com/znasllc-io/memql/core/component"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// A PRIVATE ENGINE FROM ONE LOAD OF THE TREE.
//
// readMergeTestEngine exists because a test may change its engine: stub a
// builtin, script a model, add a probe construct, take the response cache away.
// Those engines have to stay private. What does not have to be repeated is the
// load behind them: a boot over the stock tree derives the same constructs every
// time, and repeating it was the largest single cost in this package. On
// 2026-11-02, under CI's settings, Init was 158 s of the suite's 385 s of CPU,
// 112 s of it under readMergeTestEngine, whose two-replica fixtures boot two
// engines per test.
//
// Borrowing sharedReadMergeEngine is not a substitute. The replica tests need
// two engines precisely so that nothing crosses between them except the
// database, and a stubbed handler on a shared engine reaches every later test.
//
// So one load of the tree happens per process (stockLoad), and a private engine
// receives ITS OWN COPY of what that load produced (bootFromStockLoad). Each
// test still has its own engine, registries, handlers, runtime, pool and cache.
// A construct registry copied here hands out clones exactly as a booted one
// does, so an engine's Upsert, Remove or stubbed handler stays on that engine.
//
// WHAT IS NOT COPIED, AND WHY. Everything derived from something other than the
// tree is derived again, per engine, by the code a real boot runs:
//
//   - The concept registry's derived state (relationships, schema index), from
//     the registry the caller hands over. A concept a test merged into the
//     global registry counts, as it does on a boot.
//   - The builtin handlers, closures over this engine.
//   - The providers: their auth resolves against this engine's database and the
//     process environment, and t.Setenv before a boot must keep working
//     (loadStockProviders).
//   - The policies and rules, whose store is this engine's database.
//   - The AI runtime, the seed materializer and the process-wide publishes
//     (secret and variable resolvers, shopper extensions, access gates).
//
// WHAT IT DOES NOT COVER. Only the embedded tree, loaded by nobody's fixture.
// With an overlay mounted (memqldsl.RegisterTree, a test's fixture domain) the
// constructs differ, so canCopyStockLoad answers false and the caller boots for
// real. A test ABOUT booting calls Init itself: strict boot, the contract gates,
// the language lines, the catalog and the rest still run the real path, as does
// the shared engine's own boot.
//
// IF YOU ADD STATE TO initFromTree, add it to bootFromStockLoad too.
// TestStockBootCopyMatchesARealBoot compares every field of the engine and the
// contents of every registry against a real boot, and fails naming the field.
//
// The load report's counts and skips are copied as well, so a borrower reading
// it sees what the load found. Nothing here validates a tree: a problem in the
// embedded tree fails stockLoad, and every caller reports that failure.

// stockLoadState backs stockLoad. The Once body touches no *testing.T, for the
// reason sharedReadMergeEngine gives: the verdict is reported per caller.
var stockLoadState struct {
	once sync.Once
	// loads counts Once-body executions, and copies the engines given a copy,
	// so the saving is asserted as a number (TestStockBootCopySharesOneLoad).
	loads   int
	copies  atomic.Int64
	image   *MemQLEngine
	loadErr error
}

// canCopyStockLoad reports whether a boot right now would read the embedded tree
// and nothing else. memqldsl.Tree answers with the embed.FS itself when no
// overlay is registered, and with a layered view otherwise.
func canCopyStockLoad() bool {
	_, stock := memqldsl.Tree().(embed.FS)
	return stock
}

// stockLoad returns the engine holding the one load of the embedded tree, loading
// it on first use. It serves nothing and is never handed to a test.
//
// It loads with publish=false over a registry of its own, the isolated boot
// package analysis already uses, because this may run in the middle of any test:
// it must neither read global state an earlier test left behind (a concept
// retired through the global registry would fail or change the load for the rest
// of the process) nor write any, which would re-point the process-wide resolvers
// and gates at an engine with no database.
func stockLoad() (*MemQLEngine, error) {
	s := &stockLoadState
	s.once.Do(func() {
		s.loads++
		tree := memqldsl.Tree()
		concepts, _, err := BuildUnifiedConcepts(nil, tree)
		if err != nil {
			s.loadErr = fmt.Errorf("BuildUnifiedConcepts: %w", err)
			return
		}
		image, err := newEngine(nil, false, (&component.Component{}).WithLoggerWriter(io.Discard))
		if err != nil {
			s.loadErr = fmt.Errorf("newEngine: %w", err)
			return
		}
		image.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := image.initFromTree(concept.NewRegistry(concepts), tree, false); err != nil {
			s.loadErr = fmt.Errorf("load the embedded tree: %w", err)
			return
		}
		s.image = image
	})
	return s.image, s.loadErr
}

// bootPrivateEngine boots an engine for a test that is not about booting: from
// the stock load when it can, and for real when a mounted overlay means the
// constructs differ.
func bootPrivateEngine(e *MemQLEngine, concepts concept.Registry) error {
	if canCopyStockLoad() {
		return bootFromStockLoad(e, concepts)
	}
	return e.Init(concepts)
}

// bootFromStockLoad leaves e as e.Init(concepts) would over the stock tree,
// without loading the tree again. bootPrivateEngine checks that it applies.
//
// It runs in initFromTree's order, because the order is meaningful: the
// resolvers are installed before the providers load, and the runtime is built
// from the registries it reads.
func bootFromStockLoad(e *MemQLEngine, concepts concept.Registry) error {
	image, err := stockLoad()
	if err != nil {
		return err
	}
	stockLoadState.copies.Add(1)

	report := copyLoadReport(image.loadReport)
	e.loadReport = report
	e.languageLines = image.languageLines

	relationships, err := deriveConceptRegistryState(concepts.List())
	if err != nil {
		return err
	}
	e.setConceptRelationships(relationships)
	e.concepts = concepts
	schemaIdx, err := buildSchemaIndex(concepts)
	if err != nil {
		return err
	}
	e.setSchemaIndex(schemaIdx)

	if e.specs, err = copySpecRegistry(image.specs); err != nil {
		return err
	}
	if e.functions, err = copyFunctionRegistry(image.functions); err != nil {
		return err
	}
	if err := e.initBuiltinExecutorHandlers(); err != nil {
		return err
	}
	if e.shapes, err = copyShapeRegistry(image.shapes); err != nil {
		return err
	}
	if e.tools, err = copyToolRegistry(image.tools); err != nil {
		return err
	}

	// A failure below refuses the boot rather than recording a skip. The load
	// already ran these over the same constructs, so one failing here means a
	// test's global state did it, and a real boot would have refused as well.
	if errs := loadShopperExtensions(e.functions, true); len(errs) > 0 {
		return fmt.Errorf("shopper extensions: %v", errs)
	}
	gates, gateErrs := loadAccessGates(e.functions, true)
	if len(gateErrs) > 0 {
		return fmt.Errorf("access gates: %v", gateErrs)
	}
	report.SetAccessGates(gates)

	e.prompts = copyPromptRegistry(image.prompts)
	e.agents = NewAgentRegistry()
	e.seeds = copySeedRegistry(image.seeds)
	e.seedMaterializer = NewSeedMaterializer(e, e.seeds)

	SetSystemSecretResolver(e.ResolveSystemSecret)
	SetSystemVariableResolver(e.ResolveSystemVariable)

	tree := memqldsl.Tree()
	if e.providers, err = loadStockProviders(e, tree); err != nil {
		return err
	}
	if e.policies, err = loadAIPolicies(e.Logger); err != nil {
		return err
	}
	if _, err := loadUnifiedPoliciesFromTree(tree, e.Logger, e.policies); err != nil {
		return err
	}
	if err := ExpandPolicyChains(e.policies); err != nil {
		return err
	}
	e.policies.AttachStore(databasePolicyStore{database: e.database})
	e.rules = NewRuleRegistry()
	if _, err := loadUnifiedRulesFromTree(tree, e.Logger, e.rules); err != nil {
		return err
	}

	e.aiRuntime = newAIRuntime(e.Logger, e.prompts, e.providers, e.aiCacheConfig)
	e.aiRuntime.resolve = e.resolveAI
	e.aiRuntime.recordCacheServed = e.recordCacheServed
	e.aiRuntime.seam = e.modelSeam

	e.initialized = true
	return nil
}

// loadStockProviders loads the providers as a boot does, but does not ask the
// database for credentials it does not hold.
//
// One load looks up each placeholder under two names in two concepts, which is 32
// queries that find nothing unless a credential is stored, and that was 24 s of
// this suite's 297 s across its engines. A row is what the lookups exist to find,
// so with one present, or with any doubt, they run. The environment is the third
// tier and is read either way, so t.Setenv before a boot keeps working.
func loadStockProviders(e *MemQLEngine, tree fs.FS) (*ProviderRegistry, error) {
	providers, err := loadAIProviders(e.Logger)
	if err != nil {
		return nil, err
	}
	if !holdsProviderCredentials(e) {
		secrets, variables := systemSecretResolver, systemVariableResolver
		systemSecretResolver, systemVariableResolver = nil, nil
		defer func() { systemSecretResolver, systemVariableResolver = secrets, variables }()
	}
	_, err = loadUnifiedProvidersFromTree(tree, true, e.Logger, providers)
	return providers, err
}

// holdsProviderCredentials reports whether a stored row could answer a provider
// placeholder. Every placeholder in the tree is MEMQL_-prefixed, and so is each
// name authConceptLookupNames looks it up under. The prefix rather than the
// concepts alone, because a test of variable names leaves rows of its own behind.
func holdsProviderCredentials(e *MemQLEngine) bool {
	db := e.database()
	if db == nil {
		return false
	}
	var holds bool
	err := db.NewRaw(`SELECT EXISTS (SELECT 1 FROM "MemoryNodes"
		WHERE concept IN ('v1:platform:globalSecret', 'v1:platform:globalVariable')
		AND payload->>'name' LIKE 'MEMQL%')`).Scan(context.Background(), &holds)
	return err != nil || holds
}

// copyLoadReport is what the load found, on a report of the engine's own: a
// quarantine recorded later lands on that engine alone.
func copyLoadReport(from *LoadReport) *LoadReport {
	return &LoadReport{
		Registered:     maps.Clone(from.Registered),
		Skipped:        slices.Clone(from.Skipped),
		Duplicates:     slices.Clone(from.Duplicates),
		Quarantined:    slices.Clone(from.Quarantined),
		Warnings:       slices.Clone(from.Warnings),
		AccessGates:    slices.Clone(from.AccessGates),
		deprecatedUses: slices.Clone(from.deprecatedUses),
	}
}

// copyEntries fills to from from, in from's own keys. Range walks the stored
// entries without cloning, and Upsert clones each on the way in, so a registry
// that clones its entries ends up sharing none with the load.
func copyEntries[T any](to, from *baseregistry.Registry[T]) error {
	var err error
	from.Range(func(name string, item *T) bool {
		err = to.Upsert(name, item)
		return err == nil
	})
	return err
}

func copyFunctionRegistry(from *FunctionRegistry) (*FunctionRegistry, error) {
	to := newFunctionRegistry()
	return to, copyEntries(to.Registry, from.Registry)
}

func copySpecRegistry(from *SpecRegistry) (*SpecRegistry, error) {
	to := newSpecRegistry()
	for name := range from.disabled {
		to.disabled[name] = true
	}
	to.disabledBodies = append(to.disabledBodies, from.disabledBodies...)
	return to, copyEntries(to.Registry, from.Registry)
}

func copyToolRegistry(from *ToolRegistry) (*ToolRegistry, error) {
	to := newToolRegistry()
	for name := range from.disabled {
		to.disabled[name] = true
	}
	return to, copyEntries(to.Registry, from.Registry)
}

// copyShapeRegistry shares the definitions: a shape registry does not clone, and
// its definitions are read-only after load (ShapeRegistry).
func copyShapeRegistry(from *ShapeRegistry) (*ShapeRegistry, error) {
	to := newShapeRegistry()
	return to, copyEntries(to.Registry, from.Registry)
}

// copyPromptRegistry and copySeedRegistry share the definitions too, which both
// registries already hand out uncloned. Replacing one, as a test scripting a
// prompt does, replaces it in this engine's map alone.
func copyPromptRegistry(from *PromptRegistry) *PromptRegistry {
	to := newPromptRegistry()
	for name, prompt := range from.byName {
		to.byName[name] = prompt
	}
	return to
}

func copySeedRegistry(from *SeedRegistry) *SeedRegistry {
	to := NewSeedRegistry()
	for name, def := range from.byName {
		to.byName[name] = def
	}
	return to
}

// TestStockBootCopySharesOneLoad keeps the saving, as a number. A regression
// that loaded the tree per engine would still hand every test a working engine,
// and the only symptom would be the db-tests budget filling again (memql#3257,
// three times). It also fails if this test binary ever stops answering with the
// plain embedded tree, which would send every private engine back to a real boot.
func TestStockBootCopySharesOneLoad(t *testing.T) {
	require.True(t, canCopyStockLoad(), "this test binary mounts an overlay at init, so no private engine can copy the load")
	registry := loadedConceptRegistry(t)
	before := stockLoadState.copies.Load()
	for i := 0; i < 2; i++ {
		require.NoError(t, bootFromStockLoad(newQuietEngine(t), registry))
	}
	require.Equal(t, before+2, stockLoadState.copies.Load())
	require.Equal(t, 1, stockLoadState.loads, "the embedded tree was loaded %d times; the point of the copy is one load", stockLoadState.loads)
}

// TestCanCopyStockLoadRefusesAnOverlay is the fallback's half of the contract: a
// fixture domain is only visible to an engine that boots after it is mounted, so
// with one mounted the fixture must boot for real.
func TestCanCopyStockLoadRefusesAnOverlay(t *testing.T) {
	const domain = "stockbootcopyoverlay"
	require.True(t, canCopyStockLoad())
	memqldsl.RegisterTree(domain, fstest.MapFS{"concepts.memql": {Data: []byte("")}})
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })
	require.False(t, canCopyStockLoad(), "an engine copied from the stock load would not see the mounted domain")
}

// TestStockBootCopyMatchesARealBoot is what makes the copy safe to keep: a real
// boot and a copy, over one registry, are compared field by field and construct
// by construct. A field initFromTree starts setting fails the first loop, naming
// it; a loader that starts producing something else fails the rest.
func TestStockBootCopyMatchesARealBoot(t *testing.T) {
	registry := loadedConceptRegistry(t)
	real := newQuietEngine(t)
	require.NoError(t, real.Init(registry))
	publishedByReal := publishedBootState()

	// Cleared, so the copy has to publish rather than inherit from the boot above.
	clearPublishedBootState(t)
	copied := newQuietEngine(t)
	require.NoError(t, bootFromStockLoad(copied, registry))
	require.NotNil(t, systemSecretResolver, "the copy did not install the secret resolver")
	require.NotNil(t, systemVariableResolver, "the copy did not install the variable resolver")
	require.Equal(t, publishedByReal, publishedBootState(), "the copy published something other than a boot publishes")

	realFields, copiedFields := reflect.ValueOf(real).Elem(), reflect.ValueOf(copied).Elem()
	for i := 0; i < realFields.NumField(); i++ {
		if realFields.Field(i).IsZero() != copiedFields.Field(i).IsZero() {
			t.Errorf("MemQLEngine.%s: real boot empty=%v, copy empty=%v. initFromTree sets it, so bootFromStockLoad must too",
				realFields.Type().Field(i).Name, realFields.Field(i).IsZero(), copiedFields.Field(i).IsZero())
		}
	}

	requireSameConstructs(t, "function", real.functions.Snapshot(), copied.functions.Snapshot())
	requireSameConstructs(t, "spec", real.specs.Snapshot(), copied.specs.Snapshot())
	requireSameConstructs(t, "tool", real.tools.Snapshot(), copied.tools.Snapshot())
	requireSameConstructs(t, "shape", real.shapes.Snapshot(), copied.shapes.Snapshot())
	requireSameConstructs(t, "seed", real.seeds.byName, copied.seeds.byName)
	requireSameConstructs(t, "rule", real.rules.byName, copied.rules.byName)
	requireSameConstructs(t, "policy", real.policies.byName, copied.policies.byName)
	requireSameConstructs(t, "default policy", real.policies.defaults, copied.policies.defaults)
	require.Equal(t, real.specs.disabled, copied.specs.disabled)
	require.Equal(t, real.specs.disabledBodies, copied.specs.disabledBodies)
	require.Equal(t, real.tools.disabled, copied.tools.disabled)
	require.Equal(t, ruleNames(real.rules.ordered), ruleNames(copied.rules.ordered))
	require.NotNil(t, copied.policies.store)

	require.Equal(t, promptFacts(real.prompts), promptFacts(copied.prompts))
	require.Equal(t, providerFacts(real.providers), providerFacts(copied.providers))
	require.Equal(t, real.providers.declared, copied.providers.declared)

	require.Equal(t, real.languageLines, copied.languageLines)
	require.Equal(t, real.relationships, copied.relationships)
	require.Equal(t, real.schemaIdx, copied.schemaIdx)
	require.Equal(t, slices.Sorted(maps.Keys(real.builtinExecutorHandlers)), slices.Sorted(maps.Keys(copied.builtinExecutorHandlers)))

	require.Equal(t, real.loadReport, copied.loadReport)

	// What the copy wires to itself, rather than to the load or to another engine.
	require.Same(t, copied.prompts, copied.aiRuntime.prompts)
	require.Same(t, copied.providers, copied.aiRuntime.providers)
	require.Same(t, copied.modelSeam, copied.aiRuntime.seam)
	require.NotNil(t, copied.aiRuntime.resolve)
	require.NotNil(t, copied.aiRuntime.recordCacheServed)
	require.NotNil(t, copied.aiRuntime.cache)
	require.Equal(t, real.aiRuntime.cacheCfg, copied.aiRuntime.cacheCfg)
	require.Same(t, copied, copied.seedMaterializer.engine)
	require.Same(t, copied.seeds, copied.seedMaterializer.registry)
}

// TestStockBootCopiesKeepATestsChangesToThemselves is the reason a test may still
// change its private engine: what one engine has done reaches neither another
// engine nor the load the next engine copies. Every change here is one a fixture
// in this package makes.
func TestStockBootCopiesKeepATestsChangesToThemselves(t *testing.T) {
	registry := loadedConceptRegistry(t)
	boot := func() *MemQLEngine {
		e := newQuietEngine(t)
		require.NoError(t, bootFromStockLoad(e, registry))
		return e
	}
	changed, other := boot(), boot()

	// Each registry and everything wired from it is the engine's own.
	require.NotSame(t, changed.functions, other.functions)
	require.NotSame(t, changed.specs, other.specs)
	require.NotSame(t, changed.tools, other.tools)
	require.NotSame(t, changed.shapes, other.shapes)
	require.NotSame(t, changed.prompts, other.prompts)
	require.NotSame(t, changed.seeds, other.seeds)
	require.NotSame(t, changed.rules, other.rules)
	require.NotSame(t, changed.policies, other.policies)
	require.NotSame(t, changed.providers, other.providers)
	require.NotSame(t, changed.agents, other.agents)
	require.NotSame(t, changed.aiRuntime, other.aiRuntime)
	require.NotSame(t, changed.loadReport, other.loadReport)
	require.NotSame(t, changed.seedMaterializer, other.seedMaterializer)

	// No cloning registry shares a stored entry with another engine.
	requireNoSharedEntries(t, "function", changed.functions.Registry, other.functions.Registry)
	requireNoSharedEntries(t, "spec", changed.specs.Registry, other.specs.Registry)
	requireNoSharedEntries(t, "tool", changed.tools.Registry, other.tools.Registry)

	// A probe construct, a removal, a stubbed builtin, a scripted prompt and a
	// disabled response cache.
	probe, err := other.functions.Get("work.workCapabilities")
	require.NoError(t, err)
	probe.Name = "stockBootCopyProbe"
	require.NoError(t, changed.functions.Upsert(probe))
	changed.functions.Remove("work.workCapabilities")
	changed.tools.Remove("work.discoverCapabilities")
	changed.builtinExecutorHandlers["integration.work.createGoal"] = nil
	changed.prompts.byName["stockBootCopyProbe"] = &PromptTemplate{Name: "stockBootCopyProbe"}
	delete(changed.seeds.byName, changed.seeds.Names()[0])
	changed.aiRuntime.cache = nil

	for name, e := range map[string]*MemQLEngine{"another engine": other, "the next engine": boot()} {
		require.False(t, e.functions.Has("stockBootCopyProbe"), "%s saw a probe construct", name)
		require.True(t, e.functions.Has("work.workCapabilities"), "%s lost a removed construct", name)
		require.True(t, e.tools.Has("work.discoverCapabilities"), "%s lost a removed tool", name)
		_, stubbed := e.builtinExecutorHandlers["integration.work.createGoal"]
		require.False(t, stubbed, "%s saw a stubbed builtin", name)
		_, scripted := e.prompts.Get("stockBootCopyProbe")
		require.False(t, scripted, "%s saw a scripted prompt", name)
		require.Len(t, e.seeds.Names(), len(stockLoadState.image.seeds.Names()), "%s lost a seed", name)
		require.NotNil(t, e.aiRuntime.cache, "%s lost its response cache", name)
	}
}

// TestCopyLoadReportCopiesEverything covers what the embedded tree cannot: on a
// clean tree most of a report is empty, so a field the copy lost would still
// compare equal against a real boot.
func TestCopyLoadReportCopiesEverything(t *testing.T) {
	from := &LoadReport{
		Registered:     map[string]int{"functions": 1},
		Skipped:        []baseloader.Skip{{Name: "skipped"}},
		Duplicates:     []DuplicateConstruct{{Name: "duplicate"}},
		Quarantined:    []QuarantinedConstruct{{Name: "quarantined"}},
		Warnings:       []baseloader.Warning{{Code: "warning"}},
		AccessGates:    []AccessGateEntry{{Construct: "gate"}},
		deprecatedUses: []DeprecatedUseRecord{{Rule: "rule"}},
	}
	fields := reflect.ValueOf(from).Elem()
	for i := 0; i < fields.NumField(); i++ {
		if name := fields.Type().Field(i).Name; name != "mu" && fields.Field(i).IsZero() {
			t.Fatalf("LoadReport.%s is new: fill it here and copy it in copyLoadReport", name)
		}
	}
	require.Equal(t, from, copyLoadReport(from))
}

// TestStockBootCopySeesAStoredProviderCredential is loadStockProviders' half of
// the contract: skipping the lookups must never hide a credential a boot would
// have found, stored under either the placeholder or the name it elides to.
func TestStockBootCopySeesAStoredProviderCredential(t *testing.T) {
	_, db, ctx := sharedReadMergeEngine(t)
	const placeholder = "MEMQL_AI_OPENAI_SERVICE_ACCOUNT_ID"
	const stored = "stored-service-account"

	before := newQuietEngineOn(t, db)
	require.NoError(t, bootPrivateEngine(before, concept.DefaultRegistry()))
	require.NotContains(t, providerFacts(before.providers)["chat54"], stored)

	for _, name := range authConceptLookupNames(placeholder) {
		t.Run(name, func(t *testing.T) {
			id := "stock-boot-credential-" + uniqueSuffix(name)
			runMutation(t, clusterOwnerCtx(id), before, "setGlobalVariable", map[string]any{"id": id, "name": name, "value": stored})
			t.Cleanup(func() {
				_, err := db.NewDelete().Model((*concept.MemoryNode)(nil)).Where("concept = ?", "v1:platform:globalVariable").Where("payload->>'name' = ?", name).Exec(ctx)
				require.NoError(t, err)
			})
			copied := newQuietEngineOn(t, db)
			require.NoError(t, bootPrivateEngine(copied, concept.DefaultRegistry()))
			require.Contains(t, providerFacts(copied.providers)["chat54"], stored)
			real := newQuietEngineOn(t, db)
			require.NoError(t, real.Init(concept.DefaultRegistry()))
			require.Equal(t, providerFacts(real.providers), providerFacts(copied.providers))
		})
	}
}

// newQuietEngineOn is newQuietEngine over a database.
func newQuietEngineOn(t *testing.T, db *bun.DB) *MemQLEngine {
	t.Helper()
	e, err := New(db)
	require.NoError(t, err)
	t.Cleanup(e.cache.close)
	e.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return e
}

// requireSameConstructs compares two views of a registry construct by construct,
// so a difference names the construct rather than printing every definition.
func requireSameConstructs[T any](t *testing.T, kind string, real, copied map[string]*T) {
	t.Helper()
	require.Equal(t, slices.Sorted(maps.Keys(real)), slices.Sorted(maps.Keys(copied)), "the %s names differ", kind)
	require.NotEmpty(t, real, "a boot produced no %s at all", kind)
	for name := range real {
		require.Equal(t, real[name], copied[name], "%s %q differs from a real boot", kind, name)
	}
}

func requireNoSharedEntries[T any](t *testing.T, kind string, a, b *baseregistry.Registry[T]) {
	t.Helper()
	stored := map[*T]string{}
	a.Range(func(name string, item *T) bool {
		stored[item] = name
		return true
	})
	b.Range(func(name string, item *T) bool {
		if owner, shared := stored[item]; shared {
			t.Errorf("%s %q is one object on two engines (the other holds it as %q)", kind, name, owner)
		}
		return true
	})
}

func ruleNames(rules []*RuleConfig) []string {
	names := make([]string, 0, len(rules))
	for _, rule := range rules {
		names = append(names, rule.Name)
	}
	return names
}

// promptFacts is a prompt registry with each compiled template and schema reduced
// to something comparable: compiled, each holds functions, which never compare
// equal, and both are compiled from sources compared here.
func promptFacts(r *PromptRegistry) map[string]PromptTemplate {
	facts := make(map[string]PromptTemplate, len(r.byName))
	for name, prompt := range r.byName {
		fact := *prompt
		fact.tmpl, fact.inputSchema = nil, nil
		facts[name] = fact
	}
	return facts
}

// providerFacts is a provider registry without the clients, which are per engine
// by construction: whether one exists is Available, and why not is the reason.
func providerFacts(r *ProviderRegistry) map[string]string {
	facts := make(map[string]string, len(r.byName))
	for name, entry := range r.byName {
		facts[name] = fmt.Sprintf("available=%v err=%v config=%+v", entry.Available, entry.err, entry.Config)
	}
	return facts
}

// publishedBootState is what a boot publishes for the whole process, other than
// the two resolvers, which are functions and can only be seen to be installed.
func publishedBootState() map[string]any {
	shopperMu.RLock()
	defer shopperMu.RUnlock()
	accessGateMu.RLock()
	defer accessGateMu.RUnlock()
	return map[string]any{
		"shopperExtensions": maps.Clone(shopperExtensions),
		"shopperInert":      slices.Clone(shopperExtensionsInert),
		"accessGates":       maps.Clone(accessGates),
	}
}

// clearPublishedBootState empties it for the rest of a test. Each piece goes back
// to what the last boot left, so a later test sees a booted process.
func clearPublishedBootState(t *testing.T) {
	t.Helper()
	secretResolver, variableResolver := systemSecretResolver, systemVariableResolver
	shopperMu.Lock()
	extensions, inert := shopperExtensions, shopperExtensionsInert
	shopperExtensions, shopperExtensionsInert = nil, nil
	shopperMu.Unlock()
	accessGateMu.Lock()
	gates := accessGates
	accessGates = nil
	accessGateMu.Unlock()
	systemSecretResolver, systemVariableResolver = nil, nil
	t.Cleanup(func() {
		systemSecretResolver, systemVariableResolver = secretResolver, variableResolver
		shopperMu.Lock()
		shopperExtensions, shopperExtensionsInert = extensions, inert
		shopperMu.Unlock()
		accessGateMu.Lock()
		accessGates = gates
		accessGateMu.Unlock()
	})
}

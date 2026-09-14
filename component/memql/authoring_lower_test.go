package memql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/component"
)

// authoring_lower_test.go -- a session-authored query, spec or trait is lowered
// exactly as a tree construct is, on every path that makes it callable
// (memql#5366): define and stage refuse what does not lower with the
// three-part message on the author's line and column, promote lowers against
// the shared registries, and the boot walk lowers a stored row and checks it
// once every row is back.
//
// Each test boots the real engine over the embedded tree plus the lowerinit
// fixture domain (expr_lower_init_test.go).

// bootAuthoringEngine is the lowerinit engine: core ticket concept plus the
// specs isOpenTicket, owesReport, isUrgentTicket and the context spec
// callerIsOwner.
func bootAuthoringEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	eng, err := bootLowerTree(t, map[string]string{
		"concepts.memql": lowerInitConcepts,
		"specs.memql":    lowerInitSpecs,
	})
	require.NoError(t, err)
	return eng
}

// sessionGoodBundle defines a spec and a query applying it and a core spec.
const sessionGoodBundle = `use lowerinit.concepts.{ ticket }
use lowerinit.specs.{ isOpenTicket }

/// Tickets above priority three.
spec ticket isHighTicket = row => row.priority > 3

/// Open, high tickets at or above a floor.
query ticket hotTickets {
  args {
    floor  int!
  }
  filter   row => isOpenTicket(row) && isHighTicket(row) &&
             row.priority >= args.floor
  paginate 20
}
`

// sessionRefusedBundle holds one refusal only the engine's registries can make
// (a context spec applied to the row) and one the bundle alone can (arithmetic
// over the row in a spec body).
const sessionRefusedBundle = `use lowerinit.concepts.{ ticket }
use lowerinit.specs.{ callerIsOwner }

/// Refused: a context spec applied to the row.
query ticket misKindedSession {
  filter   row => row.status == "open" &&
             callerIsOwner(row)
  paginate 20
}

/// Refused: arithmetic over the row.
spec ticket arithmeticSpec = row =>
  row.priority + 1 > 3
`

// diagnosticFor finds the Gate-1 diagnostic for one construct.
func diagnosticFor(t *testing.T, diags []SandboxDiagnostic, kind, name string) SandboxDiagnostic {
	t.Helper()
	for _, d := range diags {
		if d.Kind == kind && d.Name == name {
			return d
		}
	}
	t.Fatalf("no diagnostic for %s %q in %+v", kind, name, diags)
	return SandboxDiagnostic{}
}

// positionOf is the 1-based line and column of the first occurrence of needle
// in src -- what an author's editor shows for it.
func positionOf(t *testing.T, src, needle string) (int, int) {
	t.Helper()
	idx := strings.Index(src, needle)
	require.GreaterOrEqual(t, idx, 0, "fixture lacks %q", needle)
	line := 1 + strings.Count(src[:idx], "\n")
	col := idx - strings.LastIndex(src[:idx], "\n")
	return line, col
}

// TestAuthoringLower runs the cases that only READ the lowerinit engine --
// each defines into a fresh session registry, validates, or compiles a
// bundle against it, and none writes the engine's own registries -- over ONE
// boot of it, as subtests keeping their names. The fixture cannot be booted
// once for the package: bootLowerDomains mounts it over the process-wide
// concept registry and restores that through t.Cleanup, and a parent test's
// cleanup runs when the parent finishes, so this is exactly as isolated as a
// boot per case was, at one boot. The cases that promote, stage or
// re-hydrate into an engine keep booting their own (below): they write it.
//
// ADDING A CASE: write `func authoringLower<Name>(t *testing.T, eng
// *MemQLEngine)` and list it here -- if it only reads the engine.
func TestAuthoringLower(t *testing.T) {
	eng := bootAuthoringEngine(t)
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *MemQLEngine)
	}{
		{"DefineRegistersLoweredForms", authoringLowerDefineRegistersLoweredForms},
		{"DefineRefusesWithTheThreePartMessageOnTheAuthorsLine", authoringLowerDefineRefusesWithTheThreePartMessageOnTheAuthorsLine},
		{"TheEngineRefusesWhatTheBundleAloneCannot", authoringLowerTheEngineRefusesWhatTheBundleAloneCannot},
		{"ADanglingImportKeepsTheReferenceDiagnostic", authoringLowerADanglingImportKeepsTheReferenceDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, eng) })
	}
}

func authoringLowerDefineRegistersLoweredForms(t *testing.T, eng *MemQLEngine) {
	reg := NewAuthoredRuntimeRegistry()
	{
		res, err := eng.DefineSessionBundle(reg, "owner-1", sessionGoodBundle, "")
		require.NoError(t, err, "%+v", res.Diagnostics)
		require.True(t, res.OK)
	}

	c, ok := reg.Lookup("owner-1", "spec", "isHighTicket")
	require.True(t, ok)
	spec, ok := c.Compiled.(*Spec)
	require.True(t, ok)
	require.Equal(t, SpecKindRow, spec.Kind, "the binding is resolved at define")
	require.Equal(t, `payload.priority>3`, canonicalExpression(spec.Expr), "the registered spec carries its lowered body")

	q, ok := reg.Lookup("owner-1", "query", "hotTickets")
	require.True(t, ok)
	fn, ok := q.Compiled.(*Function)
	require.True(t, ok)
	require.NotNil(t, fn.V1Filter)
	require.Contains(t, canonicalExpression(unwrapToFilter(fn.Expr)), "spec(ishighticket)")
}

func authoringLowerDefineRefusesWithTheThreePartMessageOnTheAuthorsLine(t *testing.T, eng *MemQLEngine) {
	reg := NewAuthoredRuntimeRegistry()
	var res SessionDefineResult
	var err error
	res, err = eng.DefineSessionBundle(reg, "owner-1", sessionRefusedBundle, "")
	require.Error(t, err, "a bundle that does not lower is refused at define, not at its first call")
	require.False(t, res.OK)
	require.Empty(t, reg.ListForOwner("owner-1"), "a refused define registers nothing")

	q := diagnosticFor(t, res.Diagnostics, "query", "misKindedSession")
	require.False(t, q.OK)
	require.Contains(t, q.Error, "`callerIsOwner(row)` does not lower in a query filter: `callerIsOwner` is a context spec over the actor")
	require.Contains(t, q.Error, "`callerIsOwner(actor)`", "the nearest spelling")
	line, col := positionOf(t, sessionRefusedBundle, "callerIsOwner(row)")
	require.Equal(t, line, q.Line, "the refused node's line, on the continuation line the author wrote it on")
	require.Equal(t, col, q.Column, "and its column")
	require.Equal(t, line, q.EndLine)
	require.Equal(t, col+len("callerIsOwner(row)"), q.EndColumn)

	s := diagnosticFor(t, res.Diagnostics, "spec", "arithmeticSpec")
	require.False(t, s.OK)
	require.Contains(t, s.Error, "`row.priority + 1` does not lower in a spec or trait body: arithmetic over the row runs in process")
	line, col = positionOf(t, sessionRefusedBundle, "row.priority + 1")
	require.Equal(t, line, s.Line)
	require.Equal(t, col, s.Column)
}

func authoringLowerTheEngineRefusesWhatTheBundleAloneCannot(t *testing.T, eng *MemQLEngine) {
	const misKinded = `use lowerinit.concepts.{ ticket }
use lowerinit.specs.{ callerIsOwner }

/// A context spec applied to the row: only the spec registry knows its kind.
query ticket misKindedOnly {
  filter   row => callerIsOwner(row)
  paginate 20
}
`
	{
		// With no engine the kind check is deferred -- the bundle alone cannot
		// see callerIsOwner -- so the engine-free define accepts it...
		free, err := AuthorSessionBundle(NewAuthoredRuntimeRegistry(), "owner-1", misKinded, "")
		require.NoError(t, err)
		require.True(t, free.OK)
		// ...and the engine's define, which every production caller uses,
		// refuses it at define time.
		res, err := eng.DefineSessionBundle(NewAuthoredRuntimeRegistry(), "owner-1", misKinded, "")
		require.Error(t, err)
		require.Contains(t, diagnosticFor(t, res.Diagnostics, "query", "misKindedOnly").Error, "is a context spec over the actor")
		// So does the engine's validate.
		rep := eng.ValidateAuthoredBundle(misKinded, "")
		require.False(t, rep.OK)
	}
}

func TestAuthoringLower_StageRefusesBeforeStagingAnything(t *testing.T) {
	eng := bootAuthoringEngine(t)
	store := &fakePromoteStore{}
	var err error
	_, err = eng.stageBundleDurableWithStore(context.Background(), store, "owner-1", sessionRefusedBundle, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "bundle failed validation")
	require.Empty(t, store.constructs, "a stage that does not lower persists nothing")

	{
		res, err := eng.stageBundleDurableWithStore(context.Background(), store, "owner-1", sessionGoodBundle, "")
		require.NoError(t, err, "%+v", res.Diagnostics)
	}
	staged, ok := eng.stagedAuthored.Lookup("owner-1", "spec", "isHighTicket")
	require.True(t, ok)
	require.Equal(t, `payload.priority>3`, canonicalExpression(staged.Compiled.(*Spec).Expr), "the staged spec is lowered")
	_, ok = eng.stagedAuthored.Lookup("owner-1", "query", "hotTickets")
	require.True(t, ok, "the query applying the staged spec stages after it")
}

func TestAuthoringLower_PromoteLowersAgainstTheSharedRegistries(t *testing.T) {
	eng := bootAuthoringEngine(t)
	reg := NewAuthoredRuntimeRegistry()
	const withShape = `use lowerinit.concepts.{ ticket }

/// A session shape.
@row
shape ticket ticketBrief {
  status
  priority
}

/// A spec over the session shape.
spec ticketBrief briefOpen = row => row.status == "open"
`
	{
		_, err := eng.DefineSessionBundle(reg, "owner-1", sessionGoodBundle, "")
		require.NoError(t, err)
		res, err := eng.DefineSessionBundle(reg, "owner-1", withShape, "")
		require.NoError(t, err, "%+v", res.Diagnostics)
	}

	// A spec over a concept promotes, lowered against the shared registries.
	c, ok := reg.Lookup("owner-1", "spec", "isHighTicket")
	require.True(t, ok)
	require.NoError(t, eng.PromoteAuthoredConstruct(context.Background(), c))
	promoted, err := eng.specs.Get("isHighTicket")
	require.NoError(t, err)
	require.Equal(t, SpecKindRow, promoted.Kind)
	require.Equal(t, `payload.priority>3`, canonicalExpression(promoted.Expr))

	// A spec over a SESSION shape does not: the shared registry holds no such
	// shape, and a promoted construct may depend only on what every session
	// can resolve.
	brief, ok := reg.Lookup("owner-1", "spec", "briefOpen")
	require.True(t, ok)
	require.Equal(t, `payload.status=="open"`, canonicalExpression(brief.Compiled.(*Spec).Expr), "the session lowered it over its own shape")
	err = eng.PromoteAuthoredConstruct(context.Background(), brief)
	require.Error(t, err)
	require.Contains(t, err.Error(), `binding "ticketBrief" resolves to neither`)
	_, promotedBrief := eng.specs.Lookup("briefOpen")
	require.False(t, promotedBrief, "a refused promote registers nothing")
	again, _ := reg.Lookup("owner-1", "spec", "briefOpen")
	require.NotNil(t, again.Compiled.(*Spec).Expr, "the promote lowered a clone; the session's own entry is untouched")
}

func TestAuthoringLower_RehydrationLowersStoredRowsAndChecksThemOnceAllAreBack(t *testing.T) {
	eng := bootAuthoringEngine(t)
	row := func(bundle, kind, name, source string) AuthoringConstructRow {
		return AuthoringConstructRow{Id: bundle + "-c", OwnerUserId: "owner-1", BundleId: bundle, Kind: kind, Name: name, Source: source, Status: "active"}
	}
	const querySource = `use lowerinit.concepts.{ ticket }
use lowerinit.specs.{ isOpenTicket }

/// Open, high tickets.
query ticket hotTickets {
  filter   row => isOpenTicket(row) && isHighTicket(row)
  paginate 20
}
`
	const specSource = `use lowerinit.concepts.{ ticket }

/// Tickets above priority three.
spec ticket isHighTicket = row => row.priority > 3
`
	const arithmeticSource = `use lowerinit.concepts.{ ticket }

/// Arithmetic over the row: it never lowered, and a stored copy does not either.
spec ticket arithmeticSpec = row => row.priority + 1 > 3
`
	const misKindedSource = `use lowerinit.concepts.{ ticket }
use lowerinit.specs.{ callerIsOwner }

/// A context spec applied to the row: refused once every row is back.
query ticket misKindedStored {
  filter   row => callerIsOwner(row)
  paginate 20
}
`
	// The query applying isHighTicket is stored FIRST: the walk replays one
	// row at a time, so its predicate check has to wait for the spec's row.
	store := &fakeRehydrateStore{
		bundles: []AuthoringBundleRow{
			{Id: "b1", OwnerUserId: "owner-1", Status: BundleActive},
			{Id: "b2", OwnerUserId: "owner-1", Status: BundleActive},
			{Id: "b3", OwnerUserId: "owner-1", Status: BundleActive},
			{Id: "b4", OwnerUserId: "owner-1", Status: BundleActive},
		},
		constructs: map[string][]AuthoringConstructRow{
			"b1": {row("b1", "query", "hotTickets", querySource)},
			"b2": {row("b2", "spec", "isHighTicket", specSource)},
			"b3": {row("b3", "spec", "arithmeticSpec", arithmeticSource)},
			"b4": {row("b4", "query", "misKindedStored", misKindedSource)},
		},
	}
	var res RehydrateResult
	{
		var err error
		res, err = eng.rehydratePromotedNow(context.Background(), store)
		require.NoError(t, err)
	}

	spec, err := eng.specs.Get("isHighTicket")
	require.NoError(t, err, "the stored v1 spec is back")
	require.Equal(t, `payload.priority>3`, canonicalExpression(spec.Expr), "and lowered at boot")
	_, err = eng.functions.Get("hotTickets")
	require.NoError(t, err, "the query stored before the spec it applies is back too")

	require.ElementsMatch(t, []string{"spec:arithmeticSpec", "query:misKindedStored"}, res.Failed)
	require.Equal(t, 2, res.Rehydrated)
	_, stillThere := eng.specs.Lookup("arithmeticSpec")
	require.False(t, stillThere, "a stored row that does not lower is quarantined, not registered")
	_, err = eng.functions.Get("misKindedStored")
	require.Error(t, err, "one the post-walk check refuses is taken back out")

	var quarantined []string
	for _, q := range eng.loadReport.Quarantined {
		quarantined = append(quarantined, q.Kind+":"+q.Name+" "+q.Err)
	}
	joined := strings.Join(quarantined, "\n")
	require.Contains(t, joined, "arithmeticSpec")
	require.Contains(t, joined, "arithmetic over the row runs in process")
	require.Contains(t, joined, "misKindedStored")
	require.Contains(t, joined, "is a context spec over the actor")
}

func TestAuthoringLower_AnUnloweredSessionSpecIsRefusedByNameNeverInlinedAsNothing(t *testing.T) {
	overlay := map[string]*Spec{"neverLowered": {Name: "neverLowered", Lambda: &languageParser.LambdaExpr{Params: []string{"row"}}}}
	_, err := expandSpecReferencesWithOverlay(&SpecReferenceExpression{Name: "neverLowered"}, overlay, map[string]struct{}{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "has no lowered body")
}

func authoringLowerADanglingImportKeepsTheReferenceDiagnostic(t *testing.T, eng *MemQLEngine) {
	var rep SandboxReport
	rep = SandboxCompileBundleWithEngine([]SandboxConstruct{{
		Kind: "spec",
		Name: "danglingSpec",
		Source: `use lowerinit.shapes.{ ghostShape }

/// A spec over a shape nothing declares.
spec ghostShape danglingSpec = row => row.status == "open"`,
	}}, eng)
	require.False(t, rep.OK)
	d := diagnosticFor(t, rep.Diagnostics, "spec", "danglingSpec")
	require.Contains(t, d.Error, "unresolved reference", "the reference check names the import to fix; the lowering runs after it")
	require.Contains(t, d.Error, "ghostShape")
}

// twinTicketConcepts declares a second `ticket`, in another domain, with
// `state` where lowerinit's ticket has `status` -- so which of the two a spec
// bound to `ticket` binds is observable from the fields it may read.
const twinTicketConcepts = `@version("1.0.0")
@namespace("lowertwin")
@description("A second ticket, in another domain.")
concept ticket {
  state  string!  @description("Workflow state.")
}
`

// bootTwinTicketDomains boots the engine over two domains that each declare a
// `ticket` concept -- lowerinit's with `status`, lowertwin's with `state`.
func bootTwinTicketDomains(t *testing.T) *MemQLEngine {
	t.Helper()
	eng, err := bootLowerDomains(t, map[string]map[string]string{
		"lowerinit": {"concepts.memql": lowerInitConcepts},
		"lowertwin": {"concepts.memql": twinTicketConcepts},
	})
	require.NoError(t, err, "two domains may each declare a ticket")
	return eng
}

// twinImportMessage is the refusal of a spec bound to `ticket` with no import
// to choose between the two domains that declare one: it names the imports.
const twinImportMessage = "binding \"ticket\" is declared by more than one domain, and no file-top `use` import says which -- " +
	"import the one you mean: `use lowerinit.concepts.{ ticket }` or `use lowertwin.concepts.{ ticket }`"

// TestAuthoringLowerTwoTickets runs the read-only cases over the two-domain
// engine (bootTwinTicketDomains) on one boot, as TestAuthoringLower does
// over lowerinit's: defines into fresh session registries, and a promote the
// engine refuses before it registers anything. The case that promotes and
// re-hydrates keeps its own engine.
func TestAuthoringLowerTwoTickets(t *testing.T) {
	eng := bootTwinTicketDomains(t)
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *MemQLEngine)
	}{
		{"ASessionSpecBindsThroughItsImports", authoringLowerASessionSpecBindsThroughItsImports},
		{"AnAmbiguousBindingWithNoImportIsRefusedAtPromote", authoringLowerAnAmbiguousBindingWithNoImportIsRefusedAtPromote},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, eng) })
	}
}

func authoringLowerASessionSpecBindsThroughItsImports(t *testing.T, eng *MemQLEngine) {
	define := func(origin, src string) (SessionDefineResult, *AuthoredRuntimeRegistry, error) {
		reg := NewAuthoredRuntimeRegistry()
		var res SessionDefineResult
		var err error
		res, err = eng.DefineSessionBundle(reg, "owner-1", src, origin)
		return res, reg, err
	}

	// The import decides which ticket -- also from an untitled buffer, which
	// has no domain of its own to fall back on.
	res, reg, err := define("", `use lowerinit.concepts.{ ticket }

/// Open, in lowerinit's sense.
spec ticket isOpenHere = row => row.status == "open"
`)
	require.NoError(t, err, "%+v", res.Diagnostics)
	c, ok := reg.Lookup("owner-1", "spec", "isOpenHere")
	require.True(t, ok)
	require.Equal(t, `payload.status=="open"`, canonicalExpression(c.Compiled.(*Spec).Expr))
	require.Contains(t, c.Source, "use lowerinit.concepts.{ ticket }", "the spec carries the bundle's import")

	res, _, err = define("", `use lowertwin.concepts.{ ticket }

/// Reads a field only lowerinit's ticket declares.
spec ticket readsTheOtherTicket = row => row.status == "open"
`)
	require.Error(t, err)
	require.Contains(t, diagnosticFor(t, res.Diagnostics, "spec", "readsTheOtherTicket").Error,
		"`status` is not a declared field of v1:lowertwin:ticket", "bound to the imported ticket, not the first one found")

	// With no import the name is ambiguous -- even from a file in one of the
	// two domains, since a stored row keeps no file: refused at define, naming
	// the imports that would choose.
	res, _, err = define("lowerinit/specs.memql", `/// Which ticket?
spec ticket whichTicket = row => row.status == "open"
`)
	require.Error(t, err)
	require.Contains(t, diagnosticFor(t, res.Diagnostics, "spec", "whichTicket").Error, twinImportMessage)
}

func TestAuthoringLower_AnImportedBindingSurvivesPromoteAndReHydration(t *testing.T) {
	eng := bootTwinTicketDomains(t)
	const bundle = `use lowertwin.concepts.{ ticket }

/// Open, in the twin's sense.
spec ticket isTwinOpen = row => row.state == "open"

/// Closed, in the twin's sense.
spec ticket isTwinClosed = row => row.state == "closed"
`
	persist := &fakePromoteStore{}
	{
		res, err := eng.promoteBundleDurableWithStore(context.Background(), persist, "owner-1", bundle, "", false)
		require.NoError(t, err, "%+v", res.Diagnostics)
	}
	require.Len(t, persist.constructs, 2)
	for _, row := range persist.constructs {
		require.Contains(t, row.Source, "use lowertwin.concepts.{ ticket }", "%s's stored row carries the import it binds through", row.Name)
	}

	// A single construct promoted from a session carries it too.
	reg := NewAuthoredRuntimeRegistry()
	single := &fakePromoteStore{}
	{
		res, err := eng.DefineSessionBundle(reg, "owner-1", `use lowertwin.concepts.{ ticket }

/// Held, in the twin's sense.
spec ticket isTwinHeld = row => row.state == "held"
`, "")
		require.NoError(t, err, "%+v", res.Diagnostics)
	}
	c, ok := reg.Lookup("owner-1", "spec", "isTwinHeld")
	require.True(t, ok)
	require.NoError(t, eng.promoteConstructDurableWithStore(context.Background(), single, nil, "owner-1", c))
	require.Len(t, single.constructs, 1)
	require.Contains(t, single.constructs[0].Source, "use lowertwin.concepts.{ ticket }")

	// The next boot: a fresh engine over the same tree re-hydrates the stored
	// rows, and they bind as the author's define did.
	fresh, err := New(nil, (&component.Component{}).WithLoggerWriter(io.Discard))
	require.NoError(t, err)
	fresh.Logger = eng.Logger
	require.NoError(t, fresh.Init(concept.DefaultRegistry()))
	rows := append(append([]AuthoringConstructRow{}, persist.constructs...), single.constructs...)
	store := &fakeRehydrateStore{constructs: map[string][]AuthoringConstructRow{}}
	for i, row := range rows {
		row.OwnerUserId, row.Status = "owner-1", "active"
		bundleID := row.BundleId
		if bundleID == "" {
			bundleID = fmt.Sprintf("b%d", i)
			row.BundleId = bundleID
		}
		store.bundles = append(store.bundles, AuthoringBundleRow{Id: bundleID, OwnerUserId: "owner-1", Status: BundleActive})
		store.constructs[bundleID] = append(store.constructs[bundleID], row)
	}
	var res RehydrateResult
	res, err = fresh.rehydratePromotedNow(context.Background(), store)
	require.NoError(t, err)
	require.Empty(t, res.Failed, "nothing is quarantined: the import binds at boot as it did at promote")
	require.Equal(t, 3, res.Rehydrated)
	for name, want := range map[string]string{
		"isTwinOpen":   `payload.state=="open"`,
		"isTwinClosed": `payload.state=="closed"`,
		"isTwinHeld":   `payload.state=="held"`,
	} {
		spec, err := fresh.specs.Get(name)
		require.NoError(t, err, name)
		require.Equal(t, want, canonicalExpression(spec.Expr), name)
	}
}

func authoringLowerAnAmbiguousBindingWithNoImportIsRefusedAtPromote(t *testing.T, eng *MemQLEngine) {
	persist := &fakePromoteStore{}
	var res PromoteBundleResult
	var err error
	res, err = eng.promoteBundleDurableWithStore(context.Background(), persist, "owner-1", `/// Open -- but whose ticket?
spec ticket isSomeoneOpen = row => row.state == "open"
`, "lowertwin/specs.memql", false)
	require.Error(t, err, "refused at promote, never passed to be quarantined at the next boot")
	require.Contains(t, diagnosticFor(t, res.Diagnostics, "spec", "isSomeoneOpen").Error, twinImportMessage)
	require.Empty(t, persist.constructs, "nothing is persisted")
}

// coreShapesForTest is the embedded tree's shape registry, loaded as Init
// loads it. A bare test engine needs it the moment a promote or stage lowers
// an edition-2026 spec: the spec's binding (actorEnvelope, most often) is
// resolved against the engine's shapes, and an engine that holds none can bind
// nothing. Loaded per call, not shared across the package: each engine gets its
// own registry, as each booted engine does.
func coreShapesForTest(t *testing.T) *ShapeRegistry {
	t.Helper()
	reg := newShapeRegistry()
	_, err := LoadUnifiedShapes(slog.New(slog.NewTextHandler(io.Discard, nil)), reg)
	require.NoError(t, err)
	_, ok := reg.Get("actorEnvelope")
	require.True(t, ok, "the core tree declares actorEnvelope")
	return reg
}

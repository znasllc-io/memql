package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// authoring_lower_test.go -- a session-authored query, spec or trait is lowered
// exactly as a tree construct is, on every path that makes it callable
// (memql#5366): define and stage refuse what does not lower with the
// three-part message on the author's line and column, promote lowers against
// the shared registries, and the boot walk lowers a stored row and checks it
// once every row is back.
//
// Each test boots the real engine over the embedded tree plus the lowerinit
// fixture domain (expr_lower_init_test.go), with the edition-2026 grammar
// switched on around the authoring calls -- the grammar every authored bundle
// is parsed with after the flip.

// withExpressionsV1 runs fn with the edition-2026 grammar on, as the flip will
// leave DefaultOptions. No test in this package runs in parallel.
func withExpressionsV1(t *testing.T, fn func()) {
	t.Helper()
	saved := languageParser.DefaultOptions
	languageParser.DefaultOptions = languageParser.Options{ExpressionsV1: true}
	defer func() { languageParser.DefaultOptions = saved }()
	fn()
}

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

func TestAuthoringLower_DefineRegistersLoweredForms(t *testing.T) {
	eng := bootAuthoringEngine(t)
	reg := NewAuthoredRuntimeRegistry()
	withExpressionsV1(t, func() {
		res, err := eng.DefineSessionBundle(reg, "owner-1", sessionGoodBundle, "")
		require.NoError(t, err, "%+v", res.Diagnostics)
		require.True(t, res.OK)
	})

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

func TestAuthoringLower_DefineRefusesWithTheThreePartMessageOnTheAuthorsLine(t *testing.T) {
	eng := bootAuthoringEngine(t)
	reg := NewAuthoredRuntimeRegistry()
	var res SessionDefineResult
	var err error
	withExpressionsV1(t, func() {
		res, err = eng.DefineSessionBundle(reg, "owner-1", sessionRefusedBundle, "")
	})
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

func TestAuthoringLower_TheEngineRefusesWhatTheBundleAloneCannot(t *testing.T) {
	eng := bootAuthoringEngine(t)
	const misKinded = `use lowerinit.concepts.{ ticket }
use lowerinit.specs.{ callerIsOwner }

/// A context spec applied to the row: only the spec registry knows its kind.
query ticket misKindedOnly {
  filter   row => callerIsOwner(row)
  paginate 20
}
`
	withExpressionsV1(t, func() {
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
	})
}

func TestAuthoringLower_StageRefusesBeforeStagingAnything(t *testing.T) {
	eng := bootAuthoringEngine(t)
	store := &fakePromoteStore{}
	var err error
	withExpressionsV1(t, func() {
		_, err = eng.stageBundleDurableWithStore(context.Background(), store, "owner-1", sessionRefusedBundle, "")
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "bundle failed validation")
	require.Empty(t, store.constructs, "a stage that does not lower persists nothing")

	withExpressionsV1(t, func() {
		res, err := eng.stageBundleDurableWithStore(context.Background(), store, "owner-1", sessionGoodBundle, "")
		require.NoError(t, err, "%+v", res.Diagnostics)
	})
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
	withExpressionsV1(t, func() {
		_, err := eng.DefineSessionBundle(reg, "owner-1", sessionGoodBundle, "")
		require.NoError(t, err)
		res, err := eng.DefineSessionBundle(reg, "owner-1", withShape, "")
		require.NoError(t, err, "%+v", res.Diagnostics)
	})

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
	withExpressionsV1(t, func() {
		var err error
		res, err = eng.rehydratePromotedNow(context.Background(), store)
		require.NoError(t, err)
	})

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

func TestAuthoringLower_ADanglingImportKeepsTheReferenceDiagnostic(t *testing.T) {
	eng := bootAuthoringEngine(t)
	var rep SandboxReport
	withExpressionsV1(t, func() {
		rep = SandboxCompileBundleWithEngine([]SandboxConstruct{{
			Kind: "spec",
			Name: "danglingSpec",
			Source: `use lowerinit.shapes.{ ghostShape }

/// A spec over a shape nothing declares.
spec ghostShape danglingSpec = row => row.status == "open"`,
		}}, eng)
	})
	require.False(t, rep.OK)
	d := diagnosticFor(t, rep.Diagnostics, "spec", "danglingSpec")
	require.Contains(t, d.Error, "unresolved reference", "the reference check names the import to fix; the lowering runs after it")
	require.Contains(t, d.Error, "ghostShape")
}

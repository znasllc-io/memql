package memql

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE TOP-LEVEL BUILTIN SEAM (memql#3982).
//
// A Logic whose whole body is `return <builtin>({...})` resolves to a bare
// *BuiltinFunctionExpression at plan.Root, and executeWith short-circuits it
// straight to evaluateBuiltinFunctionExpression. That branch reached NEITHER
// row-authz mechanism:
//
//   - The INJECTION seam (enforceRowAuthzOnPlan, called from parser.go)
//     returns early unless plan.BoundConcept is set, and a builtin call binds
//     no concept. Arguably correct by construction -- there is no filter to
//     AND a predicate into -- which is exactly why the second mechanism has
//     to cover it.
//   - The ROW GATE (filterRowAuthzSet) is applied inside
//     evaluateExpressionSet, and the branch returns from executeWith before
//     reaching the executor at all.
//
// The NESTED spelling of the identical call is gated: executor.go's
// `case *BuiltinFunctionExpression` sits inside evaluateExpressionSetWithContext,
// whose only non-recursive caller is evaluateExpressionSet, which applies the
// gate to whatever comes back. So the same builtin was enforced one syntactic
// level down and unenforced at the top. These tests are the fourth sibling of
// TestClusterOwnerTierInjectsTheAdminGate, TestFilteredReadPathAppliesTheRowGate
// and TestGraphExpansionAppliesTheTraversalGateBeforeItEmitsTheRow.
//
// WHY THIS MATTERS FOR REAL ROWS. Most builtins return a synthetic result
// envelope under a made-up concept (`memql:validate`, `integration:email:send`)
// that declares no tier, and nothing here changes for them. But an integration
// capability is registered into the same dispatch map by RegisterIntegration,
// and several of those run SQL against the node store and hand back the rows
// they read under the row's REAL concept and REAL payload --
// `integration.embedding.findSimilar` scans the concept out of its query,
// `integration.harnessRecall.recall` takes it from caller args. Those are graph
// rows of a declared concept reaching a caller having passed no per-row
// authorization.
//
// DELIBERATELY NOT DB-GATED. Every assertion here is about which rows survive
// a gate, and the rows are supplied by the probe handler, so a database adds
// nothing but a reason to skip. A security gate whose test self-skips wherever
// Postgres is not running is a gate that is off on most machines most of the
// time.

// rowAuthzBuiltinProbe is a test integration whose single capability hands
// back a fixed row set.
//
// It is registered through the REAL RegisterIntegration rather than by poking
// e.builtinExecutorHandlers, because that is precisely how every
// `integration.<name>.<capability>` builtin -- including the two that return
// real graph rows -- arrives in the dispatch map. Only the rows are a fixture;
// the registration, the parse, the resolution to a BuiltinFunctionExpression
// at plan.Root and the dispatch are all the production path.
type rowAuthzBuiltinProbe struct {
	rows []memorynodes.MemoryNode
}

func (p *rowAuthzBuiltinProbe) IntegrationName() string { return "rowauthzprobe" }

func (p *rowAuthzBuiltinProbe) Capabilities() []IntegrationCapability {
	return []IntegrationCapability{{
		Name:        "rows",
		Description: "returns a fixed row set, for row-authz seam tests",
		Handler: func(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			out := make([]memorynodes.MemoryNode, len(p.rows))
			copy(out, p.rows)
			return out, nil
		},
	}}
}

// rowAuthzBuiltinProbeCall is the query string under test: a bare builtin
// call, which is what a single-statement `return <builtin>({...})` logic
// body reduces to.
const rowAuthzBuiltinProbeCall = "probeBuiltinRows()"

// neverDialledDB satisfies executeWith's `database not configured` guard
// without a database.
//
// sql.OpenDB is lazy: no connection is attempted until a query runs, and the
// top-level-builtin branch returns before any query does. The DSN points at a
// port nothing listens on precisely so an accidental future dial fails loudly
// rather than silently borrowing whatever is on 5432.
func neverDialledDB(t *testing.T) *bun.DB {
	t.Helper()
	connector := pgdriver.NewConnector(
		pgdriver.WithDSN("postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable"))
	db := bun.NewDB(sql.OpenDB(connector), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// builtinProbeEngine builds a DB-free engine that can execute
// rowAuthzBuiltinProbeCall and nothing else.
func builtinProbeEngine(t *testing.T, rows []memorynodes.MemoryNode) *MemQLEngine {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	reg := newFunctionRegistry()
	if err := reg.add(&Function{
		Name:         "probeBuiltinRows",
		Type:         FunctionTypeBuiltin,
		FunctionKind: "builtin",
		Executor:     qualifiedCapabilityName("rowauthzprobe", "rows"),
		Enabled:      true,
	}); err != nil {
		t.Fatalf("register the probe builtin: %v", err)
	}

	e := &MemQLEngine{
		initialized:  true,
		functions:    reg,
		integrations: newIntegrationRegistry(),
		db:           neverDialledDB(t),
	}
	if err := e.RegisterIntegration(&rowAuthzBuiltinProbe{rows: rows}); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
	return e
}

// runBuiltinProbe executes the probe call and returns the row set the caller
// would actually receive. It asserts on the way through that the plan really
// did take the branch under test -- a test that stopped exercising the
// short-circuit would otherwise pass for the wrong reason forever.
func runBuiltinProbe(t *testing.T, e *MemQLEngine, ctx context.Context) map[string]memorynodes.MemoryNode {
	t.Helper()

	plan, err := e.parseWithFunctions(rowAuthzBuiltinProbeCall, e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse %s: %v", rowAuthzBuiltinProbeCall, err)
	}
	if _, isBuiltin := plan.Root.(*BuiltinFunctionExpression); !isBuiltin {
		t.Fatalf("%s no longer resolves to a *BuiltinFunctionExpression at plan.Root (got %T) -- "+
			"this fixture no longer exercises the seam it exists for", rowAuthzBuiltinProbeCall, plan.Root)
	}
	if plan.RowAuthzInjected {
		t.Fatalf("the builtin plan carries an INJECTED row-authz term. It binds no concept, so "+
			"there is nothing for enforceRowAuthzOnPlan to resolve a tier from; if that has "+
			"changed, this test is measuring the wrong mechanism. Bound concept: %q",
			plan.BoundConcept)
	}

	res, err := e.Execute(ctx, rowAuthzBuiltinProbeCall)
	if err != nil {
		t.Fatalf("execute %s: %v", rowAuthzBuiltinProbeCall, err)
	}
	if res == nil {
		t.Fatal("execute returned a nil result")
	}
	if res.output == nil {
		return map[string]memorynodes.MemoryNode{}
	}
	got, ok := res.output.(map[string]memorynodes.MemoryNode)
	if !ok {
		t.Fatalf("the builtin branch's output is %T, not the node map this test reads", res.output)
	}
	return got
}

func idsOf(set map[string]memorynodes.MemoryNode) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// THE REGRESSION. A top-level builtin returns rows of a clusterOwner-tier
// concept; a caller who is not the cluster owner must not receive them.
//
// The clusterOwner tier is the sharpest fixture available: it is decidable
// from the row alone, needs no payload field and no join, and its whole
// declared meaning is "administrative rows". A non-owner holding them is
// unambiguous.
func TestTopLevelBuiltinAppliesTheRowGate(t *testing.T) {
	decl := declFor(t, declaredClusterOwnerConcept)
	if decl.Tier != langparser.RowAuthzClusterOwner {
		t.Skipf("%s no longer declares clusterOwner; pick another fixture", declaredClusterOwnerConcept)
	}

	rows := []memorynodes.MemoryNode{
		rowOf(t, declaredClusterOwnerConcept, declaredClusterOwnerConcept+":c1",
			map[string]any{"number": "+15550000"}),
		rowOf(t, declaredClusterOwnerConcept, declaredClusterOwnerConcept+":c2",
			map[string]any{"number": "+15550001"}),
	}

	t.Run("a non-owner receives none of them", func(t *testing.T) {
		e := builtinProbeEngine(t, rows)
		got := runBuiltinProbe(t, e, callerCtx("user-a"))
		if len(got) != 0 {
			t.Fatalf("a caller who is not the cluster owner received %d rows of %s (%v).\n"+
				"%s declares %q. A top-level builtin call short-circuits executeWith before "+
				"BOTH row-authz mechanisms: enforceRowAuthzOnPlan injects nothing (the call "+
				"binds no concept) and filterRowAuthzSet is never reached (the branch returns "+
				"before the executor). Rows produced this way must pass the row gate before "+
				"setOutput (memql#3982).",
				len(got), declaredClusterOwnerConcept, idsOf(got),
				declaredClusterOwnerConcept, InjectedPredicate(decl))
		}
	})

	// The other half: the gate is a gate, not a blanket deny. Without this a
	// fix that dropped every builtin row would look green.
	t.Run("the cluster owner receives all of them", func(t *testing.T) {
		e := builtinProbeEngine(t, rows)
		got := runBuiltinProbe(t, e, ownerRoleCtx("root"))
		if len(got) != len(rows) {
			t.Fatalf("the cluster owner received %d of %d administrative rows (%v) -- the gate "+
				"is denying rows it declares readable", len(got), len(rows), idsOf(got))
		}
	})
}

// The gate resolves the tier from THE ROW'S OWN concept, so a mixed result set
// is filtered row by row rather than all-or-nothing. This is the property that
// makes the fix safe for the builtins that return synthetic envelopes: an
// undeclared concept passes through untouched.
func TestTopLevelBuiltinRowGateResolvesPerRow(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	if decl.Tier != langparser.RowAuthzOwned {
		t.Skipf("%s no longer declares the owned tier; pick another fixture", declaredOwnedConcept)
	}

	const (
		mine       = declaredOwnedConcept + ":mine"
		theirs     = declaredOwnedConcept + ":theirs"
		undeclared = "v1:identity:user:u1"
		envelope   = "memql:validate"
	)
	rows := []memorynodes.MemoryNode{
		rowOf(t, declaredOwnedConcept, mine, map[string]any{decl.Owner: "user-a", "title": "mine"}),
		rowOf(t, declaredOwnedConcept, theirs, map[string]any{decl.Owner: "user-b", "title": "theirs"}),
		rowOf(t, "v1:identity:user", undeclared, map[string]any{"primaryEmail": "a@b.c"}),
		// A synthetic result envelope, the shape almost every builtin actually
		// returns. It must survive: this fix must not break previewInsert,
		// validate, concepts, or any integration that reports its outcome.
		rowOf(t, envelope, envelope, map[string]any{"valid": true}),
	}

	e := builtinProbeEngine(t, rows)
	got := runBuiltinProbe(t, e, callerCtx("user-a"))

	if _, ok := got[theirs]; ok {
		t.Errorf("caller user-a received a row owned by user-b through a top-level builtin. "+
			"%s declares %q (memql#3982)", declaredOwnedConcept, InjectedPredicate(decl))
	}
	if _, ok := got[mine]; !ok {
		t.Errorf("the caller's OWN row was dropped (%v)", idsOf(got))
	}
	if _, ok := got[undeclared]; !ok {
		t.Errorf("a row of an undeclared concept was dropped. Enforcement covers the declared "+
			"population only -- denying an undeclared row would be inventing a tier (%v)", idsOf(got))
	}
	if _, ok := got[envelope]; !ok {
		t.Errorf("a synthetic builtin result envelope was dropped. That is the shape nearly "+
			"every builtin returns, and it declares no tier (%v)", idsOf(got))
	}
}

// An unauthenticated caller gets no owned-tier rows.
//
// refuseRowAuthzWithoutActor -- the plan-level refusal for this case -- does
// NOT run on this path, deliberately: it guards plan.RowAuthzInjected, and
// nothing was injected because there was no binding to inject from, so there
// is no term here comparing an owner field against the empty string. The
// actorless caller is handled on the row side instead, where rowAuthzAdmits
// denies the owned tier outright for an empty actor. This pins that the
// omission costs nothing.
func TestTopLevelBuiltinDeniesAnActorlessCaller(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	rows := []memorynodes.MemoryNode{
		rowOf(t, declaredOwnedConcept, declaredOwnedConcept+":orphan",
			map[string]any{decl.Owner: "", "title": "owned by the empty string"}),
		rowOf(t, declaredOwnedConcept, declaredOwnedConcept+":somebodys",
			map[string]any{decl.Owner: "user-b", "title": "somebody's"}),
	}

	e := builtinProbeEngine(t, rows)
	got := runBuiltinProbe(t, e, context.Background())
	if len(got) != 0 {
		t.Fatalf("a caller with no identity received %d owned-tier rows through a top-level "+
			"builtin (%v). `%s` is a predicate that MATCHES a row stored with an empty owner, "+
			"which is memql#3172 finding 4 -- and refuseRowAuthzWithoutActor cannot fire here "+
			"because nothing was injected into this plan",
			len(got), idsOf(got), InjectedPredicate(decl))
	}
}

// UNDECIDABLE FAILS CLOSED, matching graph expansion rather than the filtered
// read.
//
// The `granted` tier's predicate is a relationship spec, decidable only via the
// join a filter performs. The filtered path admits it because for a bound
// construct that filter now carries the spec as a top-level conjunct. A
// top-level builtin ran no filter and had nothing injected, so there is no
// performed join to defer to. This is the one place the builtin gate differs
// from filterRowAuthzSet, and it is the same asymmetry
// TestTraversalGateFailsClosedWhereTheFilterGateDefers pins for traversal.
func TestTopLevelBuiltinRowGateFailsClosedOnAnUndecidableTier(t *testing.T) {
	name := registerProbeConcept(t, "v1:rowauthzprobe:builtinGrantedThing",
		&langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"})
	row := rowOf(t, name, name+":g1", map[string]any{"spaceId": "space-1"})

	if got := rowAuthzAdmits(callerCtx("user-a"), row.Concept, row.ID, row.Payload); got != rowAuthzUndecided {
		t.Fatalf("the granted tier resolved to %v against a lone row, want undecided -- this "+
			"fixture measures nothing otherwise", got)
	}
	if !admitRowAuthzNode(callerCtx("user-a"), row) {
		t.Error("the FILTERED path denied a granted-tier row; there the filter carries the spec")
	}
	if admitRowAuthzBuiltinResult(callerCtx("user-a"), row) {
		t.Fatal("a top-level builtin admitted a granted-tier row. No filter ran and no predicate " +
			"was injected, so there is no join for the undecidable verdict to defer to -- it " +
			"must fail closed, exactly as the traversal gate does (memql#3982)")
	}
}

// A row with an EMPTY concept is admitted, and that is a recorded decision
// rather than a gap.
//
// rowAuthzDeclFor("") answers nil, so such a row lands in rowAuthzAdmits'
// UNDECLARED branch, which admits. Denying it here alone would give one row two
// different answers depending on which seam it left through -- the traversal
// gate, whose semantics this one copies, admits it too. It also costs nothing
// against the hole being closed: a row with no concept is by definition not a
// row of a concept that declared a tier. Changing this is a change to the
// undeclared branch, applying to every seam at once.
func TestTopLevelBuiltinRowGateAdmitsAConceptlessRow(t *testing.T) {
	row := memorynodes.MemoryNode{ID: "loose-row", Payload: []byte(`{"anything":true}`)}

	if !admitRowAuthzBuiltinResult(callerCtx("user-a"), row) {
		t.Fatal("a row with no concept was denied by the builtin gate but is admitted by the " +
			"filtered and traversal gates. One row must not get two answers depending on which " +
			"seam it left through; if concept-less rows should be denied, change rowAuthzAdmits' " +
			"undeclared branch so every seam agrees (memql#3982)")
	}
	if !admitRowAuthzTraversal(callerCtx("user-a"), row) {
		t.Fatal("the traversal gate now denies a concept-less row -- the builtin gate copied its " +
			"semantics on the strength of that, so the two have drifted")
	}
}

// THE WIRING GUARD, the sibling of TestFilteredReadPathAppliesTheRowGate.
//
// The gates above are unit-testable in isolation, and isolation is exactly how
// a gate ends up correct and uncalled. This reads the source and asserts the
// branch still applies it to what setOutput is handed -- once setOutput has the
// rows they are the caller's result.
//
// It asserts the ungated spelling is ABSENT rather than an ordering between two
// string offsets, because the gate is an ARGUMENT to setOutput
// (`setOutput(nodesToMap(filterRowAuthzBuiltinNodes(ctx, nodes)))`), so the
// gate's offset is legitimately the larger one. An offset comparison here reads
// correct and fails on correct code.
func TestTopLevelBuiltinBranchGatesWhatItReturns(t *testing.T) {
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	text := string(src)

	branch := strings.Index(text, "if builtinCall, ok := plan.Root.(*BuiltinFunctionExpression); ok {")
	if branch < 0 {
		t.Fatal("the top-level-builtin branch is gone from engine.go -- this guard needs re-aiming")
	}
	body := text[branch:]
	if end := strings.Index(body, "\n\t}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "return result, nil") {
		t.Fatalf("the extracted branch body does not end in `return result, nil`, so this guard is "+
			"reading the wrong region and would pass on anything:\n%s", body)
	}

	if !strings.Contains(body, "filterRowAuthzBuiltinNodes(") {
		t.Fatal("the top-level-builtin branch no longer applies filterRowAuthzBuiltinNodes.\n" +
			"That branch returns from executeWith before the executor, so it reaches NEITHER " +
			"row-authz mechanism on its own: enforceRowAuthzOnPlan injects nothing (a builtin " +
			"call binds no concept) and filterRowAuthzSet lives in evaluateExpressionSet, which " +
			"is never reached. Without this call, rows an integration capability read out of the " +
			"node store are handed to the caller unauthorized (memql#3982).")
	}
	if strings.Contains(body, "setOutput(nodesToMap(nodes))") {
		t.Fatal("the branch hands setOutput the UNGATED node slice. That is the exact line " +
			"memql#3982 closed -- the gate must wrap the handler's rows on their way into the " +
			"result, not sit beside them.")
	}
}

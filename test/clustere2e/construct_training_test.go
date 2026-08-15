//go:build clustere2e

package clustere2e

// construct_training_test.go -- the live-cluster gate for epic memql#3745,
// training constructs into a running cluster.
//
// WHAT THIS PROVES THAT NO UNIT TEST CAN
// --------------------------------------
// Every behaviour below is unit-tested in component/memql against an in-process
// engine. Those tests are real and they are not this. Four things only a live
// mesh can establish:
//
//   1. THE PERSISTENCE PATH RUNS. The promote store writes its rows through the
//      engine's own mutation path, and that path was BROKEN from memql#2335
//      until memql#3757 -- the store built `createAuthoringBundle({...})`, the
//      object-literal invocation form the parser had been flipped to reject, so
//      every durable promote against a real engine failed at persistence AFTER
//      the construct had already registered. It survived because the store's
//      only coverage was a fake that never builds a call string. A fake store
//      cannot fail that way, which is exactly why one is not enough.
//
//   2. THE PROMOTE CROSSES NODES. The cluster runs 2 replicas per node type and
//      the front door round-robins across them, so a concept promoted on one
//      bff replica has to be usable through a connection that landed on
//      another. That is the cross-node broadcast, and it is the memql#1448 bug
//      class: green single-node tests, silent breakage in the mesh.
//
//   3. THE WIRE CARRIES THE STRUCTURE. The classified schema diff (memql#3757)
//      and the retired-vs-removed outcome (memql#3756) are proto messages the
//      SDK maps into its own types. A field the engine populates and the proto
//      does not carry -- or the SDK does not read -- is invisible to every
//      in-process test and fatal to the editor that renders it.
//
//   4. THE OWNER GATE IS REAL. Promote and demote are owner-only on the stream
//      interceptor, not in the engine, so only a dialled connection exercises
//      it.
//
// HOW TO RUN
// ----------
//	make up SERVERS=2 && make scale N=2 && make dev   # images must carry the epic
//	MEMQL_E2E_TOKEN=<cluster-owner JWT> bash scripts/test/cluster-e2e.sh --no-build
//
// The token must belong to a CLUSTER OWNER. The harness's auto-seed logs in as
// a fresh random address, which on an already-bootstrapped cluster is not the
// owner, so promote refuses and every case here skips with the engine's own
// message rather than failing. That skip is the correct outcome for a
// non-owner: the gate this file is checking is working.
//
// A COLD MESH IS SLOWER THAN 30 SECONDS, OCCASIONALLY. The first cross-node run
// after a `make dev` rollout has been observed to exceed waitCallable's budget
// while the two freshly-restarted bff replicas were still establishing their
// peer link; every run after it passed, and the propagation logs on the reader
// replica showed a burst arriving ~32s after the promote. So a failure at step 3
// ALONE, on the first run after a rollout, is more likely to be a mesh still
// linking than a broken broadcast -- check the reader replica's
// "authoring promote propagation: bundle re-hydrated from broadcast" lines
// before concluding otherwise. A second failure is a real one.
//
// The wall-clock difference is itself the evidence that the hop is exercised:
// with both connections on one replica this file runs in ~0.5s, and with the
// reader pinned to the other replica it takes ~2.8s -- the difference being the
// broadcast actually being waited on.
//
// SIDE EFFECTS: it promotes and then withdraws its own concept, under a
// namespace and id unique to the run. It writes rows only under that concept
// and never touches a core one. A run that dies mid-way leaves a promoted
// concept behind under a name nothing else will ever claim; demoting it by hand
// is one call, and its rows are its own.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/sdk/go/authoring"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// --- the bundle under test ---------------------------------------------------

// trainingFixture is one run's private concept plus the two constructs bound to
// it. Everything is parameterised by a per-run suffix so concurrent runs, and
// re-runs against a cluster that still holds a previous run's promotion, cannot
// collide: a promoted concept's name is claimed cluster-wide.
type trainingFixture struct {
	namespace  string
	concept    string
	conceptId  string
	mutation   string
	query      string
	rowIdShort string
}

// suffixRunes keeps only the characters a MemQL identifier and a `@namespace`
// both admit. `id.NewShortId()` emits a uuid-shaped value, and its HYPHENS are
// legal in neither: `@namespace` is validated against
// ^[a-z][a-z0-9_]*(:[a-z][a-z0-9_]*)*$, and the same suffix is concatenated into
// construct names. The live gate caught this on its first real run, refusing the
// bundle at Gate-1 with the namespace pattern named -- which is the engine
// behaving correctly and the fixture being wrong.
func suffixRunes(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() >= 10 {
			break
		}
	}
	return b.String()
}

func newTrainingFixture() trainingFixture {
	suffix := suffixRunes(id.NewShortId())
	ns := "e2etrain" + suffix
	// UNIQUE PER RUN, declaration name included. A promoted concept's name is
	// claimed cluster-wide and demote resolves by name, so a shared declaration
	// name makes every run after the first ambiguous and un-demotable.
	name := "trainedWidget" + suffix
	return trainingFixture{
		namespace:  ns,
		concept:    name,
		conceptId:  fmt.Sprintf("v1:%s:%s", ns, name),
		mutation:   "mutationCreate" + strings.ToUpper(name[:1]) + name[1:],
		query:      "query" + strings.ToUpper(name[:1]) + name[1:],
		rowIdShort: "row" + suffix,
	}
}

// conceptSrc renders the concept. `label` is optional in v1; the additive and
// breaking variants below differ from this one in exactly one declaration each,
// so a failure names which change was misclassified rather than which file was
// wrong.
func (f trainingFixture) conceptSrc() string {
	return fmt.Sprintf(`@version("1.0.0")
@namespace("%s")
@description("A concept taught to a running cluster by the memql#3745 gate")
concept %s {
  ownerUserId  string  @required
  label        string
}`, f.namespace, f.concept)
}

// conceptSrcAdditive adds one OPTIONAL field. Design section 7.3: additive lands.
func (f trainingFixture) conceptSrcAdditive() string {
	return fmt.Sprintf(`@version("1.0.0")
@namespace("%s")
@description("A concept taught to a running cluster by the memql#3745 gate")
concept %s {
  ownerUserId  string  @required
  label        string
  notes        string
}`, f.namespace, f.concept)
}

// conceptSrcBreaking REMOVES a field that rows already carry. Design section
// 7.3: breaking, refused, and the refusal names the field.
func (f trainingFixture) conceptSrcBreaking() string {
	return fmt.Sprintf(`@version("1.0.0")
@namespace("%s")
@description("A concept taught to a running cluster by the memql#3745 gate")
concept %s {
  ownerUserId  string  @required
  notes        string
}`, f.namespace, f.concept)
}

// mutationSrc binds the promoted concept by its SIGNATURE, which is the binding
// a row write goes through -- the thing that proves the promoted concept is not
// merely registered but resolvable by another construct.
//
// `@actor` is REQUIRED on both this and the query below: reading the auth
// envelope is a declared capability (memql#2621), and a construct that reads
// `actor.*` without declaring it is refused at Gate-1. The live gate caught
// this too -- a promoted construct is held to exactly the same authoring rules
// as a seeded one, which is itself worth having asserted.
func (f trainingFixture) mutationSrc() string {
	return fmt.Sprintf(`use %s.concepts.{ %s }

@actor
@description("Create a trained widget")
mutate %s %s {
  args {
    widgetId  string  @required
    label     string  @required
  }
  insert {
    id:          canonicalId(args.widgetId, %s)
    ownerUserId: actor.userId
    label:       args.label
  }
}`, f.namespace, f.concept, f.concept, f.mutation, f.concept)
}

// querySrc carries NO `shape` clause on purpose. A shape is a named construct,
// not an inline projection -- `shape { row.id label }` is not a shorthand, it is
// a lookup of a template by that literal name, and the engine says so ("shape
// template not found"). Omitting it takes the default projection over the bound
// concept (memql#2035), which is what this gate wants anyway: the assertion is
// that the promoted concept is READABLE on another replica, not that any
// particular subset of its fields comes back.
func (f trainingFixture) querySrc() string {
	return fmt.Sprintf(`use %s.concepts.{ %s }

@actor
@description("Read this owner's trained widgets")
query %s %s {
  filter  ownerUserId==actor.userId
}`, f.namespace, f.concept, f.concept, f.query)
}

// bundle is what a promote actually carries: the concept AND the constructs
// bound to it. Promoting the noun alone would leave nothing able to write to it.
func (f trainingFixture) bundle() string {
	return f.conceptSrc() + "\n\n" + f.mutationSrc() + "\n\n" + f.querySrc()
}

func (f trainingFixture) bundleWith(conceptSrc string) string {
	return conceptSrc + "\n\n" + f.mutationSrc() + "\n\n" + f.querySrc()
}

// conceptOnly is the demote source for the RETIRE case, and the distinction is
// load-bearing. DurableDemoteBundle withdraws every construct the source names,
// so demoting the whole bundle takes the mutation with it -- and a write through
// a withdrawn mutation fails with "function not found", which is not the
// refusal the retire rule exists to produce. Withdrawing the noun alone is also
// the honest shape of the operation being tested: retirement is a property of
// the CONCEPT, and the constructs bound to it are meant to survive it and be
// refused by it.
func (f trainingFixture) conceptOnly() string { return f.conceptSrc() }

// --- helpers -----------------------------------------------------------------

// skipUnlessOwner turns the engine's owner-only refusal into a SKIP rather than
// a failure. A non-owner token is a property of how the harness seeded itself,
// not a defect in the code under test, and a gate that fails on it would be a
// gate nobody runs.
func skipUnlessOwner(t *testing.T, res *authoring.PromoteResult, err error) {
	t.Helper()
	msg := ""
	if err != nil {
		msg = err.Error()
	} else if res != nil {
		msg = res.Error
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "owner") && (strings.Contains(low, "require") ||
		strings.Contains(low, "permission") || strings.Contains(low, "denied")) {
		t.Skipf("MEMQL_E2E_TOKEN is not a cluster owner, which promote requires: %s", msg)
	}
}

// withdraw best-effort demotes the fixture so a failed run leaves less behind.
// Errors are logged, never failed on: a cleanup that can fail the test hides the
// failure that caused it.
func withdraw(ctx context.Context, t *testing.T, ac *authoring.Client, f trainingFixture) {
	t.Helper()
	// The whole bundle first; then the concept alone. A bundle demote fails
	// WHOLESALE if any construct it names is already withdrawn, so a cleanup that
	// only tries the bundle leaves the CONCEPT promoted after any partial run --
	// and a promoted concept's name is claimed cluster-wide forever.
	if res, err := ac.DurableDemoteBundle(ctx, f.bundle()); err == nil && res.OK {
		return
	}
	res, err := ac.DurableDemoteBundle(ctx, f.conceptOnly())
	if err != nil {
		t.Logf("cleanup: demote %s: %v", f.conceptId, err)
		return
	}
	if !res.OK {
		t.Logf("cleanup: demote %s not ok: %s", f.conceptId, res.Error)
	}
}

// conceptOutcome finds the demote outcome for the fixture's concept.
func conceptOutcome(res *authoring.DemoteResult, f trainingFixture) (authoring.DemoteOutcome, bool) {
	for _, o := range res.Outcomes {
		if o.Kind == "concept" && (o.ConceptId == f.conceptId || o.Name == f.concept) {
			return o, true
		}
	}
	return authoring.DemoteOutcome{}, false
}

// --- the gate ----------------------------------------------------------------

// TestConceptTrainingRoundTripsAcrossTheMesh is the whole epic in one sequence,
// deliberately: each step's precondition is the previous step's effect, so
// splitting them into independent tests would mean re-promoting a concept whose
// name is claimed cluster-wide four more times.
func TestConceptTrainingRoundTripsAcrossTheMesh(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// TWO CONNECTIONS, AND THE SECOND ONE'S ADDRESS IS THE WHOLE POINT.
	//
	// `conns[0]` promotes; `conns[1]` reads, and the cross-node claim is only as
	// good as the odds that it landed on a different replica. Through the front
	// door those odds are round-robin. Through `kubectl port-forward svc/bff`
	// they are ZERO: a port-forward to a Service resolves to ONE pod at start and
	// pins every connection to it, so both connections share a replica and the
	// hop is never exercised -- while the test passes and claims it was.
	//
	// So `MEMQL_E2E_ENDPOINT_B` pins the reader to a SPECIFIC second replica,
	// which makes the assertion deterministic rather than probabilistic:
	//
	//	kubectl port-forward pod/<bff-a> 50051:50051 &
	//	kubectl port-forward pod/<bff-b> 50052:50051 &
	//	MEMQL_E2E_ENDPOINT=localhost:50051 MEMQL_E2E_ENDPOINT_B=localhost:50052 ...
	//
	// When it is unset the test still runs and still asserts the construct is
	// callable on a second connection -- it just LOGS that the second connection
	// may be the same replica, rather than quietly implying a hop it did not
	// take.
	conns := openConnections(ctx, t, tok, 1)
	if addrB := os.Getenv("MEMQL_E2E_ENDPOINT_B"); addrB != "" {
		connB, err := memqlclient.Connect(ctx, memqlclient.ConnectConfig{Endpoint: addrB, Token: tok})
		if err != nil {
			t.Fatalf("connect the second replica at %s: %v", addrB, err)
		}
		conns = append(conns, connB)
		t.Logf("reader pinned to a second replica at %s -- the cross-node hop is exercised", addrB)
	} else {
		conns = append(conns, openConnections(ctx, t, tok, 1)...)
		t.Logf("MEMQL_E2E_ENDPOINT_B unset: the reader may share a replica with the promoter, " +
			"so the cross-node hop is NOT guaranteed to be exercised by this run")
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	promoter := authoring.NewClient(conns[0].Dispatcher())
	f := newTrainingFixture()
	t.Logf("training %s on the live cluster", f.conceptId)

	// --- 1. dry-run mutates nothing -----------------------------------------
	//
	// Asserted rather than assumed: the query below is invoked BEFORE any
	// promote, and must fail to resolve. If validation had leaked the
	// construct into the shared registry, it would resolve.
	val, err := promoter.ValidateBundle(ctx, f.bundle())
	if err != nil {
		t.Fatalf("validate bundle: %v", err)
	}
	for _, d := range val.Diagnostics {
		if !d.OK && !d.Skipped {
			t.Fatalf("the fixture bundle does not compile on this cluster: %s %s: %s", d.Kind, d.Name, d.Error)
		}
	}
	if !val.OK {
		t.Fatalf("validate reported not-ok with no failing diagnostic")
	}
	if err := invokeQuery(ctx, conns[0], f.query); err == nil {
		t.Fatal("a Gate-1 validate made the construct callable: it must mutate NOTHING")
	}

	// --- 2. promote ---------------------------------------------------------
	res, err := promoter.DurablePromoteBundle(ctx, f.bundle())
	skipUnlessOwner(t, res, err)
	if err != nil {
		t.Fatalf("durable promote: %v", err)
	}
	if !res.OK {
		t.Fatalf("durable promote refused: %s", res.Error)
	}
	defer withdraw(ctx, t, promoter, f)

	var promotedConcept bool
	for _, c := range res.Promoted {
		if c.Kind == "concept" {
			promotedConcept = true
		}
	}
	if !promotedConcept {
		t.Fatalf("promote reported ok but no concept among %d promoted constructs", len(res.Promoted))
	}
	// A FIRST promote is not run through the classifier at all (memql#3757).
	if len(res.ConceptDiffs) != 0 {
		t.Errorf("a first promote produced %d concept diff(s); it must not reach the classifier", len(res.ConceptDiffs))
	}

	// --- 3. it is live on ANOTHER connection --------------------------------
	//
	// This is the cross-node assertion. The propagation is a broadcast, so
	// allow it a bounded moment rather than requiring it to have already
	// happened at the instant the promote returned.
	if err := waitCallable(ctx, conns[1], f.query, 30*time.Second); err != nil {
		t.Fatalf("promoted construct never became callable on the second connection: %v", err)
	}

	// --- 4. rows write and read back ---------------------------------------
	if err := invokeMutation(ctx, conns[0], f.mutation, map[string]string{
		"widgetId": f.rowIdShort,
		"label":    "first",
	}); err != nil {
		t.Fatalf("write a row under the promoted concept: %v", err)
	}
	if err := invokeQuery(ctx, conns[1], f.query); err != nil {
		t.Fatalf("read rows under the promoted concept from the second connection: %v", err)
	}

	// --- 5. an ADDITIVE re-promote lands ------------------------------------
	add, err := promoter.DurablePromoteBundle(ctx, f.bundleWith(f.conceptSrcAdditive()))
	if err != nil {
		t.Fatalf("additive re-promote: %v", err)
	}
	if !add.OK {
		t.Fatalf("an additive schema change was refused: %s", add.Error)
	}
	if breakingIn(add.ConceptDiffs) {
		t.Errorf("an additive change was classified breaking: %+v", add.ConceptDiffs)
	}

	// --- 6. a BREAKING re-promote is refused, naming the field --------------
	brk, err := promoter.DurablePromoteBundle(ctx, f.bundleWith(f.conceptSrcBreaking()))
	if err == nil && brk.OK {
		t.Fatal("removing a field that rows carry was allowed without the override")
	}
	if brk == nil || len(brk.ConceptDiffs) == 0 {
		t.Fatalf("a breaking refusal carried no classified diff (err=%v); the editor has nothing to render", err)
	}
	if !breakingIn(brk.ConceptDiffs) {
		t.Errorf("the refusal's diff marks nothing breaking: %+v", brk.ConceptDiffs)
	}
	if !namesField(brk.ConceptDiffs, "label") {
		t.Errorf("the refusal does not name the removed field `label`: %+v", brk.ConceptDiffs)
	}
	// The row count must be a REAL count of the row written in step 4, not a
	// placeholder -- and must be reported as known.
	if !countedAtLeast(brk.ConceptDiffs, 1) {
		t.Errorf("the refusal reports no known row count, but a row was written under the concept: %+v", brk.ConceptDiffs)
	}

	// --- 7. the override lands it -------------------------------------------
	ovr, err := promoter.DurablePromoteBundle(ctx, f.bundleWith(f.conceptSrcBreaking()), authoring.AllowBreaking())
	if err != nil {
		t.Fatalf("override promote: %v", err)
	}
	if !ovr.OK {
		t.Fatalf("the explicit override did not land the breaking change: %s", ovr.Error)
	}
	if !overriddenIn(ovr.ConceptDiffs) {
		t.Errorf("the landed override is not marked overridden, so nothing records that it was one: %+v", ovr.ConceptDiffs)
	}

	// --- 8. demote with rows RETIRES ----------------------------------------
	dem, err := promoter.DurableDemoteBundle(ctx, f.conceptOnly())
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if !dem.OK {
		t.Fatalf("demote refused: %s", dem.Error)
	}
	out, ok := conceptOutcome(dem, f)
	if !ok {
		t.Fatalf("demote reported no outcome for the concept: %+v", dem.Outcomes)
	}
	if out.Outcome != authoring.DemoteOutcomeRetired {
		t.Fatalf("a concept with rows under it was %q, want %q -- removing it would strand the rows",
			out.Outcome, authoring.DemoteOutcomeRetired)
	}
	if out.RowCount < 1 {
		t.Errorf("the retire outcome reports %d rows, but one was written in step 4", out.RowCount)
	}

	// --- 9. a write to a retired concept is refused, naming the retirement --
	werr := invokeMutation(ctx, conns[0], f.mutation, map[string]string{
		"widgetId": f.rowIdShort + "b",
		"label":    "second",
	})
	if werr == nil {
		t.Fatal("a write to a RETIRED concept succeeded")
	}
	if !strings.Contains(strings.ToLower(werr.Error()), "retired") {
		t.Errorf("the write refusal does not name the retirement, so it reads as a generic validation failure: %v", werr)
	}

	// --- 10. re-promoting UN-RETIRES ----------------------------------------
	//
	// The ADDITIVE schema, not the original. By this point the override has
	// landed {ownerUserId, notes}; the original is {ownerUserId, label}, which
	// REMOVES notes and is therefore breaking. The additive schema carries both,
	// so it adds `label` back without taking `notes` away and lands with no
	// override -- which is the point being made: un-retiring is an ordinary
	// promote, not a privileged one.
	un, err := promoter.DurablePromoteBundle(ctx, f.bundleWith(f.conceptSrcAdditive()))
	if err != nil {
		t.Fatalf("re-promote to un-retire: %v", err)
	}
	if !un.OK {
		t.Fatalf("re-promote refused: %s", un.Error)
	}
	if err := invokeMutation(ctx, conns[0], f.mutation, map[string]string{
		"widgetId": f.rowIdShort + "c",
		"label":    "third",
	}); err != nil {
		t.Fatalf("writes did not resume after the concept was un-retired: %v", err)
	}
}

// TestDemoteOfAnEmptyConceptRemovesIt is the other half of memql#3756's rule,
// and it needs its own fixture: the concept must have NEVER had a row written
// under it, which the sequence above cannot provide.
func TestDemoteOfAnEmptyConceptRemovesIt(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	defer conns[0].Close()

	ac := authoring.NewClient(conns[0].Dispatcher())
	f := newTrainingFixture()

	res, err := ac.DurablePromoteBundle(ctx, f.bundle())
	skipUnlessOwner(t, res, err)
	if err != nil {
		t.Fatalf("durable promote: %v", err)
	}
	if !res.OK {
		t.Fatalf("durable promote refused: %s", res.Error)
	}

	dem, err := ac.DurableDemoteBundle(ctx, f.bundle())
	if err != nil {
		t.Fatalf("demote: %v", err)
	}
	if !dem.OK {
		t.Fatalf("demote refused: %s", dem.Error)
	}
	out, ok := conceptOutcome(dem, f)
	if !ok {
		t.Fatalf("demote reported no outcome for the concept: %+v", dem.Outcomes)
	}
	if out.Outcome != authoring.DemoteOutcomeRemoved {
		t.Fatalf("a concept with ZERO rows was %q, want %q -- a concept promoted by typo must be cleanly withdrawable",
			out.Outcome, authoring.DemoteOutcomeRemoved)
	}
	if out.RowCount != 0 {
		t.Errorf("the remove outcome reports %d rows; it should have found none", out.RowCount)
	}

	// The name is free again: promoting it a second time must succeed rather
	// than collide with a registry entry that was never withdrawn.
	again, err := ac.DurablePromoteBundle(ctx, f.bundle())
	if err != nil {
		t.Fatalf("re-promote after remove: %v", err)
	}
	if !again.OK {
		t.Fatalf("the name was not freed by the remove: %s", again.Error)
	}
	withdraw(ctx, t, ac, f)
}

// --- diff predicates ---------------------------------------------------------

func breakingIn(diffs []authoring.ConceptSchemaDiff) bool {
	for _, d := range diffs {
		if d.Breaking {
			return true
		}
	}
	return false
}

func overriddenIn(diffs []authoring.ConceptSchemaDiff) bool {
	for _, d := range diffs {
		if d.Overridden {
			return true
		}
	}
	return false
}

func namesField(diffs []authoring.ConceptSchemaDiff, field string) bool {
	for _, d := range diffs {
		for _, c := range d.Changes {
			if c.Field == field {
				return true
			}
		}
	}
	return false
}

// countedAtLeast requires a KNOWN count of at least n. RowCountKnown is the
// point: a node with no database cannot count, and a zero it did not take must
// never be read as "no rows".
func countedAtLeast(diffs []authoring.ConceptSchemaDiff, n int64) bool {
	for _, d := range diffs {
		for _, c := range d.Changes {
			if c.RowCountKnown && c.RowsAffected >= n {
				return true
			}
		}
	}
	return false
}

// --- invoking a named construct ---------------------------------------------
//
// A promoted construct has no generated Go binding by definition -- it does not
// exist in this engine's DSL tree, which is the whole point of the epic. So the
// gate goes through ExecuteNamed, the SDK's supported escape hatch for exactly
// that case, and builds the call string the same way the generated builders do:
// `query name(arg: <literal>)` / `mutation name(arg: <literal>)`, with values
// quoted by the engine's own parser rather than by fmt.

// invokeQuery calls a promoted query by name with no arguments and reports
// whether the cluster could resolve and run it.
func invokeQuery(ctx context.Context, conn *memqlclient.Connection, name string) error {
	return invokeCall(ctx, conn, name, "query "+name+"()")
}

// invokeMutation calls a promoted mutation with named arguments. Keys are
// emitted in sorted order so a failure message is stable across runs.
func invokeMutation(ctx context.Context, conn *memqlclient.Connection, name string, args map[string]string) error {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("mutation ")
	b.WriteString(name)
	b.WriteString("(")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(langparser.QuoteString(args[k]))
	}
	b.WriteString(")")
	return invokeCall(ctx, conn, name, b.String())
}

// invokeCall is the single place this file talks to the execute path, so the
// gate's assertions read as "callable / not callable" rather than as dispatcher
// plumbing.
func invokeCall(ctx context.Context, conn *memqlclient.Connection, name, call string) error {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := memqlclient.NewQueryClient(conn.Dispatcher()).ExecuteNamed(callCtx, name, call)
	return err
}

// waitCallable polls until a construct resolves on this connection or the
// deadline passes. The promote's cross-node propagation is a broadcast, so
// "callable everywhere" is eventual over a short window, not instantaneous.
func waitCallable(ctx context.Context, conn *memqlclient.Connection, name string, within time.Duration) error {
	deadline := time.Now().Add(within)
	var last error
	for time.Now().Before(deadline) {
		if last = invokeQuery(ctx, conn, name); last == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return last
}

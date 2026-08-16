//go:build clustere2e

package clustere2e

// construct_staging_test.go -- the live-cluster gate for epic memql#3928, the
// durable STAGED tier.
//
// THE NEGATIVE ASSERTION IS THE POINT
// -----------------------------------
// construct_training_test.go's central claim is `waitCallable` -- a promoted
// construct becomes callable on a SECOND replica within seconds, because the
// promote broadcasts. Staging is that path with the broadcast omitted, and the
// omission IS the tier. So the assertion here is the inverse: a staged
// construct is callable by its author on the node it was staged against, and
// stays NOT callable on the other replica -- and only becomes callable there
// once it is trained.
//
// That inverse is the one shape no in-process test can produce. A unit test can
// assert that the engine did not call publishAuthoringPromote; it cannot assert
// that a second engine, on a second machine, sharing one database, still does
// not have the construct. Those are different claims, and the second is the one
// an author is relying on when they stage something.
//
// OWNER-SCOPED MEANS PER-OWNER, NOT PER-NODE, and the two tests in this file
// assert OPPOSITE things about the second replica because of it. Before a
// restart the construct is absent on the other replica because nothing
// propagated it -- that is the no-broadcast proof. After one it is PRESENT
// there, because every node re-hydrates the same persisted rows at boot. What
// stays true across both is the TIER: the other replica must report it `staged`,
// never `promoted`. A reader who takes the first test's absence assertion as the
// invariant will read the second one as a contradiction; it is not.
//
// A STAGED CONSTRUCT STILL HAS TO BIND, so this gate PROMOTES a concept first
// and stages the verbs bound to it. That is not a workaround -- it is the
// documented workflow, and the reason is structural: a concept registers into
// the one shared concept registry and its merge rebuilds relationship state
// cluster-wide, so there is no owner-scoped form of it to stage. The refusal is
// asserted too, by name.
//
// HOW TO RUN -- identical to the training gate, and MEMQL_E2E_ENDPOINT_B matters
// MORE here than it does there:
//
//	kubectl port-forward pod/<bff-a> 50051:50051 &
//	kubectl port-forward pod/<bff-b> 50052:50051 &
//	MEMQL_E2E_ENDPOINT=localhost:50051 MEMQL_E2E_ENDPOINT_B=localhost:50052 \
//	  MEMQL_E2E_TOKEN=<cluster-owner JWT> bash scripts/test/cluster-e2e.sh --no-build
//
// Without it the reader may share a replica with the stager, and a "not
// callable on the other replica" assertion made against the SAME replica proves
// nothing -- it would fail, correctly, and for the wrong reason. So the
// negative half SKIPS when the pin is absent rather than asserting on an
// undetermined topology. The positive half (callable for the author, callable
// everywhere once trained) still runs.
//
// SIDE EFFECTS: promotes a concept and stages two constructs under a namespace
// and id unique to the run, then withdraws all three.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/sdk/go/authoring"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
	"github.com/znasllc-io/memql/sdk/go/constructs"
)

// stagedBundle is the fixture's verbs WITHOUT its concept -- what a stage
// actually carries. trainingFixture.bundle() carries all three, which is what a
// promote carries; the two differ by exactly the construct that cannot be
// staged, which is the distinction this file exists to check.
func stagedBundle(f trainingFixture) string {
	return f.mutationSrc() + "\n\n" + f.querySrc()
}

// skipUnlessOwnerStage is skipUnlessOwner for a StageResult. Staging takes the
// owner-or-developer bar, so it does not itself refuse a non-owner -- but this
// gate PROMOTES the concept first, and that does. The skip therefore keys off
// the promote, exactly as the training gate's does.
func skipUnlessStageable(t *testing.T, res *authoring.StageResult, err error) {
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
		t.Skipf("MEMQL_E2E_TOKEN cannot author on this cluster: %s", msg)
	}
}

// stayNotCallable is waitCallable's inverse, and the reason it POLLS rather than
// checking once is the whole of its value. A single check immediately after a
// stage would pass against a build that broadcasts, because the broadcast has
// not arrived yet -- it would be asserting latency, not absence. Holding the
// assertion open for a window comfortably longer than the training gate's
// observed propagation makes it a claim about the tier.
func stayNotCallable(ctx context.Context, conn *memqlclient.Connection, name string, forDuration time.Duration) error {
	deadline := time.Now().Add(forDuration)
	for time.Now().Before(deadline) {
		if err := invokeQuery(ctx, conn, name); err == nil {
			return fmt.Errorf("resolved on the other replica after %s -- a staged construct must not "+
				"propagate; something published an authoring broadcast for it",
				forDuration-time.Until(deadline))
		}
		time.Sleep(2 * time.Second)
	}
	return nil
}

// catalogOrigin reports the origin the catalog gives for a construct, and
// whether it appeared at all.
//
// THE CATALOG IS THE INSTRUMENT THAT CAN TELL THE TWO TIERS APART, and
// callability is not. Every node re-hydrates the persisted rows at boot, so
// after a restart a staged construct is callable by its author on EVERY
// replica -- owner-scoped means per-OWNER, not per-node. What distinguishes it
// from a shared one, with a single identity to test from, is `origin`: "staged"
// if the boot walk routed the row owner-scoped, "promoted" if it routed it into
// the shared registry, which is the failure the restart gate below exists for.
//
// "" with found=false is the honest answer for a construct this connection's
// catalog does not carry.
func catalogOrigin(ctx context.Context, t *testing.T, conn *memqlclient.Connection, kind, name string) (string, bool) {
	t.Helper()
	entry, found, err := constructs.NewClient(conn.Dispatcher()).Find(ctx, kind, name)
	if err != nil {
		t.Fatalf("list constructs: %v", err)
	}
	if !found {
		return "", false
	}
	return entry.Origin, true
}

// --- the gate ----------------------------------------------------------------

// TestStagedConstructIsOwnerScopedThenTrainsAcrossTheMesh is the tier's life in
// one sequence: staged (mine alone), then trained (everyone's). Each step's
// precondition is the previous step's effect, so they cannot be split without
// re-promoting a concept whose name is claimed cluster-wide.
func TestStagedConstructIsOwnerScopedThenTrainsAcrossTheMesh(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	pinned := false
	if addrB := os.Getenv("MEMQL_E2E_ENDPOINT_B"); addrB != "" {
		connB, err := memqlclient.Connect(ctx, memqlclient.ConnectConfig{Endpoint: addrB, Token: tok})
		if err != nil {
			t.Fatalf("connect the second replica at %s: %v", addrB, err)
		}
		conns = append(conns, connB)
		pinned = true
		t.Logf("reader pinned to a second replica at %s -- the absence assertion is meaningful", addrB)
	} else {
		conns = append(conns, openConnections(ctx, t, tok, 1)...)
		t.Log("MEMQL_E2E_ENDPOINT_B unset: the reader may share a replica with the stager, so the " +
			"NEGATIVE half of this gate is skipped -- 'not callable on the other replica' asserted " +
			"against the same replica is not a claim about the mesh")
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	author := authoring.NewClient(conns[0].Dispatcher())
	f := newTrainingFixture()
	t.Logf("staging the verbs bound to %s on the live cluster", f.conceptId)

	// --- 1. a concept cannot be staged, and the refusal names it ------------
	//
	// BEFORE anything else, so a build that accepted it would be caught here
	// rather than by a confusing failure four steps later.
	refused, rerr := author.StageBundle(ctx, f.conceptSrc())
	skipUnlessStageable(t, refused, rerr)
	msg := ""
	if rerr != nil {
		msg = rerr.Error()
	} else if refused != nil {
		msg = refused.Error
	}
	if (rerr == nil && refused != nil && refused.OK) || msg == "" {
		t.Fatal("staging a concept was accepted; there is no owner-scoped form of a concept to stage")
	}
	if !strings.Contains(msg, f.concept) {
		t.Errorf("the concept refusal does not name the concept, so an author cannot act on it: %s", msg)
	}

	// --- 2. train the concept, so the staged verbs have something to bind to
	promoted, err := author.DurablePromoteBundle(ctx, f.conceptSrc())
	skipUnlessOwner(t, promoted, err)
	if err != nil {
		t.Fatalf("promote the concept: %v", err)
	}
	if !promoted.OK {
		t.Fatalf("promote the concept refused: %s", promoted.Error)
	}
	defer withdraw(ctx, t, author, f)

	// --- 3. stage the verbs -------------------------------------------------
	staged, err := author.StageBundle(ctx, stagedBundle(f))
	if err != nil {
		t.Fatalf("stage the verbs: %v", err)
	}
	if !staged.OK {
		t.Fatalf("stage refused: %s", staged.Error)
	}
	if len(staged.Staged) != 2 {
		t.Fatalf("staged %d construct(s), want the mutation and the query: %+v", len(staged.Staged), staged.Staged)
	}

	// --- 4. callable BY ITS AUTHOR, on the node it was staged against -------
	//
	// Immediately, with no wait: the registration is in-process on this node and
	// there is nothing to propagate. A poll here would hide a stage that only
	// worked because something else eventually published it.
	if err := invokeQuery(ctx, conns[0], f.query); err != nil {
		t.Fatalf("a staged construct is not callable by its own author on the node it was staged against: %v", err)
	}
	if origin, found := catalogOrigin(ctx, t, conns[0], "query", f.query); !found || origin != constructs.OriginStaged {
		t.Errorf("the author's catalog reports (found=%v origin=%q) for the staged query; want origin \"staged\" -- "+
			"the editor renders a read-only sealed construct for anything it reads as core", found, origin)
	}

	// --- 5. and NOT callable on the other replica ---------------------------
	if pinned {
		if err := stayNotCallable(ctx, conns[1], f.query, 20*time.Second); err != nil {
			t.Fatalf("the staged construct crossed the mesh: %v", err)
		}
		if _, found := catalogOrigin(ctx, t, conns[1], "query", f.query); found {
			t.Error("the other replica's catalog carries the staged construct -- it holds no registration " +
				"for it and must not report one")
		}
	}

	// --- 6. training it makes it everyone's, within seconds -----------------
	//
	// The SAME source through durablePromoteBundle: there is no train message,
	// because training a staged construct IS a promote. The engine flips the
	// persisted row rather than writing a second one.
	trained, err := author.DurablePromoteBundle(ctx, stagedBundle(f))
	if err != nil {
		t.Fatalf("train the staged constructs: %v", err)
	}
	if !trained.OK {
		t.Fatalf("training the staged constructs was refused: %s", trained.Error)
	}
	if err := waitCallable(ctx, conns[1], f.query, 45*time.Second); err != nil {
		t.Fatalf("a TRAINED construct never became callable on the second connection, so the "+
			"transition did not fire the broadcast: %v", err)
	}
	if origin, found := catalogOrigin(ctx, t, conns[0], "query", f.query); !found || origin != constructs.OriginPromoted {
		t.Errorf("after training, the catalog reports (found=%v origin=%q); want \"promoted\" -- "+
			"a construct left reading as staged offers Train on something already trained", found, origin)
	}

	// --- 7. rows write and read across the mesh -----------------------------
	if err := invokeMutation(ctx, conns[0], f.mutation, map[string]string{
		"widgetId": f.rowIdShort,
		"label":    "staged-then-trained",
	}); err != nil {
		t.Fatalf("write a row through the trained mutation: %v", err)
	}
	if err := invokeQuery(ctx, conns[1], f.query); err != nil {
		t.Fatalf("read the row from the second connection: %v", err)
	}

	// Withdraw the verbs; the deferred withdraw takes the concept.
	if res, derr := author.DurableDemoteBundle(ctx, stagedBundle(f)); derr != nil {
		t.Logf("cleanup: demote the verbs: %v", derr)
	} else if !res.OK {
		t.Logf("cleanup: demote the verbs not ok: %s", res.Error)
	}
}

// stagedRestartFixtureEnv names the environment variable that carries a staged
// fixture across a cluster restart. See the test below for why a restart gate
// has to be two invocations.
const stagedRestartFixtureEnv = "MEMQL_E2E_STAGED_FIXTURE"

// TestStagedConstructComesBackOwnerScopedAfterARestart is the boot re-hydration
// assertion, and it is TWO INVOCATIONS because a restart is.
//
// WHAT IT GUARDS. The boot walk reads each persisted row's status and routes it
// three ways: retired skips, staged registers OWNER-SCOPED, active promotes to
// the shared registry. Get that route wrong -- route staged to the shared branch
// -- and every private construct on the cluster becomes public at the next
// restart, silently, in a way no author would think to check. That is the one
// failure this tier can have that nobody notices until it has already happened,
// and it is unreachable from in-process code: the walk runs at boot, against
// rows a previous process wrote.
//
// HOW TO RUN IT:
//
//	# 1. leaves a construct staged on purpose and prints the suffix
//	MEMQL_E2E_TOKEN=... bash scripts/test/cluster-e2e.sh --no-build \
//	  -run TestStagedConstructComesBackOwnerScopedAfterARestart
//
//	# 2. restart the mesh (the bff replicas re-run the boot walk)
//	kubectl rollout restart -n memql deploy/bff && kubectl rollout status -n memql deploy/bff
//
//	# 3. re-run with the suffix it printed
//	MEMQL_E2E_STAGED_FIXTURE=<suffix> MEMQL_E2E_TOKEN=... bash ... -run TestStaged...
//
// The first invocation SKIPS after seeding rather than passing, because it has
// asserted nothing about a restart and a pass would say it had.
func TestStagedConstructComesBackOwnerScopedAfterARestart(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, 1)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	author := authoring.NewClient(conns[0].Dispatcher())

	suffix := os.Getenv(stagedRestartFixtureEnv)
	if suffix == "" {
		// PHASE ONE: seed and stop.
		f := newTrainingFixture()
		promoted, err := author.DurablePromoteBundle(ctx, f.conceptSrc())
		skipUnlessOwner(t, promoted, err)
		if err != nil {
			t.Fatalf("promote the concept: %v", err)
		}
		if !promoted.OK {
			t.Fatalf("promote the concept refused: %s", promoted.Error)
		}
		staged, serr := author.StageBundle(ctx, stagedBundle(f))
		if serr != nil {
			t.Fatalf("stage the verbs: %v", serr)
		}
		if !staged.OK {
			t.Fatalf("stage refused: %s", staged.Error)
		}
		// DELIBERATELY NOT WITHDRAWN. The whole point is that these rows outlive
		// this process; a cleanup here would remove the evidence phase two reads.
		t.Skipf("seeded %s staged on the live cluster. Now restart the mesh "+
			"(kubectl rollout restart -n memql deploy/bff) and re-run with %s=%s",
			f.query, stagedRestartFixtureEnv, fixtureSuffix(f))
	}

	// PHASE TWO: the same fixture, a restarted cluster.
	f := trainingFixtureFromSuffix(suffix)
	defer withdraw(ctx, t, author, f)

	// Callable for its author: the row came back and registered.
	if err := waitCallable(ctx, conns[0], f.query, 30*time.Second); err != nil {
		t.Fatalf("the staged construct did not come back after the restart -- the boot walk either "+
			"skipped its row or failed to recompile it: %v", err)
	}
	// And came back STAGED, not shared. This is the assertion the whole
	// two-phase shape exists for: a boot walk that routed the row to the shared
	// branch would satisfy the line above and fail this one.
	if origin, found := catalogOrigin(ctx, t, conns[0], "query", f.query); !found || origin != constructs.OriginStaged {
		t.Fatalf("after the restart the catalog reports (found=%v origin=%q); want \"staged\" -- "+
			"a staged row that comes back PROMOTED has made a private construct public on every "+
			"node, which is the failure this gate exists for", found, origin)
	}
	if addrB := os.Getenv("MEMQL_E2E_ENDPOINT_B"); addrB != "" {
		connB, err := memqlclient.Connect(ctx, memqlclient.ConnectConfig{Endpoint: addrB, Token: tok})
		if err != nil {
			t.Fatalf("connect the second replica at %s: %v", addrB, err)
		}
		defer connB.Close()
		// THE OTHER REPLICA RAN THE SAME BOOT WALK, so the expectation here is
		// the OPPOSITE of the pre-restart one and the difference is worth
		// stating: before a restart the construct is absent on B because nothing
		// propagated it; after one it is PRESENT on B because B re-hydrated the
		// same row from the same database. Owner-scoped means per-OWNER, not
		// per-node.
		//
		// So what is asserted is the tier, not the absence: B must report it
		// staged. B reporting it PROMOTED is the whole failure -- a private
		// construct made public on every node by a restart.
		origin, found := catalogOrigin(ctx, t, connB, "query", f.query)
		if !found {
			t.Error("the other replica did not re-hydrate the staged row at all; a staged construct " +
				"is durable on the CLUSTER, not on the node it was staged against")
		} else if origin != constructs.OriginStaged {
			t.Errorf("the other replica reports origin %q; want %q -- it routed a staged row into "+
				"the shared registry, which makes a private construct public cluster-wide", origin, constructs.OriginStaged)
		}
	} else {
		t.Log("MEMQL_E2E_ENDPOINT_B unset: the cross-replica half of the restart assertion is skipped")
	}

	// Cleanup: the verbs, then the deferred withdraw for the concept.
	if res, derr := author.DurableDemoteBundle(ctx, stagedBundle(f)); derr != nil {
		t.Logf("cleanup: demote the verbs: %v", derr)
	} else if !res.OK {
		t.Logf("cleanup: demote the verbs not ok: %s", res.Error)
	}
}

// fixtureSuffix recovers the per-run suffix from a fixture, so phase one can
// print what phase two needs. It reads the NAMESPACE rather than storing a
// field, because the namespace is where the suffix is least ambiguous -- it is
// the whole of the name after the fixed prefix.
func fixtureSuffix(f trainingFixture) string {
	return strings.TrimPrefix(f.namespace, "e2etrain")
}

// trainingFixtureFromSuffix rebuilds a fixture from its suffix. It MUST derive
// every name exactly as newTrainingFixture does, or phase two would look for a
// construct phase one never staged and report a re-hydration failure that never
// happened -- so the two share this arithmetic rather than restating it.
func trainingFixtureFromSuffix(suffix string) trainingFixture {
	ns := "e2etrain" + suffix
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

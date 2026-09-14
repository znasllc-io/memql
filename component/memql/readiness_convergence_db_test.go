package memql

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/memql/readiness"
)

// EVERY CURRENT ROW CONVERGES AFTER SETUP AND REVOCATION, WITHOUT A RESTART,
// AND A LAGGING ROW NEVER READS AS A FRESH `unconfigured` (memql#5259).
//
// Two nodes share one database, the way every replica does. The AGENT holds
// the machine's stream and writes its registration, so it always hears its
// own event; the EDGE is a node nobody dials and hears nothing at all -- the
// shape of the seven stale rows in the issue. Everything below runs through the
// real evaluator, the real @serverOnly mutation, the real standing read and
// the real fold; only the fleet itself is supplied, because the `ai` verdict
// reads EVERY registration in the database and another package's test running
// beside this one may hold a qualifying machine.
//
// What it pins, in order:
//
//  1. Pairing seen by the agent alone: the verdict is `configured` at once and
//     the edge's boot row is named STALE -- not folded into `partial`.
//  2. The edge's own next pass (the safety net, with no event) converges its
//     row, and nothing is stale.
//  3. Revocation seen by the agent alone: the verdict is `unconfigured` at
//     once, even though the edge's row still counts the machine.
//  4. The edge converges again, and a failed fleet read on it afterwards is
//     `unknown` in the pass and is never written over its known row.
func TestEveryRowConvergesAfterSetupAndRevocationWithoutARestart(t *testing.T) {
	e, db, _ := sharedReadMergeEngine(t)
	ctx := context.Background()
	manifest, err := envregistry.LoadManifest("")
	if err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	agent := fmt.Sprintf("readiness-converge-agent-%d", suffix)
	edge := fmt.Sprintf("readiness-converge-edge-%d", suffix)
	cleanupReadinessRows(t, db, agent, edge)

	fleet := &switchableFleet{}
	resolvers := e.readinessResolvers()
	resolvers.Registrations = fleet.read
	pass := func(nodeId, nodeType string, mem *readinessMemory, at time.Time) (int, error) {
		return writeModuleReadiness(ctx, resolvers, manifest.Modules, nodeId, nodeType, mem,
			e.standingReadiness(ctx, nodeId), e.readinessExecutor(ctx), at)
	}
	memAgent, memEdge := &readinessMemory{}, &readinessMemory{}
	t0 := time.Now().UTC().Truncate(time.Second)

	// BOOT: both evaluate before any machine exists, and agree.
	mustPass(t, "agent boot", pass, agent, "agent", memAgent, t0)
	mustPass(t, "edge boot", pass, edge, "edge", memEdge, t0)
	assertAI(t, "after boot", e, t0.Add(time.Second), []string{agent, edge}, readiness.Unconfigured, nil)

	// 1. A MACHINE IS PAIRED. The agent wrote the registration and hears its
	// own event; the edge hears nothing.
	fleet.set(true, false)
	mustPass(t, "agent after pairing", pass, agent, "agent", memAgent, t0.Add(5*time.Second))
	assertAI(t, "the pairing seen by one node", e, t0.Add(6*time.Second), []string{agent, edge}, readiness.Configured, []string{edge})

	// 2. THE EDGE'S SAFETY-NET PASS, with no event and no restart.
	mustPass(t, "edge safety net", pass, edge, "edge", memEdge, t0.Add(10*time.Second))
	assertAI(t, "after the edge converged", e, t0.Add(11*time.Second), []string{agent, edge}, readiness.Configured, nil)

	// 3. THE MACHINE IS REVOKED, again seen by the agent alone. The freshest
	// fact decides: the edge's row still counts the machine and is stale.
	fleet.set(true, true)
	mustPass(t, "agent after revocation", pass, agent, "agent", memAgent, t0.Add(20*time.Second))
	assertAI(t, "the revocation seen by one node", e, t0.Add(21*time.Second), []string{agent, edge}, readiness.Unconfigured, []string{edge})

	// 4. The edge converges; then its fleet read BREAKS, and the pass is
	// unknown without touching the known row.
	mustPass(t, "edge safety net after revocation", pass, edge, "edge", memEdge, t0.Add(30*time.Second))
	assertAI(t, "after the edge converged again", e, t0.Add(31*time.Second), []string{agent, edge}, readiness.Unconfigured, nil)

	fleet.fail(true)
	if _, err := pass(edge, "edge", &readinessMemory{}, t0.Add(40*time.Second)); !IsReadinessUnknown(err) {
		t.Fatalf("a failed fleet read on the edge answered %v, want a ReadinessUnknownError", err)
	}
	row := readinessRowFor(t, e, edge, "ai")
	if row.State != readiness.Unconfigured || !row.ReportedAt.Equal(t0.Add(30*time.Second)) || row.Reason != "" {
		t.Fatalf("the edge's known ai row moved under a failed read: %+v", row)
	}
}

// THE LOOP ITSELF CONVERGES A NODE THAT HEARS NOTHING, THROUGH A FAILED WRITE
// (memql#5259: "include ... a failed readiness write"). The edge's recompute
// loop runs the production retry and safety-net logic at test speed around the
// real write path; its first write is refused the way the 2026-09-13 rollout
// refused it, and no event is ever delivered to it. Its row must still arrive
// at the agent's verdict.
func TestTheRecomputeLoopConvergesANodeThatHearsNothing(t *testing.T) {
	e, db, _ := sharedReadMergeEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manifest, err := envregistry.LoadManifest("")
	if err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	agent := fmt.Sprintf("readiness-loop-agent-%d", suffix)
	edge := fmt.Sprintf("readiness-loop-edge-%d", suffix)
	cleanupReadinessRows(t, db, agent, edge)

	fleet := &switchableFleet{}
	resolvers := e.readinessResolvers()
	resolvers.Registrations = fleet.read
	realExec := e.readinessExecutor(ctx)
	var refused atomic.Int32
	edgeWrite := func(ctx context.Context) (int, error) {
		exec := func(ctx context.Context, call string) error {
			// The first write the edge attempts is refused, as a saturated
			// database refused it; every later one lands.
			if refused.Add(1) == 1 {
				return errors.New("FATAL: remaining connection slots are reserved (SQLSTATE 53300)")
			}
			return realExec(ctx, call)
		}
		return writeModuleReadiness(ctx, resolvers, manifest.Modules, edge, "edge", &readinessMemory{},
			e.standingReadiness(ctx, edge), exec, time.Now().UTC())
	}

	// The edge boots and writes its (unconfigured) rows through the loop --
	// its first attempt refused, the retry landing.
	sub := newReadinessRecomputeSubscriberFor(edgeWrite, readinessTimings{
		Debounce:  20 * time.Millisecond,
		RetryBase: 100 * time.Millisecond,
		RetryMax:  200 * time.Millisecond,
		SafetyNet: 1500 * time.Millisecond,
	})
	sub.Start(ctx)
	sub.Notify("boot")
	waitFor(t, 10*time.Second, "the edge's boot rows, through a refused first write", func() bool {
		r, ok := tryReadinessRowFor(e, edge, "ai")
		return ok && r.State == readiness.Unconfigured
	})
	if refused.Load() < 2 {
		t.Fatalf("the refused write was not retried: %d attempts", refused.Load())
	}

	// A machine is paired. ONLY the agent writes; the edge's loop receives no
	// Notify at all.
	fleet.set(true, false)
	if _, err := writeModuleReadiness(ctx, resolvers, manifest.Modules, agent, "agent", &readinessMemory{},
		e.standingReadiness(ctx, agent), realExec, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the edge's row converging on its safety net, with no event", func() bool {
		r, ok := tryReadinessRowFor(e, edge, "ai")
		return ok && r.State == readiness.Configured
	})
	now := time.Now().UTC().Add(time.Second)
	assertAI(t, "after the loop converged", e, now, []string{agent, edge}, readiness.Configured, nil)
}

// switchableFleet is the one machine these tests pair and revoke.
type switchableFleet struct {
	mu                       sync.Mutex
	paired, revoked, failing bool
}

func (f *switchableFleet) set(paired, revoked bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paired, f.revoked = paired, revoked
}

func (f *switchableFleet) fail(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = on
}

func (f *switchableFleet) read(context.Context) ([]readiness.RegistrationFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		return nil, errors.New("read tcp: i/o timeout")
	}
	if !f.paired {
		return nil, nil
	}
	machine := readiness.RegistrationFacts{
		Labels:          map[string]string{"model:llama3.1:8b": "ctx=8192,structured=true"},
		ConnectedNodeId: "agent-with-the-stream",
		LastSeenAt:      time.Now().UTC(),
	}
	if f.revoked {
		machine.RevokedAt = time.Now().UTC()
	}
	return []readiness.RegistrationFacts{machine}, nil
}

func mustPass(t *testing.T, what string, pass func(string, string, *readinessMemory, time.Time) (int, error), nodeId, nodeType string, mem *readinessMemory, at time.Time) {
	t.Helper()
	if _, err := pass(nodeId, nodeType, mem, at); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// assertAI folds the `ai` rows of exactly these nodes -- every other node's
// rows in a shared database are some other test's -- with both nodes live.
func assertAI(t *testing.T, when string, e *MemQLEngine, now time.Time, nodes []string, want readiness.State, wantStale []string) {
	t.Helper()
	var reports []readiness.NodeReport
	var live []readiness.NodeLiveness
	for _, id := range nodes {
		reports = append(reports, readinessRowFor(t, e, id, "ai"))
		live = append(live, readiness.NodeLiveness{NodeId: id, Health: "healthy", LastSeen: now})
	}
	verdicts := readiness.Fold(reports, live, now)
	if len(verdicts) != 1 {
		t.Fatalf("%s: %d verdicts for one module", when, len(verdicts))
	}
	v := verdicts[0]
	if wantStale == nil {
		wantStale = []string{}
	}
	if v.State != want || fmt.Sprint(v.Stale) != fmt.Sprint(wantStale) || len(v.Disagreement) != 0 {
		t.Fatalf("%s: ai is %s, stale %v, disagreement %v; want %s, stale %v, no disagreement",
			when, v.State, v.Stale, v.Disagreement, want, wantStale)
	}
}

func readinessRowFor(t *testing.T, e *MemQLEngine, nodeId, module string) readiness.NodeReport {
	t.Helper()
	r, ok := tryReadinessRowFor(e, nodeId, module)
	if !ok {
		t.Fatalf("no %s row for %s", module, nodeId)
	}
	return r
}

func tryReadinessRowFor(e *MemQLEngine, nodeId, module string) (readiness.NodeReport, bool) {
	rows, err := e.readModuleReadinessRows(readerContext())
	if err != nil {
		return readiness.NodeReport{}, false
	}
	for _, r := range rows {
		if r.NodeId == nodeId && r.Module == module {
			return r, true
		}
	}
	return readiness.NodeReport{}, false
}

// cleanupReadinessRows removes every version these tests wrote, scoped by
// concept AND by the run's unique node ids -- never by key name alone.
func cleanupReadinessRows(t *testing.T, db *bun.DB, nodeIds ...string) {
	t.Helper()
	t.Cleanup(func() {
		_, err := db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).
			Where("concept = ?", ModuleReadinessConcept).
			Where("payload->>'nodeId' IN (?)", bun.In(nodeIds)).
			Exec(context.Background())
		if err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
}

package planner

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// memql#1389 -- the stranded-plan watchdog. A v1:planner:plan only starts
// via the plan-created event (PlannerAgentLoop.HandlePlanCreated); if that
// single event is lost (mesh drop / replica crash) the plan strands in
// planning/queued forever. The watchdog detects such plans (older than the
// strand threshold, in a pre-dispatch status) and re-drives them through
// the agent loop -- cross-replica-claimed so exactly one replica recovers.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

// --- kind gating -----------------------------------------------------------

func TestWatchdogRecoverableKind(t *testing.T) {
	// Dispatcher-owned + non-agent-loop kinds must NOT be re-driven.
	for _, k := range []string{"trainSpecialist", "embedDomainItems", "adHocAction", "scopeElevation", "agentInvocation"} {
		if watchdogRecoverableKind(k) {
			t.Fatalf("kind %q must be excluded from watchdog recovery", k)
		}
	}
	// Agent-loop kinds (and the empty/unknown default) ARE recoverable.
	for _, k := range []string{"userGoal", "produceArtifact", "refineAnalysis", ""} {
		if !watchdogRecoverableKind(k) {
			t.Fatalf("kind %q must be recoverable by the watchdog", k)
		}
	}
}

// --- age detection ---------------------------------------------------------

func TestWatchdogIsStranded(t *testing.T) {
	w := &StrandedPlanWatchdog{cfg: watchdogConfig{threshold: 5 * time.Minute}}
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	old := now.Add(-6 * time.Minute).Format(time.RFC3339)
	if !w.isStranded(map[string]any{"createdAt": old}, now) {
		t.Fatal("a plan older than the threshold must be stranded")
	}

	fresh := now.Add(-1 * time.Minute).Format(time.RFC3339)
	if w.isStranded(map[string]any{"createdAt": fresh}, now) {
		t.Fatal("a plan younger than the threshold must NOT be stranded")
	}

	// Exactly at the threshold counts as stranded (>= comparison).
	at := now.Add(-5 * time.Minute).Format(time.RFC3339)
	if !w.isStranded(map[string]any{"createdAt": at}, now) {
		t.Fatal("a plan exactly at the threshold must be stranded")
	}

	// Missing / unparseable createdAt is treated as NOT stranded (don't
	// risk re-driving a brand-new row we can't date).
	if w.isStranded(map[string]any{}, now) {
		t.Fatal("a missing createdAt must NOT be treated as stranded")
	}
	if w.isStranded(map[string]any{"createdAt": "not-a-time"}, now) {
		t.Fatal("an unparseable createdAt must NOT be treated as stranded")
	}
}

// candidatePlanRow builds a queryStrandedCandidatePlans / queryPlanById
// response in the flat shape MaterializeRows reads (rows under `output`).
func candidatePlanRows(rows ...map[string]any) any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return map[string]any{"output": out}
}

func strandedRow(planId, kind, createdAt string) map[string]any {
	return map[string]any{
		"id":           planId,
		"kind":         kind,
		"status":       "queued",
		"goal":         "make me a list of 10 birds",
		"spaceId":      "v1:cognition:space:s1",
		"ownerAgentId": "v1:agents:agent:a1",
		"requestedBy":  "v1:identity:user:u1",
		"createdAt":    createdAt,
	}
}

// watchdogEngine answers the candidate query with the given rows and routes
// every other query (queryPlanById during recovery, the InvokeAI gate, the
// mutations) through the shared fakeEngine so recovery runs without panicking.
func newWatchdogEngine(candidate any) *fakeEngine {
	return &fakeEngine{
		execResponder: func(q string) (any, error) {
			switch {
			case strings.Contains(q, "queryStrandedCandidatePlans"):
				return candidate, nil
			case strings.Contains(q, "queryPlanById"):
				// Recovery re-loads the plan; hand back a queued userGoal so
				// invokeAndDispatch proceeds into triage/gate (which, with a
				// nil aiResponder, parks/fails harmlessly -- we only assert
				// that recovery was ATTEMPTED, i.e. the plan was re-loaded).
				return candidatePlanRows(map[string]any{
					"id": "v1:planner:plan:p1", "kind": "userGoal", "status": "queued",
					"goal": "g", "spaceId": "s1", "requestedBy": "u1",
				}), nil
			default:
				return nil, nil
			}
		},
	}
}

// --- recovery happy path ---------------------------------------------------

func TestWatchdogRecoversStrandedPlan(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-10 * time.Minute).Format(time.RFC3339)
	eng := newWatchdogEngine(candidatePlanRows(strandedRow("v1:planner:plan:p1", "userGoal", stale)))
	loop := NewPlannerAgentLoop(eng, quietLogger())
	w := NewStrandedPlanWatchdog(eng, loop, nil, quietLogger())

	w.pollOnce(context.Background())

	execCalls, _, _ := eng.snapshot()
	if countContains(execCalls, "queryStrandedCandidatePlans") != 1 {
		t.Fatalf("watchdog should run the candidate query once, got %d", countContains(execCalls, "queryStrandedCandidatePlans"))
	}
	// Recovery re-loads the plan via queryPlanById (proof RecoverStranded ->
	// invokeAndDispatch fired for the stranded plan).
	if countContains(execCalls, "queryPlanById") == 0 {
		t.Fatal("watchdog recovery should re-drive the plan (queryPlanById expected)")
	}
}

func TestWatchdogSkipsFreshAndExcludedKinds(t *testing.T) {
	now := time.Now().UTC()
	fresh := now.Add(-30 * time.Second).Format(time.RFC3339)
	stale := now.Add(-10 * time.Minute).Format(time.RFC3339)
	eng := newWatchdogEngine(candidatePlanRows(
		strandedRow("v1:planner:plan:fresh", "userGoal", fresh),         // too young
		strandedRow("v1:planner:plan:train", "trainSpecialist", stale),  // excluded kind
		strandedRow("v1:planner:plan:embed", "embedDomainItems", stale), // excluded kind
	))
	loop := NewPlannerAgentLoop(eng, quietLogger())
	w := NewStrandedPlanWatchdog(eng, loop, nil, quietLogger())

	w.pollOnce(context.Background())

	execCalls, _, _ := eng.snapshot()
	if countContains(execCalls, "queryPlanById") != 0 {
		t.Fatalf("no recovery should fire for fresh / excluded-kind plans, got %d queryPlanById", countContains(execCalls, "queryPlanById"))
	}
}

// --- in-process idempotency (the per-plan once-guard, memql#1155) ----------

func TestWatchdogRecoverStrandedIsIdempotentPerReplica(t *testing.T) {
	eng := newWatchdogEngine(nil)
	loop := NewPlannerAgentLoop(eng, quietLogger())

	if !loop.RecoverStranded(context.Background(), "v1:planner:plan:p1", "userGoal") {
		t.Fatal("first RecoverStranded should attempt recovery")
	}
	// Second call for the same plan is collapsed by the once-guard.
	if loop.RecoverStranded(context.Background(), "v1:planner:plan:p1", "userGoal") {
		t.Fatal("second RecoverStranded for the same plan must be a no-op (once-guard)")
	}
}

// --- cross-replica safety (the cluster is the default runtime) -------------
//
// Two watchdogs (two planner replicas) share ONE firstWinsClaimer, mirroring
// the DB-PK ClusterExecutionGuard (memql#1363). The candidate query reaches
// both replicas; without the claim both re-drive the same stranded plan and
// feed the duplicate-dispatch storm. With it, exactly ONE replica recovers.

func TestWatchdogCrossReplicaRecoversExactlyOnce(t *testing.T) {
	now := time.Now().UTC()
	stale := now.Add(-10 * time.Minute).Format(time.RFC3339)
	rows := candidatePlanRows(strandedRow("v1:planner:plan:p1", "userGoal", stale))

	claimer := &firstWinsClaimer{}

	type replica struct {
		eng  *fakeEngine
		loop *PlannerAgentLoop
		wd   *StrandedPlanWatchdog
	}
	mk := func() replica {
		eng := newWatchdogEngine(rows)
		loop := NewPlannerAgentLoop(eng, quietLogger())
		return replica{eng, loop, NewStrandedPlanWatchdog(eng, loop, claimer, quietLogger())}
	}
	a, b := mk(), mk()

	// Poll both replicas concurrently against the shared claim ledger.
	var wg sync.WaitGroup
	for _, r := range []replica{a, b} {
		wg.Add(1)
		go func(r replica) { defer wg.Done(); r.wd.pollOnce(context.Background()) }(r)
	}
	wg.Wait()

	aCalls, _, _ := a.eng.snapshot()
	bCalls, _, _ := b.eng.snapshot()
	// Count REPLICAS that recovered (re-loaded the plan), not total calls --
	// the winning replica's invokeAndDispatch may queryPlanById more than once.
	recoveredReplicas := 0
	if countContains(aCalls, "queryPlanById") > 0 {
		recoveredReplicas++
	}
	if countContains(bCalls, "queryPlanById") > 0 {
		recoveredReplicas++
	}
	if recoveredReplicas == 0 {
		t.Fatal("at least one replica must recover the stranded plan")
	}
	// Exactly one replica wins the claim and re-drives; the loser is gated
	// before any re-drive.
	if recoveredReplicas > 1 {
		t.Fatalf("the cluster claim must collapse recovery to ONE replica, got %d replicas re-driving", recoveredReplicas)
	}
	// Both replicas attempted the claim (both saw the candidate).
	if claimer.callCount() != 2 {
		t.Fatalf("both replicas should attempt the claim, got %d", claimer.callCount())
	}
}

// --- config -----------------------------------------------------------------

func TestWatchdogConfigDefaultsOn(t *testing.T) {
	cfg := loadWatchdogConfig()
	if !cfg.enabled {
		t.Fatal("watchdog must default ON (data-loss safety net)")
	}
	if cfg.threshold != time.Duration(defaultStrandThresholdSeconds)*time.Second {
		t.Fatalf("unexpected default threshold: %s", cfg.threshold)
	}
	if cfg.poll != time.Duration(defaultWatchdogPollSeconds)*time.Second {
		t.Fatalf("unexpected default poll: %s", cfg.poll)
	}
}

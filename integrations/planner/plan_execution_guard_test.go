package planner

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/integrations"
)

// memql#1363 -- cross-replica plan-execution claim + terminal-status
// protection. The plan-approved event reaches EVERY planner replica; without
// the cluster claim each replica dispatches a full agent turn and the losing
// replica's empty duplicate turn overwrites the winner's succeeded with
// FAILED. These tests cover both layers at the planner seam.

// --- claim key -------------------------------------------------------------

func TestPlanExecutionClaimKey(t *testing.T) {
	planId := "v1:planner:plan:868acfa1"

	// One running spell -> one key, identical on every replica (both read
	// the same row, so the same startedAt).
	k1 := planExecutionClaimKey(planId, "2026-06-12T00:11:49Z")
	k2 := planExecutionClaimKey(planId, "2026-06-12T00:11:49Z")
	if k1 != k2 {
		t.Fatalf("same (planId, startedAt) must derive the same key: %q vs %q", k1, k2)
	}

	// A legitimate re-run re-stamps startedAt -> fresh key (the memql#800
	// release contract: later re-runs must stay possible).
	k3 := planExecutionClaimKey(planId, "2026-06-12T00:30:00Z")
	if k3 == k1 {
		t.Fatalf("a re-run with a fresh startedAt must claim under a fresh key")
	}

	// Defensive fallback: empty startedAt claims on the bare plan id.
	if got := planExecutionClaimKey(planId, ""); got != planId {
		t.Fatalf("empty startedAt should fall back to the plan id, got %q", got)
	}
	if got := planExecutionClaimKey(planId, "  "); got != planId {
		t.Fatalf("blank startedAt should fall back to the plan id, got %q", got)
	}
}

// --- fakes -------------------------------------------------------------------

// firstWinsClaimer mirrors the DB PK semantics of the automations
// ClusterExecutionGuard: across all callers (the "replicas"), the first
// Claim for a (name, key) wins and every subsequent one loses. Safe for
// concurrent use, exactly like the real guard's INSERT .. ON CONFLICT.
type firstWinsClaimer struct {
	mu      sync.Mutex
	claimed map[string]bool
	calls   []string
}

func (c *firstWinsClaimer) Claim(_ context.Context, name, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := name + "\x00" + key
	c.calls = append(c.calls, k)
	if c.claimed == nil {
		c.claimed = map[string]bool{}
	}
	if c.claimed[k] {
		return false
	}
	c.claimed[k] = true
	return true
}

func (c *firstWinsClaimer) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// staticClaimer always answers the same verdict.
type staticClaimer struct {
	verdict bool
	mu      sync.Mutex
	calls   int
}

func (c *staticClaimer) Claim(context.Context, string, string) bool {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.verdict
}

// fakeTurnForwarder satisfies AgentForwarder; Forward returns a pre-closed
// channel carrying one successful AgentGenerateTurnComplete so the dispatch
// runs end-to-end. Counts Forward calls (= dispatched agent turns).
type fakeTurnForwarder struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeTurnForwarder) Forward(
	_ context.Context, _ string, _ node.NodeType, _ auth.ForwardedPrincipal, _ *memqlv1.MemqlClientMessage,
) (<-chan *memqlv1.MemqlServerMessage, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	ch := make(chan *memqlv1.MemqlServerMessage, 1)
	ch <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_AgentGenerateTurnComplete{
			AgentGenerateTurnComplete: &memqlv1.AgentGenerateTurnComplete{
				FinalText: "Done -- the work landed.",
			},
		},
	}
	close(ch)
	return ch, nil
}

func (f *fakeTurnForwarder) ForwardContinuation(
	string, auth.ForwardedPrincipal, *memqlv1.MemqlClientMessage,
) error {
	return nil
}

func (f *fakeTurnForwarder) forwardCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// guardTestPlanResponder answers the engine queries executeApprovedPlan and
// the markPlan* gate issue for a scopeElevation plan in the given status.
func guardTestPlanResponder(planId, status string) func(query string) (any, error) {
	return func(query string) (any, error) {
		switch {
		case strings.Contains(query, "planById"):
			return map[string]any{"data": map[string]any{
				"id":           planId,
				"kind":         "scopeElevation",
				"status":       status,
				"goal":         "install the tool on my machine",
				"partitionId":  "v1:cognition:space:s1",
				"ownerAgentId": "v1:agents:agent:a1",
				"requestedBy":  "v1:identity:user:u1",
				"startedAt":    "2026-06-12T00:11:49Z",
			}}, nil
		case strings.Contains(query, "invocationsForPlan"):
			// One successful worker invocation -> the dispatch outcome gate
			// reports succeeded.
			return map[string]any{"data": map[string]any{"outcome": "success"}}, nil
		default:
			return nil, nil
		}
	}
}

func newGuardTestIntegration(t *testing.T, eng Engine, fwd AgentForwarder, claimer ExecutionClaimer, logger *slog.Logger) *PlannerIntegration {
	t.Helper()
	base, err := integrations.NewIntegration(ComponentName)
	if err != nil {
		t.Fatalf("base integration: %v", err)
	}
	return &PlannerIntegration{
		Integration:    base,
		engine:         eng,
		agentForwarder: fwd,
		clusterClaimer: claimer,
		logger:         logger,
	}
}

// --- layer 1: cross-replica claim -------------------------------------------

// Two concurrent executors for the same plan id (simulating the two planner
// replicas, each of which has its OWN in-process executing map -- so calling
// executeApprovedPlan directly models the cross-replica race) -> exactly one
// agent turn is dispatched and exactly one terminal stamp is written.
func TestExecuteApprovedPlan_CrossReplicaClaim_ExactlyOneExecutes(t *testing.T) {
	const planId = "v1:planner:plan:claim-race"
	eng := &fakeEngine{execResponder: guardTestPlanResponder(planId, "running")}
	fwd := &fakeTurnForwarder{}
	claimer := &firstWinsClaimer{}
	p := newGuardTestIntegration(t, eng, fwd, claimer, testLogger())

	var wg sync.WaitGroup
	for _, requestId := range []string{"req-replica-1", "req-replica-2"} {
		wg.Add(1)
		go func(rid string) {
			defer wg.Done()
			p.executeApprovedPlan(contextWithSystemActor(context.Background()), planId, rid)
		}(requestId)
	}
	wg.Wait()

	if got := claimer.callCount(); got != 2 {
		t.Fatalf("both replicas must consult the claim; got %d calls", got)
	}
	if got := fwd.forwardCount(); got != 1 {
		t.Fatalf("exactly one replica must dispatch the agent turn; got %d", got)
	}
	execCalls, _, _ := eng.snapshot()
	if got := countContains(execCalls, "updatePlanStatus"); got != 1 {
		t.Fatalf("exactly one terminal stamp must be written; got %d: %v", got, execCalls)
	}
	if got := countContains(execCalls, `status:"succeeded"`); got != 1 {
		t.Fatalf("the single stamp must be succeeded; calls: %v", execCalls)
	}
}

// The claim loser skips the dispatch ENTIRELY: no agent turn, no markPlan*,
// and the skip is WARN-logged with plan id + claim key.
func TestExecuteApprovedPlan_ClaimLoserSkipsEntirely(t *testing.T) {
	const planId = "v1:planner:plan:claim-lost"
	eng := &fakeEngine{execResponder: guardTestPlanResponder(planId, "running")}
	fwd := &fakeTurnForwarder{}
	claimer := &staticClaimer{verdict: false}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	p := newGuardTestIntegration(t, eng, fwd, claimer, logger)

	p.executeApprovedPlan(contextWithSystemActor(context.Background()), planId, "req-loser")

	if claimer.calls != 1 {
		t.Fatalf("claim must be consulted exactly once; got %d", claimer.calls)
	}
	if fwd.forwardCount() != 0 {
		t.Fatalf("the claim loser must not dispatch an agent turn")
	}
	execCalls, _, _ := eng.snapshot()
	if got := countContains(execCalls, "updatePlanStatus"); got != 0 {
		t.Fatalf("the claim loser must not stamp any plan status; got %v", execCalls)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "duplicate cross-replica dispatch prevented") {
		t.Fatalf("duplicate prevention must be WARN-logged; got: %s", logged)
	}
	if !strings.Contains(logged, planId) || !strings.Contains(logged, planExecutionClaimName) {
		t.Fatalf("the WARN must name the plan id + claim; got: %s", logged)
	}
}

// Without a wired claimer (dev / single-binary builds) the dispatch proceeds
// exactly as before -- the claim layer is strictly additive.
func TestExecuteApprovedPlan_NoClaimerFailsOpen(t *testing.T) {
	const planId = "v1:planner:plan:no-claimer"
	eng := &fakeEngine{execResponder: guardTestPlanResponder(planId, "running")}
	fwd := &fakeTurnForwarder{}
	p := newGuardTestIntegration(t, eng, fwd, nil, testLogger())

	p.executeApprovedPlan(contextWithSystemActor(context.Background()), planId, "req-1")

	if fwd.forwardCount() != 1 {
		t.Fatalf("dispatch must proceed without a claimer; got %d forwards", fwd.forwardCount())
	}
}

// --- layer 2: terminal-status protection -------------------------------------

// A late markPlanFailed must never overwrite succeeded (the exact memql#1363
// corruption: the losing replica's empty duplicate turn stamping FAILED over
// the winner's real result), and the refusal is WARN-logged with both
// statuses.
func TestMarkPlanFailed_RefusesOverwriteSucceeded(t *testing.T) {
	const planId = "v1:planner:plan:protect-1"
	eng := &fakeEngine{execResponder: guardTestPlanResponder(planId, "succeeded")}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	p := newGuardTestIntegration(t, eng, &fakeTurnForwarder{}, nil, logger)

	p.markPlanFailed(contextWithSystemActor(context.Background()), planId, "duplicate turn came back empty")

	execCalls, _, _ := eng.snapshot()
	if got := countContains(execCalls, "updatePlanStatus"); got != 0 {
		t.Fatalf("failed must NOT overwrite succeeded; got %v", execCalls)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "REFUSING to overwrite a terminal plan status") {
		t.Fatalf("the refusal must be WARN-logged; got: %s", logged)
	}
	if !strings.Contains(logged, "current_status=succeeded") || !strings.Contains(logged, "incoming_status=failed") {
		t.Fatalf("the WARN must carry both statuses; got: %s", logged)
	}
}

// The reverse direction: succeeded must not overwrite failed either.
func TestMarkPlanSucceeded_RefusesOverwriteFailed(t *testing.T) {
	const planId = "v1:planner:plan:protect-2"
	eng := &fakeEngine{execResponder: guardTestPlanResponder(planId, "failed")}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	p := newGuardTestIntegration(t, eng, &fakeTurnForwarder{}, nil, logger)

	p.markPlanSucceeded(contextWithSystemActor(context.Background()), planId, "Done.")

	execCalls, _, _ := eng.snapshot()
	if got := countContains(execCalls, "updatePlanStatus"); got != 0 {
		t.Fatalf("succeeded must NOT overwrite failed; got %v", execCalls)
	}
	if !strings.Contains(logBuf.String(), "REFUSING to overwrite a terminal plan status") {
		t.Fatalf("the refusal must be WARN-logged; got: %s", logBuf.String())
	}
}

// Neither direction may overwrite cancelled.
func TestMarkPlanTerminal_RefusesOverwriteCancelled(t *testing.T) {
	const planId = "v1:planner:plan:protect-3"
	eng := &fakeEngine{execResponder: guardTestPlanResponder(planId, "cancelled")}
	p := newGuardTestIntegration(t, eng, &fakeTurnForwarder{}, nil, testLogger())

	ctx := contextWithSystemActor(context.Background())
	p.markPlanSucceeded(ctx, planId, "Done.")
	p.markPlanFailed(ctx, planId, "boom")

	execCalls, _, _ := eng.snapshot()
	if got := countContains(execCalls, "updatePlanStatus"); got != 0 {
		t.Fatalf("cancelled must never be overwritten; got %v", execCalls)
	}
}

// A non-terminal plan stamps normally -- the protection must not block the
// legitimate completion write.
func TestMarkPlanTerminal_NonTerminalWritesThrough(t *testing.T) {
	const planId = "v1:planner:plan:protect-4"
	eng := &fakeEngine{execResponder: guardTestPlanResponder(planId, "running")}
	p := newGuardTestIntegration(t, eng, &fakeTurnForwarder{}, nil, testLogger())

	p.markPlanSucceeded(contextWithSystemActor(context.Background()), planId, "Done.")

	execCalls, _, _ := eng.snapshot()
	if got := countContains(execCalls, `status:"succeeded"`); got != 1 {
		t.Fatalf("a running plan must stamp succeeded normally; got %v", execCalls)
	}
}

// Same-status re-stamp is a documented no-op (idempotent; no duplicate
// append-only version row).
func TestMarkPlanTerminal_SameStatusIsIdempotentNoOp(t *testing.T) {
	const planId = "v1:planner:plan:protect-5"
	eng := &fakeEngine{execResponder: guardTestPlanResponder(planId, "succeeded")}
	p := newGuardTestIntegration(t, eng, &fakeTurnForwarder{}, nil, testLogger())

	p.markPlanSucceeded(contextWithSystemActor(context.Background()), planId, "Done.")

	execCalls, _, _ := eng.snapshot()
	if got := countContains(execCalls, "updatePlanStatus"); got != 0 {
		t.Fatalf("same-status re-stamp must be a no-op; got %v", execCalls)
	}
}

// The pre-#1363 out-of-band-halt contract is preserved: a paused (passed)
// plan whose turn happens to finish stays paused for resume (memql#907).
func TestMarkPlanTerminal_PausedStaysPaused(t *testing.T) {
	const planId = "v1:planner:plan:protect-6"
	eng := &fakeEngine{execResponder: guardTestPlanResponder(planId, "paused")}
	p := newGuardTestIntegration(t, eng, &fakeTurnForwarder{}, nil, testLogger())

	ctx := contextWithSystemActor(context.Background())
	p.markPlanSucceeded(ctx, planId, "Done.")
	p.markPlanFailed(ctx, planId, "boom")

	execCalls, _, _ := eng.snapshot()
	if got := countContains(execCalls, "updatePlanStatus"); got != 0 {
		t.Fatalf("a paused plan must not be stamped terminal; got %v", execCalls)
	}
}

// Best-effort contract: when the pre-write status lookup fails, the stamp
// proceeds (a lookup outage must not strand a finished turn unstamped).
func TestMarkPlanTerminal_LookupMissWritesThrough(t *testing.T) {
	const planId = "v1:planner:plan:protect-7"
	eng := &fakeEngine{execResponder: func(query string) (any, error) {
		if strings.Contains(query, "planById") {
			return nil, nil // "plan not found" -> lookup miss
		}
		return nil, nil
	}}
	p := newGuardTestIntegration(t, eng, &fakeTurnForwarder{}, nil, testLogger())

	p.markPlanFailed(contextWithSystemActor(context.Background()), planId, "boom")

	execCalls, _, _ := eng.snapshot()
	if got := countContains(execCalls, `status:"failed"`); got != 1 {
		t.Fatalf("a lookup miss must not block the stamp; got %v", execCalls)
	}
}

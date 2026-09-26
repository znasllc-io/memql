package work

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type compileObservation struct {
	request           CompileRequest
	run               common.RunContext
	actor             string
	budget, cancelled bool
	authority         auth.ForwardedAuthority
	hasAuthority      bool
}

type compileProbe struct {
	called   chan compileObservation
	proceed  chan struct{}
	finished chan error
	writer   *Integration
}

func (p *compileProbe) Compile(ctx context.Context, req CompileRequest) {
	rc, _ := common.RunFromContext(ctx)
	airoute.Observe(ctx, airoute.CallObservation{ID: "compile-probe", Phase: "completed", Provider: "fleet:test", Model: "test-model"})
	authority, hasAuthority := auth.ForwardedAuthorityFromContext(ctx)
	p.called <- compileObservation{request: req, run: rc, actor: callerUserId(ctx), budget: hasBudgetScope(ctx), cancelled: ctx.Err() != nil, authority: authority, hasAuthority: hasAuthority}
	if p.proceed != nil {
		<-p.proceed
	}
	p.finished <- p.writer.RecordCompileOutcome(ctx, req.OwnerUserId, req.RunId, map[string]any{"status": "running", "automationName": "probe"})
}

func compileDB(t *testing.T) (*bun.DB, *Integration, []*Integration, *compileProbe) {
	t.Helper()
	db, bff := sweepDB(t)
	if _, err := db.ExecContext(context.Background(), `CREATE TEMP TABLE automation_execution_claims (LIKE public.automation_execution_claims INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}
	eng := dispatchDBEngine(t, db)
	bff.engine = eng
	// BFF intake has no local Compiler; event-forward is the honest handoff
	// (memql#5268 fold). Without this, createGoal refuses with no compile surface.
	bff.EnableCompileViaEvent()
	probe := &compileProbe{called: make(chan compileObservation, 20), finished: make(chan error, 20)}
	planners := []*Integration{New(eng, testLogger()), New(eng, testLogger())}
	for _, planner := range planners {
		planner.bunDB = func() *bun.DB { return db }
		planner.admitRow = memql.AdmitSourceRow
		planner.SetCompiler(probe)
	}
	probe.writer = planners[0]
	return db, bff, planners, probe
}

func awaitCompile(t *testing.T, probe *compileProbe) compileObservation {
	t.Helper()
	select {
	case got := <-probe.called:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("persisted compiling run never reached a planner")
	}
	return compileObservation{}
}

func finishCompile(t *testing.T, probe *compileProbe) {
	t.Helper()
	select {
	case err := <-probe.finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("compiler did not record its outcome")
	}
}

func TestCompileDB_BFFRunEventCrossesToOnePlannerReplica(t *testing.T) {
	t.Setenv("MEMQL_NODE_ID", "planner-receiver")
	_, bff, planners, probe := compileDB(t)
	bus := events.NewBus()
	bff.engine.(*memql.MemQLEngine).SetEventBus(bus)
	for _, topic := range []string{"graph.node.created.v1:work:run", "graph.node.updated.v1:work:run"} {
		for _, planner := range planners {
			unsub := bus.Subscribe(topic, planner.HandleRunEvent)
			defer unsub()
		}
	}
	ctx, cancel := context.WithCancel(actorCtx("compile-alice"))
	nodes, err := bff.handleCreateGoal(ctx, map[string]any{"statement": "reconcile the invoices", "input": map[string]any{"month": "September"}, "ceilings": map[string]any{"maxModelCalls": 3}}, 0)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	reply := decodeReply(t, nodes)
	if reply["compileDispatched"] != true {
		t.Fatal("BFF with event-forward must report compileDispatched; the run graph event is the handoff")
	}
	got := awaitCompile(t, probe)
	if got.request.RunId != reply["runId"] || got.request.GoalId != reply["goalId"] || got.request.Statement != "reconcile the invoices" || got.request.Input["month"] != "September" {
		t.Fatalf("planner did not reconstruct persisted goal/run: %+v", got)
	}
	if got.actor != canonicalUser("compile-alice") || got.run.RunId != reply["runId"] || got.run.GoalId != reply["goalId"] || got.run.OwnerUserId != canonicalUser("compile-alice") || got.run.Mode != "live" || !got.budget || got.cancelled {
		t.Fatalf("compile lost actor, run, budget, or detached context across hop: %+v", got)
	}
	if !got.hasAuthority {
		t.Fatal("compilation cannot forward model calls: no persisted-owner authority")
	}
	verified, err := auth.VerifyForwardedAuthority(node.ForwardedAuthorityFromProto(node.ForwardedAuthorityToProto(got.authority, "planner", "planner")), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if verified.UserId != canonicalUser("compile-alice") || verified.Role != auth.RoleWriter || verified.Synthetic || verified.Unranked || verified.IsClusterOwner() {
		t.Fatalf("compiler forwarded the wrong authority: %+v", verified)
	}
	finishCompile(t, probe)
	observations, err := bff.engine.Execute(actorCtx("compile-alice"), "query workObservationsForOwnerRun(runId: \""+reply["runId"].(string)+"\")")
	if err != nil {
		t.Fatal(err)
	}
	rows := memql.MaterializeRows(observations.OutputPayload())
	if len(rows) != 1 || rowMap(rowMap(rows[0], "data"), "execution")["model"] != "test-model" {
		t.Fatalf("compilation model call was lost across the BFF/planner hop: %+v", rows)
	}
	// Duplicate delivery and stale events after the outcome must both be inert.
	ev := runEvent(reply["runId"].(string), "compiling", "work.compile", canonicalUser("compile-alice"))
	for range 10 {
		for _, planner := range planners {
			planner.HandleRunEvent(ev)
		}
	}
	select {
	case duplicate := <-probe.called:
		t.Fatalf("duplicate planner compile: %+v", duplicate)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCompileDB_SweepRecoversAnUnclaimedRunAfterLostEvent(t *testing.T) {
	_, bff, planners, probe := compileDB(t)
	opened := time.Now().Add(-2 * time.Minute)
	bff.SetNow(func() time.Time { return opened })
	nodes, err := bff.handleCreateGoal(actorCtx("compile-bob"), map[string]any{"statement": "analyze the report"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := planners[0].SweepWaiting(auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns")), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res.Redispatched != 1 || res.Abandoned != 0 {
		t.Fatalf("lost compile event was not recovered: %+v", res)
	}
	got := awaitCompile(t, probe)
	if got.request.RunId != decodeReply(t, nodes)["runId"] {
		t.Fatalf("wrong run recovered: %+v", got)
	}
	finishCompile(t, probe)
}

func TestCompileDB_TerminalAndCancelledEventsDoNotCompile(t *testing.T) {
	_, bff, planners, probe := compileDB(t)
	for _, status := range []string{"running", "succeeded", "failed", "cancelled", "abandoned", "waiting"} {
		nodes, err := bff.handleCreateGoal(actorCtx("compile-carol"), map[string]any{"statement": "analyze the report"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		runID := decodeReply(t, nodes)["runId"].(string)
		if err := bff.store().updateRun(actorCtx("compile-carol"), runID, map[string]any{"status": status}); err != nil {
			t.Fatal(err)
		}
		for _, planner := range planners {
			planner.dispatchCompile(context.Background(), CompileRequest{RunId: runID, OwnerUserId: canonicalUser("compile-carol")})
		}
	}
	select {
	case got := <-probe.called:
		t.Fatalf("stale event compiled a run that already moved: %+v", got)
	default:
	}
}

func TestCompileDB_LocalDispatchAndTwoPlannerReplicasShareClaim(t *testing.T) {
	_, bff, planners, probe := compileDB(t)
	nodes, err := bff.handleCreateGoal(actorCtx("compile-dave"), map[string]any{"statement": "reconcile receipts"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	reply := decodeReply(t, nodes)
	var wg sync.WaitGroup
	for range 10 {
		for _, planner := range planners {
			wg.Go(func() {
				planner.dispatchCompile(context.Background(), CompileRequest{RunId: reply["runId"].(string), OwnerUserId: canonicalUser("compile-dave")})
			})
		}
	}
	wg.Wait()
	awaitCompile(t, probe)
	finishCompile(t, probe)
	select {
	case duplicate := <-probe.called:
		t.Fatalf("local/event race compiled twice: %+v", duplicate)
	default:
	}
}

func TestCompileDB_HeartbeatKeepsLongCompileAliveWithoutLeaseTakeover(t *testing.T) {
	t.Setenv("MEMQL_NODE_ID", "planner-long-compile")
	db, bff, planners, probe := compileDB(t)
	probe.proceed = make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(probe.proceed) }) }
	defer release()
	nodes, err := bff.handleCreateGoal(actorCtx("compile-long"), map[string]any{"statement": "reconcile a large report"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	runID := decodeReply(t, nodes)["runId"].(string)
	owner := canonicalUser("compile-long")
	hint := CompileRequest{RunId: runID, OwnerUserId: owner}
	if !planners[0].dispatchCompile(context.Background(), hint) {
		t.Fatal("planner did not claim pending compile")
	}
	awaitCompile(t, probe)
	run, err := bff.store().runForOwner(actorCtx(owner), runID)
	if err != nil {
		t.Fatal(err)
	}
	initial := rowString(run, "heartbeatAt")
	if initial == "" || rowString(run, "nodeId") != "planner-long-compile" {
		t.Fatalf("planner ownership was not persisted: %+v", run)
	}
	// The lease expiring does not grant permission to restart an owned
	// compile. This tests that persisted ownership, not a local map, fences it.
	if _, err := db.ExecContext(context.Background(), `UPDATE automation_execution_claims SET claimed_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if planners[1].dispatchCompile(context.Background(), hint) {
		t.Fatal("a second planner stole an active compile after lease expiry")
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		run, err = bff.store().runForOwner(actorCtx(owner), runID)
		if err != nil {
			t.Fatal(err)
		}
		if rowString(run, "heartbeatAt") != initial {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("long compile never refreshed its heartbeat")
		}
		time.Sleep(50 * time.Millisecond)
	}
	res, err := planners[1].SweepWaiting(auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns")), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res.Abandoned != 0 || res.Redispatched != 0 {
		t.Fatalf("sweep stole/abandoned live compile: %+v", res)
	}
	release()
	finishCompile(t, probe)
	run, err = bff.store().runForOwner(actorCtx(owner), runID)
	if err != nil || rowString(run, "status") != "running" {
		t.Fatalf("heartbeat overwrote compile outcome: %+v, %v", run, err)
	}
}

func TestCompileDB_CancelDuringCompileCannotResurrectRun(t *testing.T) {
	_, bff, planners, probe := compileDB(t)
	probe.proceed = make(chan struct{})
	nodes, err := bff.handleCreateGoal(actorCtx("compile-cancel"), map[string]any{"statement": "analyze a report"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	runID := decodeReply(t, nodes)["runId"].(string)
	owner := canonicalUser("compile-cancel")
	if !planners[0].dispatchCompile(context.Background(), CompileRequest{RunId: runID, OwnerUserId: owner}) {
		t.Fatal("planner did not claim pending compile")
	}
	awaitCompile(t, probe)
	if err := bff.store().updateRun(actorCtx(owner), runID, map[string]any{"cancelRequested": true}); err != nil {
		t.Fatal(err)
	}
	close(probe.proceed)
	select {
	case err := <-probe.finished:
		if err == nil {
			t.Fatal("cancelled compile accepted a successful outcome")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("compiler did not finish")
	}
	run, err := bff.store().runForOwner(actorCtx(owner), runID)
	if err != nil || rowString(run, "status") != "cancelled" {
		t.Fatalf("compile resurrected cancelled work: %+v, %v", run, err)
	}
}

func TestCompileDB_ClaimStorageFailureNeverStartsCompiler(t *testing.T) {
	db, bff, planners, probe := compileDB(t)
	nodes, err := bff.handleCreateGoal(actorCtx("compile-storage"), map[string]any{"statement": "reconcile receipts"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `ALTER TABLE automation_execution_claims RENAME COLUMN dedup_key TO unavailable_key`); err != nil {
		t.Fatal(err)
	}
	for _, planner := range planners {
		if planner.dispatchCompile(context.Background(), CompileRequest{RunId: decodeReply(t, nodes)["runId"].(string), OwnerUserId: canonicalUser("compile-storage")}) {
			t.Fatal("compile started without a durable claim")
		}
	}
	select {
	case got := <-probe.called:
		t.Fatalf("unguarded compile: %+v", got)
	default:
	}
}

func TestCompileDB_UnclaimedRunDoesNotReportNodeLoss(t *testing.T) {
	_, bff, _, _ := compileDB(t)
	opened := time.Now().Add(-2 * time.Minute)
	bff.SetNow(func() time.Time { return opened })
	nodes, err := bff.handleCreateGoal(actorCtx("compile-no-planner"), map[string]any{"statement": "reconcile receipts"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	bff.SetNow(time.Now)
	res, err := bff.SweepWaiting(auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns")), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res.Abandoned != 1 {
		t.Fatalf("unclaimed compile was not closed: %+v", res)
	}
	run, err := bff.store().runForOwner(actorCtx("compile-no-planner"), decodeReply(t, nodes)["runId"].(string))
	if err != nil || rowString(run, "errorCode") != "compile_unclaimed" {
		t.Fatalf("pending run falsely reported a lost node: %+v, %v", run, err)
	}
}

func TestCompileDB_SweepDoesNotCloseAPlannerClaimInProgress(t *testing.T) {
	db, bff, planners, _ := compileDB(t)
	opened := time.Now().Add(-2 * time.Minute)
	bff.SetNow(func() time.Time { return opened })
	nodes, err := bff.handleCreateGoal(actorCtx("compile-claim-race"), map[string]any{"statement": "reconcile receipts"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	runID := decodeReply(t, nodes)["runId"].(string)
	bff.SetNow(time.Now)
	if !planners[0].claimCompile(context.Background(), runID) {
		t.Fatal("planner could not start its claim")
	}
	// Stop between PostgreSQL arbitration and the first ownership heartbeat.
	// A sweep on another node must not close the run in this gap.
	maintenance := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns"))
	res, err := bff.SweepWaiting(maintenance, time.Minute)
	if err != nil || res.Abandoned != 0 {
		t.Fatalf("sweep raced the planner's ownership write: %+v, %v", res, err)
	}
	// If the claimant died in that gap, expiration lets the sweep close the
	// unstarted run honestly, without leaving it pending forever.
	if _, err := db.ExecContext(context.Background(), `UPDATE automation_execution_claims SET claimed_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	res, err = bff.SweepWaiting(maintenance, time.Minute)
	if err != nil || res.Abandoned != 1 {
		t.Fatalf("dead incomplete claim never closed: %+v, %v", res, err)
	}
}

// Simulate a database operation that has stalled after compilation started.
// Every other read/write still uses the real engine and PostgreSQL.
type blockedHeartbeatEngine struct {
	Engine
	entered chan struct{}
	unblock chan struct{}
}

func (e *blockedHeartbeatEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if strings.Contains(query, "mutation updateWorkRun") && strings.Contains(query, "heartbeatAt:") && !strings.Contains(query, "nodeId:") {
		select {
		case e.entered <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-e.unblock:
			return nil, context.Canceled
		}
	}
	return e.Engine.Execute(ctx, query)
}

func TestCompileDB_OutcomeCancelsAStalledHeartbeat(t *testing.T) {
	_, bff, planners, probe := compileDB(t)
	blocked := &blockedHeartbeatEngine{Engine: planners[0].engine, entered: make(chan struct{}, 1), unblock: make(chan struct{})}
	planners[0].engine = blocked
	defer close(blocked.unblock)
	probe.proceed = make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(probe.proceed) }) }
	defer release()
	nodes, err := bff.handleCreateGoal(actorCtx("compile-blocked"), map[string]any{"statement": "reconcile receipts"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !planners[0].dispatchCompile(context.Background(), CompileRequest{RunId: decodeReply(t, nodes)["runId"].(string), OwnerUserId: canonicalUser("compile-blocked")}) {
		t.Fatal("planner did not claim compile")
	}
	awaitCompile(t, probe)
	select {
	case <-blocked.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("heartbeat never reached the database")
	}
	release()
	select {
	case err := <-probe.finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("compile outcome blocked joining a heartbeat whose database call was never cancelled")
	}
}

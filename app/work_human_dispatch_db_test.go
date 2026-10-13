//go:build agent

package app

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	permission "github.com/znasllc-io/memql/core/computerpermission"
	"github.com/znasllc-io/memql/core/id"
	work "github.com/znasllc-io/memql/integrations/work"
)

type humanReadyDispatcher struct {
	*workRunDispatcher
	admitted chan work.DispatchRequest
}

func (d *humanReadyDispatcher) Dispatch(ctx context.Context, req work.DispatchRequest) {
	j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(ctx), d.app.engine, req.RunId)
	if err != nil || !workRunCanStart(req, j, time.Now()) {
		return
	}
	d.admitted <- req
}

type observedHumanClaim struct {
	actual   work.RunClaimer
	attempts atomic.Int32
}

func (c *observedHumanClaim) ClaimWithTTL(ctx context.Context, name, key string, ttl time.Duration) bool {
	c.attempts.Add(1)
	return c.actual.ClaimWithTTL(ctx, name, key, ttl)
}

// The real decision and multi-replica event/claim path must not consume the
// answer while the old nested invocation is still unwinding. If its bounded
// readiness wait expires, the ordinary sweep must recover the SAME unspent
// answer without waiting for the original execution lease.
func TestWorkDBHumanDispatchWaitsBeforeClaimAndRecoversTimedOutWakeup(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer gates.Close()
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			key := id.NewShortId()
			owner, runID, goalID, approval, child := key+"owner", key+"run", key+"goal", key+"approval", key+"child"
			ctx := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
			write := func(name string, args map[string]any) {
				templateMutation(t, writer, auth.ContextWithInternalOrigin(ctx), name, args)
			}
			write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic early typed approval", "origin": "user"})
			write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "parent", "templateFingerprint": "parent-fixture", "status": "waiting", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano), "heartbeatAt": time.Now().UTC().Format(time.RFC3339Nano), "waitingOn": map[string]any{"kind": "approval", "subject": approval}})
			write("createWorkRun", map[string]any{"runId": child, "parentRunId": runID, "automationName": "child", "templateFingerprint": "child-fixture", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
			write("createWorkStep", map[string]any{"stepId": key + "step", "runId": runID, "key": "section", "seq": 0, "stepType": "automation", "status": "running", "attempt": 1, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
			argsHash, _ := permission.Hash("workerHost", "fs_read", map[string]any{"path": "/fixture/file"})
			scope := permission.Scope{Version: 1, Kind: "directory_read", RegistrationID: "fixture-worker", MachineID: "fixture-machine", MachineLabel: "Test machine", Tool: "workerHost", Action: "fs_read", Directory: "/fixture", ArgsHash: argsHash, AllowedModes: []string{"once", "always"}}
			subject := map[string]any{"agentId": "fixture-agent", "computerPermission": scope.Map()}
			write("createWorkApproval", map[string]any{"approvalId": approval, "runId": runID, "stepKey": "section", "kind": "scopeElevation", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "requestedAt": time.Now().UTC().Format(time.RFC3339Nano), "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
			peers := []*work.Integration{}
			probes := []*observedHumanClaim{}
			admitted := make(chan work.DispatchRequest, 4)
			for _, eng := range []*memql.MemQLEngine{writer, reader} {
				integration := work.New(eng, eng.Logger, func() *bun.DB { return db })
				integration.SetGateDB(func() *sql.DB { return gates })
				d := &humanReadyDispatcher{workRunDispatcher: &workRunDispatcher{app: &App{engine: eng, Logger: eng.Logger}}, admitted: admitted}
				probe := &observedHumanClaim{actual: automations.NewClusterExecutionGuard(func() *bun.DB { return db }, eng.Logger).StrictClaimer()}
				integration.SetDispatcher(d)
				integration.SetRunClaimer(probe)
				peers = append(peers, integration)
				probes = append(probes, probe)
			}
			initial := automations.NewClusterExecutionGuard(func() *bun.DB { return db }, writer.Logger).StrictClaimer()
			if !initial.ClaimWithTTL(ctx, "work.run.dispatch", runID, 4*time.Minute) {
				t.Fatal("initial lease unavailable")
			}
			decided := false
			for _, cap := range peers[1].Capabilities() {
				if cap.Name == "decideApproval" {
					if _, err := cap.Handler(ctx, map[string]any{"approvalId": approval, "decision": "approved", "answer": map[string]any{"permissionMode": "once"}}, 0); err != nil {
						t.Fatal(err)
					}
					decided = true
				}
			}
			if !decided {
				t.Fatal("decision handler missing")
			}
			event := events.Event{Payload: map[string]any{"id": runID, "payload": map[string]any{"status": "running", "automationName": "parent", "goalId": goalID, "ownerUserId": owner, "humanResumeId": approval}}}
			for range 2 {
				for _, peer := range peers {
					peer.HandleRunEvent(event)
				}
			}
			delay := 100 * time.Millisecond
			if timeout {
				delay = 2200 * time.Millisecond
			}
			time.Sleep(delay)
			for _, probe := range probes {
				if probe.attempts.Load() != 0 {
					t.Fatal("answer claim spent before suspended receipt")
				}
			}
			write("updateWorkRun", map[string]any{"runId": child, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": approval}})
			write("updateWorkStep", map[string]any{"stepId": key + "step", "status": "waiting", "childRunId": child})
			if timeout {
				// This is the real sweep entry, while the first execution's lease remains.
				if !peers[1].DispatchRun(ctx, runID, owner) {
					t.Fatal("timed-out wakeup could not claim durable answer")
				}
			}
			select {
			case req := <-admitted:
				if req.HumanResumeId != approval || req.Recovery {
					t.Fatalf("wrong answer claim: %+v", req)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ready suspension was not dispatched")
			}
			for _, peer := range peers {
				peer.HandleRunEvent(event)
			}
			time.Sleep(75 * time.Millisecond)
			select {
			case <-admitted:
				t.Fatal("duplicate event dispatched same answer twice")
			default:
			}
			if timeout {
				// An answer winner that died before recording an intent may be
				// recovered after its lease. The ordinary earlier sweep still has
				// its existing bounded abandonment policy; we do not extend it.
				write("updateWorkRun", map[string]any{"runId": runID, "heartbeatAt": time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano)})
				// Expire only the synthetic answer claim. The original run claim
				// remains held: takeover must use the same answer identity.
				_, err := db.DB.ExecContext(ctx, `UPDATE automation_execution_claims SET claimed_at=now()-interval '5 minutes', expires_at=NULL WHERE automation_name=$1 AND dedup_key=$2`, "work.run.dispatch", runID+"#answer:"+approval+"#rerun:")
				if err != nil {
					t.Fatal(err)
				}
				if !peers[0].DispatchRun(ctx, runID, owner) {
					t.Fatal("spent answer prevented stale leased recovery")
				}
				recovered := <-admitted
				if !recovered.Recovery || !recovered.HumanResumeRecovery {
					t.Fatalf("not same-answer leased recovery: %+v", recovered)
				}
				j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(ctx), reader, runID)
				if err != nil {
					t.Fatal(err)
				}
				j.HeartbeatAt = time.Now()
				if workRunCanStart(recovered, j, time.Now()) {
					t.Fatal("recovery ignored a newly live heartbeat before intent")
				}
			}
			d := &workRunDispatcher{app: &App{engine: reader, Logger: reader.Logger}}
			stale := work.DispatchRequest{RunId: runID, HumanResumeId: "another"}
			if _, ok := d.PrepareDispatch(ctx, stale); ok {
				t.Fatal("stale answer admitted")
			}
			write("updateWorkRun", map[string]any{"runId": runID, "cancelRequested": true})
			if _, ok := d.PrepareDispatch(ctx, work.DispatchRequest{RunId: runID, HumanResumeId: approval}); ok {
				t.Fatal("cancelled answer admitted")
			}
		})
	}
}

// Nonhuman recovery must still reach the existing due-wait admission after
// installing the human-readiness preparer in the actual app dispatcher.
func TestWorkDBHumanDispatchPreservesScheduledRecovery(t *testing.T) {
	engine := workTemplateDBEngine(t)
	now := time.Now().UTC()
	since := now.Add(-time.Minute).Format(time.RFC3339Nano)
	for _, kind := range []string{"retry", "inference", "scheduler answer"} {
		t.Run(kind, func(t *testing.T) {
			key := id.NewShortId()
			ctx := auth.ContextWithUserActor(context.Background(), key+"owner")
			waiting := map[string]any{"kind": "retry", "subject": "fetch", "since": since, "resumeAt": now.Add(-time.Second).Format(time.RFC3339Nano)}
			if kind == "inference" {
				waiting["kind"] = "approval"
				waiting["approvalKind"] = "inferenceUnavailable"
			}
			templateMutation(t, engine, auth.ContextWithInternalOrigin(ctx), "createWorkRun", map[string]any{"runId": key, "automationName": "fixture", "templateFingerprint": "fixture", "startedAt": time.Now().UTC().Format(time.RFC3339Nano), "status": "waiting", "mode": "live", "waitingOn": waiting})
			req := work.DispatchRequest{RunId: key, Status: "waiting", Recovery: true, RetryWait: since}
			if kind == "scheduler answer" {
				templateMutation(t, engine, auth.ContextWithInternalOrigin(ctx), "updateWorkRun", map[string]any{"runId": key, "status": "running", "humanResumeId": key + "approval"})
				templateMutation(t, engine, auth.ContextWithInternalOrigin(ctx), "createWorkStep", map[string]any{"stepId": key + "step", "runId": key, "key": "section", "seq": 0, "stepType": "function", "status": "waiting", "attempt": 1})
				req.Status, req.RetryWait = "running", ""
			}
			d := &workRunDispatcher{app: &App{engine: engine, Logger: engine.Logger}}
			got, ok := d.PrepareDispatch(ctx, req)
			if !ok || !reflect.DeepEqual(got, req) {
				t.Fatalf("ordinary recovery changed: %+v, %v", got, ok)
			}
			journal, err := automations.LoadRunJournal(memql.ContextWithFreshRead(ctx), engine, key)
			if err != nil {
				t.Fatal(err)
			}
			if !workRunCanStart(got, journal, now) {
				t.Fatal("due nonhuman recovery refused")
			}
		})
	}
}

// Both sweep replicas see the same stale, unspent answer and pause before any
// resumed intent or heartbeat can be written. They must compete on one row.
func TestWorkDBStaleUnspentHumanAnswerHasOneClaim(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	key := id.NewShortId()
	ctx := auth.ContextWithUserActor(context.Background(), key+"owner")
	write := func(name string, args map[string]any) {
		templateMutation(t, writer, auth.ContextWithInternalOrigin(ctx), name, args)
	}
	write("createWorkRun", map[string]any{"runId": key, "goalId": key + "goal", "automationName": "fixture", "templateFingerprint": "fixture", "startedAt": time.Now().UTC().Format(time.RFC3339Nano), "status": "running", "mode": "live", "heartbeatAt": time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano)})
	write("updateWorkRun", map[string]any{"runId": key, "humanResumeId": key + "approval"})
	write("createWorkStep", map[string]any{"stepId": key + "step", "runId": key, "key": "section", "seq": 0, "stepType": "automation", "status": "waiting", "attempt": 1})
	admitted := make(chan work.DispatchRequest, 2)
	peers := []*work.Integration{}
	for _, engine := range []*memql.MemQLEngine{writer, reader} {
		integration := work.New(engine, engine.Logger, func() *bun.DB { return db })
		integration.SetDispatcher(&humanReadyDispatcher{workRunDispatcher: &workRunDispatcher{app: &App{engine: engine, Logger: engine.Logger}}, admitted: admitted})
		integration.SetRunClaimer(automations.NewClusterExecutionGuard(func() *bun.DB { return db }, engine.Logger).StrictClaimer())
		peers = append(peers, integration)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(peer *work.Integration) { defer wg.Done(); <-start; peer.DispatchRun(ctx, key, key+"owner") }(peer)
	}
	close(start)
	wg.Wait()
	if len(admitted) != 1 {
		t.Fatalf("stale unspent answer dispatched %d times", len(admitted))
	}
	winner := <-admitted
	if !winner.HumanResumeRecovery || !winner.Recovery {
		t.Fatalf("not stale answer recovery: %+v", winner)
	}
	for _, peer := range peers {
		if peer.DispatchRun(ctx, key, key+"owner") {
			t.Fatal("fresh answer claim allowed second recovery before intent")
		}
	}
}

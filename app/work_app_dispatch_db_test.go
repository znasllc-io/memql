//go:build agent

package app

import (
	"context"
	"database/sql"
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
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	work "github.com/znasllc-io/memql/integrations/work"
)

type dispatchDoorProbe struct {
	local bool
	owner string
	reads atomic.Int32
}

func (p *dispatchDoorProbe) Doors(ctx context.Context, owner string) ([]memql.AppDoor, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.Synthetic || memql.BareShortId(ac.UserId) != p.owner || memql.BareShortId(owner) != p.owner {
		return nil, context.Canceled
	}
	p.reads.Add(1)
	return []memql.AppDoor{{AppId: "codex", Machines: []memql.AppMachine{{RegistrationId: "machine", Online: true, LocalStream: p.local, Forwardable: !p.local}}}}, nil
}

// Two engine caches and two real claim clients see the same retry. Only the
// replica with the owned App's local stream may spend the retry's claim.
// No App session or model is invoked; the dispatch receipt is the observation.
func TestWorkDBAppsRetryIsClaimedOnlyWhereTheComputerStreamLives(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer gates.Close()
	key := id.NewShortId()
	owner, runID, goalID := key+"owner", key+"run", key+"goal"
	ctx := displayNameActor(owner, auth.RoleDeveloper)
	write := func(name string, args map[string]any) {
		templateMutation(t, writer, auth.ContextWithInternalOrigin(ctx), name, args)
	}
	write("identity.createUserOnFirstLogin", map[string]any{"userId": owner, "displayName": "Placement owner", "primaryEmail": owner + "@example.test", "role": "developer", "internal": false})
	route := common.RouteChoice{Source: "route:apps", By: owner, NoPaidProviders: true}
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic Apps placement retry", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "testPlacement", "templateFingerprint": "fixture", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano), "routing": route.Map()})
	write("updateWorkRun", map[string]any{"runId": runID, "rerun": map[string]any{"requestId": "retry", "reason": "rerun", "stepKey": "reason"}})
	admitted := make(chan work.DispatchRequest, 4)
	peers := []*work.Integration{}
	claims := []*observedHumanClaim{}
	doors := []*dispatchDoorProbe{}
	for index, engine := range []*memql.MemQLEngine{writer, reader} {
		local := index == 1
		door := &dispatchDoorProbe{local: local, owner: owner}
		doors = append(doors, door)
		app := &App{engine: engine, Logger: engine.Logger, appInference: door}
		integration := work.New(engine, engine.Logger, func() *bun.DB { return db })
		integration.SetGateDB(func() *sql.DB { return gates })
		integration.SetDispatcher(&humanReadyDispatcher{workRunDispatcher: &workRunDispatcher{app: app}, admitted: admitted})
		claim := &observedHumanClaim{actual: automations.NewClusterExecutionGuard(func() *bun.DB { return db }, nil).StrictClaimer()}
		integration.SetRunClaimer(claim)
		peers = append(peers, integration)
		claims = append(claims, claim)
	}
	event := events.Event{Payload: map[string]any{"id": runID, "payload": map[string]any{"status": "running", "automationName": "testPlacement", "goalId": goalID, "ownerUserId": owner, "routing": route.Map(), "rerun": map[string]any{"requestId": "retry"}}}}
	// Put the wrong replica first, repeatedly; arrival order must not win.
	for range 3 {
		peers[0].HandleRunEvent(event)
	}
	time.Sleep(100 * time.Millisecond)
	if claims[0].attempts.Load() != 0 {
		t.Fatal("replica without stream consumed an execution claim")
	}
	peers[1].HandleRunEvent(event)
	select {
	case got := <-admitted:
		if !got.AppPlacement || got.RerunRequestId != "retry" {
			t.Fatalf("wrong claim %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream holder never admitted retry")
	}
	for _, peer := range peers {
		peer.HandleRunEvent(event)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-admitted:
		t.Fatal("duplicate execution on another replica")
	default:
	}
	if claims[0].attempts.Load() != 0 || claims[1].attempts.Load() == 0 || doors[0].reads.Load() == 0 || doors[1].reads.Load() == 0 {
		t.Fatalf("incorrect placement/claims: %d %d", claims[0].attempts.Load(), claims[1].attempts.Load())
	}
	// A due failure wait stays waiting until its retry starts. Locality must
	// preserve that existing admission contract rather than requiring running.
	since := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	write("updateWorkRun", map[string]any{"runId": runID, "status": "waiting", "rerun": map[string]any{}, "waitingOn": map[string]any{"kind": "retry", "subject": "reason", "since": since, "resumeAt": time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)}})
	for index, engine := range []*memql.MemQLEngine{writer, reader} {
		d := &workRunDispatcher{app: &App{engine: engine, Logger: engine.Logger, appInference: doors[index]}}
		req, ready := d.PrepareDispatch(ctx, work.DispatchRequest{RunId: runID, OwnerUserId: owner, GoalId: goalID, Status: "waiting", Recovery: true, RetryWait: since})
		if ready != (index == 1) || !req.AppPlacement {
			t.Fatalf("due wait placement on replica %d: ready=%t request=%+v", index, ready, req)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM automation_execution_claims WHERE automation_name='work.run.dispatch' AND dedup_key LIKE ?`, runID+"%")
		_, _ = db.Exec(`DELETE FROM "MemoryNodes" WHERE payload->>'ownerUserId' IN (?,?) OR id=?`, owner, "v1:identity:user:"+owner, "v1:identity:user:"+owner)
	})
}

func (c *observedHumanClaim) HasLiveClaim(ctx context.Context, name string, keys []string, ttl time.Duration) (bool, error) {
	return c.actual.(interface {
		HasLiveClaim(context.Context, string, []string, time.Duration) (bool, error)
	}).HasLiveClaim(ctx, name, keys, ttl)
}

package work

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
)

type workRoleTestValidator struct{ revoked atomic.Bool }

func (v *workRoleTestValidator) ValidateRoleTest(_ context.Context, session, owner string, role auth.Role) error {
	if v.revoked.Load() || session != "work-test-session" || owner != canonicalUser("role-test-work") || role != auth.RoleReader {
		return errors.New("test authority refused")
	}
	return nil
}

func roleTestWorkContext(t *testing.T) context.Context {
	t.Helper()
	ac := &auth.AccessContext{UserId: canonicalUser("role-test-work"), Role: auth.RoleReader, RoleTestSessionID: "work-test-session"}
	a, err := auth.ForwardedAuthorityForUser(ac, auth.RoleTestClass, "", time.Now().Add(20*time.Minute), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return auth.BindForwardedContext(context.Background(), a.Principal().Claims, ac, a)
}

type roleRunDispatcher struct {
	engine Engine
	ctx    context.Context
	err    error
}

func (d *roleRunDispatcher) Dispatch(ctx context.Context, req DispatchRequest) {
	d.ctx = ctx
	_, d.err = automations.LoadRunJournal(memql.ContextWithFreshRead(ctx), d.engine, req.RunId)
}

func TestRoleTestWorkDB_PlannerAndExecutionRestoreBoundedAuthority(t *testing.T) {
	db, bff, planners, probe := compileDB(t)
	insertSweepRow(t, db, canonicalUser("role-test-work"), "v1:identity:user", time.Now(), map[string]any{"role": "reader", "active": true})
	validator := &workRoleTestValidator{}
	auth.SetRoleTestValidator(validator)
	t.Cleanup(func() { auth.SetRoleTestValidator(nil) })
	ctx := roleTestWorkContext(t)
	nodes, err := bff.handleCreateGoal(ctx, map[string]any{"statement": "what is 17 plus 25"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	runID := decodeReply(t, nodes)["runId"].(string)
	// The receiving planners hold none of the BFF's context or credentials.
	// They must restore the grant from the run before their first owned read.
	for _, p := range planners {
		p.HandleRunEvent(runEvent(runID, "compiling", "work.compile", canonicalUser("role-test-work")))
	}
	got := awaitCompile(t, probe)
	if !got.hasAuthority || got.authority.CredentialClass != auth.RoleTestClass ||
		got.authority.RoleTestSessionID != "work-test-session" || got.authority.Role != auth.RoleReader ||
		got.actor != canonicalUser("role-test-work") {
		t.Fatalf("planner changed the bounded Viewer: %+v", got)
	}
	finishCompile(t, probe)

	// A second engine represents the agent receiving the compiled run. Its
	// journal load is the same first read as the production work dispatcher.
	agent := New(dispatchDBEngine(t, db), testLogger())
	d := &roleRunDispatcher{engine: agent.engine}
	agent.SetDispatcher(d)
	agent.SetRunClaimer(&stubClaimer{grant: true})
	req := DispatchRequest{RunId: runID, OwnerUserId: canonicalUser("role-test-work"), Status: "running", AutomationName: "probe"}
	if !agent.dispatchRun(context.Background(), req) || d.err != nil || d.ctx == nil {
		t.Fatalf("agent did not load the bounded journal: %v", d.err)
	}
	a, ok := auth.ForwardedAuthorityFromContext(d.ctx)
	if !ok || a.CredentialClass != auth.RoleTestClass || a.Role != auth.RoleReader || a.RoleTestSessionID != got.authority.RoleTestSessionID {
		t.Fatalf("agent widened or lost the session: %+v", a)
	}
	validator.revoked.Store(true)
	d.ctx = nil
	if agent.dispatchRun(context.Background(), req) || d.ctx != nil {
		t.Fatal("revoked persona reached the execution dispatcher")
	}
	if err := auth.ValidateRoleTestContext(ctx); err == nil {
		t.Fatal("original context survived revocation")
	}
}

func TestRoleTestWorkDB_RecoveryAndForgedOrExpiredGrants(t *testing.T) {
	db, bff, planners, probe := compileDB(t)
	owner := canonicalUser("role-test-work")
	insertSweepRow(t, db, owner, "v1:identity:user", time.Now(), map[string]any{"role": "reader", "active": true})
	validator := &workRoleTestValidator{}
	auth.SetRoleTestValidator(validator)
	t.Cleanup(func() { auth.SetRoleTestValidator(nil) })
	ctx := roleTestWorkContext(t)
	nodes, err := bff.handleCreateGoal(ctx, map[string]any{"statement": "a missed Viewer compile event"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	runID := decodeReply(t, nodes)["runId"].(string)
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		owner string
	}{
		{"mismatched event", context.Background(), canonicalUser("role-test-other")},
		{"another caller", actorCtx("compile-alice"), owner},
		{"unbounded persona", ownerActor(context.Background(), owner), owner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := planners[0].runOwnerActor(tc.ctx, runID, tc.owner); err == nil {
				t.Fatal("untrusted authority restored the test session")
			}
		})
	}
	maintenance := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("sweepWaitingWorkRuns"))
	res, err := planners[0].SweepWaiting(maintenance, time.Minute)
	if err != nil || res.Redispatched != 1 {
		t.Fatalf("missed event was not recovered under the test grant: %+v %v", res, err)
	}
	awaitCompile(t, probe)
	finishCompile(t, probe)
	for _, grant := range []map[string]any{
		nil,
		{"roleCeiling": "reader", "credentialClass": "user"},
		{"roleCeiling": "reader", "credentialClass": auth.RoleTestClass, "roleTestSessionId": "work-test-session", "expiresAt": time.Now().Add(-time.Minute).Format(time.RFC3339Nano)},
		{"roleCeiling": "reader", "credentialClass": auth.RoleTestClass, "roleTestSessionId": "wrong-session", "expiresAt": time.Now().Add(time.Minute).Format(time.RFC3339Nano)},
	} {
		// The public update mutation cannot change this intake stamp. Seed a
		// malformed stored row to exercise recovery's own refusal as well.
		insertSweepRow(t, db, runID, runConcept, time.Now(), map[string]any{
			"ownerUserId": owner, "executionAuthority": grant,
		})
		if _, err := planners[1].runOwnerActor(context.Background(), runID, owner); err == nil {
			t.Fatalf("invalid durable grant was restored: %+v", grant)
		}
	}
}

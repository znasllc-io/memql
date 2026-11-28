package calls

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// Observe or pause an actual named DB read after its result was obtained. The
// wrapper never substitutes authority rows; another real service commits races.
type presentationIdleReadEngine struct {
	Engine
	reads atomic.Int32
	after func(context.Context, int32) error
}

func (e *presentationIdleReadEngine) Execute(ctx context.Context, query string) (any, error) {
	out, err := e.Engine.Execute(ctx, query)
	if err == nil && strings.HasPrefix(query, "query ") && strings.Contains(query, "callsWorkflowStateById(") {
		n := e.reads.Add(1)
		if e.after != nil {
			err = e.after(ctx, n)
		}
	}
	return out, err
}

func presentationIdleGateCounter(t *testing.T, svc *Service) *atomic.Int32 {
	t.Helper()
	original := svc.gateDB
	count := &atomic.Int32{}
	svc.gateDB = func() *sql.DB {
		count.Add(1)
		return original()
	}
	t.Cleanup(func() { svc.gateDB = original })
	return count
}

func presentationIdlePause(t *testing.T, svc *Service, readNumber int32) (*presentationIdleReadEngine, <-chan struct{}, func()) {
	t.Helper()
	entered, resume, pause := admissionPause(t)
	original := svc.store.engine
	probe := &presentationIdleReadEngine{Engine: original, after: func(ctx context.Context, n int32) error {
		if n == readNumber {
			return pause(ctx)
		}
		return nil
	}}
	svc.store.engine = probe
	t.Cleanup(func() { svc.store.engine = original })
	return probe, entered, resume
}

func presentationIdleWait(t *testing.T, ctx context.Context, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fresh workflow read did not reach the pause")
	}
}

func TestPresentationDatabaseAbsentPolicyDoesNotAcquireGates(t *testing.T) {
	for _, state := range []string{"no_run", "run_without_policy"} {
		t.Run(state, func(t *testing.T) {
			cw, key, _, start := workflowFixture(t, "idle-"+state, true)
			if cw == nil {
				return
			}
			if state == "run_without_policy" {
				workflowDo(t, cw, key, callrules.WorkflowRequest{Action: "start", SkillID: "working-session", Version: 1, IdempotencyKey: "idle-start"})
			}
			ctx := context.Background()
			room := mustProcessingRoom(t, cw, start.Call.ID)
			count := presentationIdleGateCounter(t, cw.svc)
			probe := &presentationIdleReadEngine{Engine: cw.svc.store.engine}
			original := cw.svc.store.engine
			cw.svc.store.engine = probe
			t.Cleanup(func() { cw.svc.store.engine = original })
			require.NoError(t, cw.svc.reconcilePresentation(ctx, *room))
			require.Equal(t, int32(1), probe.reads.Load(), "one real fresh absence read")
			require.Zero(t, count.Load(), "idle polling must not even resolve the gate pool")
		})
	}
}

func TestPresentationDatabaseAbsentReadErrorRefuses(t *testing.T) {
	cw, _, _, start := workflowFixture(t, "presentation-idle-error", true)
	if cw == nil {
		return
	}
	room := mustProcessingRoom(t, cw, start.Call.ID)
	count := presentationIdleGateCounter(t, cw.svc)
	original := cw.svc.store.engine
	failure := errors.New("injected workflow read unavailable")
	cw.svc.store.engine = &presentationIdleReadEngine{Engine: original, after: func(context.Context, int32) error { return failure }}
	t.Cleanup(func() { cw.svc.store.engine = original })
	require.ErrorIs(t, cw.svc.reconcilePresentation(context.Background(), *room), failure)
	require.Zero(t, count.Load(), "an unknown read is not an absence or a reason to mutate")
}

func TestPresentationDatabaseConcurrentCreationSurvivesAbsentRead(t *testing.T) {
	f := newPresentationFixture(t, "presentation-idle-create")
	if f.cw == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	room := mustProcessingRoom(t, f.cw, f.start.Call.ID)
	req := f.request(t, "presentation_set", "idle-create", f.hostSeat(t))
	peer := allowanceReplica(t, f.cw)
	peer.commerce = func() CommerceConfig { return *f.commerce }
	peer.media = f.cw.svc.media
	count := presentationIdleGateCounter(t, f.cw.svc)
	_, entered, resume := presentationIdlePause(t, f.cw.svc, 1)
	finished := make(chan error, 1)
	go func() { finished <- f.cw.svc.reconcilePresentation(ctx, *room) }()
	presentationIdleWait(t, ctx, entered)
	created, err := peer.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, req)
	require.NoError(t, err)
	require.True(t, created.Presentation.Active)
	resume()
	require.NoError(t, <-finished)
	require.Zero(t, count.Load(), "the old absence pass must remain a no-op")
	stored, err := peer.store.workflowState(memql.ContextWithFreshRead(ctx), f.cw.owner, workflowStateID(f.cw.site, "run", room.ID))
	require.NoError(t, err)
	require.True(t, stored.Presentation.Active)
	require.Equal(t, created.Presentation.Revision, stored.Presentation.Revision)
	forwarded := false
	require.Error(t, f.guestLease.Forward(ctx, func(context.Context) error { forwarded = true; return nil }))
	require.False(t, forwarded, "the old pass cannot bypass the newly committed restriction")
	require.False(t, f.desired(t, f.guestJoin.Pass).CanPublish)
	count.Store(0)
	require.NoError(t, f.cw.svc.reconcilePresentation(ctx, *room))
	require.Positive(t, count.Load(), "a later pass must see the new policy and take gates")
}

func TestPresentationDatabasePresentPolicyRereadsAfterGate(t *testing.T) {
	f := newPresentationFixture(t, "presentation-idle-clear")
	if f.cw == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	workflowDo(t, f.cw, f.key, f.request(t, "presentation_set", "idle-before-clear", f.hostSeat(t)))
	req := f.request(t, "presentation_clear", "idle-clear", "")
	room := mustProcessingRoom(t, f.cw, f.start.Call.ID)
	peer := allowanceReplica(t, f.cw)
	peer.commerce = func() CommerceConfig { return *f.commerce }
	peer.media = f.cw.svc.media
	count := presentationIdleGateCounter(t, f.cw.svc)
	probe, entered, resume := presentationIdlePause(t, f.cw.svc, 1)
	finished := make(chan error, 1)
	go func() { finished <- f.cw.svc.reconcilePresentation(ctx, *room) }()
	presentationIdleWait(t, ctx, entered)
	cleared, err := peer.Workflow(ctx, f.cw.ref, f.key.result.Session.Token, req)
	require.NoError(t, err)
	require.False(t, cleared.Presentation.Active)
	resume()
	require.NoError(t, <-finished)
	require.Positive(t, count.Load())
	require.GreaterOrEqual(t, probe.reads.Load(), int32(2), "precheck may not supply the gated policy")
	stored, err := peer.store.workflowState(memql.ContextWithFreshRead(ctx), f.cw.owner, workflowStateID(f.cw.site, "run", room.ID))
	require.NoError(t, err)
	require.False(t, stored.Presentation.Active, "an old precheck must not resurrect a cleared policy")
	require.Equal(t, cleared.Presentation.Revision, stored.Presentation.Revision)
	require.True(t, f.desired(t, f.guestJoin.Pass).CanPublish)
}

func TestPresentationDatabaseAbsentSnapshotStillRejectsRevocation(t *testing.T) {
	cw, key, config, _ := workflowFixture(t, "presentation-idle-revoked", true)
	if cw == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	peer := allowanceReplica(t, cw)
	peer.commerce = func() CommerceConfig { return *config }
	// Snapshot first checks mute absence, then presentation absence. Pause the
	// latter after its actual read; revocation commits on another service.
	_, entered, resume := presentationIdlePause(t, cw.svc, 2)
	type answer struct {
		out callrules.WorkflowResult
		err error
	}
	finished := make(chan answer, 1)
	go func() {
		out, err := cw.svc.Workflow(ctx, cw.ref, key.result.Session.Token, callrules.WorkflowRequest{Action: "snapshot"})
		finished <- answer{out, err}
	}()
	presentationIdleWait(t, ctx, entered)
	release, err := peer.gates(ctx, accountGate(key.result.Account.KeyID))
	require.NoError(t, err)
	credential, err := peer.store.credentialByID(memql.ContextWithFreshRead(ctx), cw.owner, key.result.Credentials[0].ID)
	require.NoError(t, err)
	err = peer.store.updateCredential(ctx, cw.owner, *credential, map[string]any{"revokedAt": versionStamp(cw.clock.Now())}, cw.clock.Now())
	release()
	require.NoError(t, err)
	resume()
	result := <-finished
	require.Error(t, result.err)
	require.Nil(t, result.out.Run)
	require.Nil(t, result.out.Binding)
	require.Empty(t, result.out.ResourceOptions)
}

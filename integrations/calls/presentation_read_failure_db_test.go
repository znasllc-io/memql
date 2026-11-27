package calls

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// Fail a real named read after an active restrictive policy was committed.
// Other reads still use the isolated native database; no fake authorization row
// is substituted and the provider seam records every permission mutation.
type presentationReadFailureEngine struct {
	Engine
	query, value string
	failures     int
}

func (e *presentationReadFailureEngine) Execute(ctx context.Context, query string) (any, error) {
	if strings.HasPrefix(query, "query ") && strings.Contains(query, e.query+"(") && (e.value == "" || strings.Contains(query, e.value)) {
		e.failures++
		return nil, errors.New("injected presentation authority read failure")
	}
	return e.Engine.Execute(ctx, query)
}

type presentationReadFailureBackend struct {
	*presentationTestBackend
	updates int
}

func (b *presentationReadFailureBackend) UpdatePermissions(ctx context.Context, room, identity string, spec callrules.GrantSpec) (callrules.GrantSpec, error) {
	b.updates++
	return b.presentationTestBackend.UpdatePermissions(ctx, room, identity, spec)
}
func TestPresentationDatabaseUnknownReadsNeverReleasePolicy(t *testing.T) {
	for _, query := range []string{"callsCommerceStateById", "callsSeatById", "callsResourceStateById", "callsMediaSessionsForSeat"} {
		t.Run(query, func(t *testing.T) {
			f := newPresentationFixture(t, "presentation-unknown-read")
			if f.cw == nil {
				return
			}
			ctx := context.Background()
			presenter := f.hostSeat(t)
			workflowDo(t, f.cw, f.key, f.request(t, "presentation_set", "read-failure-policy", presenter))
			f.reconnectGuest(t)
			room, _, _, binding, _, err := f.cw.svc.authenticateMediaSignal(ctx, f.guestJoin.Pass)
			require.NoError(t, err)
			before, err := f.cw.svc.store.workflowState(memql.ContextWithFreshRead(ctx), f.cw.owner, workflowStateID(f.cw.site, "run", room.ID))
			require.NoError(t, err)
			require.True(t, before.Presentation.Active)
			require.False(t, f.backend.grant(f.guestJoin.Identity).CanPublish)
			backend := &presentationReadFailureBackend{presentationTestBackend: f.backend}
			f.cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return backend, nil }
			original := f.cw.svc.store.engine
			failing := &presentationReadFailureEngine{Engine: original, query: query}
			if query == "callsSeatById" || query == "callsMediaSessionsForSeat" {
				failing.value = memql.BareShortId(presenter)
			}
			f.cw.svc.store.engine = failing
			defer func() { f.cw.svc.store.engine = original }()
			_, err = f.cw.svc.currentMediaGrant(ctx, *room, binding)
			require.Error(t, err, "unknown presenter/entitlement authority must not become the unrestricted base grant")
			forwarded := false
			require.Error(t, f.guestLease.Forward(ctx, func(context.Context) error { forwarded = true; return nil }))
			require.False(t, forwarded)
			_, err = f.cw.svc.BeginMediaSignal(ctx, f.guestJoin.Pass)
			require.Error(t, err, "reentry must not remint a broader provider token during an unknown read")
			require.Error(t, f.cw.svc.presentationResourceStartAllowed(ctx, *room), "a competing source must not start during unknown policy authority")
			require.Error(t, f.cw.svc.reconcilePresentation(ctx, *room))
			require.GreaterOrEqual(t, failing.failures, 5)
			require.Zero(t, backend.updates, "unknown reads must not mutate provider permissions")
			f.cw.svc.store.engine = original
			after, err := f.cw.svc.store.workflowState(memql.ContextWithFreshRead(ctx), f.cw.owner, workflowStateID(f.cw.site, "run", room.ID))
			require.NoError(t, err)
			require.Equal(t, before.Presentation, after.Presentation, "unknown authority cannot persist an inactive policy or a new revision")
			require.Equal(t, before.Run.Revision, after.Run.Revision)
			require.False(t, f.desired(t, f.guestJoin.Pass).CanPublish)
			require.NoError(t, f.guestLease.Forward(ctx, func(context.Context) error { return nil }))
		})
	}
}
func TestPresentationDatabaseUnknownSpeakerReadPreservesAssignment(t *testing.T) {
	f := newPresentationFixture(t, "presentation-speaker-unknown")
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	req := f.request(t, "presentation_set", "speaker-read-failure", f.hostSeat(t))
	req.Presentation.SpeakerSeatID, req.Presentation.SpeakerSeconds = f.guest.SeatID, 30
	workflowDo(t, f.cw, f.key, req)
	f.reconnectGuest(t)
	room := mustProcessingRoom(t, f.cw, f.start.Call.ID)
	before, err := f.cw.svc.store.workflowState(memql.ContextWithFreshRead(ctx), f.cw.owner, workflowStateID(f.cw.site, "run", room.ID))
	require.NoError(t, err)
	original := f.cw.svc.store.engine
	failing := &presentationReadFailureEngine{Engine: original, query: "callsSeatById", value: memql.BareShortId(f.guest.SeatID)}
	f.cw.svc.store.engine = failing
	defer func() { f.cw.svc.store.engine = original }()
	require.Error(t, f.cw.svc.reconcilePresentation(ctx, *room))
	require.Equal(t, 1, failing.failures)
	f.cw.svc.store.engine = original
	after, err := f.cw.svc.store.workflowState(memql.ContextWithFreshRead(ctx), f.cw.owner, workflowStateID(f.cw.site, "run", room.ID))
	require.NoError(t, err)
	require.Equal(t, before.Presentation, after.Presentation)
	require.Equal(t, []string{"microphone"}, f.backend.grant(f.guestJoin.Identity).Sources)
}

func TestPresentationDatabaseUnknownReadRefusesPendingConfirmation(t *testing.T) {
	f := newPresentationFixture(t, "presentation-confirm-unknown")
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	workflowDo(t, f.cw, f.key, f.request(t, "presentation_set", "confirm-read-failure", f.hostSeat(t)))
	pending, err := f.cw.svc.BeginMediaSignal(ctx, f.guestJoin.Pass)
	require.NoError(t, err)
	require.Equal(t, false, rawVideoGrant(t, pending.UpstreamToken)["canPublish"])
	backend := &presentationReadFailureBackend{presentationTestBackend: f.backend}
	f.cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return backend, nil }
	original := f.cw.svc.store.engine
	failing := &presentationReadFailureEngine{Engine: original, query: "callsCommerceStateById"}
	f.cw.svc.store.engine = failing
	defer func() { f.cw.svc.store.engine = original }()
	require.Error(t, pending.Confirm(ctx), "an uncertain policy cannot release an upstream initial response")
	require.Equal(t, 1, failing.failures)
	require.Zero(t, backend.updates, "Confirm cannot widen the already restricted provider grant")
	f.cw.svc.store.engine = original
	room := mustProcessingRoom(t, f.cw, f.start.Call.ID)
	journal, err := f.cw.svc.store.mediaSignals(memql.ContextWithFreshRead(ctx), *room)
	require.NoError(t, err)
	require.Equal(t, "draining", journal.find(pending.IntentID).State)
	policy, err := f.cw.svc.store.workflowState(memql.ContextWithFreshRead(ctx), f.cw.owner, workflowStateID(f.cw.site, "run", room.ID))
	require.NoError(t, err)
	require.True(t, policy.Presentation.Active)
	require.NoError(t, pending.Close(ctx, true))
}

func TestPresentationDatabaseUnknownEnvelopeAndMalformedConfigurationPreservePolicy(t *testing.T) {
	f := newPresentationFixture(t, "presentation-envelope-unknown")
	if f.cw == nil {
		return
	}
	ctx := context.Background()
	workflowDo(t, f.cw, f.key, f.request(t, "presentation_set", "envelope-read-failure", f.hostSeat(t)))
	room, _, _, binding, _, err := f.cw.svc.authenticateMediaSignal(ctx, f.guestJoin.Pass)
	require.NoError(t, err)
	before, err := f.cw.svc.store.workflowState(memql.ContextWithFreshRead(ctx), f.cw.owner, workflowStateID(f.cw.site, "run", room.ID))
	require.NoError(t, err)
	malformed := *room
	malformed.UsageConfig = "{"
	_, _, err = f.cw.svc.presentationEffective(ctx, malformed, before)
	require.Error(t, err, "corrupt configuration is not proof of entitlement loss")
	prior := f.commerce.ValidUntil
	f.commerce.ValidUntil = f.cw.clock.Now()
	_, err = f.cw.svc.currentMediaGrant(ctx, *room, binding)
	require.ErrorContains(t, err, "admission_stale")
	require.ErrorContains(t, f.cw.svc.reconcilePresentation(ctx, *room), "admission_stale")
	f.commerce.ValidUntil = prior
	after, err := f.cw.svc.store.workflowState(memql.ContextWithFreshRead(ctx), f.cw.owner, workflowStateID(f.cw.site, "run", room.ID))
	require.NoError(t, err)
	require.Equal(t, before.Presentation, after.Presentation)
	require.False(t, f.desired(t, f.guestJoin.Pass).CanPublish)
}

package calls

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

func mediaSignalFixture(t *testing.T, name string) (*callWorld, callrules.Session, callrules.StartResult) {
	t.Helper()
	cw := newCallWorld(t, name, 10)
	if cw == nil {
		return nil, callrules.Session{}, callrules.StartResult{}
	}
	t.Cleanup(func() {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", mediaSignalConcept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(context.Background())
	})
	host, start := cw.started("Guarded host")
	return cw, host, start
}
func TestMediaSignalDatabaseAdmissionReconnectAndReplay(t *testing.T) {
	cw, host, start := mediaSignalFixture(t, "signal-reconnect")
	if cw == nil {
		return
	}
	ctx := context.Background()
	lease, e := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, e)
	require.Equal(t, start.Join.Identity, lease.Identity)
	require.NotEmpty(t, lease.UpstreamGuard)
	require.NotEqual(t, start.Join.Pass, lease.UpstreamToken)
	_, e = cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.ErrorContains(t, e, "media_connection_limit")
	require.NoError(t, lease.Forward(ctx, func(context.Context) error { return nil }))
	cw.media.setPresent(start.Call.ID, person(lease.Identity))
	require.NoError(t, lease.Confirm(ctx))
	cw.clock.Advance(15 * time.Second)
	require.NoError(t, lease.Forward(ctx, func(context.Context) error { return nil }))
	cw.clock.Advance(15 * time.Second)
	require.NoError(t, lease.Forward(ctx, func(context.Context) error { return nil }), "heartbeat renews established lease beyond handshake deadline")
	require.NoError(t, lease.Close(ctx, true))
	status, e := cw.svc.ReconcileMediaSignals(ctx, start.Call.ID)
	require.NoError(t, e)
	require.Equal(t, "clear", status.State)
	require.Empty(t, cw.media.present[start.Call.ID])
	second, e := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, e)
	require.NotEqual(t, lease.IntentID, second.IntentID)
	require.NoError(t, second.Confirm(ctx))
	require.NoError(t, cw.svc.Leave(ctx, host))
	_, e = cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.Error(t, e, "a still-valid issued JWT cannot reopen the closed native admission")
}
func TestMediaSignalDatabasePendingHandshakeSurvivesReplicaAndExpiry(t *testing.T) {
	cw, _, start := mediaSignalFixture(t, "signal-uncertain")
	if cw == nil {
		return
	}
	ctx := context.Background()
	lease, e := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, e)
	require.NoError(t, lease.Forward(ctx, func(context.Context) error { return nil }))
	cw.clock.Advance(21 * time.Second)
	replica := allowanceReplica(t, cw)
	status, e := replica.ReconcileMediaSignals(ctx, start.Call.ID)
	require.NoError(t, e)
	require.Equal(t, "pending", status.State)
	require.Equal(t, "handshake_outcome_unknown", status.Reason)
	_, e = replica.BeginMediaSignal(ctx, start.Join.Pass)
	require.ErrorContains(t, e, "media_cleanup_pending")
	// A definitive late upstream reply settles uncertainty but cannot revive
	// an expired connection. Only then may provider cleanup become complete.
	cw.media.setPresent(start.Call.ID, person(lease.Identity))
	require.ErrorContains(t, lease.Confirm(ctx), "media_connection_expired")
	require.NoError(t, lease.Close(ctx, true))
	status, e = replica.ReconcileMediaSignals(ctx, start.Call.ID)
	require.NoError(t, e)
	require.Equal(t, "clear", status.State)
}
func TestMediaSignalDatabaseRevocationOrdersForwardAndLateHandshake(t *testing.T) {
	cw, host, start := mediaSignalFixture(t, "signal-race")
	if cw == nil {
		return
	}
	ctx := context.Background()
	lease, e := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, e)
	entered, finish := make(chan struct{}), make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- lease.Forward(ctx, func(context.Context) error { close(entered); <-finish; return nil })
	}()
	<-entered
	leaveDone := make(chan error, 1)
	go func() { leaveDone <- cw.svc.Leave(ctx, host) }()
	select {
	case e := <-leaveDone:
		t.Fatalf("revocation bypassed in-flight gated write: %v", e)
	case <-time.After(100 * time.Millisecond):
	}
	close(finish)
	require.NoError(t, <-writeDone)
	require.NoError(t, <-leaveDone)
	var forwarded atomic.Bool
	require.Error(t, lease.Forward(ctx, func(context.Context) error { forwarded.Store(true); return nil }))
	require.False(t, forwarded.Load())
	// The upstream created a participant after native removal. Pending intent
	// keeps cleanup unconfirmed until this definitive response is observed.
	cw.media.setPresent(start.Call.ID, person(lease.Identity))
	status, e := cw.svc.ReconcileMediaSignals(ctx, start.Call.ID)
	require.NoError(t, e)
	require.Equal(t, "pending", status.State)
	require.Error(t, lease.Confirm(ctx))
	require.NoError(t, lease.Close(ctx, true))
	status, e = cw.svc.ReconcileMediaSignals(ctx, start.Call.ID)
	require.NoError(t, e)
	require.Equal(t, "clear", status.State)
	require.Empty(t, cw.media.present[start.Call.ID])
}

type mediaSignalFailReadback struct {
	MediaBackend
	fail bool
}

func (b *mediaSignalFailReadback) Participants(ctx context.Context, room string) ([]MediaParticipant, error) {
	if b.fail {
		return nil, errors.New("provider unavailable")
	}
	return b.MediaBackend.Participants(ctx, room)
}
func TestMediaSignalDatabaseProviderFailureAndLegacyInventoryNeverReportClear(t *testing.T) {
	cw, _, start := mediaSignalFixture(t, "signal-cleanup")
	if cw == nil {
		return
	}
	ctx := context.Background()
	cw.media.setPresent(start.Call.ID, person(start.Join.Identity))
	status, e := cw.svc.ReconcileMediaSignals(ctx, start.Call.ID)
	require.NoError(t, e)
	require.Equal(t, "legacy_media_unverified", status.Reason)
	lease, e := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, e)
	require.NoError(t, lease.Confirm(ctx))
	require.NoError(t, lease.Close(ctx, true))
	original := cw.svc.media
	cw.svc.media = func(server MediaServer, credential MediaCredential) (MediaBackend, error) {
		b, e := original(server, credential)
		return &mediaSignalFailReadback{MediaBackend: b, fail: true}, e
	}
	status, e = cw.svc.ReconcileMediaSignals(ctx, start.Call.ID)
	require.Error(t, e)
	require.Equal(t, "pending", status.State)
	r, e := cw.svc.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, e)
	require.Equal(t, "pending", cw.svc.mediaCleanupStatus(ctx, *r).State)
	cw.svc.media = original
	status, e = cw.svc.ReconcileMediaSignals(ctx, start.Call.ID)
	require.NoError(t, e)
	require.Equal(t, "clear", status.State)
	// A participant not represented by any native session/journal is removed
	// even when no pending-intent record names it.
	cw.media.setPresent(start.Call.ID, person("unadmitted-replay"))
	status, e = cw.svc.ReconcileMediaSignals(ctx, start.Call.ID)
	require.NoError(t, e)
	require.Equal(t, "clear", status.State)
	require.Empty(t, cw.media.present[start.Call.ID])
}
func TestMediaSignalDatabaseEndedSessionAndMissingProofDenied(t *testing.T) {
	cw, host, start := mediaSignalFixture(t, "signal-session")
	if cw == nil {
		return
	}
	ctx := context.Background()
	r, e := cw.svc.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, e)
	server, e := cw.svc.store.MediaServerByID(ctx, r.MediaServerID)
	require.NoError(t, e)
	credential, e := cw.svc.credential(ctx, *server)
	require.NoError(t, e)
	unbound, e := MintPass(credential.Key, credential.Secret, callrules.KindPerson, r.MediaRoom, start.Join.Identity, "Unbound", nil, time.Minute)
	require.NoError(t, e)
	_, e = cw.svc.BeginMediaSignal(ctx, unbound)
	require.ErrorContains(t, e, "media_admission_invalid")
	row, _, e := cw.svc.session(ctx, cw.ref, host.Token, cw.clock.Now())
	require.NoError(t, e)
	release, e := cw.svc.gates(ctx, accountGate(row.KeyID))
	require.NoError(t, e)
	e = cw.svc.store.updateAccountSession(ctx, cw.owner, row, map[string]any{"endedAt": versionStamp(cw.clock.Now())}, cw.clock.Now())
	release()
	require.NoError(t, e)
	_, e = cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.ErrorContains(t, e, "media_session_closed")
}

func TestMediaSignalDatabaseOverlappingResumeFencesOldConnection(t *testing.T) {
	cw, _, start := mediaSignalFixture(t, "signal-overlap")
	if cw == nil {
		return
	}
	ctx := context.Background()
	first, e := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, e)
	require.NoError(t, first.Confirm(ctx))
	second, e := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, e)
	var sent bool
	require.Error(t, first.Forward(ctx, func(context.Context) error { sent = true; return nil }))
	require.False(t, sent)
	require.NoError(t, first.Close(ctx, true), "old close must not drain a replacement connection")
	require.NoError(t, second.Confirm(ctx))
	require.NoError(t, second.Forward(ctx, func(context.Context) error { return nil }))
	require.NoError(t, second.Close(ctx, true))
}

func TestMediaSignalDatabaseUnknownJournalIsBounded(t *testing.T) {
	cw, _, start := mediaSignalFixture(t, "signal-bounds")
	if cw == nil {
		return
	}
	ctx := context.Background()
	r, _, _, binding, _, e := cw.svc.authenticateMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, e)
	release, e := cw.svc.gates(ctx, roomGate(r.ID))
	require.NoError(t, e)
	j, e := cw.svc.store.mediaSignals(ctx, *r)
	require.NoError(t, e)
	for i := 0; i < maxMediaSignalIntents; i++ {
		b := binding
		b.Identity = fmt.Sprintf("uncertain-%d", i)
		j.Intents = append(j.Intents, mediaSignalIntent{ID: fmt.Sprintf("intent-%d", i), Binding: b, State: "uncertain", CreatedAt: cw.clock.Now(), ExpiresAt: cw.clock.Now().Add(mediaHandshakeTTL)})
	}
	e = cw.svc.store.saveMediaSignals(ctx, *r, j, cw.clock.Now())
	release()
	require.NoError(t, e)
	_, e = cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.ErrorContains(t, e, "media_barrier_limit")
	got, e := cw.svc.store.mediaSignals(ctx, *r)
	require.NoError(t, e)
	require.Len(t, got.Intents, maxMediaSignalIntents)
}

func TestMediaSignalDatabaseCredentialRevocationDeniesIssuedPass(t *testing.T) {
	cw, _, start := mediaSignalFixture(t, "signal-credential")
	if cw == nil {
		return
	}
	ctx := context.Background()
	k := accountRegister(t, cw, newSoftwareAuthenticator(t))
	joined, _, e := cw.svc.Join(ctx, cw.ref, k.result.Session, callrules.JoinBody{Token: start.Call.GuestToken, Name: "Secured guest", Confirm: confirmed})
	require.NoError(t, e)
	lease, e := cw.svc.BeginMediaSignal(ctx, joined.Pass)
	require.NoError(t, e)
	require.NoError(t, lease.Confirm(ctx))
	credential, e := cw.svc.store.credentialByID(ctx, cw.owner, k.result.Credentials[0].ID)
	require.NoError(t, e)
	release, e := cw.svc.gates(ctx, accountGate(k.result.Session.KeyID))
	require.NoError(t, e)
	e = cw.svc.store.updateCredential(ctx, cw.owner, *credential, map[string]any{"revokedAt": versionStamp(cw.clock.Now())}, cw.clock.Now())
	release()
	require.NoError(t, e)
	_, e = cw.svc.BeginMediaSignal(ctx, joined.Pass)
	require.Error(t, e)
	require.Error(t, lease.Forward(ctx, func(context.Context) error { t.Fatal("revoked credential forwarded"); return nil }))
}

func TestMediaSignalDatabaseDelegationRevisionAndRevocationDenyPass(t *testing.T) {
	cw, k, _ := commerceFixture(t, "signal-delegation")
	if cw == nil {
		return
	}
	ctx := context.Background()
	cleanupDelegationTestRows(t, cw)
	commerceVerified(t, cw, k)
	credential, grantID := delegationTestGrant(t, cw, k, delegationTestRequest(cw))
	quoted, e := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "quote", ParticipantLimit: 2, DurationMinutes: 60})
	require.NoError(t, e)
	result, e := delegationTestAgent(t, cw, cw.svc, ctx, cw.ref, credential, callrules.AgentRequest{Action: "start", IdempotencyKey: "signal-agent-start", QuoteID: quoted.(*callrules.CallQuote).QuoteID, Name: "Assistant", Terms: callrules.AgentTermsVersion, Participation: "adults_only"})
	require.NoError(t, e)
	start := result.(callrules.StartResult)
	lease, e := cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.NoError(t, e)
	require.NoError(t, lease.Confirm(ctx))
	release, e := cw.svc.gates(ctx, accountGate(k.result.Session.KeyID), delegationGate(grantID))
	require.NoError(t, e)
	d, e := cw.svc.store.delegation(ctx, cw.owner, grantID)
	require.NoError(t, e)
	d.State.ApprovalRevision++
	e = cw.svc.store.saveDelegation(ctx, cw.owner, d, cw.clock.Now())
	release()
	require.NoError(t, e)
	_, e = cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.ErrorContains(t, e, "agent_approval_changed")
	release, e = cw.svc.gates(ctx, accountGate(k.result.Session.KeyID), delegationGate(grantID))
	require.NoError(t, e)
	d, e = cw.svc.store.delegation(ctx, cw.owner, grantID)
	require.NoError(t, e)
	d.State.RevokedAt = cw.clock.Now()
	e = cw.svc.store.saveDelegation(ctx, cw.owner, d, cw.clock.Now())
	release()
	require.NoError(t, e)
	_, e = cw.svc.BeginMediaSignal(ctx, start.Join.Pass)
	require.ErrorContains(t, e, "delegation_inactive")
}

func TestMediaSignalDatabaseSamplerRequiresExactConsentedLease(t *testing.T) {
	cw, _, host, start, _ := processingFixture(t, true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	consentProcessing(t, cw, host, true, true, true)
	b, e := cw.svc.processingLease(ctx, cw.ref, start.Call.ID, "operator", "", false, 100)
	require.NoError(t, e)
	lease, e := cw.svc.BeginMediaSignal(ctx, b.MediaToken)
	require.NoError(t, e)
	require.NoError(t, lease.Confirm(ctx))
	consentProcessing(t, cw, host, false, false, false)
	_, e = cw.svc.BeginMediaSignal(ctx, b.MediaToken)
	require.ErrorContains(t, e, "media_processing_lease_closed")
	require.Error(t, lease.Forward(ctx, func(context.Context) error { t.Fatal("withdrawn sampler forwarded"); return nil }))
}

func TestMediaSignalDatabaseResourceExemptionRequiresExactLiveIngress(t *testing.T) {
	cw, k, start, _ := resourceFixture(t, "signal-resource")
	if cw == nil {
		return
	}
	ctx := context.Background()
	source := registerTestResource(t, cw, k, "signal-resource-register")
	_, e := cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "start", ResourceID: source.ID, RoomID: start.Call.ID, ExpectedRevision: source.Revision, RequestID: "signal-resource-start"})
	require.NoError(t, e)
	d, e := cw.svc.store.resourceState(ctx, cw.owner, cw.site, k.result.Session.KeyID)
	require.NoError(t, e)
	require.Len(t, d.State.Leases, 1)
	r, e := cw.svc.store.roomByID(ctx, cw.owner, start.Call.ID)
	require.NoError(t, e)
	allowed, e := cw.svc.mediaResourceIdentityAllowed(ctx, *r, d.State.Leases[0].Identity)
	require.NoError(t, e)
	require.True(t, allowed)
	allowed, e = cw.svc.mediaResourceIdentityAllowed(ctx, *r, "resource-forged")
	require.NoError(t, e)
	require.False(t, allowed)
	_, e = cw.svc.Resource(ctx, cw.ref, k.result.Session.Token, callrules.ResourceRequest{Action: "stop", RoomID: r.ID, ResourceID: source.ID})
	require.NoError(t, e)
	allowed, e = cw.svc.mediaResourceIdentityAllowed(ctx, *r, d.State.Leases[0].Identity)
	require.NoError(t, e)
	require.False(t, allowed)
}

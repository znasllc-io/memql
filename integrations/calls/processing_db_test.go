package calls

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/core/common"
)

type processingTestMedia struct {
	MediaBackend
	mu         sync.Mutex
	packets    [][]byte
	recipients [][]string
}

func (b *processingTestMedia) PublishProcessing(_ context.Context, _ string, data []byte, recipients []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.packets = append(b.packets, append([]byte(nil), data...))
	b.recipients = append(b.recipients, append([]string(nil), recipients...))
	return nil
}
func processingFixture(t *testing.T, insights bool) (*callWorld, accountTestKey, callrules.Session, callrules.StartResult, *processingTestMedia) {
	return processingDurationFixture(t, insights, 60)
}
func processingDurationFixture(t *testing.T, insights bool, minutes int) (*callWorld, accountTestKey, callrules.Session, callrules.StartResult, *processingTestMedia) {
	t.Helper()
	cw, k, _ := commerceFixture(t, "processing")
	if cw == nil {
		return nil, k, callrules.Session{}, callrules.StartResult{}, nil
	}
	commerceVerified(t, cw, k)
	if minutes > 60 {
		commercePurchase(t, cw, k, "processing-long-call-funds")
	}
	ctx := context.Background()
	_, e := cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "processingEnabled": 1})
	require.NoError(t, e)
	t.Cleanup(func() {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept IN (?, ?, ?)", processingConcept, processingReceiptConcept, processingRateConcept).Where("payload->>'siteId' = ?", siteConcept+":"+cw.site).Exec(ctx)
	})
	quoted := commerceDo(t, cw, k, callrules.AccountRequest{Action: "quote_call", ParticipantLimit: 3, DurationMinutes: minutes, Insights: insights})
	host := *k.result.Session
	start, e := cw.svc.Start(ctx, host, callrules.StartBody{Name: "Host", Confirm: confirmed, QuoteID: quoted.Quote.QuoteID, IdempotencyKey: "processing-start-0001"})
	require.NoError(t, e)
	backend, e := cw.svc.backendFor(ctx, *mustProcessingRoom(t, cw, start.Call.ID))
	require.NoError(t, e)
	media := &processingTestMedia{MediaBackend: backend}
	cw.svc.media = func(MediaServer, MediaCredential) (MediaBackend, error) { return media, nil }
	call, e := cw.svc.SessionCall(ctx, host)
	require.NoError(t, e)
	cw.media.setPresent(start.Call.ID, MediaParticipant{Identity: call.Identity, Tracks: []MediaTrack{{SID: "TR_microphone", Source: "microphone"}, {SID: "TR_camera", Source: "camera"}}})
	return cw, k, host, start, media
}
func mustProcessingRoom(t *testing.T, cw *callWorld, room string) *roomRow {
	t.Helper()
	r, e := cw.svc.store.roomByID(context.Background(), cw.owner, room)
	require.NoError(t, e)
	require.NotNil(t, r)
	return r
}
func consentProcessing(t *testing.T, cw *callWorld, sess callrules.Session, safety, transcription, insights bool) callrules.SeatCall {
	t.Helper()
	ctx := context.Background()
	c, e := cw.svc.SessionCall(ctx, sess)
	require.NoError(t, e)
	require.NotNil(t, c.Processing)
	e = cw.svc.Host(ctx, sess, callrules.HostActionRequest{Action: "processing_consent", Processing: &callrules.ProcessingConsentRequest{RoomID: c.RoomID, Identity: c.Identity, Revision: c.Processing.Consent.Revision, Safety: safety, Transcription: transcription, Insights: insights}})
	require.NoError(t, e)
	c, e = cw.svc.SessionCall(ctx, sess)
	require.NoError(t, e)
	return c
}
func speechEvent(b processingBundle) processingEvent {
	for _, t := range b.Tracks {
		if t.TranscriptionAllowed {
			return processingEvent{Kind: "transcript", RoomID: b.RoomID, ParticipantID: t.ParticipantID, MediaIdentity: t.MediaIdentity, TrackID: t.TrackID, DisplayName: "forged worker name", Epoch: t.Epoch, ConsentRevision: t.ConsentRevision, Source: t.Source, StartSample: 0, EndSample: 16000, Text: "Hello from this speaker", Language: "en", SegmentID: fmtSegment(t.Epoch, 0, 16000)}
		}
	}
	return processingEvent{}
}
func fmtSegment(epoch, start, end int64) string { return fmt.Sprintf("%d:%d:%d", epoch, start, end) }
func TestProcessingConsentLeasePublicationAndWithdrawal(t *testing.T) {
	cw, _, host, start, media := processingFixture(t, true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	room := start.Call.ID
	_, e := cw.svc.processingLease(ctx, cw.ref, room, "operator", "", false, 100)
	require.ErrorContains(t, e, "processing_no_consent")
	c := consentProcessing(t, cw, host, true, true, true)
	require.NotEqual(t, "0", c.Processing.Consent.Revision)
	b, e := cw.svc.processingLease(ctx, cw.ref, room, "operator", "", false, 100)
	require.NoError(t, e)
	require.Len(t, b.Tracks, 2)
	allowed, e := cw.svc.processingSampler(ctx, *mustProcessingRoom(t, cw, room), b.MediaIdentity)
	require.NoError(t, e)
	require.True(t, allowed)
	allowed, e = cw.svc.processingSampler(ctx, *mustProcessingRoom(t, cw, room), "sampler-forged")
	require.NoError(t, e)
	require.False(t, allowed)
	ev := speechEvent(b)
	bad := ev
	bad.MediaIdentity = "someone-else"
	_, e = cw.svc.processingPublish(ctx, cw.ref, room, "operator", b.LeaseID, b.Revision, bad)
	require.ErrorContains(t, e, "processing_authority_changed")
	receipt, e := cw.svc.processingPublish(ctx, cw.ref, room, "operator", b.LeaseID, b.Revision, ev)
	require.NoError(t, e)
	require.Equal(t, "published", receipt["status"])
	duplicate, e := cw.svc.processingPublish(ctx, cw.ref, room, "operator", b.LeaseID, b.Revision, ev)
	require.NoError(t, e)
	require.Equal(t, "duplicate", duplicate["status"])
	require.Len(t, media.packets, 1)
	var packet map[string]any
	require.NoError(t, json.Unmarshal(media.packets[0], &packet))
	require.Equal(t, "Host", packet["display_name"])
	require.Equal(t, ev.Text, packet["text"])
	journal, e := cw.svc.store.processing(ctx, cw.owner, room)
	require.NoError(t, e)
	raw, _ := json.Marshal(journal)
	require.NotContains(t, string(raw), ev.Text)
	require.NotContains(t, string(raw), b.MediaToken)
	renewed, e := cw.svc.processingLease(ctx, cw.ref, room, "operator", b.LeaseID, false, 100)
	require.NoError(t, e)
	require.Equal(t, b.Revision, renewed.Revision)
	require.Equal(t, b.MediaIdentity, renewed.MediaIdentity)
	consentProcessing(t, cw, host, false, false, false)
	_, e = cw.svc.processingPublish(ctx, cw.ref, room, "operator", b.LeaseID, b.Revision, ev)
	require.ErrorContains(t, e, "processing_lease_expired")
	require.Contains(t, cw.media.removals(), fakeMediaRemoval{Server: cw.media.created[0].Server, Room: room, Identity: b.MediaIdentity})
}
func TestProcessingBaseVisualCapacityResetAndAdmissionBinding(t *testing.T) {
	cw, _, host, start, _ := processingFixture(t, false)
	if cw == nil {
		return
	}
	ctx := context.Background()
	room := start.Call.ID
	c, e := cw.svc.SessionCall(ctx, host)
	require.NoError(t, e)
	require.True(t, c.Processing.Capabilities.Safety)
	require.False(t, c.Processing.Capabilities.Transcription)
	_, e = cw.svc.processingLease(ctx, cw.ref, room, "operator", "", false, 0)
	require.ErrorContains(t, e, "processing_capacity", "zero operator budget is an explicit capacity refusal even before a track is eligible")
	req := callrules.HostActionRequest{Action: "processing_consent", Processing: &callrules.ProcessingConsentRequest{RoomID: room, Identity: c.Identity, Revision: "0", Transcription: true}}
	require.ErrorContains(t, cw.svc.Host(ctx, host, req), "processing_not_available")
	c = consentProcessing(t, cw, host, true, false, false)
	_, e = cw.svc.processingLease(ctx, cw.ref, room, "operator", "", false, 0)
	require.ErrorContains(t, e, "processing_capacity")
	c, e = cw.svc.SessionCall(ctx, host)
	require.NoError(t, e)
	require.Equal(t, "processing_capacity", c.Processing.Reason)
	require.False(t, c.Processing.Active)
	b, e := cw.svc.processingLease(ctx, cw.ref, room, "operator", "", false, 100)
	require.NoError(t, e)
	require.Len(t, b.Tracks, 1)
	require.Equal(t, "camera", b.Tracks[0].Source)
	reset, e := cw.svc.processingLease(ctx, cw.ref, room, "operator", b.LeaseID, true, 100)
	require.NoError(t, e)
	require.Greater(t, reset.Revision, b.Revision)
	require.NotEqual(t, b.MediaIdentity, reset.MediaIdentity)
	_, e = cw.svc.processingLease(ctx, cw.ref, room, "operator", b.LeaseID, true, 100)
	require.ErrorContains(t, e, "processing_lease_busy")
	req.Processing = &callrules.ProcessingConsentRequest{RoomID: room, Identity: "previous-admission", Revision: "0", Safety: true}
	require.ErrorContains(t, cw.svc.Host(ctx, host, req), "processing_admission_changed")
	cw.clock.Advance(31 * time.Second)
	after, e := cw.svc.processingLease(ctx, cw.ref, room, "operator", "", false, 100)
	require.NoError(t, e)
	require.Greater(t, after.Revision, reset.Revision)
}
func TestProcessingConcurrentLeaseAndPublicationChanges(t *testing.T) {
	cw, _, host, start, _ := processingFixture(t, true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	room := start.Call.ID
	consentProcessing(t, cw, host, true, true, false)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := cw.svc.processingLease(ctx, cw.ref, room, "operator", "", false, 100)
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else {
			require.ErrorContains(t, e, "processing_lease_busy")
		}
	}
	require.Equal(t, 1, success)
	journal, e := cw.svc.store.processing(ctx, cw.owner, room)
	require.NoError(t, e)
	// Native track changes invalidate every old result before publication.
	cw.media.setPresent(room, MediaParticipant{Identity: journal.Lease.Tracks[0].MediaIdentity, Tracks: []MediaTrack{{SID: "TR_replaced", Source: "microphone"}}})
	b := processingBundle{RoomID: room, Tracks: journal.Lease.Tracks}
	ev := speechEvent(b)
	_, e = cw.svc.processingPublish(ctx, cw.ref, room, "operator", journal.Lease.ID, journal.Epoch, ev)
	require.ErrorContains(t, e, "processing_tracks_changed")
}

func TestProcessingCredentialLossAndReentryCannotReuseConsent(t *testing.T) {
	cw, k, host, start, _ := processingFixture(t, true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	room := start.Call.ID
	guestJoin, _, joinErr := cw.join(start.Call.GuestToken, "Guest")
	require.NoError(t, joinErr)
	c := consentProcessing(t, cw, host, true, true, false)
	b, e := cw.svc.processingLease(ctx, cw.ref, room, "operator", "", false, 100)
	require.NoError(t, e)
	cw.media.setPresent(room, MediaParticipant{Identity: c.Identity, Tracks: []MediaTrack{{SID: "TR_microphone", Source: "microphone"}, {SID: "TR_camera", Source: "camera"}}}, MediaParticipant{Identity: guestJoin.Identity})
	old := callrules.HostActionRequest{Action: "processing_consent", Processing: &callrules.ProcessingConsentRequest{RoomID: room, Identity: c.Identity, Revision: c.Processing.Consent.Revision, Safety: true, Transcription: true}}
	require.NoError(t, cw.svc.LeaveWithOptions(ctx, host, callrules.LeaveRequest{Mode: callrules.LeaveKeepRunning, LeaveToken: c.LeaveToken, IdempotencyKey: "processing-leave-0001"}))
	_, host, e = cw.svc.Join(ctx, cw.ref, &host, callrules.JoinBody{HostToken: start.Call.HostToken, Name: "Host returned", Confirm: confirmed})
	require.NoError(t, e)
	c, e = cw.svc.SessionCall(ctx, host)
	require.NoError(t, e)
	require.Equal(t, "0", c.Processing.Consent.Revision)
	require.False(t, c.Processing.Consent.Safety)
	require.ErrorContains(t, cw.svc.Host(ctx, host, old), "processing_admission_changed")
	_, e = cw.svc.processingPublish(ctx, cw.ref, room, "operator", b.LeaseID, b.Revision, speechEvent(b))
	require.ErrorContains(t, e, "processing_tracks_changed")
	cw.media.setPresent(room, MediaParticipant{Identity: c.Identity, Tracks: []MediaTrack{{SID: "TR_new", Source: "microphone"}}})
	consentProcessing(t, cw, host, false, true, false)
	b, e = cw.svc.processingLease(ctx, cw.ref, room, "operator", "", false, 100)
	require.NoError(t, e)
	cred, e := cw.svc.store.credentialByID(ctx, cw.owner, k.result.Credentials[0].ID)
	require.NoError(t, e)
	require.NoError(t, cw.svc.store.updateCredential(ctx, cw.owner, *cred, map[string]any{"revokedAt": versionStamp(cw.clock.Now())}, cw.clock.Now()))
	_, e = cw.svc.processingPublish(ctx, cw.ref, room, "operator", b.LeaseID, b.Revision, speechEvent(b))
	require.ErrorContains(t, e, "processing_tracks_changed")
	allowed, e := cw.svc.processingSampler(ctx, *mustProcessingRoom(t, cw, room), b.MediaIdentity)
	require.NoError(t, e)
	require.False(t, allowed)
}
func TestProcessingOperatorDiscoveryPolicyDisableAndFamilyExclusion(t *testing.T) {
	cw, _, host, start, _ := processingFixture(t, true)
	if cw == nil {
		return
	}
	ctx := context.Background()
	room := start.Call.ID
	args := map[string]any{"siteId": cw.site, "roomId": room}
	_, e := cw.calls.handleProcessingLease(actorContext(cw.developer, auth.RoleDeveloper), args, 0)
	require.ErrorContains(t, e, "processing_operator_required")
	operator := actorContext(cw.owner, auth.RoleOwner)
	_, e = cw.calls.handleProcessingLease(common.ContextWithRun(operator, common.RunContext{RunId: "never-journal"}), args, 0)
	require.ErrorContains(t, e, "outside runs")
	consentProcessing(t, cw, host, true, true, false)
	rows, e := cw.calls.handleProcessingRooms(operator, map[string]any{"siteId": cw.site}, 0)
	require.NoError(t, e)
	require.Len(t, rows, 1)
	require.Contains(t, string(rows[0].Payload), room)
	_, e = cw.builtin(actorContext(cw.developer, auth.RoleDeveloper), "callsPolicySet", map[string]any{"siteId": cw.site, "processingEnabled": 0})
	require.NoError(t, e)
	_, e = cw.svc.processingLease(ctx, cw.ref, room, "operator", "", false, 100)
	require.ErrorContains(t, e, "processing_not_available")
	partial := consentProcessing(t, cw, host, true, false, false)
	require.True(t, partial.Processing.Consent.Safety)
	c := consentProcessing(t, cw, host, false, false, false)
	require.False(t, c.Processing.Available)
	r := mustProcessingRoom(t, cw, room)
	config, e := decodeUsageConfig(r.UsageConfig)
	require.NoError(t, e)
	config.ParticipationMode = familyMode
	raw, _ := json.Marshal(config)
	r.UsageConfig = string(raw)
	p := callrules.DefaultPolicy()
	p.ProcessingEnabled = true
	require.False(t, processingRoomEligible(*r, p, cw.clock.Now()))
}

func TestProcessingAgentScopeCannotConsentForOthersAndReductionAllowsWithdrawal(t *testing.T) {
	cw, k, host, start, _ := processingFixture(t, false)
	if cw == nil {
		return
	}
	ctx := context.Background()
	cleanupDelegationTestRows(t, cw)
	portraitCleanup(t, cw)
	grant := delegationTestRequest(cw)
	grant.Scopes = []string{"read", "join", "processing"}
	token, grantID := delegationTestGrant(t, cw, k, grant)
	pending := requestGuestAgent(t, cw, token, start, "processing-agent-request-01")
	hostView, e := cw.svc.SessionCall(ctx, host)
	require.NoError(t, e)
	require.NoError(t, cw.svc.Host(ctx, host, callrules.HostActionRequest{Action: "agent_approve", RequestID: pending.RequestID, ReviewDigest: hostView.PendingAgents[0].ReviewDigest}))
	raw, e := cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "guest_join", GuestToken: start.Call.GuestToken, CallID: start.Call.ID, RequestID: pending.RequestID, IdempotencyKey: "processing-agent-admit-01", Acceptance: acceptedAgent(t, cw, token)})
	require.NoError(t, e)
	joined := raw.(callrules.JoinResult)
	raw, e = cw.svc.Agent(ctx, cw.ref, token, callrules.AgentRequest{Action: "session"})
	require.NoError(t, e)
	session := raw.(callrules.SeatCall)
	request := callrules.AgentRequest{Action: "processing_consent", Processing: &callrules.ProcessingConsentRequest{RoomID: session.RoomID, Identity: hostView.Identity, Revision: "0", Safety: true}}
	_, e = cw.svc.Agent(ctx, cw.ref, token, request)
	require.ErrorContains(t, e, "processing_admission_changed")
	request.Processing.Identity = session.Identity
	raw, e = cw.svc.Agent(ctx, cw.ref, token, request)
	require.NoError(t, e)
	state := raw.(*callrules.ProcessingStatus)
	require.True(t, state.Consent.Safety)
	cw.media.setPresent(start.Call.ID, MediaParticipant{Identity: hostView.Identity}, MediaParticipant{Identity: joined.Identity, Tracks: []MediaTrack{{SID: "TR_agent_camera", Source: "camera"}}})
	bundle, e := cw.svc.processingLease(ctx, cw.ref, start.Call.ID, "operator", "", false, 100)
	require.NoError(t, e)
	require.Equal(t, "agent_video", bundle.Tracks[0].Source)
	// Simulate a committed human-approved scope reduction; fresh admission is not
	// implied by it. Removing processing must still allow an explicit withdrawal.
	d, e := cw.svc.store.delegation(ctx, cw.owner, grantID)
	require.NoError(t, e)
	d.State.Request.Scopes = []string{"read", "join"}
	d.State.ApprovalRevision++
	require.NoError(t, cw.svc.store.saveDelegation(ctx, cw.owner, d, cw.clock.Now()))
	request.Processing.Revision = state.Consent.Revision
	_, e = cw.svc.Agent(ctx, cw.ref, token, request)
	require.ErrorContains(t, e, "delegation_scope")
	request.Processing.Safety = false
	raw, e = cw.svc.Agent(ctx, cw.ref, token, request)
	require.NoError(t, e)
	require.False(t, raw.(*callrules.ProcessingStatus).Consent.Safety)
}

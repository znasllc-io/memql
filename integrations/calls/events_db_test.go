package calls

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"

	callrules "github.com/znasllc-io/memql/component/calls"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// events_db_test.go -- the media server's webhooks (events.go) against a real engine and
// database: a delivery staged as the inbound receiver stages one, then worked by the
// callsApplyMediaEvent builtin as the applyMediaEvent automation runs it. The media
// server is the fake of service_db_test.go; its participant list (setPresent) is what a
// takeover reads and what reconciliation compares against.
//
// Postgres-gated: an unreachable database skips, and MEMQL_REQUIRE_DB=1 turns the skip
// into a failure.

const inboundRequestConcept = "v1:platform:inboundRequest"

// receiverContext is the context the inbound receiver stages a delivery under
// (component/inbound systemActorContext): its named actor, at internal origin, because
// stageInboundRequest is @serverOnly.
func receiverContext() context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithToken(context.Background(), &auth.TokenInfo{
		Subject: "system:inbound",
		Claims:  map[string]any{"sub": "system:inbound", "role": "system"},
	}))
}

// automationContext is the context the applyMediaEvent automation's builtin runs under:
// the actor the executor mints for an automation of the engine's own tree, at internal
// origin. The origin is stated as a constant, as the maintenance list's gate asks of a
// test (component/automations maintenance_actor_gate_test.go).
func automationContext() context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(),
		auth.AutomationActor(auth.AutomationOriginEngine, applyMediaEventAutomation)))
}

// tick moves the clock two seconds and answers the whole second it is in: a media
// server's event time, strictly after every stamp written before the move.
func (cw *callWorld) tick() time.Time {
	cw.clock.Advance(2 * time.Second)
	return cw.clock.Now().Truncate(time.Second)
}

// advanceTo moves the clock to at.
func (cw *callWorld) advanceTo(at time.Time) {
	cw.t.Helper()
	require.False(cw.t, at.Before(cw.clock.Now()), "the clock does not run backwards")
	cw.clock.Advance(at.Sub(cw.clock.Now()))
}

// eventBody is one media server webhook about the media room `room`, as LiveKit's
// notifier writes it (webhook_sdk_test.go liveKitBody). identity is the participant's,
// empty for a room's own event; track is the published track, or nil.
func eventBody(t *testing.T, kind, room, identity string, at time.Time, track *livekit.TrackInfo) []byte {
	t.Helper()
	ev := &livekit.WebhookEvent{Event: kind, Id: "EV_" + hex.EncodeToString(randomBytes(t, 6)), Room: &livekit.Room{Sid: "RM_" + room, Name: room}, Track: track}
	if !at.IsZero() {
		ev.CreatedAt = at.Unix()
	}
	if identity != "" {
		ev.Participant = &livekit.ParticipantInfo{Sid: "PA_" + identity, Identity: identity}
	}
	return liveKitBody(t, ev)
}

func screenShare(sid string) *livekit.TrackInfo {
	return &livekit.TrackInfo{Sid: sid, Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_SCREEN_SHARE}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// stage writes a delivery as the inbound receiver does, at requestID (a new one when
// empty), and answers the row's canonical id: what the automation hands its builtin. The
// receiver's dedupeKey is left out: it is recorded for a reader and decides nothing here,
// and the row id stands in for the receiver's derivation (the body's digest), which
// app/transport_inbound_livekit_test.go holds.
func (cw *callWorld) stage(requestID string, body []byte, verified bool, tier, source string) string {
	cw.t.Helper()
	if requestID == "" {
		requestID = "inb" + hex.EncodeToString(randomBytes(cw.t, 16))
	} else {
		requestID = bare(requestID)
	}
	args := map[string]any{
		"requestId": requestID, "source": source, "medium": "webhook", "body": string(body),
		"contentType": "application/webhook+json", "headersJson": "{}",
		"signatureVerified": verified, "receivedAt": cw.clock.Now().Format(time.RFC3339Nano),
	}
	if tier != "" {
		args["verifiedBy"] = tier
	}
	call, err := langparser.RenderCall("stageInboundRequest", args)
	require.NoError(cw.t, err)
	_, err = cw.eng.Execute(receiverContext(), "mutation "+call)
	require.NoError(cw.t, err)
	canonical := inboundRequestConcept + ":" + requestID
	cw.rows = append(cw.rows, canonical)
	return canonical
}

// applyDelivery runs the applyMediaEvent automation's builtin on a staged delivery, and
// answers its reply.
func (cw *callWorld) applyDelivery(requestID string) (map[string]any, error) {
	cw.t.Helper()
	nodes, err := cw.calls.handleApplyMediaEvent(automationContext(), map[string]any{"requestId": requestID}, 0)
	if err != nil {
		return nil, err
	}
	require.Len(cw.t, nodes, 1)
	var reply map[string]any
	require.NoError(cw.t, json.Unmarshal(nodes[0].Payload, &reply))
	return reply, nil
}

// deliver is one webhook from the media server, start to finish: staged as a verified
// delivery of its source by the registered tier, then worked. It answers what the
// builtin did with it, and holds that the delivery was stamped processed.
func (cw *callWorld) deliver(body []byte) string {
	cw.t.Helper()
	id := cw.stage("", body, true, "registered", WebhookSource)
	reply, err := cw.applyDelivery(id)
	require.NoError(cw.t, err)
	require.Equal(cw.t, "processed", cw.delivery(id)["status"], "a worked delivery was not stamped processed")
	return reply["outcome"].(string)
}

// delivery is a staged delivery's newest version.
func (cw *callWorld) delivery(canonicalID string) map[string]any {
	cw.t.Helper()
	payload, _ := latestPayload(cw.t, cw.db, inboundRequestConcept, canonicalID)
	return payload
}

// mutedTracks is the track sids the media server was asked to mute, in order.
func (cw *callWorld) mutedTracks() []string {
	var out []string
	for _, m := range cw.media.mutes() {
		out = append(out, m.Track)
	}
	return out
}

// ---------------------------------------------------------------------------
// who is connected
// ---------------------------------------------------------------------------

// TestParticipantJoinedPutsASeatInTheCallAndRemovesAStranger: a participant_joined
// moves an admitted seat in_call and records its time as the seat's last sign of life;
// a seat in the call already is seen again. An identity that holds no seat in the call
// now -- one nobody let in, a seat that left and connects again on the pass it still
// holds, a removed seat -- is disconnected again, because a self-hosted media server
// cannot revoke a pass, and the seat is left as it was. A media room no call has, an
// event the service does not act on and an event with no time change nothing.
func TestParticipantJoinedPutsASeatInTheCallAndRemovesAStranger(t *testing.T) {
	cw := newCallWorld(t, "joined", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	hostSess, started := cw.started("Host")
	room := started.Call.ID
	in1, g1, err := cw.join(started.Call.GuestToken, "First")
	require.NoError(t, err)
	in2, g2, err := cw.join(started.Call.GuestToken, "Second")
	require.NoError(t, err)

	at := cw.tick()
	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventParticipantJoined, room, started.Join.Identity, at, nil)))
	host := cw.row(seatConcept, hostSess.SeatID)
	require.Equal(t, "in_call", host["status"], "a participant_joined left the seat admitted")
	require.Equal(t, at, rowTime(host, "lastSeenAt"), "the seat's last sign of life is not the event's time")
	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventParticipantJoined, room, in1.Identity, at, nil)))
	require.Equal(t, "in_call", cw.row(seatConcept, g1.SeatID)["status"])
	again := cw.tick()
	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventParticipantJoined, room, started.Join.Identity, again, nil)))
	require.Equal(t, again, rowTime(cw.row(seatConcept, hostSess.SeatID), "lastSeenAt"), "a seat in the call already was not seen again")

	removed := func(identity string) {
		t.Helper()
		require.Contains(t, cw.media.removals(), fakeMediaRemoval{Server: BootstrapMediaServerID, Room: room, Identity: identity},
			"an identity with no seat in the call was left connected")
	}
	require.Equal(t, outcomeRemoved, cw.deliver(eventBody(t, eventParticipantJoined, room, "stranger-identity", cw.tick(), nil)))
	removed("stranger-identity")

	require.NoError(t, cw.svc.Leave(ctx, g1))
	before := len(cw.media.removals())
	require.Equal(t, outcomeRemoved, cw.deliver(eventBody(t, eventParticipantJoined, room, in1.Identity, cw.tick(), nil)))
	require.Len(t, cw.media.removals(), before+1, "a seat that left, connecting again on its pass, was not disconnected")
	require.Equal(t, "left", cw.row(seatConcept, g1.SeatID)["status"], "a connection took a seat that left back into the call")

	require.NoError(t, cw.act(hostSess, callrules.HostRemove, g2.SeatID))
	before = len(cw.media.removals())
	require.Equal(t, outcomeRemoved, cw.deliver(eventBody(t, eventParticipantJoined, room, in2.Identity, cw.tick(), nil)))
	require.Len(t, cw.media.removals(), before+1, "a removed seat, connecting again on its pass, was not disconnected")
	require.Equal(t, "removed", cw.row(seatConcept, g2.SeatID)["status"])

	// Nothing to act on.
	seatVersions := cw.versions(seatConcept, hostSess.SeatID)
	roomVersions := cw.versions(roomConcept, room)
	before = len(cw.media.removals())
	require.Equal(t, outcomeUnknownRoom, cw.deliver(eventBody(t, eventParticipantJoined, "no-such-room", "someone", cw.tick(), nil)))
	for _, kind := range []string{"room_started", "track_unpublished", "participant_connection_aborted"} {
		require.Equal(t, outcomeIgnored, cw.deliver(eventBody(t, kind, room, started.Join.Identity, cw.tick(), nil)), kind)
	}
	require.Equal(t, outcomeIgnored, cw.deliver(eventBody(t, eventParticipantLeft, room, started.Join.Identity, time.Time{}, nil)),
		"an event with no time was applied: it cannot be ordered against anything")
	require.Equal(t, seatVersions, cw.versions(seatConcept, hostSess.SeatID), "an event with nothing to act on wrote the seat")
	require.Equal(t, roomVersions, cw.versions(roomConcept, room), "an event with nothing to act on wrote the room")
	require.Len(t, cw.media.removals(), before, "an event with nothing to act on disconnected somebody")
}

// TestAParticipantLeftIsNotALeave: a media server's disconnect is not a hang-up. A
// browser disconnects the same way when its page is refreshed, its tab closed or its
// call left, so participant_left records the disconnect as the seat's last sign of life
// and changes nothing else: the only person in an ad hoc call is still in it, and their
// page, refreshed, joins again with its session and gets a fresh pass into the same call
// on the same seat. Nobody comes back: reconciliation's first pass to miss the seat
// records that, and the pass that finds it missing for a pass lifetime closes it, AT the
// disconnect, and the call then ends empty. A host who disconnects starts the host-left
// countdown at the disconnect too, not at the pass that noticed.
func TestAParticipantLeftIsNotALeave(t *testing.T) {
	cw := newCallWorld(t, "leftisnotleave", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	policy := callrules.DefaultPolicy()
	hostSess, started := cw.started("Host")
	room := started.Call.ID
	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventParticipantJoined, room, started.Join.Identity, cw.tick(), nil)))

	left := cw.tick()
	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventParticipantLeft, room, started.Join.Identity, left, nil)))
	seat := cw.row(seatConcept, hostSess.SeatID)
	require.Equal(t, "in_call", seat["status"], "a disconnect closed the seat: only the Leave route, a hang-up, does")
	require.Equal(t, left, rowTime(seat, "lastSeenAt"), "the disconnect is not the seat's last sign of life")
	roomNow := cw.row(roomConcept, room)
	require.Equal(t, "open", roomNow["status"], "a disconnect ended the call")
	require.True(t, rowTime(roomNow, "hostLeftAt").IsZero(), "a disconnect started the countdown at once")

	// The page is refreshed: its session joins again and is let back in, on its seat.
	back, again, err := cw.svc.Join(ctx, cw.ref, &hostSess, callrules.JoinBody{Name: "Host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, back.Outcome, "a refreshed page was not let back into its call")
	require.NotEmpty(t, back.Pass)
	verifyPass(t, back.Pass, testMediaConfig().APIKey, testAPISecret)
	require.Equal(t, started.Join.Identity, back.Identity, "the refreshed page got another media identity")
	require.Equal(t, hostSess.SeatID, again.SeatID, "the refreshed page took another seat")
	require.Equal(t, room, again.RoomID)

	// They do not come back.
	cw.media.setPresent(room)
	cw.advanceTo(left.Add(policy.PassTTL))
	cw.reconcile()
	missed := cw.clock.Now()
	require.Equal(t, "in_call", cw.row(seatConcept, hostSess.SeatID)["status"], "the first pass to miss the seat closed it")
	cw.advanceTo(missed.Add(policy.PassTTL - time.Second))
	cw.reconcile()
	require.Equal(t, "in_call", cw.row(seatConcept, hostSess.SeatID)["status"], "the seat was closed before it had been missing for a pass lifetime")
	cw.clock.Advance(time.Second)
	cw.reconcile()
	gone := cw.row(seatConcept, hostSess.SeatID)
	require.Equal(t, "left", gone["status"], "a seat nobody came back to was not closed")
	require.Equal(t, left, rowTime(gone, "leftAt"), "the seat was not closed at the disconnect")
	ended := cw.row(roomConcept, room)
	require.Equal(t, "ended", ended["status"], "the call its only person left stayed open")
	require.Equal(t, callrules.EndReasonEmpty, ended["endReason"])

	// A host who disconnects while a guest stays: the countdown runs from the disconnect.
	hostB, b := cw.started("Host B")
	inB, _, err := cw.join(b.Call.GuestToken, "Guest B")
	require.NoError(t, err)
	at := cw.tick()
	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventParticipantJoined, b.Call.ID, b.Join.Identity, at, nil)))
	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventParticipantJoined, b.Call.ID, inB.Identity, at, nil)))
	hostGone := cw.tick()
	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventParticipantLeft, b.Call.ID, b.Join.Identity, hostGone, nil)))
	cw.media.setPresent(b.Call.ID, person(inB.Identity))
	cw.advanceTo(hostGone.Add(policy.PassTTL))
	cw.reconcile()
	cw.clock.Advance(policy.PassTTL)
	cw.reconcile()
	require.Equal(t, "left", cw.row(seatConcept, hostB.SeatID)["status"])
	require.Equal(t, hostGone, rowTime(cw.row(roomConcept, b.Call.ID), "hostLeftAt"), "the countdown did not start at the host's disconnect")
	cw.advanceTo(hostGone.Add(policy.HostLeftGrace - time.Second))
	cw.reconcile()
	require.Equal(t, "open", cw.row(roomConcept, b.Call.ID)["status"], "the countdown ended the call before the grace from the disconnect")
	cw.clock.Advance(time.Second)
	cw.reconcile()
	endedB := cw.row(roomConcept, b.Call.ID)
	require.Equal(t, "ended", endedB["status"], "the countdown from the host's disconnect ran out and the call goes on")
	require.Equal(t, callrules.EndReasonHostLeft, endedB["endReason"])
}

// TestALateOrRepeatedMediaEventChangesNothing: an event is applied only when it is
// newer, to the second, than everything recorded about its seat. A participant_left
// from a seat's earlier stint, delivered after the seat came back in, changes nothing;
// neither does an event in the same second as the seat's last change, which is
// unordered against it. A delivery is worked once: one redelivered on its own row (the
// receiver's id is the body's digest) keeps the row's handling state, and run again it
// is answered as worked and writes nothing; the same event delivered on another row is
// old.
func TestALateOrRepeatedMediaEventChangesNothing(t *testing.T) {
	cw := newCallWorld(t, "lateevents", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, started := cw.started("Host")
	room := started.Call.ID
	in, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventParticipantJoined, room, in.Identity, cw.tick(), nil)))
	firstStintEnd := cw.tick()
	require.NoError(t, cw.svc.Leave(ctx, guest))
	cw.clock.Advance(2 * time.Second)
	back, guest, err := cw.rejoin(guest, "")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, back.Outcome, "the guest was not let back in")
	require.NotEqual(t, in.Identity, back.Identity, "a new admission must retire the previous media identity")
	reentered := cw.row(seatConcept, guest.SeatID)
	versions := cw.versions(seatConcept, guest.SeatID)

	require.Equal(t, outcomeIgnored, cw.deliver(eventBody(t, eventParticipantLeft, room, in.Identity, firstStintEnd, nil)),
		"a disconnect from the seat's earlier stint was applied to its new one")
	sameSecond := rowTime(reentered, "joinedAt").Truncate(time.Second)
	require.Equal(t, outcomeOld, cw.deliver(eventBody(t, eventParticipantJoined, room, back.Identity, sameSecond, nil)),
		"an event in the same second as the seat's last change was applied")
	require.Equal(t, versions, cw.versions(seatConcept, guest.SeatID), "an old event wrote the seat")
	require.Equal(t, "admitted", cw.row(seatConcept, guest.SeatID)["status"])

	// A late connection using the retired token is removed, without touching the
	// newer admission. Time ordering still applies within the current identity.
	require.Equal(t, outcomeRemoved, cw.deliver(eventBody(t, eventParticipantJoined, room, in.Identity, cw.tick(), nil)))
	require.Equal(t, versions, cw.versions(seatConcept, guest.SeatID))
	body := eventBody(t, eventParticipantJoined, room, back.Identity, cw.tick(), nil)
	id := cw.stage("", body, true, "registered", WebhookSource)
	reply, err := cw.applyDelivery(id)
	require.NoError(t, err)
	require.Equal(t, outcomeApplied, reply["outcome"])
	applied := cw.versions(seatConcept, guest.SeatID)
	require.Equal(t, "in_call", cw.row(seatConcept, guest.SeatID)["status"])

	cw.stage(id, body, true, "registered", WebhookSource)
	require.Equal(t, "processed", cw.delivery(id)["status"], "a redelivery reset a worked delivery's handling state")
	rowVersions := cw.versions(inboundRequestConcept, bare(id))
	reply, err = cw.applyDelivery(id)
	require.NoError(t, err)
	require.Equal(t, outcomeWorked, reply["outcome"], "a worked delivery was worked again")
	require.Equal(t, rowVersions, cw.versions(inboundRequestConcept, bare(id)), "a worked delivery was stamped again")
	require.Equal(t, outcomeOld, cw.deliver(body), "the same event, delivered again, was applied again")
	require.Equal(t, applied, cw.versions(seatConcept, guest.SeatID), "one event wrote its seat twice")
}

// ---------------------------------------------------------------------------
// one screen share at a time
// ---------------------------------------------------------------------------

// TestANewerScreenShareTakesOver: a call has one screen share at a time, and a new one
// takes over -- every other participant's share and its audio are stopped -- while the
// room records whose share it is and since when. Delivered out of order, the older of
// two shares changes nothing and the newer keeps running. Any other track changes
// nothing, and a share from an identity no seat holds is disconnected and takes nothing
// over.
func TestANewerScreenShareTakesOver(t *testing.T) {
	cw := newCallWorld(t, "share", 50)
	if cw == nil {
		return
	}
	_, started := cw.started("Host")
	room := started.Call.ID
	inA, _, err := cw.join(started.Call.GuestToken, "A")
	require.NoError(t, err)
	inB, b, err := cw.join(started.Call.GuestToken, "B")
	require.NoError(t, err)
	camera := MediaTrack{SID: "TR_camera", Source: "camera"}
	shareA, audioA := MediaTrack{SID: "TR_shareA", Source: "screen_share"}, MediaTrack{SID: "TR_audioA", Source: "screen_share_audio"}
	shareB := MediaTrack{SID: "TR_shareB", Source: "screen_share"}
	cw.media.setPresent(room, person(started.Join.Identity, camera), person(inA.Identity, shareA, audioA), person(inB.Identity, shareB))
	olderA, newerB := cw.tick(), cw.tick()

	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventTrackPublished, room, inB.Identity, newerB, screenShare("TR_shareB"))))
	require.ElementsMatch(t, []string{"TR_shareA", "TR_audioA"}, cw.mutedTracks(), "the newer share did not stop the other one and its audio, or stopped something else")
	for _, m := range cw.media.mutes() {
		require.Equal(t, inA.Identity, m.Identity)
	}
	recorded := cw.row(roomConcept, room)
	require.Equal(t, b.SeatID, bare(recorded["shareSeatId"]), "the room does not record whose share it is")
	require.Equal(t, newerB, rowTime(recorded, "shareStartedAt"))

	roomVersions := cw.versions(roomConcept, room)
	require.Equal(t, outcomeOld, cw.deliver(eventBody(t, eventTrackPublished, room, inA.Identity, olderA, screenShare("TR_shareA"))),
		"the older share, delivered late, took over")
	require.Equal(t, outcomeOld, cw.deliver(eventBody(t, eventTrackPublished, room, inB.Identity, newerB, screenShare("TR_shareB2"))),
		"a share in the same second as the recorded one took over")
	require.Len(t, cw.media.mutes(), 2, "an old share stopped the newer one")
	require.Equal(t, roomVersions, cw.versions(roomConcept, room), "an old share wrote the room")

	require.Equal(t, outcomeIgnored, cw.deliver(eventBody(t, eventTrackPublished, room, started.Join.Identity, cw.tick(),
		&livekit.TrackInfo{Sid: "TR_camera", Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_CAMERA})))
	require.Equal(t, outcomeRemoved, cw.deliver(eventBody(t, eventTrackPublished, room, "stranger-identity", cw.tick(), screenShare("TR_x"))))
	require.Contains(t, cw.media.removals(), fakeMediaRemoval{Server: BootstrapMediaServerID, Room: room, Identity: "stranger-identity"})
	require.Len(t, cw.media.mutes(), 2, "another track, or a stranger's share, stopped a share")
	require.Equal(t, roomVersions, cw.versions(roomConcept, room))

	// In order: the host's share, newer still, takes over from B. A's share is stopped
	// already, so only B's is.
	hostShare := MediaTrack{SID: "TR_shareHost", Source: "screen_share"}
	shareA.Muted, audioA.Muted = true, true
	cw.media.setPresent(room, person(started.Join.Identity, camera, hostShare), person(inA.Identity, shareA, audioA), person(inB.Identity, shareB))
	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventTrackPublished, room, started.Join.Identity, cw.tick(), screenShare("TR_shareHost"))))
	require.Equal(t, []string{"TR_shareA", "TR_audioA", "TR_shareB"}, cw.mutedTracks(), "the host's share did not take over from B, or stopped the host's own tracks")
}

// ---------------------------------------------------------------------------
// a media room the server closed
// ---------------------------------------------------------------------------

// TestRoomFinishedEndsOnlyAnEmptyCall: a media server closes a room nobody has been in
// for a while, and says so. That ends the call only when the empty-room rule would --
// nobody admitted, in the call, waiting behind its lock or holding a place in line --
// and it ends it empty. Otherwise the event is ignored, and the next pass into the call
// opens its media room again. An ended call and a media room no call has change nothing.
func TestRoomFinishedEndsOnlyAnEmptyCall(t *testing.T) {
	cw := newCallWorld(t, "roomfinished", 3)
	if cw == nil {
		return
	}
	ctx := context.Background()
	finished := func(room string) string {
		t.Helper()
		return cw.deliver(eventBody(t, eventRoomFinished, room, "", cw.tick(), nil))
	}

	// In the call, admitted, then queued: the server's three seats are taken, and a new
	// call's host waits in line for one.
	_, inCall := cw.started("In the call")
	require.Equal(t, outcomeApplied, cw.deliver(eventBody(t, eventParticipantJoined, inCall.Call.ID, inCall.Join.Identity, cw.tick(), nil)))
	_, admitted := cw.started("Admitted")
	hostC, waiting := cw.started("Waiting")
	queued, err := cw.svc.Start(ctx, cw.host(), callrules.StartBody{Name: "Queued", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, queued.Join.Outcome)
	// Waiting behind the lock: the host locks the call, a guest waits, the host leaves.
	require.NoError(t, cw.act(hostC, callrules.HostLock, ""))
	waits, _, err := cw.join(waiting.Call.GuestToken, "Waiter")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, waits.Outcome)
	require.NoError(t, cw.svc.Leave(ctx, hostC))

	for name, room := range map[string]string{
		"somebody in the call": inCall.Call.ID, "somebody admitted": admitted.Call.ID,
		"somebody waiting behind the lock": waiting.Call.ID, "somebody in line": queued.Call.ID,
	} {
		require.Equal(t, outcomeIgnored, finished(room), "room_finished acted on a call with %s", name)
		require.Equal(t, "open", cw.row(roomConcept, room)["status"], "room_finished ended a call with %s", name)
	}

	// The queued host gives up its place, and the call is empty. A waiter behind a lock
	// holds no place to give up, and still counts.
	cw.clock.Advance(callrules.DefaultPolicy().QueueEntryTTL)
	require.Equal(t, outcomeEnded, finished(queued.Call.ID))
	ended := cw.row(roomConcept, queued.Call.ID)
	require.Equal(t, "ended", ended["status"], "room_finished left an empty call open")
	require.Equal(t, callrules.EndReasonEmpty, ended["endReason"])
	require.Equal(t, outcomeIgnored, finished(waiting.Call.ID), "a waiter behind a lock stopped counting as the time passed")
	require.Equal(t, "open", cw.row(roomConcept, waiting.Call.ID)["status"])

	versions := cw.versions(roomConcept, queued.Call.ID)
	require.Equal(t, outcomeIgnored, finished(queued.Call.ID), "room_finished acted on a call that has ended")
	require.Equal(t, versions, cw.versions(roomConcept, queued.Call.ID))
	require.Equal(t, outcomeUnknownRoom, finished("no-such-room"))
}

// ---------------------------------------------------------------------------
// who may apply an event, and which deliveries
// ---------------------------------------------------------------------------

// TestApplyMediaEventRunsOnlyForItsAutomation: the callsApplyMediaEvent builtin acts for
// the engine's own applyMediaEvent automation, at internal origin, and refuses every
// other caller -- a person, the cluster's operator, the cluster acting outside that
// automation, another automation, and an automation of that name from anywhere but the
// engine's own tree -- before it reads anything. The automation's own call works only a
// delivery the media server's credential verified: one of another source is left as it
// is, and one not signature-verified, or verified by another tier's secret, or whose
// body is no event, is stamped failed with the reason and changes nothing.
func TestApplyMediaEventRunsOnlyForItsAutomation(t *testing.T) {
	cw := newCallWorld(t, "eventsgate", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	hostSess, started := cw.started("Host")
	body := eventBody(t, eventParticipantJoined, started.Call.ID, started.Join.Identity, cw.tick(), nil)
	id := cw.stage("", body, true, "registered", WebhookSource)

	internal := func(ac *auth.AccessContext) context.Context {
		return auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, ac))
	}
	cluster, err := asCluster(ctx)
	require.NoError(t, err)
	for name, caller := range map[string]context.Context{
		"the site's owner":                       actorContext(cw.owner, auth.RoleWriter),
		"the cluster's operator":                 actorContext(cw.operator, auth.RoleOwner),
		"the cluster acting":                     cluster,
		"another automation":                     internal(auth.AutomationActor(auth.AutomationOriginEngine, reconcileAutomation)),
		"a pack's automation of the same name":   internal(auth.AutomationActor(auth.AutomationOriginOther, applyMediaEventAutomation)),
		"the automation without internal origin": auth.ContextWithClientOrigin(auth.ContextWithAccess(ctx, auth.AutomationActor(auth.AutomationOriginEngine, applyMediaEventAutomation))),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := cw.calls.handleApplyMediaEvent(caller, map[string]any{"requestId": id}, 0)
			require.Error(t, err, "%s applied a media event", name)
		})
	}
	_, err = cw.builtin(actorContext(cw.owner, auth.RoleWriter), "callsApplyMediaEvent", map[string]any{"requestId": id})
	require.Error(t, err, "a person applied a media event through the engine")
	require.Equal(t, "received", cw.delivery(id)["status"], "a refused caller stamped the delivery")
	require.Equal(t, "admitted", cw.row(seatConcept, hostSess.SeatID)["status"], "a refused caller wrote a seat")

	// Another source's delivery is not this builtin's to stamp.
	github := cw.stage("", body, true, "registered", "github")
	reply, err := cw.applyDelivery(github)
	require.NoError(t, err)
	require.Equal(t, outcomeNotMine, reply["outcome"])
	require.Equal(t, "received", cw.delivery(github)["status"], "another source's delivery was stamped")

	for name, staged := range map[string]string{
		"not signature-verified":           cw.stage("", body, false, "", WebhookSource),
		"verified by an operator's secret": cw.stage("", body, true, "env", WebhookSource),
		"verified by nothing it names":     cw.stage("", body, true, "", WebhookSource),
		"a body that is no event":          cw.stage("", []byte(`{"not":"an event"`), true, "registered", WebhookSource),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := cw.applyDelivery(staged)
			require.Error(t, err, "a delivery %s was worked", name)
			row := cw.delivery(staged)
			require.Equal(t, "failed", row["status"], "a delivery %s was not stamped failed", name)
			require.NotEmpty(t, row["lastError"], "a failed delivery does not say why")
		})
	}
	require.Equal(t, "admitted", cw.row(seatConcept, hostSess.SeatID)["status"], "a delivery the media server's credential did not verify wrote a seat")
	_, err = cw.applyDelivery(inboundRequestConcept + ":inb-no-such-delivery")
	require.Error(t, err, "a delivery that does not exist was answered as worked")

	// The automation's own call, on the verified delivery: worked once.
	reply, err = cw.applyDelivery(id)
	require.NoError(t, err)
	require.Equal(t, outcomeApplied, reply["outcome"])
	require.Equal(t, "in_call", cw.row(seatConcept, hostSess.SeatID)["status"])
	require.Equal(t, "processed", cw.delivery(id)["status"])
}

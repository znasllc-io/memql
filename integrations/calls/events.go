package calls

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// events.go -- the media server's webhooks, applied as they arrive (design sections 7.7
// and 10): who connected and who disconnected, whose screen share is new, and which media
// room the server closed. The engine's own applyMediaEvent automation hands each staged
// delivery to the callsApplyMediaEvent builtin, whose handler is the last thing in this
// file.
//
// # A webhook is a hint, and the pass is the truth
//
// A webhook says within a moment what reconciliation would find within thirty seconds
// (reconcile.go), and it can be late, reordered, repeated or lost. So nothing here is
// trusted for its order or its arrival: each event is applied only when it is NEWER than
// what the rows already record, an older one is handled and changes nothing, and one that
// never comes is repaired by the next pass. A failure here is never a reason to retry the
// delivery: the pass is the retry.
//
// # How a delivery arrives
//
// The media server posts to POST /inbound/livekit. The bff admits that source with the
// policy it registers from the sealed calls.media.webhook credential
// (app/transport_inbound.go): the jwt-hs256-body-sha256 scheme, the credential's key as
// the token's issuer and its secret as the key. It stages the delivery as a
// v1:platform:inboundRequest, whose node.created fires applyMediaEvent on the replica
// that staged it (a delivery never leaves its node: component/node routing.go), so either
// bff replica works a webhook it received, from the rows alone. The builtin is handed the
// row's id and reads the rest from the row: the source, the verifying tier, the status
// and the body.
//
// ONE CREDENTIAL FOR EVERY SERVER (known limitation). Every media server's webhooks are
// verified against calls.media.webhook, the bootstrap server's. A server with webhook
// keys of its own needs the receiver to find the credential by the token's issuer, which
// comes with adding servers beyond the bootstrap one.
//
// # The order of events
//
// An event's time is its createdAt, in WHOLE SECONDS, by the media server's clock. A
// participant event is applied only when that second is strictly after the latest of the
// seat's own stamps -- joinedAt, leftAt, letInAt and lastSeenAt -- so an event from a
// seat's earlier stint, before it came back in, changes nothing, and neither does one in
// the same second as the seat's last change: the two are unordered, and reconciliation
// repairs whatever the ignored one would have changed. A screen share is ordered the same
// way against the share the room records (shareSeatId, shareStartedAt). The stamps are the
// engine's clock and the event is the media server's, so skew between them moves which
// events count as newer; the receiver's tolerance bounds how far ahead the server's clock
// may run (a token it signs ahead of that is refused), and the pass covers the rest.
//
// # The four events
//
//   - participant_joined puts an admitted seat in the call (in_call) and records the
//     event's time as its last sign of life (lastSeenAt); a seat in the call already is
//     seen again.
//   - participant_left records the event's time as the seat's last sign of life, and
//     NOTHING ELSE. A MEDIA DISCONNECT IS NOT A LEAVE: LiveKit's browser client
//     disconnects the same way when its page is refreshed, its tab closed or its call
//     hung up, so applying the Leave route's rules would end a one-person call on a
//     refresh. Only that route, an explicit hang-up, closes a seat at once. A seat that
//     stays gone is closed by reconciliation's vanish rule one pass lifetime after this
//     time, AT this time (calls.SeatGone), which stamps hostLeftAt for a host seat and
//     applies the empty-room rule there: a vanished host's countdown starts at the
//     disconnect, whenever the webhook arrived.
//   - track_published of a screen share, or its audio, takes over: one share at a time per
//     call, and a new one stops every other participant's share and share audio (design
//     7.7). The room records the share under its gate, and the mutes are made after the
//     gates, once the record is read again and still names this share. A share from before
//     its seat's latest admission is an earlier stint's and takes nothing over. Any other
//     track changes nothing.
//   - room_finished, the media server closing a room nobody was in, ends the call only
//     when the empty-room rule would (host.go endsEmpty: nobody admitted, in the call,
//     waiting or in line, and a booked call only after its booked end), and ends it empty.
//     Otherwise it is ignored, and the next pass into the call opens its media room again
//     (service.go reopenMediaRoom).
//
// Anything else the server sends -- room_started, track_unpublished, egress events,
// participant_connection_aborted (a connection that never completed, which the pass
// closes as a seat never seen) -- is handled and changes nothing.
//
// A CONNECTION WITH NO SEAT IS REMOVED AGAIN. An identity a participant_joined or a screen
// share shows connected, that holds no admitted or in_call seat of the call now -- one
// nobody let in, a seat that left or was removed, a seat waiting in line -- is
// disconnected, because a self-hosted media server cannot revoke a pass (design 10), and
// a connection the rows count as free would put one more person on the server than its
// seats count. The pass does the same (reconcile.go step 2). Every participant this
// service lets in holds a seat; a kind that connects without one -- the safety sampler,
// later -- needs a seat or an exemption here, as it does there.
//
// # Which gates
//
// A participant event and a share take the ROOM's gate alone: they move no media server
// seat (admitted and in_call both hold one) and no queue entry, so they do not wait on the
// server's gate, which every admission on the server takes, and every other writer of
// the call's rows takes the room's gate too. room_finished may end the call, so it takes
// a call's end gates (host.go endGates). Every decision reads fresh under its gate and
// writes fenced on the version it read (service.go's file note); every media call is made
// after the gates and bounded (reconcile.go mediaCallBound).
//
// KNOWN LIMITATION (share order): an older share's event that is worked after a newer
// share was published, but before the newer one's own event arrives, stops both shares,
// and the newer sharer starts again. The record orders the events it has seen; it cannot
// see a share whose event is still on its way.
//
// # Who acts, and what a delivery is stamped
//
// The builtin admits the engine's own applyMediaEvent automation at internal origin and
// nobody else, before it reads anything (handleApplyMediaEvent). It reads the delivery
// (@rowAuthz(clusterOwner)) and the room an event names as the cluster acting -- a
// webhook names a media room and no deployable -- and each room's seats as that room's
// site owner (rooms.go rowsAs). It works only a delivery of WebhookSource that the
// receiver verified with the calls credential (verifiedBy "registered"): one verified by
// an operator's environment pin on the same name was signed by another secret, and is
// stamped failed rather than allowed to move a seat. A delivery is stamped processed when
// its event was applied, was old, changed nothing or named no call; failed, with the
// reason, when it was not verified by the calls credential, its body is no event, or
// applying it did not complete; and one already worked is answered and left as it is.

// applyMediaEventAutomation is the automation the builtin acts for
// (dsl/calls/automations.memql).
const applyMediaEventAutomation = "applyMediaEvent"

// WebhookSource is the inbound source the media server's webhooks arrive under: the
// {source} of POST /inbound/{source}, the name the bff registers the calls credential's
// policy for (app/transport_inbound.go), and what applyMediaEvent's trigger filters on
// (webhook_sdk_test.go TestTheAutomationReadsTheWebhookSource holds the two together).
const WebhookSource = "livekit"

// inboundReceived is the status the receiver stages a delivery with: nobody has worked it.
const inboundReceived = "received"

// The events applied, in the media server's spelling.
const (
	eventParticipantJoined = "participant_joined"
	eventParticipantLeft   = "participant_left"
	eventTrackPublished    = "track_published"
	eventRoomFinished      = "room_finished"
)

// What became of a delivery, in the builtin's reply. Each is stable.
const (
	outcomeApplied     = "applied"              // the event changed a seat or the room
	outcomeOld         = "old"                  // something newer is recorded; nothing changed
	outcomeRemoved     = "removed"              // a connection holding no seat was disconnected
	outcomeIgnored     = "ignored"              // nothing to act on
	outcomeUnknownRoom = "unknown_room"         // no call has the media room
	outcomeEnded       = "ended"                // room_finished ended the call, empty
	outcomeWorked      = "already_worked"       // the delivery was worked before; nothing done
	outcomeNotMine     = "not_a_media_delivery" // another source's delivery, left as it is
)

// mediaEvent is one webhook as the service applies it. Room is the media room's name,
// which is the call's room id (service.go placeRoom); Identity the participant's media
// identity, a seat's; TrackSource the track's source in the spelling a pass's grants use
// (screen_share, ...); At the event's time, whole seconds, zero when it carried none.
type mediaEvent struct {
	ID, Kind, Room, Identity, TrackSID, TrackSource string
	At                                              time.Time
}

// parseMediaEvent reads a webhook body as LiveKit's notifier writes it -- a
// livekit.WebhookEvent in protojson -- the way LiveKit's own receiver reads one
// (webhook.ReceiveWebhookEvent): unknown fields discarded and a partial message allowed,
// so a field a newer server adds is no failure. Its error never repeats the body.
func parseMediaEvent(body []byte) (mediaEvent, error) {
	var ev livekit.WebhookEvent
	if err := (protojson.UnmarshalOptions{AllowPartial: true, DiscardUnknown: true}).Unmarshal(body, &ev); err != nil {
		return mediaEvent{}, errors.New("calls: the delivery's body is not a media server event")
	}
	out := mediaEvent{
		ID: ev.GetId(), Kind: ev.GetEvent(), Room: ev.GetRoom().GetName(), Identity: ev.GetParticipant().GetIdentity(),
	}
	if track := ev.GetTrack(); track != nil {
		out.TrackSID = track.GetSid()
		out.TrackSource = strings.ToLower(track.GetSource().String())
	}
	if secs := ev.GetCreatedAt(); secs > 0 {
		out.At = time.Unix(secs, 0).UTC()
	}
	return out, nil
}

// shareSources is the track sources a screen share publishes: exactly the ones the
// stop_share control acts on (calls.HostActionSources), so a takeover stops what the host
// would.
func shareSources() []string {
	sources, _ := callrules.HostActionSources(callrules.HostStopShare)
	return sources
}

// ---------------------------------------------------------------------------
// applying an event
// ---------------------------------------------------------------------------

// applyMediaEvent applies one event to the call it names (the file note) and answers
// what became of it.
func (s *Service) applyMediaEvent(ctx context.Context, ev mediaEvent) (string, error) {
	switch ev.Kind {
	case eventParticipantJoined, eventParticipantLeft, eventTrackPublished, eventRoomFinished:
	default:
		return outcomeIgnored, nil
	}
	// AN EVENT WITH NO TIME CANNOT BE ORDERED against anything the rows record, so it is
	// not applied; the pass sees what it would have said.
	if ev.At.IsZero() || strings.TrimSpace(ev.Room) == "" {
		return outcomeIgnored, nil
	}
	if ev.Kind != eventRoomFinished && strings.TrimSpace(ev.Identity) == "" {
		return outcomeIgnored, nil
	}
	if ev.Kind == eventTrackPublished && !slices.Contains(shareSources(), ev.TrackSource) {
		return outcomeIgnored, nil
	}
	room, err := s.store.roomForMediaRoom(ctx, ev.Room)
	if err != nil {
		return "", err
	}
	if room == nil {
		return outcomeUnknownRoom, nil
	}
	switch ev.Kind {
	case eventRoomFinished:
		return s.roomFinished(ctx, *room)
	case eventTrackPublished:
		return s.shareTakesOver(ctx, *room, ev)
	}
	return s.participant(ctx, *room, ev)
}

// participant applies a participant_joined or participant_left (the file note), and
// disconnects, after the gates, a connection that holds no seat in the call.
func (s *Service) participant(ctx context.Context, hint roomRow, ev mediaEvent) (string, error) {
	outcome, err := s.participantGated(ctx, hint, ev)
	if err != nil || outcome != outcomeRemoved {
		return outcome, err
	}
	return outcomeRemoved, s.removeConnection(ctx, hint, ev)
}

func (s *Service) participantGated(ctx context.Context, hint roomRow, ev mediaEvent) (string, error) {
	release, err := s.gates(ctx, roomGate(hint.ID))
	if err != nil {
		return "", err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	owner := hint.OwnerUserID
	room, seat, err := s.roomAndSeatOf(fresh, hint, ev.Identity)
	if err != nil || room == nil {
		return outcomeUnknownRoom, err
	}
	if seat == nil || !seat.occupying() || !room.open() {
		// NO SEAT IN THE CALL. A connection is disconnected again; a disconnect has
		// nothing left to disconnect, and changes nothing.
		if ev.Kind == eventParticipantJoined {
			return outcomeRemoved, nil
		}
		return outcomeIgnored, nil
	}
	if !lastRecorded(*seat).Before(ev.At) {
		return outcomeOld, nil
	}
	fields := map[string]any{"lastSeenAt": ev.At}
	if ev.Kind == eventParticipantJoined && seat.Status == seatAdmitted {
		fields["status"] = seatInCall
	}
	if err := s.store.updateSeat(fresh, owner, *seat, fields, memql.VersionTimeAfter(seat.Version, s.now())); err != nil {
		return "", err
	}
	return outcomeApplied, nil
}

// lastRecorded is the latest of a seat's own stamps: what a participant event must be
// newer than, to the second, to be applied (the file note).
func lastRecorded(seat seatRow) time.Time {
	latest := seat.JoinedAt
	for _, at := range []time.Time{seat.LeftAt, seat.LetInAt, seat.LastSeenAt} {
		if at.After(latest) {
			latest = at
		}
	}
	return latest
}

// shareTakesOver applies a screen share's track_published (the file note): the room's
// record under its gate, then every other participant's share stopped after it.
func (s *Service) shareTakesOver(ctx context.Context, hint roomRow, ev mediaEvent) (string, error) {
	outcome, sharer, err := s.shareGated(ctx, hint, ev)
	if err != nil {
		return "", err
	}
	switch outcome {
	case outcomeRemoved:
		return outcome, s.removeConnection(ctx, hint, ev)
	case outcomeApplied:
		return outcome, s.stopOtherShares(ctx, hint, sharer, ev)
	}
	return outcome, nil
}

func (s *Service) shareGated(ctx context.Context, hint roomRow, ev mediaEvent) (string, seatRow, error) {
	release, err := s.gates(ctx, roomGate(hint.ID))
	if err != nil {
		return "", seatRow{}, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	room, seat, err := s.roomAndSeatOf(fresh, hint, ev.Identity)
	if err != nil || room == nil {
		return outcomeUnknownRoom, seatRow{}, err
	}
	if seat == nil || !seat.occupying() || !room.open() {
		return outcomeRemoved, seatRow{}, nil
	}
	// AN EARLIER STINT'S SHARE, from before the seat's latest admission, is over. The
	// seat's admission alone is asked, not its last sign of life: a pass or a participant
	// event may have seen the seat after its share began, and the share still runs. Its
	// own second is allowed, since nothing repairs a share takeover that was missed.
	if ev.At.Before(seat.JoinedAt.Truncate(time.Second)) {
		return outcomeOld, seatRow{}, nil
	}
	if !room.ShareStartedAt.Before(ev.At) {
		return outcomeOld, seatRow{}, nil
	}
	fields := map[string]any{"shareSeatId": seat.ID, "shareStartedAt": ev.At}
	if err := s.store.updateRoom(fresh, hint.OwnerUserID, *room, fields, memql.VersionTimeAfter(room.Version, s.now())); err != nil {
		return "", seatRow{}, err
	}
	return outcomeApplied, *seat, nil
}

// stopOtherShares mutes every unmuted share track of every participant but the sharer,
// once the room's record, read again, still names this share: a newer share recorded in
// the meantime is the one that stands, and its own takeover stops this one. A track
// already gone is no failure.
func (s *Service) stopOtherShares(ctx context.Context, room roomRow, sharer seatRow, ev mediaEvent) error {
	current, err := s.store.roomByID(memql.ContextWithFreshRead(ctx), room.OwnerUserID, room.ID)
	if err != nil {
		return err
	}
	if current == nil || !current.open() || current.ShareSeatID != sharer.ID || !current.ShareStartedAt.Equal(ev.At) {
		return nil
	}
	backend, err := s.backendFor(ctx, room)
	if err != nil || backend == nil {
		return err
	}
	var present []MediaParticipant
	if err := bounded(ctx, func(ctx context.Context) error {
		var err error
		present, err = backend.Participants(ctx, room.MediaRoom)
		return err
	}); err != nil {
		return err
	}
	sources := shareSources()
	for _, participant := range present {
		if participant.Identity == sharer.Identity {
			continue
		}
		for _, track := range participant.Tracks {
			if track.Muted || !slices.Contains(sources, track.Source) {
				continue
			}
			err := bounded(ctx, func(ctx context.Context) error {
				return backend.MuteTrack(ctx, room.MediaRoom, participant.Identity, track.SID)
			})
			if err != nil && !isNotFound(err) {
				return err
			}
		}
	}
	return nil
}

// removeConnection disconnects an identity that holds no seat in the call, after the
// gates and bounded (the file note).
func (s *Service) removeConnection(ctx context.Context, room roomRow, ev mediaEvent) error {
	if err := bounded(ctx, func(ctx context.Context) error { return s.disconnect(ctx, room, ev.Identity) }); err != nil {
		return err
	}
	s.logger.Info("calls: a connection holding no seat in the call was disconnected", "component", pluginName,
		"room", room.ID, "event", ev.Kind)
	return nil
}

// roomFinished applies room_finished: the call ends, empty, only when the empty-room rule
// would end it (host.go endsEmpty), under a call's end gates.
func (s *Service) roomFinished(ctx context.Context, hint roomRow) (string, error) {
	release, err := s.gates(ctx, endGates(hint)...)
	if err != nil {
		return "", err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	room, err := s.store.roomByID(fresh, hint.OwnerUserID, hint.ID)
	if err != nil || room == nil {
		return outcomeUnknownRoom, err
	}
	if !room.open() {
		return outcomeIgnored, nil
	}
	v, err := s.viewRoom(fresh, *room)
	if err != nil {
		return "", err
	}
	ends, err := s.endsEmpty(fresh, v, now)
	if err != nil || !ends {
		return outcomeIgnored, err
	}
	// The media server closed the media room already, so none is deleted.
	if err := s.endRows(fresh, v, callrules.EndReasonEmpty, now); err != nil {
		return "", err
	}
	return outcomeEnded, nil
}

// roomAndSeatOf is a call read again under its gate, and the seat of it whose media
// identity is identity, or nil when none is.
func (s *Service) roomAndSeatOf(ctx context.Context, hint roomRow, identity string) (*roomRow, *seatRow, error) {
	room, err := s.store.roomByID(ctx, hint.OwnerUserID, hint.ID)
	if err != nil || room == nil {
		return nil, nil, err
	}
	seats, err := s.store.seatsForRoom(ctx, hint.OwnerUserID, room.ID)
	if err != nil {
		return nil, nil, err
	}
	for i := range seats {
		if seats[i].Identity == identity {
			return room, &seats[i], nil
		}
	}
	return room, nil, nil
}

// ---------------------------------------------------------------------------
// the rows a webhook reaches
// ---------------------------------------------------------------------------

// delivery is the slice of a staged v1:platform:inboundRequest the builtin reads.
type delivery struct {
	ID, Source, Body, Status, VerifiedBy string
	SignatureVerified                    bool
}

// deliveryByID reads one staged delivery as the cluster acting (the concept is
// @rowAuthz(clusterOwner)), answered by the database: whether it was worked is read off
// it. A miss is (nil, nil).
func (s *Store) deliveryByID(ctx context.Context, requestID string) (*delivery, error) {
	cluster, err := asCluster(ctx)
	if err != nil {
		return nil, err
	}
	call, err := langparser.RenderCall("inboundRequestById", map[string]any{"requestId": requestID})
	rows, err := s.read(memql.ContextWithFreshRead(auth.ContextWithInternalOrigin(cluster)), "the staged delivery", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &delivery{
		ID:                memql.BareShortId(rowString(r, "id")),
		Source:            rowString(r, "source"),
		Body:              rowString(r, "body"),
		Status:            rowString(r, "status"),
		VerifiedBy:        rowString(r, "verifiedBy"),
		SignatureVerified: rowBool(r, "signatureVerified"),
	}, nil
}

// stampDelivery records what became of a delivery: its handling status, the reason when
// it failed (empty otherwise, so nothing earlier is kept), and when.
// updateInboundRequestStatus is @serverOnly, so the write is stamped internal origin
// here, in the file that renders it (test/dslconformance
// server_only_callers_stamp_test.go).
func (s *Store) stampDelivery(ctx context.Context, requestID, status, reason string, at time.Time) error {
	cluster, err := asCluster(ctx)
	if err != nil {
		return err
	}
	call, err := langparser.RenderCall("updateInboundRequestStatus", map[string]any{
		"requestId": requestID, "status": status, "lastError": reason, "processedAt": at.UTC().Format(time.RFC3339),
	})
	return s.write(auth.ContextWithInternalOrigin(cluster), "the delivery's handling status", call, err)
}

// roomForMediaRoom is the call a media room is, read as the cluster acting: a webhook
// names its media room and no deployable, and a call's media room is named by its room's
// id (service.go placeRoom). A media room no call has is (nil, nil).
func (s *Store) roomForMediaRoom(ctx context.Context, mediaRoom string) (*roomRow, error) {
	name := strings.TrimSpace(mediaRoom)
	cluster, err := clusterRowsAs(ctx)
	if err != nil {
		return nil, err
	}
	call, err := langparser.RenderCall("callsRoomById", map[string]any{"id": memql.BareShortId(name)})
	rows, err := s.read(auth.ContextWithInternalOrigin(cluster), "the call a media room names", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	room := roomFromRow(rows[0])
	if room.MediaRoom != name || room.OwnerUserID == "" {
		return nil, nil
	}
	return &room, nil
}

// ---------------------------------------------------------------------------
// the builtin
// ---------------------------------------------------------------------------

// handleApplyMediaEvent is the callsApplyMediaEvent builtin: one staged delivery of the
// media server's webhooks, worked for the engine's own applyMediaEvent automation and
// nobody else.
//
// THE AUTOMATION, AT INTERNAL ORIGIN, AND ONLY IT, for handleReconcile's reason: the
// delivery is applied to a deployable's rows as its owner, so no person may apply one,
// whatever part they hold, nor any other automation or one of the same name from a pack
// or a runtime domain (auth.IsEngineAutomationActor). The check comes before anything is
// read, and the builtin declares no capability for that reason
// (dsl/calls/builtins.memql).
//
// A DELIVERY ALREADY WORKED IS LEFT AS IT IS. The trigger fires for a `received` row
// only, because the stamp this writes publishes node.created again; this is the half
// that holds when a run reaches a row somebody stamped since -- a redelivery that fired
// while the first was at work, a run tried again. It does not stop two runs that both
// read the row before either stamps it: both apply, and the second finds nothing newer
// to apply (the file note's order of events).
func (i *Integration) handleApplyMediaEvent(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || !auth.IsEngineAutomationActor(ac, applyMediaEventAutomation) || !auth.OriginFromContext(ctx).IsInternal() {
		return nil, refuse("A media server's webhooks are applied by the cluster's own applyMediaEvent automation as each one arrives. Nothing else applies one.")
	}
	requestID := strings.TrimSpace(asString(args["requestId"]))
	if requestID == "" {
		return nil, refuse("callsApplyMediaEvent works one staged delivery, named by requestId.")
	}
	d, err := i.store.deliveryByID(ctx, requestID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("calls: no staged delivery has the id %s", requestID)
	}
	reply := func(event, outcome string) ([]memorynodes.MemoryNode, error) {
		return i.node("media-event:"+d.ID, map[string]any{"requestId": d.ID, "event": event, "outcome": outcome})
	}
	// ANOTHER SOURCE'S DELIVERY is somebody else's to work and to stamp. The trigger
	// filters on the source, so only a call that reached the builtin another way gets
	// here.
	if d.Source != WebhookSource {
		return reply("", outcomeNotMine)
	}
	if status := strings.TrimSpace(d.Status); status != "" && status != inboundReceived {
		return reply("", outcomeWorked)
	}
	if !d.SignatureVerified || d.VerifiedBy != memqlsync.VerifiedByRegistered {
		return nil, i.failDelivery(ctx, d, "the delivery was not verified with the calls media credential "+
			"(signatureVerified, and verifiedBy \"registered\"), so it may not change a call; an environment pin on "+
			"the source name is verified by another secret, and is refused for that reason")
	}
	ev, err := parseMediaEvent([]byte(d.Body))
	if err != nil {
		return nil, i.failDelivery(ctx, d, err.Error())
	}
	outcome, err := i.service.applyMediaEvent(ctx, ev)
	if err != nil {
		return nil, i.failDelivery(ctx, d, "applying the event did not complete ("+failureReason(err)+"); reconciliation's next pass repairs the call")
	}
	if err := i.store.stampDelivery(ctx, d.ID, "processed", "", i.now()); err != nil {
		return nil, err
	}
	return reply(ev.Kind, outcome)
}

// failDelivery stamps a delivery failed with the reason and answers it as the builtin's
// error, so the same fact is both queryable (inboundRequestsByStatus) and in the run.
// Nothing retries it: reconciliation repairs whatever it would have changed.
func (i *Integration) failDelivery(ctx context.Context, d *delivery, reason string) error {
	if err := i.store.stampDelivery(ctx, d.ID, "failed", reason, i.now()); err != nil {
		i.logger.Warn("calls: a media server delivery could not be stamped failed", "component", pluginName,
			"request", d.ID, "reason", failureReason(err))
	}
	i.logger.Warn("calls: a media server delivery was not applied", "component", pluginName, "request", d.ID, "reason", reason)
	return fmt.Errorf("calls: the media server delivery %s was not applied: %s", d.ID, reason)
}

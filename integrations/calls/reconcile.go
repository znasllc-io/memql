package calls

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// reconcile.go -- reconciliation: every open call's rows repaired against what its media
// server shows (design section 10, "Pass rules"; section 8, "End"). The engine's own
// reconcileCalls automation runs it every thirty seconds on one bff replica
// (dsl/calls/automations.memql), through the callsReconcile builtin, whose handler is
// the last thing in this file.
//
// # Why a pass, and what it trusts
//
// A media server's participant list is the truth about who is in a call, and the rows
// follow it: a participant who drops without a word leaves a seat holding one of the
// server's seats; a removed participant can come back on a pass a self-hosted server
// cannot revoke; a waiter who closes their tab leaves a place in line; and a host who
// left leaves a countdown nothing else would end. Nothing here trusts an event's order,
// which is why the pass repairs what the events (and the requests) missed.
//
// # Call by call, under each call's gates
//
// The pass walks every media server not removed (a draining one still carries its calls)
// and, on each, every open room, by id. A room's participant list is read OUTSIDE its
// gates -- a network call under the server's gate would hold up every admission on the
// server -- and the room is then read again, fresh, under its booking's, its own and its
// server's gates (host.go endGates), where everything is decided and written:
//
//  1. Each seat in the call against the list. A seat the list shows is in the call
//     (in_call) and seen now, its lastSeenAt written only once the stored one is
//     lastSeenRefresh old, so a present seat costs one write per two minutes and not one
//     per pass. A seat the list does not show is closed (left) once its last sign of
//     life is a pass's lifetime old -- before that it may still be connecting on the pass
//     it was handed -- AT that sign: its lastSeenAt, or its admission when it was never
//     seen since (calls.SeatGone). The list read before the gates cannot show a seat let
//     in after it, and the same grace is what keeps that seat from being closed.
//  2. Each identity the list shows that holds no seat in the call now -- a stranger, a
//     removed seat back on its pass, a seat that left while its connection lingered -- is
//     disconnected, after the gates. A seat that is not in the call holds no media
//     server's seat by the rows' account, so a connection left on it would be one more
//     person in the call than the server's seats count. Every participant this service
//     lets in holds a seat (only people are seated so far); a kind that connects without
//     one -- the safety sampler -- will need a seat, or an exemption here, or the pass
//     disconnects it.
//  3. The queue: every waiting entry of the call past its deployable's QueueEntryTTL is
//     marked expired -- the calls whose own requests mark theirs are the busy ones; this
//     reaches the quiet ones. Nobody is admitted and no pass is minted: a waiter whose
//     turn has come is let in on its own next poll, the only reply that can carry its
//     pass (service.go). This comes BEFORE the host and the empty decisions, for step 5's
//     reason.
//  4. The host: a host seat closed in step 1 stamps the room's hostLeftAt at its leave
//     time when no host seat is left in the call; a host in the call clears a stale one;
//     then the countdown (host.go hostLeft) hands the call to a key holder in it, or finds
//     it due to end.
//  5. A call nobody is in and nobody waits for ends, empty, in this same gated phase -- a
//     booked call only after its booked end (host.go endsEmpty); a call the countdown found
//     due ends host_left in a second one, after its media room is deleted outside the
//     gates (host.go's file note, "Where the media room is deleted"), and only if it is
//     still due then (endIfDue).
//
// After each step that wrote, the call is read again before the next decides, so every
// decision is made from rows as they stand and every write is fenced on the version it
// was decided from.
//
// THE TWO PHASES OF A host_left END DECIDE FROM THE SAME ROWS. The first marks every
// expired entry before it decides, so endIfDue reads what the first phase decided from,
// and anything else between them is written by somebody else: a host let in, a host
// joining the line, a key holder coming in, a leave. Each of those is a real reason to
// change the decision. An entry whose lifetime runs out in between can only take a seat
// out of the count of those waiting, never put one in, and a host seat counts as coming
// only with a live place (host.go stillWaiting), so it cannot turn a due end into none. A
// pass therefore never deletes a call's media room and then keeps the call open because of
// a row it wrote itself.
//
// # What a pass costs
//
// Per media server, one read of its open rooms; per open call, one participant list
// from the media server and four reads under its gates (room, seats, queue entries,
// limits), read again only after a step that wrote. A call with people in it writes at
// least once every lastSeenRefresh, so while calls run most passes write something, and
// a scheduled automation's first write opens its run in the work journal (component/
// automations journal.go heldUntilWrite): a pass that changed nothing leaves no run.
//
// # What a failure costs, and what survives
//
// One call that cannot be reconciled -- a media server that does not answer, a write that
// loses its fence -- is logged by its room id and a fixed reason and skipped; the pass
// goes on with the others and answers that it was incomplete, and the next pass tries
// again. Nothing is kept between passes but the rows, so a lease that moves to another
// replica mid-pass costs a repeat and nothing else. Every media call is bounded
// (mediaCallBound), so one stuck server cannot stall the pass.
//
// # Who acts
//
// The pass has no session and is handed no deployable: it reads the media servers and
// their open rooms AS THE CLUSTER ACTING (store.go), and each room's rows as THAT ROOM'S
// site owner, the owner the room row names, with internal origin (rooms.go rowsAs). The
// builtin admits only the engine's own reconcileCalls automation at internal origin, so
// no person and no other automation runs a pass (handleReconcile).

// reconcileAutomation is the automation the reconciliation builtin acts for
// (dsl/calls/automations.memql).
const reconcileAutomation = "reconcileCalls"

// lastSeenRefresh is how old a present seat's lastSeenAt gets before reconciliation
// writes it again: the bound on what seeing a seat costs, one write per seat per two
// minutes. It is also how far its leave time can lag when it vanishes.
const lastSeenRefresh = 2 * time.Minute

// mediaCallBound bounds each call reconciliation makes to a media server's control API.
const mediaCallBound = 10 * time.Second

// reconcileTally is what one pass changed, for its log line.
type reconcileTally struct {
	rooms, closed, seen, disconnected, expired, handedOver, ended int
}

func (t reconcileTally) changed() bool {
	return t.closed+t.disconnected+t.expired+t.handedOver+t.ended > 0
}

// Reconcile repairs every open call against what its media server shows (the file note).
// It answers an error when any call could not be reconciled, after trying every other.
func (s *Service) Reconcile(ctx context.Context) error {
	servers, err := s.store.MediaServersNotRemoved(ctx)
	if err != nil {
		return err
	}
	sort.Slice(servers, func(a, b int) bool { return servers[a].ID < servers[b].ID })
	tally := &reconcileTally{}
	failed := 0
	var first error
	for _, server := range servers {
		n, err := s.reconcileServer(ctx, server, tally)
		failed += n
		if first == nil {
			first = err
		}
	}
	if tally.changed() {
		s.logger.Info("calls: reconciled the open calls", "component", pluginName,
			"rooms", tally.rooms, "seatsSeen", tally.seen, "seatsClosed", tally.closed, "identitiesDisconnected", tally.disconnected,
			"entriesExpired", tally.expired, "handedOver", tally.handedOver, "ended", tally.ended)
	}
	if first != nil {
		return fmt.Errorf("calls: %d calls or media servers could not be reconciled, and the next pass tries again: %w", max(failed, 1), first)
	}
	return nil
}

// reconcileServer reconciles every open call on one media server, and answers how many
// could not be, with the first failure.
func (s *Service) reconcileServer(ctx context.Context, server MediaServer, tally *reconcileTally) (int, error) {
	credential, err := s.credential(ctx, server)
	if err != nil {
		s.logger.Warn("calls: a media server could not be reconciled", "component", pluginName, "mediaServer", server.ID, "reason", failureReason(err))
		return 1, err
	}
	backend, err := s.media(server, credential)
	if err != nil {
		s.logger.Warn("calls: a media server could not be reconciled", "component", pluginName, "mediaServer", server.ID, "reason", failureReason(err))
		return 1, err
	}
	rooms, err := s.store.openRoomsOn(ctx, server)
	if err != nil {
		return 1, err
	}
	failed := 0
	var first error
	for _, room := range rooms {
		tally.rooms++
		if err := s.reconcileRoom(ctx, server, backend, room, tally); err != nil {
			failed++
			if first == nil {
				first = err
			}
			s.logger.Warn("calls: a call could not be reconciled; the next pass tries again", "component", pluginName,
				"room", room.ID, "reason", failureReason(err))
		}
	}
	return failed, first
}

// reconcilePlan is what a call's gated phase decided for after the gates: the identities
// to disconnect, and how the call ends, if it does -- empty, already written, or
// host_left, due once its media room is deleted.
type reconcilePlan struct {
	disconnect []string
	end        string
}

// reconcileRoom reconciles one call: its participant list read outside the gates, its
// rows repaired under them, and the media calls their decisions need made after them.
func (s *Service) reconcileRoom(ctx context.Context, server MediaServer, backend MediaBackend, hint roomRow, tally *reconcileTally) error {
	var present []MediaParticipant
	if err := bounded(ctx, func(ctx context.Context) error {
		var err error
		present, err = backend.Participants(ctx, hint.MediaRoom)
		return err
	}); err != nil {
		return err
	}
	plan, err := s.reconcileGated(ctx, server, hint, present, tally)
	if err != nil {
		return err
	}
	switch plan.end {
	case callrules.EndReasonHostLeft:
		// People are in the call: its media room goes first, and a deletion that fails
		// writes nothing, so the next pass tries again (host.go's file note).
		if err := bounded(ctx, func(ctx context.Context) error { return backend.DeleteRoom(ctx, hint.MediaRoom) }); err != nil {
			return err
		}
		ended, err := s.endIfDue(ctx, hint)
		if err != nil {
			return err
		}
		if ended {
			tally.ended++
		}
		return nil
	case callrules.EndReasonEmpty:
		tally.ended++
		if err := bounded(ctx, func(ctx context.Context) error { return backend.DeleteRoom(ctx, hint.MediaRoom) }); err != nil {
			s.logger.Warn("calls: a call ended empty but its media room could not be deleted", "component", pluginName,
				"room", hint.ID, "reason", failureReason(err))
		}
		return nil
	}
	for _, identity := range plan.disconnect {
		if err := bounded(ctx, func(ctx context.Context) error { return backend.RemoveParticipant(ctx, hint.MediaRoom, identity) }); err != nil {
			s.logger.Warn("calls: an identity no seat holds could not be disconnected; the next pass tries again", "component", pluginName,
				"room", hint.ID, "reason", failureReason(err))
			continue
		}
		tally.disconnected++
	}
	return nil
}

// reconcileGated is steps 1 to 5 of the file note, under the call's gates, in the note's
// order: the seats, the identities, the queue, the host, and how the call ends.
func (s *Service) reconcileGated(ctx context.Context, server MediaServer, hint roomRow, present []MediaParticipant, tally *reconcileTally) (reconcilePlan, error) {
	release, err := s.gates(ctx, endGates(hint)...)
	if err != nil {
		return reconcilePlan{}, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	owner := hint.OwnerUserID
	room, err := s.store.roomByID(fresh, owner, hint.ID)
	if err != nil || room == nil || !room.open() || room.MediaServerID != server.ID {
		return reconcilePlan{}, err
	}
	v, err := s.viewRoom(fresh, *room)
	if err != nil {
		return reconcilePlan{}, err
	}
	shown := map[string]bool{}
	for _, participant := range present {
		shown[participant.Identity] = true
	}

	// 1. Each seat in the call against the media server.
	wrote := false
	var hostGoneAt time.Time
	for _, seat := range v.seats {
		if !seat.occupying() {
			continue
		}
		if shown[seat.Identity] {
			fields := map[string]any{}
			if seat.Status == seatAdmitted {
				fields["status"] = seatInCall
			}
			if seat.LastSeenAt.IsZero() || now.Sub(seat.LastSeenAt) >= lastSeenRefresh {
				fields["lastSeenAt"] = now
			}
			if len(fields) == 0 {
				continue
			}
			if err := s.store.updateSeat(fresh, owner, seat, fields, memql.VersionTimeAfter(seat.Version, now)); err != nil {
				return reconcilePlan{}, err
			}
			wrote = true
			tally.seen++
			continue
		}
		gone, at := callrules.SeatGone(seat.LastSeenAt, seat.JoinedAt, now, v.policy.PassTTL)
		if !gone {
			continue
		}
		if err := s.store.updateSeat(fresh, owner, seat, map[string]any{"status": seatLeft, "leftAt": at}, memql.VersionTimeAfter(seat.Version, now)); err != nil {
			return reconcilePlan{}, err
		}
		wrote = true
		tally.closed++
		if seat.Host && at.After(hostGoneAt) {
			hostGoneAt = at
		}
	}
	if wrote {
		if v, err = s.viewAgain(fresh, v); err != nil {
			return reconcilePlan{}, err
		}
	}

	// 2. Identities the media server shows that hold no seat in the call now.
	var plan reconcilePlan
	inCall := map[string]bool{}
	for _, seat := range v.seats {
		if seat.occupying() {
			inCall[seat.Identity] = true
		}
	}
	for _, participant := range present {
		if !inCall[participant.Identity] {
			plan.disconnect = append(plan.disconnect, participant.Identity)
		}
	}

	// 3. The queue's expired entries, marked BEFORE the host and the empty decisions, so
	// this phase decides from the rows endIfDue reads again: a pass never deletes a call's
	// media room on a decision that an entry it marked itself then reverses (the file note).
	var waiting []callrules.QueueEntry
	rows := map[string]queueEntryRow{}
	for _, entry := range v.entries {
		if entry.waiting() {
			waiting = append(waiting, entry.rule())
			rows[entry.ID] = entry
		}
	}
	expired := callrules.ExpiredEntries(waiting, now, v.policy.QueueEntryTTL)
	for _, entryID := range expired {
		entry := rows[entryID]
		if err := s.store.updateQueueEntry(fresh, owner, entry, entryExpired, time.Time{}, memql.VersionTimeAfter(entry.Version, now)); err != nil {
			return reconcilePlan{}, err
		}
		tally.expired++
	}
	if len(expired) > 0 {
		if v, err = s.viewAgain(fresh, v); err != nil {
			return reconcilePlan{}, err
		}
	}

	// 4. The host.
	wrote = false
	switch {
	case v.hostPresent() && !v.room.HostLeftAt.IsZero():
		// A host is in the call, so nothing counts down (a host let back in whose write
		// clearing the record failed, or a hand-over cut short).
		if err := s.store.updateRoom(fresh, owner, v.room, map[string]any{"hostLeftAt": ""}, memql.VersionTimeAfter(v.room.Version, now)); err != nil {
			return reconcilePlan{}, err
		}
		wrote = true
	case !v.hostPresent() && !hostGoneAt.IsZero() && v.room.HostLeftAt.IsZero():
		// The last host seat vanished: the countdown runs from when it went.
		if err := s.store.updateRoom(fresh, owner, v.room, map[string]any{"hostLeftAt": hostGoneAt}, memql.VersionTimeAfter(v.room.Version, now)); err != nil {
			return reconcilePlan{}, err
		}
		wrote = true
	}
	if wrote {
		if v, err = s.viewAgain(fresh, v); err != nil {
			return reconcilePlan{}, err
		}
	}
	action, to, err := s.hostLeft(fresh, v, now)
	if err != nil {
		return reconcilePlan{}, err
	}
	due := false
	switch action {
	case callrules.HostLeftHandOver:
		if err := s.handOver(fresh, v, *to, now); err != nil {
			return reconcilePlan{}, err
		}
		tally.handedOver++
		if v, err = s.viewAgain(fresh, v); err != nil {
			return reconcilePlan{}, err
		}
	case callrules.HostLeftEnd:
		due = true
	}

	// 5. How the call ends, if it does.
	empty, err := s.endsEmpty(fresh, v, now)
	if err != nil {
		return reconcilePlan{}, err
	}
	if empty {
		if err := s.endRows(fresh, v, callrules.EndReasonEmpty, now); err != nil {
			return reconcilePlan{}, err
		}
		return reconcilePlan{end: callrules.EndReasonEmpty}, nil
	}
	if due {
		plan.end = callrules.EndReasonHostLeft
	}
	return plan, nil
}

// viewAgain reads a call again after a write, keeping the limits already read: a write
// does not move them, and the next decision is fenced on the rows as they stand.
func (s *Service) viewAgain(ctx context.Context, v roomView) (roomView, error) {
	owner := v.room.OwnerUserID
	room, err := s.store.roomByID(ctx, owner, v.room.ID)
	if err != nil {
		return roomView{}, err
	}
	if room == nil {
		return roomView{}, fmt.Errorf("calls: the call %s is gone", v.room.ID)
	}
	seats, err := s.store.seatsForRoom(ctx, owner, room.ID)
	if err != nil {
		return roomView{}, err
	}
	entries, err := s.store.queueEntriesForRoom(ctx, owner, room.ID)
	if err != nil {
		return roomView{}, err
	}
	return roomView{room: *room, seats: seats, entries: entries, policy: v.policy}, nil
}

// endIfDue is the second gated phase of a host_left end (the file note's step 5): the
// call read again, fresh, and ended only if the countdown still finds it due -- a host
// back, a host joining the line, or a key holder come in, since the first phase keeps it
// open. Those are the only things that change its answer: the first phase marked the
// expired entries before it decided ("The two phases", in the file note). A call that
// emptied meanwhile ends as empty when the empty-room rule ends it (host.go endsEmpty). It
// answers whether the call ended here.
func (s *Service) endIfDue(ctx context.Context, hint roomRow) (bool, error) {
	release, err := s.gates(ctx, endGates(hint)...)
	if err != nil {
		return false, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	room, err := s.store.roomByID(fresh, hint.OwnerUserID, hint.ID)
	if err != nil || room == nil || !room.open() {
		return false, err
	}
	v, err := s.viewRoom(fresh, *room)
	if err != nil {
		return false, err
	}
	empty, err := s.endsEmpty(fresh, v, now)
	if err != nil {
		return false, err
	}
	if empty {
		return true, s.endRows(fresh, v, callrules.EndReasonEmpty, now)
	}
	action, _, err := s.hostLeft(fresh, v, now)
	if err != nil || action != callrules.HostLeftEnd {
		return false, err
	}
	return true, s.endRows(fresh, v, callrules.EndReasonHostLeft, now)
}

// bounded runs one call to a media server's control API within mediaCallBound.
func bounded(ctx context.Context, call func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, mediaCallBound)
	defer cancel()
	return call(ctx)
}

// handleReconcile is the callsReconcile builtin: one reconciliation pass, for the engine's
// own reconcileCalls automation and nobody else.
//
// THE AUTOMATION, AT INTERNAL ORIGIN, AND ONLY IT. A pass reads every deployable's calls
// and writes them as each one's owner, so no person may start one, whatever part they
// hold, and neither may the cluster's operator, the cluster acting outside that
// automation, another automation, or an automation of the same name from a pack or a
// runtime domain: auth.IsEngineAutomationActor admits the actor the executor mints for an
// automation of the engine's own tree, and no other. The check comes before anything is
// read. The builtin declares no capability for that reason (dsl/calls/builtins.memql).
func (i *Integration) handleReconcile(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || !auth.IsEngineAutomationActor(ac, reconcileAutomation) || !auth.OriginFromContext(ctx).IsInternal() {
		return nil, refuse("Calls are reconciled by the cluster's own reconcileCalls automation, every thirty seconds. Nothing else runs a pass.")
	}
	return nil, i.service.Reconcile(ctx)
}

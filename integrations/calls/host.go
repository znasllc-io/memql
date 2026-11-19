package calls

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// host.go -- a call's host controls, how a call ends, the host-left countdown and the
// empty-room rule (design sections 7.7, "Host controls", and 8, "End"). The rules are the
// pure module's (component/calls host.go and countdown.go); this file reads the state
// they decide from, under the gates service.go's file note describes, and writes what
// they decided. Reconciliation (reconcile.go) applies the countdown and the empty-room
// rule on its pass; Leave (service.go) applies them as a seat leaves; and the media
// server's room_finished (events.go) applies the empty-room rule.
//
// # Who is a host
//
// A HOST CONTROL IS MADE BY A HOST SEAT IN THE CALL NOW: a seat of the call marked host --
// the key holder who started it, anyone who joined with its host link, or a key holder the
// call was handed to -- whose status is admitted or in_call. Holding the host link is what
// gives host controls (design 8: the host is whoever joins with the host link), so a call
// can have several hosts, and any of them acts. Everybody else is refused ErrForbidden
// (not_host): a guest, a session seated nowhere, a host still waiting for a seat (not in
// the call yet) and a host who has left. A control on an ended call is ErrEnded
// (call_ended), asked first. A seat a control names that is not in the host's own call is
// ErrNotFound, so a host learns nothing about other calls. The session is read again on
// every control (service.go's file note), and the host is asked again, fresh, under the
// gates before anything is written.
//
// # The controls
//
//   - mute, camera_off and stop_share act on the media server alone: each of the seat's
//     tracks of the matching sources (calls.HostActionSources) that the media server shows
//     unmuted is muted. A seat with none of them, or nobody connected, is no failure: the
//     state asked for holds. Only the person can unmute themselves; a muted track is
//     unmuted by its publisher. No row records a mute, so the control takes no gate.
//   - remove sets the seat removed, gives up its place in line, and disconnects its
//     identity after the gates, best effort. Its session never comes back in: a removed
//     seat refuses every join and poll its session makes (service.go). A pass cannot be
//     revoked, so reconciliation disconnects the identity again whenever the media server
//     shows it (reconcile.go). KNOWN LIMITATION: a person who clears their session and
//     comes back through the link is somebody the call has never seen, and takes a new
//     seat -- this part has no identity of theirs to bar; keyed-fingerprint bans come
//     later. A host does not remove their own seat; they leave.
//   - lock and unlock set room.locked.
//   - admit lets one waiting seat in past the lock: the seat records when (letInAt) and
//     from then on passes the lock as a host does (seatRow.passesLock). The admission
//     hands out no pass, since only a waiter's own request can carry its pass
//     (service.go), so the seat is decided on its own next poll or rejoin exactly as a
//     running call's new ask is -- admitted when the media server has a seat for it after
//     the line ahead, queued otherwise -- and a queue entry held behind the lock goes back
//     in line at its own arrival (serverLoad.queue). A seat already in is no failure; one
//     that left or was removed is ErrConflict (not_waiting).
//   - end deletes the media room, then ends the call (endRows).
//
// # The roster
//
// A HOST SEES WHOM ITS CONTROLS NAME through the call's roster (Roster, calls.Roster):
// whether the call is locked now (a booked call opens locked, and any host of it locks or
// unlocks it), so a host's page offers Lock or Unlock and never both; the seats in the
// call in the order they were let in (joinedAt, which every admission writes), each with
// its media identity, so a client matches the media server's participants to seats; and
// the seats waiting to come in, in line order, each with when it began waiting
// (waitingSince, written whenever a seat begins to wait). LINE ORDER is the order the
// call's waiters would come in: first those holding a place in the media server's line,
// in that line's order (calls.OrderQueue: class, then arrival), then those held behind
// the lock with no place, by when they began waiting, since a lifted lock sends each of
// them to the back of the line on its own next poll. The roster is the host's alone, by
// the rule for who is a host above: anybody else, and an ended call, read none (nil, not
// an error). It is a read that writes nothing, made fresh on every ask, since a host's
// page polls it to see who is knocking and whether another host locked the call. KNOWN
// LIMITATION: a waiter held behind the lock keeps no record of its polls, so one who
// closed the page is listed until the call ends or a host removes them; and a waiter
// whose place in line ran out is listed in its old place until its entry is marked
// expired.
//
// # Ending a call
//
// A call ends three ways, each recorded on the room (endReason): a host ends it (ended);
// the last person in it leaves with nobody waiting to come in (empty), which a booked
// call does only after its booked end; or nobody hosting it has been in it for the
// policy's HostLeftGrace and no key holder in it took over (host_left). EVERY WAY WRITES
// ONE SEQUENCE (endRows) under the booking's, the room's and the server's gates
// (endGates): every seat in the call or waiting leaves at that moment, every waiting
// queue entry expires, THE BOOKING IS MARKED ENDED, and the ROOM IS WRITTEN LAST. A
// failure part-way leaves the room open, and the next attempt -- a retried end,
// or reconciliation's next pass -- ends it again; a booking marked ended while its room
// is still open holds no slot and is joined through its room. Marking the booking ended
// is what keeps a key's bookings read (callsScheduledCallsForKey) to the key's open calls
// and upcoming bookings (schedule.go's file note).
//
// WHERE THE MEDIA ROOM IS DELETED. A call that ends with people in it -- a host ending it,
// or the countdown running out -- has its media room deleted BEFORE its rows are written,
// and outside the gates: a media server slow to answer must not hold up its server's
// admissions, and a deletion that fails writes nothing, so the end is retried rather than
// leaving people connected to a call the rows say is over (a connected participant keeps
// their media session for as long as they stay). A decision that changes between the
// deletion and the gated write -- a host back in that instant -- leaves an open call with
// no media room, which the next pass anybody in it asks for opens again (service.go
// ensureMediaRoom). A call that ends empty has nobody in it by the rows' account, so its
// rows are written under the gates where that was decided, and its media room is deleted
// after them, best effort: a media server closes an empty room on its own.
//
// # The host-left countdown
//
// The countdown runs from hostLeftAt (calls.HostLeft) -- for a booked call, from its booked
// start when that is later. The last host seat in the call leaving stamps it (service.go
// Leave; reconciliation stamps it too when it closes a vanished host seat, at that seat's
// leave time: its last sign of life, which a media server's participant_left records as
// the disconnect, events.go), and a host seat let back in clears it (service.go). A BOOKED
// CALL COUNTS FROM ITS BOOKED START AT THE EARLIEST, so a host who opens it early to check
// it and leaves does not end it before it begins (hostLeft). A HOST WHO NEVER GOT IN NEVER
// LEFT: a call whose host has been queued since Start counts nothing down. A BOOKED CALL
// WHOSE HOST NEVER CAME counts from its booked start, or from its opening when a guest
// opened it after the start, so a guest who arrives late is given the grace a host who
// left is given and does not open a call that is already over; while a host seat of the
// call waits in line with a live place, the host is coming and nothing counts. A host seat
// passes the lock, so it only ever waits in line: once its place has expired (its holder
// stopped polling) the host is not coming, and the countdown runs from the booked start
// as for a host who never came. A call with a KEY HOLDER in it -- a seat whose session a
// key opened, the key the deployable's and active -- is handed to them at once (design 8:
// "their pass or key then covers the guests"): their seat becomes a host seat and the
// room's hostSeatId, and the countdown is cleared. Reconciliation applies the countdown
// (reconcile.go), so a hand-over or an end comes within one pass.
//
// # The empty-room rule
//
// A call nobody is in and nobody is waiting for ends at once, empty: when the last seat in
// it leaves (service.go Leave), on reconciliation's pass, which also ends a call whose
// last waiter gave up, and when its media server says it closed the media room
// (events.go room_finished). A BOOKED CALL ONLY AFTER ITS BOOKED END (endsEmpty): before
// it, an emptied booked call stays open for its host, and the countdown above ends it if
// nobody comes. A waiting seat counts while it holds a live place in line (an entry
// inside its deployable's QueueEntryTTL). A seat HELD BEHIND A LOCK -- a guest's,
// not a host's and not one a host admitted -- counts while it holds no place at all: a
// lock keeps no entry (service.go), and a locked call with nobody hosting it is ended by
// the countdown instead, which counts only host seats (hostComing). A seat that passes the
// lock (seatRow.passesLock) waits only in line, and with no live place it has given up,
// as has a waiter in an unlocked call with no live entry (stillWaiting). KNOWN
// LIMITATION: a waiter behind a lock its host has just lifted, or one a host has just
// admitted, holds no entry until its next poll, so a call whose last person leaves in
// that moment ends with that waiter still asking.

// Host makes one host control with a session (the file note).
func (s *Service) Host(ctx context.Context, sess callrules.Session, req callrules.HostActionRequest) error {
	action := strings.TrimSpace(req.Action)
	if !callrules.IsHostAction(action) {
		return fmt.Errorf("%w: no host control has that name", callrules.ErrInvalid)
	}
	target := memql.BareShortId(strings.TrimSpace(req.Seat))
	if callrules.HostActionNamesSeat(action) && target == "" {
		return fmt.Errorf("%w: the %s control names the seat it is done to", callrules.ErrInvalid, action)
	}
	h, err := s.hostOf(ctx, sess)
	if err != nil {
		return err
	}
	switch action {
	case callrules.HostMute, callrules.HostCameraOff, callrules.HostStopShare:
		return s.muteTracks(ctx, h, target, action)
	case callrules.HostRemove:
		return s.remove(ctx, h, target)
	case callrules.HostLock, callrules.HostUnlock:
		return s.setLock(ctx, h, action == callrules.HostLock)
	case callrules.HostAdmit:
		return s.admit(ctx, h, target)
	}
	return s.end(ctx, h)
}

// acting is who makes a host control: the host's deployable, session and seat, and their
// call, as read before the gates.
type acting struct {
	site callrules.SiteRef
	seat seatRow
	room roomRow
}

// hostOf resolves the session making a host control: a host seat in an open call, in it
// now (the file note). An ended call is refused first, so a host of a call that ended
// hears that it ended.
func (s *Service) hostOf(ctx context.Context, sess callrules.Session) (acting, error) {
	site, err := checkSite(sess.Site)
	if err != nil {
		return acting{}, fmt.Errorf("%w: %v", callrules.ErrNoSession, err)
	}
	row, _, err := s.session(ctx, site, sess.Token, s.now())
	if err != nil {
		return acting{}, err
	}
	seat, room, err := s.seatAndRoom(ctx, site.OwnerUserID, row)
	if err != nil {
		return acting{}, err
	}
	if seat == nil || room == nil {
		return acting{}, notHost()
	}
	if !room.open() {
		return acting{}, callEnded()
	}
	if !seat.Host || !seat.occupying() {
		return acting{}, notHost()
	}
	return acting{site: site, seat: *seat, room: *room}, nil
}

// stillHost asks again, fresh and under the gates, what hostOf asked: the call is open and
// the actor's seat is still a host seat in it. It answers the room as read.
func (s *Service) stillHost(ctx context.Context, h acting) (roomRow, error) {
	owner := h.site.OwnerUserID
	room, err := s.store.roomByID(ctx, owner, h.room.ID)
	if err != nil {
		return roomRow{}, err
	}
	if room == nil || !room.open() {
		return roomRow{}, callEnded()
	}
	seat, err := s.store.seatByID(ctx, owner, h.seat.ID)
	if err != nil {
		return roomRow{}, err
	}
	if seat == nil || seat.RoomID != room.ID || !seat.Host || !seat.occupying() {
		return roomRow{}, notHost()
	}
	return *room, nil
}

// targetIn is the seat a control names, which must be a seat of the host's own call.
func (s *Service) targetIn(ctx context.Context, h acting, seatID string) (seatRow, error) {
	seat, err := s.store.seatByID(ctx, h.site.OwnerUserID, seatID)
	if err != nil {
		return seatRow{}, err
	}
	if seat == nil || seat.RoomID != h.room.ID {
		return seatRow{}, fmt.Errorf("%w: no seat of this call has that id", callrules.ErrNotFound)
	}
	return *seat, nil
}

func notHost() error {
	return callrules.Refuse(callrules.ErrForbidden, callrules.ReasonNotHost, "only a host in the call makes a host control")
}

func callEnded() error {
	return callrules.Refuse(callrules.ErrEnded, callrules.ReasonCallEnded, "the call has ended")
}

// ---------------------------------------------------------------------------
// the roster
// ---------------------------------------------------------------------------

// Roster is the call a session's host seat is in, as the host controls name it (the file
// note): nil for a session that is not a host seat in an open call, ErrNoSession for one
// that opens nothing. A read that writes nothing, answered by the database.
func (s *Service) Roster(ctx context.Context, sess callrules.Session) (*callrules.Roster, error) {
	fresh := memql.ContextWithFreshRead(ctx)
	h, err := s.hostOf(fresh, sess)
	// NOT A HOST IN AN OPEN CALL IS NO ROSTER, NOT A REFUSAL: a guest, a seat outside the
	// call, a session seated nowhere and an ended call all ask their session about
	// themselves, and only a host is told about the others.
	if errors.Is(err, callrules.ErrForbidden) || errors.Is(err, callrules.ErrEnded) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	owner := h.site.OwnerUserID
	seats, err := s.store.seatsForRoom(fresh, owner, h.room.ID)
	if err != nil {
		return nil, err
	}
	entries, err := s.store.queueEntriesForRoom(fresh, owner, h.room.ID)
	if err != nil {
		return nil, err
	}
	return rosterOf(h.room, seats, entries), nil
}

// rosterOf orders one call's seats into its roster (the file note's order), with whether
// the call is locked, from the room as read. Ties fall to the seat's id, so two reads of
// the same rows list them the same way.
func rosterOf(room roomRow, seats []seatRow, entries []queueEntryRow) *callrules.Roster {
	roster := &callrules.Roster{Locked: room.Locked, InCall: []callrules.RosterSeat{}, Waiting: []callrules.RosterWaiter{}}
	var in, waiting []seatRow
	for _, seat := range seats {
		switch {
		case seat.occupying():
			in = append(in, seat)
		case seat.Status == seatWaiting:
			waiting = append(waiting, seat)
		}
	}
	slices.SortStableFunc(in, func(a, b seatRow) int {
		if c := a.JoinedAt.Compare(b.JoinedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, seat := range in {
		roster.InCall = append(roster.InCall, callrules.RosterSeat{
			Seat: seat.ID, DisplayName: seat.DisplayName, Kind: seat.Kind, Status: seat.Status, Identity: seat.Identity, Host: seat.Host,
		})
	}

	// A waiter's place in line is its waiting entry, the newest when somehow there are two.
	places := map[string]callrules.QueueEntry{}
	for _, entry := range entries {
		if !entry.waiting() || entry.SeatID == "" {
			continue
		}
		if prior, held := places[entry.SeatID]; !held || entry.EnqueuedAt.After(prior.EnqueuedAt) {
			places[entry.SeatID] = entry.rule()
		}
	}
	var line []callrules.QueueEntry
	seatOf := map[string]seatRow{}
	var behind []seatRow
	for _, seat := range waiting {
		if place, inLine := places[seat.ID]; inLine {
			line = append(line, place)
			seatOf[place.ID] = seat
			continue
		}
		behind = append(behind, seat)
	}
	slices.SortStableFunc(behind, func(a, b seatRow) int {
		if c := a.WaitingSince.Compare(b.WaitingSince); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	ordered := make([]seatRow, 0, len(waiting))
	for _, place := range callrules.OrderQueue(line) {
		ordered = append(ordered, seatOf[place.ID])
	}
	ordered = append(ordered, behind...)
	for _, seat := range ordered {
		roster.Waiting = append(roster.Waiting, callrules.RosterWaiter{
			Seat: seat.ID, DisplayName: seat.DisplayName, Kind: seat.Kind, Since: seat.WaitingSince,
		})
	}
	return roster
}

// ---------------------------------------------------------------------------
// the controls
// ---------------------------------------------------------------------------

// muteTracks mutes the named seat's tracks of the sources a track control acts on, each
// one the media server shows unmuted (the file note).
func (s *Service) muteTracks(ctx context.Context, h acting, target, action string) error {
	seat, err := s.targetIn(ctx, h, target)
	if err != nil {
		return err
	}
	sources, _ := callrules.HostActionSources(action)
	backend, err := s.backendFor(ctx, h.room)
	if err != nil || backend == nil {
		return err
	}
	present, err := backend.Participants(ctx, h.room.MediaRoom)
	if err != nil {
		return err
	}
	for _, participant := range present {
		if participant.Identity != seat.Identity {
			continue
		}
		for _, track := range participant.Tracks {
			if track.Muted || !slices.Contains(sources, track.Source) {
				continue
			}
			if err := backend.MuteTrack(ctx, h.room.MediaRoom, participant.Identity, track.SID); err != nil {
				return err
			}
		}
	}
	return nil
}

// remove takes a seat out of its call for good (the file note), and disconnects its
// identity once the gates are released.
func (s *Service) remove(ctx context.Context, h acting, target string) error {
	if target == h.seat.ID {
		return fmt.Errorf("%w: a host leaves the call rather than removing their own seat", callrules.ErrInvalid)
	}
	seat, err := s.targetIn(ctx, h, target)
	if err != nil {
		return err
	}
	if err := s.removeGated(ctx, h, seat.ID); err != nil {
		return err
	}
	// After the gates, for Leave's reason, and best effort: reconciliation disconnects
	// the identity again whenever the media server shows it.
	if err := s.disconnect(ctx, h.room, seat.Identity); err != nil {
		s.logger.Warn("calls: a seat was removed but its media identity could not be disconnected; reconciliation tries again",
			"component", pluginName, "room", h.room.ID, "seat", seat.ID, "reason", failureReason(err))
	}
	return nil
}

func (s *Service) removeGated(ctx context.Context, h acting, seatID string) error {
	release, err := s.gates(ctx, roomGate(h.room.ID), serverGate(h.room.MediaServerID))
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	owner := h.site.OwnerUserID
	now := s.now()
	if _, err := s.stillHost(fresh, h); err != nil {
		return err
	}
	seat, err := s.store.seatByID(fresh, owner, seatID)
	if err != nil {
		return err
	}
	if seat == nil || seat.RoomID != h.room.ID {
		return fmt.Errorf("%w: no seat of this call has that id", callrules.ErrNotFound)
	}
	if seat.Status == seatRemoved {
		return nil
	}
	// A seat in the call or waiting leaves it now; one that had left already keeps the
	// time it left, and is barred from coming back.
	fields := map[string]any{"status": seatRemoved}
	if seat.placed() {
		fields["leftAt"] = now
	}
	if err := s.store.updateSeat(fresh, owner, *seat, fields, memql.VersionTimeAfter(seat.Version, now)); err != nil {
		return err
	}
	if seat.Status == seatWaiting {
		return s.giveUpPlace(fresh, owner, h.room.ID, seat.ID, now)
	}
	return nil
}

// giveUpPlace expires a seat's waiting queue entries: its place in line goes to whoever
// is next.
func (s *Service) giveUpPlace(ctx context.Context, owner, roomID, seatID string, now time.Time) error {
	entries, err := s.store.queueEntriesForRoom(ctx, owner, roomID)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.SeatID != seatID || !entry.waiting() {
			continue
		}
		if err := s.store.updateQueueEntry(ctx, owner, entry, entryExpired, time.Time{}, memql.VersionTimeAfter(entry.Version, now)); err != nil {
			return err
		}
	}
	return nil
}

// setLock locks or unlocks a call; a call already so is not written.
func (s *Service) setLock(ctx context.Context, h acting, locked bool) error {
	release, err := s.gates(ctx, roomGate(h.room.ID), serverGate(h.room.MediaServerID))
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	room, err := s.stillHost(fresh, h)
	if err != nil {
		return err
	}
	if room.Locked == locked {
		return nil
	}
	return s.store.updateRoom(fresh, h.site.OwnerUserID, room, map[string]any{"locked": locked}, memql.VersionTimeAfter(room.Version, now))
}

// admit lets one waiting seat in past the lock (the file note).
func (s *Service) admit(ctx context.Context, h acting, target string) error {
	if _, err := s.targetIn(ctx, h, target); err != nil {
		return err
	}
	release, err := s.gates(ctx, roomGate(h.room.ID), serverGate(h.room.MediaServerID))
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	owner := h.site.OwnerUserID
	now := s.now()
	if _, err := s.stillHost(fresh, h); err != nil {
		return err
	}
	seat, err := s.store.seatByID(fresh, owner, target)
	if err != nil {
		return err
	}
	switch {
	case seat == nil || seat.RoomID != h.room.ID:
		return fmt.Errorf("%w: no seat of this call has that id", callrules.ErrNotFound)
	case seat.occupying():
		return nil
	case seat.Status != seatWaiting:
		return callrules.Refuse(callrules.ErrConflict, callrules.ReasonNotWaiting, "only a seat waiting to come in is admitted")
	case !seat.LetInAt.IsZero():
		return nil
	}
	return s.store.updateSeat(fresh, owner, *seat, map[string]any{"letInAt": now}, memql.VersionTimeAfter(seat.Version, now))
}

// end ends a call for everyone in it: the media room first, outside the gates, then the
// rows under them (the file note). A call some other request ended meanwhile is no
// failure: the state asked for holds.
func (s *Service) end(ctx context.Context, h acting) error {
	if err := s.deleteMediaRoom(ctx, h.room); err != nil {
		return err
	}
	release, err := s.gates(ctx, endGates(h.room)...)
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	room, err := s.stillHost(fresh, h)
	if errors.Is(err, callrules.ErrEnded) {
		return nil
	}
	if err != nil {
		return err
	}
	v, err := s.viewRoom(fresh, room)
	if err != nil {
		return err
	}
	return s.endRows(fresh, v, callrules.EndReasonEnded, now)
}

// ---------------------------------------------------------------------------
// a call as one decision sees it, and how it ends
// ---------------------------------------------------------------------------

// roomView is one call as a decision under its gates reads it: the room, its seats, its
// queue entries and its deployable's limits, all fresh.
type roomView struct {
	room    roomRow
	seats   []seatRow
	entries []queueEntryRow
	policy  callrules.Policy
}

// viewRoom reads a call's seats, queue entries and limits as the room's site owner. The
// caller holds the room's gates and passes a fresh context.
func (s *Service) viewRoom(ctx context.Context, room roomRow) (roomView, error) {
	owner := room.OwnerUserID
	seats, err := s.store.seatsForRoom(ctx, owner, room.ID)
	if err != nil {
		return roomView{}, err
	}
	entries, err := s.store.queueEntriesForRoom(ctx, owner, room.ID)
	if err != nil {
		return roomView{}, err
	}
	policy, err := s.store.PolicyFor(ctx, owner, room.SiteID)
	if err != nil {
		return roomView{}, err
	}
	return roomView{room: room, seats: seats, entries: entries, policy: policy}, nil
}

// hostPresent reports whether a host seat of the call holds a media server's seat now.
func (v roomView) hostPresent() bool {
	for _, seat := range v.seats {
		if seat.Host && seat.occupying() {
			return true
		}
	}
	return false
}

// stillWaiting reports whether a seat still waits to come into the call (the file note's
// empty-room rule): it holds a live place in line, or it is held behind the call's lock,
// which keeps no place at all.
//
// ONLY A SEAT THE LOCK HOLDS waits with no place. A seat that passes the lock -- a host
// seat, or one a host admitted (seatRow.passesLock) -- is never held behind it: it waits
// only for a seat on the media server, and a seat is waited for in line, so once its
// place has expired it has given that place up, whether or not anything has marked the
// entry expired yet. Counting it as held would keep a booked call whose queued host gave
// up open for good: a host seat that waits counts as the host coming (hostComing), which
// stops the countdown, and as somebody waiting, which keeps the call from ending empty.
func (v roomView) stillWaiting(seat seatRow, now time.Time) bool {
	if seat.Status != seatWaiting {
		return false
	}
	placed := false
	for _, entry := range v.entries {
		if entry.SeatID != seat.ID || !entry.waiting() {
			continue
		}
		placed = true
		if len(callrules.ExpiredEntries([]callrules.QueueEntry{entry.rule()}, now, v.policy.QueueEntryTTL)) == 0 {
			return true
		}
	}
	return !placed && v.room.Locked && !seat.passesLock()
}

// endsEmpty is the empty-room rule as it ENDS a call (the file note): nobody in it and
// nobody waiting to come in (empty), and, for a booked call, its booked end reached. Every
// decision that may end a call empty asks this, never empty alone: a leave, both phases of
// a pass, and a media server's room_finished.
//
// A BOOKED CALL IS ITS HOST'S UNTIL ITS BOOKED END. A candidate who opens an interview
// early to try the link and leaves would otherwise end it, booking and all, and the
// interviewer arriving at the start would be refused. Before its end an emptied booked
// call stays open, holding its key's call for up to the countdown, and the countdown ends
// it if nobody comes (hostLeft: from the later of its booked start and hostLeftAt, or,
// when its host never came, its opening), so it is never ended before its booked start. A
// booking that is gone, or carries no end, is decided as an ad hoc call's: without its
// start the countdown has nothing to count from for a host who never came, and the call
// would otherwise stay open for good.
func (s *Service) endsEmpty(ctx context.Context, v roomView, now time.Time) (bool, error) {
	if !v.empty(now) {
		return false, nil
	}
	if v.room.ScheduledCallID == "" {
		return true, nil
	}
	booking, err := s.store.bookingByID(ctx, v.room.OwnerUserID, v.room.ScheduledCallID)
	if err != nil {
		return false, err
	}
	if booking == nil || booking.End.IsZero() {
		return true, nil
	}
	return !now.Before(booking.End), nil
}

// empty reports whether nobody is in the call and nobody waits to come in. It decides
// nothing alone: a call ends empty on endsEmpty.
func (v roomView) empty(now time.Time) bool {
	if occupants(v.seats, v.room.ID) > 0 {
		return false
	}
	for _, seat := range v.seats {
		if v.stillWaiting(seat, now) {
			return false
		}
	}
	return true
}

// hostComing reports whether a host seat of the call still waits to come in. A host seat
// passes the lock, so it waits only with a live place in line (stillWaiting): a host who
// stopped polling for its deployable's QueueEntryTTL is not coming.
func (v roomView) hostComing(now time.Time) bool {
	for _, seat := range v.seats {
		if seat.Host && v.stillWaiting(seat, now) {
			return true
		}
	}
	return false
}

// endGates is the gates a decision that may end a call takes, in the global order: the
// call's booking's when it is a booked call's (an end marks the booking ended), then the
// room's and its server's. A room's booking and server never change, so they are read
// before the gates are taken.
func endGates(room roomRow) []string {
	var keys []string
	if room.ScheduledCallID != "" {
		keys = append(keys, bookingGate(room.ScheduledCallID))
	}
	return append(keys, roomGate(room.ID), serverGate(room.MediaServerID))
}

// endRows writes a call's end, under the gates the caller holds (endGates), in the order
// the file note gives: every seat in the call or waiting leaves now, every waiting queue
// entry expires, the booking is marked ended, and the room is written last.
func (s *Service) endRows(ctx context.Context, v roomView, reason string, now time.Time) error {
	owner := v.room.OwnerUserID
	for _, seat := range v.seats {
		if !seat.placed() {
			continue
		}
		if err := s.store.updateSeat(ctx, owner, seat, map[string]any{"status": seatLeft, "leftAt": now}, memql.VersionTimeAfter(seat.Version, now)); err != nil {
			return err
		}
	}
	for _, entry := range v.entries {
		if !entry.waiting() {
			continue
		}
		if err := s.store.updateQueueEntry(ctx, owner, entry, entryExpired, time.Time{}, memql.VersionTimeAfter(entry.Version, now)); err != nil {
			return err
		}
	}
	if err := s.endBooking(ctx, owner, v.room, now); err != nil {
		return err
	}
	return s.store.updateRoom(ctx, owner, v.room, map[string]any{
		"status": "ended", "endedAt": now, "endReason": reason,
	}, memql.VersionTimeAfter(v.room.Version, now))
}

// hostLeft is what the host-left countdown says of a call now (the file note), and the
// seat a hand-over goes to: nothing while a host is in it, or while none ever left and
// none is due; a hand-over to a key holder in it; a countdown; or the end.
//
// A BOOKED CALL COUNTS FROM ITS BOOKED START AT THE EARLIEST: from the later of that start
// and when its last host left, or, when its host never came, its opening. A host who opens
// the call early to check it and leaves would otherwise see it end before it began,
// booking and all, and whoever arrived at the start would be refused. With endsEmpty,
// which ends a booked call empty only after its booked end, nothing but a host's end ends
// a booked call before its start. A booking that is gone, or carries no start, changes
// nothing: a host who left counts from hostLeftAt, and a call whose host never came
// counts nothing, as before.
func (s *Service) hostLeft(ctx context.Context, v roomView, now time.Time) (callrules.HostLeftAction, *seatRow, error) {
	if v.hostPresent() {
		return callrules.HostLeftNone, nil, nil
	}
	since := v.room.HostLeftAt
	// A host who never got in never left, so an ad hoc call with none who left counts
	// nothing; nor does a booked call whose host is in line now.
	if since.IsZero() && (v.room.ScheduledCallID == "" || v.hostComing(now)) {
		return callrules.HostLeftNone, nil, nil
	}
	if v.room.ScheduledCallID != "" {
		booking, err := s.store.bookingByID(ctx, v.room.OwnerUserID, v.room.ScheduledCallID)
		if err != nil {
			return callrules.HostLeftNone, nil, err
		}
		switch {
		case booking != nil && !booking.Start.IsZero():
			if since.IsZero() {
				// Its host never came: from its opening, when that was after its start.
				since = v.room.OpenedAt
			}
			if booking.Start.After(since) {
				since = booking.Start
			}
		case since.IsZero():
			return callrules.HostLeftNone, nil, nil
		}
	}
	to, err := s.keyHolderIn(ctx, v)
	if err != nil {
		return callrules.HostLeftNone, nil, err
	}
	state := callrules.HostState{HostLeftAt: since}
	if to != nil {
		state.OtherKeyHolderSeat = to.ID
	}
	action, _ := callrules.HostLeft(state, now, v.policy.HostLeftGrace)
	return action, to, nil
}

// keyHolderIn is the seat a call is handed to when nobody hosting it is in it: a seat in
// the call, not a host, whose holder unlocked a key that is the deployable's and active --
// the earliest let in, ties by id, so every replica picks the same one. Nil is none.
func (s *Service) keyHolderIn(ctx context.Context, v roomView) (*seatRow, error) {
	site := callrules.SiteRef{SiteID: v.room.SiteID, OwnerUserID: v.room.OwnerUserID}
	var to *seatRow
	for i := range v.seats {
		seat := v.seats[i]
		if !seat.occupying() || seat.Host || seat.KeyID == "" {
			continue
		}
		if to != nil && (to.JoinedAt.Before(seat.JoinedAt) || (to.JoinedAt.Equal(seat.JoinedAt) && to.ID < seat.ID)) {
			continue
		}
		opens, err := s.keyOpensCalls(ctx, site, seat.KeyID)
		if err != nil {
			return nil, err
		}
		if opens {
			to = &v.seats[i]
		}
	}
	return to, nil
}

// handOver makes a key holder's seat the call's host: the seat first, then the room
// (its host seat, and no countdown), so a failure between the two leaves a host in the
// call, and reconciliation's next pass clears the countdown it finds beside one.
func (s *Service) handOver(ctx context.Context, v roomView, to seatRow, now time.Time) error {
	owner := v.room.OwnerUserID
	if err := s.store.updateSeat(ctx, owner, to, map[string]any{"host": true}, memql.VersionTimeAfter(to.Version, now)); err != nil {
		return err
	}
	return s.store.updateRoom(ctx, owner, v.room, map[string]any{"hostSeatId": to.ID, "hostLeftAt": ""}, memql.VersionTimeAfter(v.room.Version, now))
}

// ---------------------------------------------------------------------------
// the media server's control API, for one call
// ---------------------------------------------------------------------------

// backendFor is the control API of the media server a call is placed on. A server whose
// row is gone carries nothing any more, and is answered nil.
func (s *Service) backendFor(ctx context.Context, room roomRow) (MediaBackend, error) {
	server, err := s.store.MediaServerByID(ctx, room.MediaServerID)
	if err != nil || server == nil {
		return nil, err
	}
	credential, err := s.credential(ctx, *server)
	if err != nil {
		return nil, err
	}
	backend, err := s.media(*server, credential)
	if err != nil {
		return nil, fmt.Errorf("calls: reach media server %s: %w", server.ID, err)
	}
	return backend, nil
}

// deleteMediaRoom ends a call's media room and disconnects everyone in it. A room the
// server no longer has is not an error (MediaBackend.DeleteRoom).
func (s *Service) deleteMediaRoom(ctx context.Context, room roomRow) error {
	backend, err := s.backendFor(ctx, room)
	if err != nil || backend == nil {
		return err
	}
	if err := backend.DeleteRoom(ctx, room.MediaRoom); err != nil {
		return fmt.Errorf("calls: end the call's media room: %w", err)
	}
	return nil
}

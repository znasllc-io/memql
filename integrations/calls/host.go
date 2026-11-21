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
// A legacy call ends three ways, each recorded on the room (endReason): a host ends it (ended);
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
// A simulated-funded room additionally ends at its persisted allowance deadline
// (allowance_exhausted). Its payer key gate precedes these gates, its original sponsor
// remains fixed, and automatic host takeover by an unrelated key is disabled.
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
// it and leaves does not end it before it begins (hostLeft). An AD HOC CALL whose host
// never got in counts from that host's explicit leave or expired queue lease, once no
// host is still waiting. The same durable timestamps drive session reads and reconciliation
// on every replica; a host still polling in line has not abandoned the call. A BOOKED CALL
// WHOSE HOST NEVER CAME is its host's until its BOOKED END, and counts from there (design
// section 8: the host is whoever joins with the host link): the key holder booked that
// window, so a guest who opens it -- early, on time or late -- waits for the host, and a
// host who is late is let in by the host link inside it rather than finding both links
// dead. While a host seat of the call waits in line with
// a live place, the host is coming and nothing counts. A host seat passes the lock, so it
// only ever waits in line: once its place has expired (its holder stopped polling) the
// host never came, and the countdown runs from the booked end. A call with a KEY HOLDER in
// it -- a seat whose session a key opened, the key the deployable's and active -- is handed
// to them at once (design 8: "their pass or key then covers the guests"): their seat
// becomes a host seat and the room's hostSeatId, and the countdown is cleared.
// Reconciliation applies the countdown (reconcile.go), so a hand-over or an end comes
// within one pass. KNOWN LIMITATION: guests can hold a hostless booked call, and the key's
// call with it, until its booked end and the grace.
//
// # What a seat is told
//
// EVERY SEAT IN AN OPEN CALL ASKS AFTER ITSELF (GET /session, SessionCall) while it is in
// the call, and is told two things the rows alone record: whether it hosts the call now,
// which is how a key holder the call was handed to learns it is theirs (a hand-over hands
// out no new pass), and, while the countdown runs, when it ends unless a host is back
// first. That deadline is calls.HostLeft's, decided by hostLeft from the same rows
// reconciliation ends the call from, so the instant every page shows is the instant the
// pass ends the call at (design 8: "a countdown on screen"). A host is told its roster too.
// A seat waiting to come in is told the deadline as well; a seat that left or was removed,
// a session seated nowhere and a seat of an ended call are told nothing.
//
// # The empty-room rule
//
// A call nobody is in and nobody is waiting for ends at once, empty: when the last seat in
// it leaves (service.go Leave), on reconciliation's pass, which also ends a call whose
// last waiter gave up, and when its media server says it closed the media room
// (events.go room_finished). A BOOKED CALL ONLY AFTER ITS BOOKED END (endsEmpty): before
// it, an emptied booked call stays open for its host -- the countdown above ends it early
// only when a host who came has left and is not back -- and one whose host never came
// ends empty at its booked end. A waiting seat counts while it holds a live place in line
// (an entry inside its deployable's QueueEntryTTL). A seat HELD BEHIND A LOCK -- a guest's,
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
	if req.Spotlight != nil && strings.TrimSpace(req.Action) != "spotlight" {
		return callrules.ErrInvalid
	}
	if req.Action == "processing_consent" {
		return s.processingConsent(ctx, sess, req)
	}
	if req.Processing != nil {
		return callrules.ErrInvalid
	}
	ctx = context.WithValue(ctx, agentScopeContext{}, "control")
	action := strings.TrimSpace(req.Action)
	if action == "spotlight" {
		return s.setSpotlight(ctx, sess, req)
	}
	if req.Spotlight != nil {
		return callrules.ErrInvalid
	}
	if action == "agent_approve" || action == "agent_reject" {
		return s.reviewGuestAgent(ctx, sess, req)
	}
	if req.RequestID != "" || req.ReviewDigest != "" {
		return callrules.ErrInvalid
	}
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
	if action == callrules.HostEnd || action == callrules.HostRemove {
		s.checkpointUsage(ctx, h.room)
	}
	switch action {
	case callrules.HostStopShare:
		if managed, err := s.stopHostedResource(ctx, sess, h, target); managed || err != nil {
			return err
		}
		return s.muteTracks(ctx, h, target, action)
	case callrules.HostMute, callrules.HostCameraOff:
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
	site    callrules.SiteRef
	seat    seatRow
	room    roomRow
	session sessionRow
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
	if err := s.delegatedEffectAllowed(ctx, site, seat.KeyID); err != nil {
		return acting{}, err
	}
	return acting{site: site, seat: *seat, room: *room, session: *row}, nil
}

// stillHost asks again, fresh and under the gates, what hostOf asked: the call is open and
// the actor's seat is still a host seat in it. It answers the room as read.
func (s *Service) stillHost(ctx context.Context, h acting) (roomRow, error) {
	if err := s.delegatedEffectAllowed(ctx, h.site, h.seat.KeyID); err != nil {
		return roomRow{}, err
	}
	owner := h.site.OwnerUserID
	room, err := s.store.roomByID(ctx, owner, h.room.ID)
	if err != nil {
		return roomRow{}, err
	}
	if room == nil || !room.open() {
		return roomRow{}, callEnded()
	}
	// An occupied stable seat alone is not current authority. Its session may
	// have ended, moved elsewhere or lost its credential while provider I/O was
	// pending; real re-entry also replaces the media admission generation.
	session, err := s.store.sessionByID(ctx, owner, h.session.ID)
	if err != nil {
		return roomRow{}, err
	}
	if !session.usable(s.now()) || session.SiteID != h.site.SiteID || session.KeyID != h.session.KeyID || session.CredentialID != h.session.CredentialID || session.SeatID != h.seat.ID || session.RoomID != room.ID {
		return roomRow{}, notHost()
	}
	seat, err := s.store.seatByID(ctx, owner, h.seat.ID)
	if err != nil {
		return roomRow{}, err
	}
	if familyRoom(*room) && (seat == nil || seat.KeyID != room.KeyID || seat.Kind != string(callrules.KindPerson)) {
		return roomRow{}, notHost()
	}
	if seat == nil || seat.RoomID != room.ID || !seat.Host || !seat.occupying() || seat.Identity != h.seat.Identity || !seat.JoinedAt.Equal(h.seat.JoinedAt) {
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
// what a seat is told, and the roster
// ---------------------------------------------------------------------------

// SessionCall is what a session seated in an open call is told about it (the file note,
// "What a seat is told"; calls.SeatCall): whether its seat hosts the call now, when the
// host-left countdown ends while one runs, and, to a host, the roster. The zero SeatCall
// for a session seated nowhere, a seat that left or was removed, and a seat of an ended
// call: NOT A HOST IN AN OPEN CALL IS NO ROSTER, NOT A REFUSAL, since every seat asks its
// session about itself and only a host is told about the others. ErrNoSession for a
// session that opens nothing.
//
// A READ THAT WRITES NOTHING, answered by the database (a sibling replica's write is
// visible at once), and taken under no gate: a page polls it every few seconds, and a
// countdown or a hand-over the next poll shows is what it is for. The limits are read only
// when nobody hosting the call is in it, since only the countdown needs them.
func (s *Service) SessionCall(ctx context.Context, sess callrules.Session) (callrules.SeatCall, error) {
	none := callrules.SeatCall{}
	site, err := checkSite(sess.Site)
	if err != nil {
		return none, fmt.Errorf("%w: %v", callrules.ErrNoSession, err)
	}
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	row, pepper, err := s.session(fresh, site, sess.Token, now)
	if err != nil {
		return none, err
	}
	seat, room, err := s.seatAndRoom(fresh, site.OwnerUserID, row)
	if err != nil || seat == nil || room == nil || !room.open() || !seat.placed() {
		return none, err
	}
	owner := room.OwnerUserID
	v := roomView{room: *room}
	if v.seats, err = s.store.seatsForRoom(fresh, owner, room.ID); err != nil {
		return none, err
	}
	if v.entries, err = s.store.queueEntriesForRoom(fresh, owner, room.ID); err != nil {
		return none, err
	}
	var out callrules.SeatCall
	out.RoomID = room.ID
	out.Title = callTitle(room.Title, room.OpenedAt)
	out.AllowanceEndsAt = room.FundingDeadline
	out.FundingMode = room.FundingMode
	if room.FundingMode == usageMeteredMode {
		c, e := decodeUsageConfig(room.UsageConfig)
		if e != nil {
			return none, e
		}
		out.ParticipantLimit, out.DurationMinutes = c.ParticipantLimit, c.DurationMinutes
		out.Insights, out.Resources = c.Insights, c.Resources
		out.ParticipationMode = c.ParticipationMode
		if seat.Host {
			reserved := room.UsageReservedCents
			out.ReservedCents = &reserved
			out.CostCents = room.UsageCostCents
		}
	}
	if seat.occupying() {
		out.Spotlight, err = spotlightView(*room, v.seats)
		if err != nil {
			return none, err
		}
		out.OpenedAt = room.OpenedAt
		out.Identity = seat.Identity
		out.LeaveToken = leaveToken(pepper, site.SiteID, *row, *seat)
		for _, member := range v.seats {
			if member.occupying() && member.Kind == string(callrules.KindAgent) {
				agent := parseAgentPresentation(member.AgentData)
				agent.Seat, agent.Identity, agent.Name = member.ID, member.Identity, member.DisplayName
				out.Agents = append(out.Agents, agent)
			}
		}
	}
	if seat.occupying() {
		reportPolicy, e := s.store.PolicyFor(fresh, site.OwnerUserID, site.SiteID)
		if e != nil {
			return callrules.SeatCall{}, e
		}
		out.Reports = reportCapabilities(reportPolicy)
		out.Processing, err = s.processingStatus(fresh, *room, *seat, v.seats)
		if err != nil {
			return none, err
		}
	}
	if seat.Host && seat.occupying() {
		out.Host = true
		out.Roster = rosterOf(*room, v.seats, v.entries)
		out.PendingAgents, err = s.pendingGuestAgents(fresh, site, *room, *seat)
		if err != nil {
			return none, err
		}
	}
	// A present host needs no automatic-grace policy read, but must still see a
	// firm wrap-up selected by another host. That commitment lives in the seats.
	if !v.hostPresent() {
		if v.policy, err = s.store.PolicyFor(fresh, owner, room.SiteID); err != nil {
			return none, err
		}
	}
	verdict, err := s.hostLeft(fresh, v, now)
	if err != nil {
		return none, err
	}
	if verdict.action == callrules.HostLeftCountdown || verdict.action == callrules.HostLeftEnd {
		out.CountdownEndsAt = verdict.deadline
		out.CountdownKind = "host_left"
		if verdict.firm {
			out.CountdownKind = callrules.LeaveWrapUp
		}
	}
	return out, nil
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
			// Media inventory is external I/O. Recheck after it returns so a stale
			// request cannot retain host/delegated authority while waiting on media.
			if _, err := s.stillHost(memql.ContextWithFreshRead(ctx), h); err != nil {
				return err
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
	if familyRoom(h.room) {
		return accountRefusal("family_revoke_required")
	}
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
	if familyRoom(h.room) && !locked {
		return accountRefusal("family_private_call")
	}
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
	// Revalidate before dispatching an irreversible provider effect as well as
	// before recording its result. Provider I/O still runs outside room gates;
	// an already-dispatched authorized effect cannot be retroactively canceled.
	if _, err := s.stillHost(memql.ContextWithFreshRead(ctx), h); err != nil {
		if errors.Is(err, callrules.ErrEnded) {
			return nil
		}
		return err
	}
	if familyRoom(h.room) {
		return s.closeFamily(ctx, h.room, "family_host_ended")
	}
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
// call stays open, holding its key's call: the countdown ends it early only when a host
// who came has left (hostLeft: from the later of its booked start and hostLeftAt), so it
// is never ended before its booked start, and one whose host never came ends here at its
// booked end. A booking that is gone, or carries no end, is decided as an ad hoc call's:
// without its end the countdown has nothing to count from for a host who never came, and
// the call would otherwise stay open for good.
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
	if room.bounded() {
		keys = append(keys, keyGate(room.KeyID))
	}
	if room.ScheduledCallID != "" {
		keys = append(keys, bookingGate(room.ScheduledCallID))
	}
	return append(keys, roomGate(room.ID), serverGate(room.MediaServerID))
}

// endRows writes a call's end, under the gates the caller holds (endGates), in the order
// the file note gives: every seat in the call or waiting leaves now, every waiting queue
// entry expires, the booking is marked ended, and the room is written last.
func (s *Service) endRows(ctx context.Context, v roomView, reason string, now time.Time) error {
	// A delayed pass, empty leave, or later host End cannot charge time after a
	// committed firm wrap-up. The same cutoff applies to every settlement path.
	if _, deadline := departurePolicy(v.room, v.seats); !deadline.IsZero() && !now.Before(deadline) {
		now, reason = deadline, callrules.EndReasonWrapUp
	}

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
	cost, err := s.settleUsage(ctx, v.room, now)
	if err != nil {
		return err
	}
	fields := map[string]any{"status": "ended", "endedAt": now, "endReason": reason}
	if cost != nil {
		fields["usageCostCents"] = *cost
	}
	return s.store.updateRoom(ctx, owner, v.room, fields, memql.VersionTimeAfter(v.room.Version, now))
}

// hostLeftVerdict is what the host-left countdown says of a call now (hostLeft): what to
// do, the deadline while a countdown runs and once it has run out, and the seat a
// hand-over goes to.
type hostLeftVerdict struct {
	firm     bool
	action   callrules.HostLeftAction
	deadline time.Time
	to       *seatRow
}

// hostLeft is what the host-left countdown says of a call now (the file note), and the
// seat a hand-over goes to: nothing while a host is in it, or while none ever left and
// none is due; a hand-over to a key holder in it; a countdown, with its deadline; or the
// end. Reconciliation acts on it (reconcile.go), and every seat is told its deadline
// (SessionCall), so the instant a page shows is the one the pass ends the call at.
//
// WHERE THE COUNTDOWN COUNTS FROM is all this decides beside calls.HostLeft:
//
//   - An ad hoc call: from hostLeftAt, when its last host seat left. If no host ever
//     got in, from the last queued host's explicit leave or queue-lease expiry. A host
//     still polling in line counts nothing, but abandoning that line cannot leave guests
//     an unlimited room or hold the key's one call forever (abandonedHostAt).
//   - A booked call whose host came and left: from the later of hostLeftAt and its booked
//     start. A host who opens the call early to check it and leaves would otherwise see it
//     end before it began, booking and all.
//   - A BOOKED CALL WHOSE HOST NEVER CAME: from its booked END. The key
//     holder booked that window, and a host who has not come yet is late, not gone: a
//     guest who opened the call, early, on time or late, waits for them, and the host link
//     lets them in until then. With endsEmpty, which ends a booked call empty only after its
//     booked end, nothing but a host's end ends such a call inside its window; after it,
//     the grace is what any call whose host is missing gets. A host seat waiting in line
//     with a live place is coming, and counts nothing at all; one whose place expired
//     stopped polling and never came. COST: guests can hold a hostless booked call, and its
//     key's one call, until its booked end and the grace.
//   - A booking that is gone, or carries no start or no end, changes nothing: a host who
//     left counts from hostLeftAt, and a booked call whose host never came counts
//     nothing without a booked end.
//
// A moment still ahead of now is a countdown that has not begun, which calls.HostLeft
// answers as none with no deadline, so no seat is shown a countdown before it starts; a key
// holder in the call takes it over at once all the same.
func (s *Service) hostLeft(ctx context.Context, v roomView, now time.Time) (hostLeftVerdict, error) {
	none := hostLeftVerdict{action: callrules.HostLeftNone}
	keep, deadline := departurePolicy(v.room, v.seats)
	if !deadline.IsZero() {
		action := callrules.HostLeftCountdown
		if !now.Before(deadline) {
			action = callrules.HostLeftEnd
		}
		return hostLeftVerdict{action: action, deadline: deadline, firm: true}, nil
	}
	if keep {
		return none, nil
	}
	if v.hostPresent() {
		return none, nil
	}
	since := v.room.HostLeftAt
	if since.IsZero() {
		if v.hostComing(now) {
			return none, nil
		}
		if v.room.ScheduledCallID == "" {
			since = v.abandonedHostAt()
		}
	}
	if v.room.ScheduledCallID != "" {
		booking, err := s.store.bookingByID(ctx, v.room.OwnerUserID, v.room.ScheduledCallID)
		if err != nil {
			return none, err
		}
		switch {
		case since.IsZero() && (booking == nil || booking.End.IsZero()):
			return none, nil
		case since.IsZero():
			// Its host never came: the booking's window is theirs, and the count starts
			// at its end.
			since = booking.End
		case booking != nil && booking.Start.After(since):
			since = booking.Start
		}
	}
	to, err := s.keyHolderIn(ctx, v)
	if err != nil {
		return none, err
	}
	state := callrules.HostState{HostLeftAt: since}
	if to != nil {
		state.OtherKeyHolderSeat = to.ID
	}
	action, deadline := callrules.HostLeft(state, now, v.policy.HostLeftGrace)
	return hostLeftVerdict{action: action, deadline: deadline, to: to}, nil
}

// abandonedHostAt is the last host's abandonment of an ad hoc room before any host
// was admitted. Ask only when no host is present or still polling in line. Explicit
// leave uses the seat's leftAt; a waiting seat uses its last queue lease's expiry even
// after another replica marked that entry expired. Never use the reconciliation time:
// retries, a restarted replica and session reads must agree on one finite deadline.
// The room's opening is a fallback for an orphaned room with no host/queue rows.
func (v roomView) abandonedHostAt() time.Time {
	since := v.room.OpenedAt
	for _, seat := range v.seats {
		if !seat.Host {
			continue
		}
		if seat.LeftAt.After(since) {
			since = seat.LeftAt
		}
		if seat.Status != seatWaiting {
			continue
		}
		for _, entry := range v.entries {
			if entry.SeatID != seat.ID {
				continue
			}
			seen := entry.LastPollAt
			if seen.IsZero() {
				seen = entry.EnqueuedAt
			}
			if expiry := seen.Add(v.policy.QueueEntryTTL); expiry.After(since) {
				since = expiry
			}
		}
	}
	return since
}

// recordAbandonedHost freezes an ad hoc room's abandonment before a join/poll can
// replace its expired host lease or create another host waiter. Merely requeueing is
// not returning to the call: only hostIsBack after admission may clear this record.
// Callers already hold the room/server gates and have read this load fresh; filtering
// it to this room avoids extra queries and keeps other sites' rows out of the decision.
func (s *Service) recordAbandonedHost(ctx context.Context, room roomRow, load serverLoad, policy callrules.Policy, now time.Time) (roomRow, error) {
	if room.ScheduledCallID != "" || !room.HostLeftAt.IsZero() {
		return room, nil
	}
	v := roomView{room: room, policy: policy}
	for _, seat := range load.seats {
		if seat.RoomID == room.ID {
			v.seats = append(v.seats, seat)
		}
	}
	for _, entry := range load.entries {
		if entry.RoomID == room.ID {
			v.entries = append(v.entries, entry)
		}
	}
	if v.hostPresent() || v.hostComing(now) {
		return room, nil
	}
	since := v.abandonedHostAt()
	version := memql.VersionTimeAfter(room.Version, now)
	if err := s.store.updateRoom(ctx, room.OwnerUserID, room, map[string]any{"hostLeftAt": since}, version); err != nil {
		return room, err
	}
	room.HostLeftAt, room.Version = since, version
	return room, nil
}

// keyHolderIn is the seat a call is handed to when nobody hosting it is in it: a seat in
// the call, not a host, whose holder unlocked a key that is the deployable's and active --
// the earliest let in, ties by id, so every replica picks the same one. Nil is none.
func (s *Service) keyHolderIn(ctx context.Context, v roomView) (*seatRow, error) {
	// Simulated funding sponsors this room under its original key and deadline.
	// Merely attending with another key is not consent to become its payer or
	// extend that sponsorship. Explicit host links keep their usual controls.
	if v.room.bounded() {
		return nil, nil
	}
	site := callrules.SiteRef{SiteID: v.room.SiteID, OwnerUserID: v.room.OwnerUserID}
	var to *seatRow
	for i := range v.seats {
		seat := v.seats[i]
		if !seat.occupying() || seat.Host || seat.KeyID == "" || seat.Kind == string(callrules.KindAgent) {
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
	if to.Kind == string(callrules.KindAgent) {
		return accountRefusal("human_host_required")
	}
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
	if familyRoom(room) {
		return s.closeFamily(ctx, room, "family_ended")
	}
	if err := s.StopRoomResources(ctx, room); err != nil {
		return err
	}
	backend, err := s.backendFor(ctx, room)
	if err != nil || backend == nil {
		return err
	}
	if err := backend.DeleteRoom(ctx, room.MediaRoom); err != nil {
		return fmt.Errorf("calls: end the call's media room: %w", err)
	}
	return nil
}

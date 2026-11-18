package calls

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// schedule.go -- booked calls: a key holder books a call for a later time and is handed
// its host link, its guest link and a calendar file, once; moves or cancels it; and the
// first join inside its window opens its room (design sections 7.7 and 8, "Schedule a
// call"). The rules are the pure module's (component/calls: ValidateSchedule,
// DecideBookingJoin, ScheduledCallOpensEarly); this file reads the state they decide
// from, under the gates service.go's file note describes, and writes what they decided.
//
// # Who acts on a booking
//
// Booking, moving and cancelling need a session a KEY opened (Unlock) whose key is
// active: the key holds the booking's slot and pays for it. A booking is acted on only by
// a session of its own deployable and its own key; any other is refused ErrForbidden, and
// an id the deployable's owner holds no booking under is ErrNotFound. Whoever booked the
// call need not attend it: the host is whoever joins with the host link, so an agent can
// book an interview a person runs.
//
// # A booking holds its slot
//
// A booking may not overlap another of its key's that still holds a slot (ValidateSchedule
// against the key's upcoming spans, heldSpan). A booking holds its booked span until its
// booked end; a STARTED one holds its slot until its room ends, however long the call runs
// past its booked end; a cancelled or ended one holds nothing. A move is checked against
// the key's other bookings and never against itself. The check spans every booking of the
// key that may hold a slot, so booking and moving hold the key's gate, and that read is
// filtered to those bookings (callsScheduledCallsForKey), so the policy's cap bounds it.
//
// TWO OVERLAPS REMAIN, the only ways a key has two calls at once. Opening a booked call's
// room does not look at the key's open rooms, and Start does not look at the key's
// bookings:
//   - an ad hoc call started before a booked slot and still running when the slot comes
//     overlaps it;
//   - a booking opened in its early window (ScheduledCallOpensEarly) while the key's
//     previous call still runs overlaps that call. That is a back-to-back booking -- the
//     core use, one interview per candidate, each ending as the next begins -- or one booked
//     to start after a call that is running past its booked end, since a running call's span
//     reaches only to now and every booking starts at or after now.
// Both are allowed; what an overlap costs comes with payments.
//
// # Links and the calendar file
//
// The links are built on the deployable's public host: its live custom domain when it has
// one, the first by hostname of several, and otherwise its own hostname (publicHost). The
// site row is read as its owner. A custom domain is a CLUSTER-OWNED row
// (@rowAuthz(clusterOwner), and every query over it states actor.isClusterOwner), so the
// domains are read as the cluster acting, as the media servers are: read as the site's
// owner they would always answer none. The host is read before anything is written, so no
// booking is written whose links could not be built.
//
// The tokens ride the links' fragments (calls.GuestLink) and only their digests are
// stored, so a booking's links are handed out once -- by Schedule, or by the Reschedule
// that replaced them -- and never again. The calendar file is what a scheduler forwards
// to guests: its URL and description carry the guest link, and the host token is nowhere
// in it. Its UID is <id>@<public host> as it stood at booking, KEPT ON THE BOOKING
// (calendarUid), so a moved call's file updates the entry the first file made although
// the public host may have changed in between.
//
// # Opening a booked call's room
//
// A link that finds no room (service.go Join) is looked up as a booking by its digest.
// More than ScheduledCallOpensEarly before the start it is answered not_yet, with the time
// until the call opens; a cancelled booking cancelled; an ended one, or one whose end
// passed with nobody having opened it, call_ended. Each answer is immediate and writes
// nothing: no gate, no session, no seat, no place in a queue. Inside the window the first
// join opens the room THROUGH THE SAME OPENING START USES (service.go placeRoom and
// openRoom) -- on the active media server with the most free seats, its media room created
// before any row -- on the booking's key, LOCKED (a booked call's waiting room is on by
// default: the host admits each person), and carrying the booking's id and both of its
// links' digests. The digests are what make every later join
// find the room by its link as an ad hoc call's joins find theirs. A host link's holder
// passes the lock and a guest waits; when the server is full the joiner queues in the
// booked call's class, ClassScheduledDue.
//
// TWO FIRST JOINS ON TWO REPLICAS OPEN ONE ROOM. The opening join takes the booking's
// gate, then the new room's and the server's (the order key, booking, room, server is
// every caller's), and reads the booking again under them, BY ITS LINK, so a move made
// meanwhile leaves the old link opening nothing. If another join opened the room
// meanwhile, this one releases its gates and joins that room as any join does
// (test/clustere2e calls_schedule_hop_db_test.go).
//
// THE WRITES RUN session, seat, queue entry, booking, and the room LAST. A booking marked
// started whose room was never written opened nothing (bookedRoom), so the next join opens
// it; the reverse order would leave a room whose booking still reads as scheduled, which a
// cancel or a move would act on while the call ran. The media room a losing join created
// before its gates stays empty, and the media server closes it on its own, as Start's
// does.
//
// A FROZEN OR BANNED KEY OPENS NO CALL, booked or not (calls.ReasonKeyNotActive): the
// opening join is refused key_not_active, as Start is. An open room stays open past its
// booked end -- a live call is never cut -- and later joins reach it through the room,
// never the booking.
//
// # What a lost reply leaves
//
// A booking's links exist only in its reply. If a Schedule's reply is lost after the
// booking was written, the booking holds its slot until its end with links nobody has;
// part 1 lists no key's bookings to find it by. A lost Reschedule reply is recoverable:
// the scheduler still has the id, and moving it again mints another pair.

// scheduledCallConcept is the booking's concept.
const scheduledCallConcept = "v1:calls:scheduledCall"

// calendarProductID is every calendar file's PRODID: product-neutral, because the
// capability names no product and the file goes to whoever the scheduler invites.
const calendarProductID = "-//Calls//Scheduled call//EN"

// defaultCallTitle is the calendar summary of a booking with no title.
const defaultCallTitle = "Scheduled call"

// liveDomain is the custom-domain walk's status at which the edge serves a hostname
// (dsl/platform liveCustomDomainByHostname); every other status is not an address yet.
const liveDomain = "live"

// ---------------------------------------------------------------------------
// booking, moving and cancelling
// ---------------------------------------------------------------------------

// Schedule books a call for a key holder's session: a span the policy allows that
// overlaps none of the key's other bookings, with a title of at most 80 characters. It
// answers the call's id and span, both links' secrets and both links, once, and a
// calendar file. Nothing is placed or opened until somebody joins (joinBooking).
func (s *Service) Schedule(ctx context.Context, sess callrules.Session, req callrules.ScheduleRequest) (callrules.ScheduledCall, error) {
	none := callrules.ScheduledCall{}
	want, err := checkBooking(req)
	if err != nil {
		return none, err
	}
	h, err := s.keyHolderOf(ctx, sess)
	if err != nil {
		return none, err
	}
	owner := h.site.OwnerUserID
	policy, err := s.store.PolicyFor(ctx, owner, h.site.SiteID)
	if err != nil {
		return none, err
	}
	span := want.span(policy)
	if err := invalidBooking(s.now(), span, policy); err != nil {
		return none, err
	}
	host, err := s.store.publicHost(ctx, owner, h.site.SiteID)
	if err != nil {
		return none, err
	}
	links, err := s.mintLinks()
	if err != nil {
		return none, err
	}
	callID := id.NewShortId()
	n := newBooking{
		ID: callID, SiteID: h.site.SiteID, KeyID: h.row.KeyID, Title: want.title, Start: span.Start, End: span.End,
		GuestDigest: callrules.Digest(h.pepper, []byte(links.GuestToken)), HostDigest: callrules.Digest(h.pepper, []byte(links.HostToken)),
		CalendarUID: callID + "@" + host,
	}
	now, err := s.bookGated(ctx, h, policy, n)
	if err != nil {
		return none, err
	}
	return scheduledCall(callID, span, want.title, links, host, n.CalendarUID, now), nil
}

// bookGated is a booking's check against the key's other bookings and its write, under
// the key's gate. It answers the time the booking was decided at.
func (s *Service) bookGated(ctx context.Context, h keyHolder, policy callrules.Policy, n newBooking) (time.Time, error) {
	release, err := s.gates(ctx, keyGate(h.row.KeyID))
	if err != nil {
		return time.Time{}, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	if err := s.heldKey(fresh, h, now); err != nil {
		return now, err
	}
	upcoming, err := s.upcoming(fresh, h.site.OwnerUserID, h.row.KeyID, now, "")
	if err != nil {
		return now, err
	}
	if err := callrules.ValidateSchedule(now, callrules.Interval{Start: n.Start, End: n.End}, policy, upcoming); err != nil {
		return now, callrules.BookingRefusal(err)
	}
	return now, s.store.createBooking(fresh, h.site.OwnerUserID, n, now)
}

// Reschedule moves a booking that has not started to a new span, checked as a new
// booking is but never against itself, and replaces both of its links: the old ones open
// nothing from then on. It answers the new links once, with a calendar file under the
// call's own UID. A request with no title keeps the call's title; one with a title
// replaces it.
func (s *Service) Reschedule(ctx context.Context, sess callrules.Session, callID string, req callrules.ScheduleRequest) (callrules.ScheduledCall, error) {
	none := callrules.ScheduledCall{}
	want, err := checkBooking(req)
	if err != nil {
		return none, err
	}
	h, err := s.keyHolderOf(ctx, sess)
	if err != nil {
		return none, err
	}
	owner := h.site.OwnerUserID
	b, err := s.bookingOf(ctx, h, callID)
	if err != nil {
		return none, err
	}
	policy, err := s.store.PolicyFor(ctx, owner, h.site.SiteID)
	if err != nil {
		return none, err
	}
	span := want.span(policy)
	if err := invalidBooking(s.now(), span, policy); err != nil {
		return none, err
	}
	host, err := s.store.publicHost(ctx, owner, h.site.SiteID)
	if err != nil {
		return none, err
	}
	links, err := s.mintLinks()
	if err != nil {
		return none, err
	}
	moved, now, err := s.moveGated(ctx, h, b.ID, policy, want.title, span, links)
	if err != nil {
		return none, err
	}
	uid := moved.CalendarUID
	if uid == "" {
		// Every booking keeps the UID it was booked under; one written before the field
		// existed is given the one its first file would have carried.
		uid = moved.ID + "@" + host
	}
	return scheduledCall(moved.ID, span, moved.Title, links, host, uid, now), nil
}

// moveGated is a move's decision and its write, under the key's gate and the booking's.
// It answers the booking as written and the time it was decided at.
func (s *Service) moveGated(ctx context.Context, h keyHolder, callID string, policy callrules.Policy, title string, span callrules.Interval, links callrules.CallLinks) (bookingRow, time.Time, error) {
	release, err := s.gates(ctx, keyGate(h.row.KeyID), bookingGate(callID))
	if err != nil {
		return bookingRow{}, time.Time{}, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	owner := h.site.OwnerUserID
	now := s.now()
	if err := s.heldKey(fresh, h, now); err != nil {
		return bookingRow{}, now, err
	}
	current, err := s.bookingOf(fresh, h, callID)
	if err != nil {
		return bookingRow{}, now, err
	}
	room, err := s.bookedRoom(fresh, owner, *current)
	if err != nil {
		return bookingRow{}, now, err
	}
	switch standing(*current, room, now) {
	case callrules.BookingCancelled:
		return bookingRow{}, now, callrules.Refuse(callrules.ErrEnded, callrules.ReasonCancelled, "a cancelled call is not moved")
	case callrules.BookingEnded:
		return bookingRow{}, now, callrules.Refuse(callrules.ErrEnded, callrules.ReasonCallEnded, "the call is over")
	case callrules.BookingStarted:
		return bookingRow{}, now, callrules.Refuse(callrules.ErrConflict, callrules.ReasonStarted, "a call that has started is not moved")
	}
	upcoming, err := s.upcoming(fresh, owner, h.row.KeyID, now, current.ID)
	if err != nil {
		return bookingRow{}, now, err
	}
	if err := callrules.ValidateSchedule(now, span, policy, upcoming); err != nil {
		return bookingRow{}, now, callrules.BookingRefusal(err)
	}
	fields := map[string]any{
		"start": span.Start, "end": span.End,
		"guestDigest": callrules.Digest(h.pepper, []byte(links.GuestToken)),
		"hostDigest":  callrules.Digest(h.pepper, []byte(links.HostToken)),
	}
	if title != "" {
		fields["title"] = title
		current.Title = title
	}
	if current.status() != callrules.BookingScheduled {
		// Marked started by an opening join whose room was never written (the file note):
		// it opened nothing, and the move books it again.
		fields["status"] = callrules.BookingScheduled
	}
	if err := s.store.updateBooking(fresh, owner, *current, fields, memql.VersionTimeAfter(current.Version, now)); err != nil {
		return bookingRow{}, now, err
	}
	current.Start, current.End = span.Start, span.End
	return *current, now, nil
}

// Cancel cancels a booking that has not started: from then on both of its links are
// answered that the call was cancelled. Cancelling a cancelled booking changes nothing
// and is not an error; a started call is a conflict, and an ended one has ended.
func (s *Service) Cancel(ctx context.Context, sess callrules.Session, callID string) error {
	h, err := s.keyHolderOf(ctx, sess)
	if err != nil {
		return err
	}
	b, err := s.bookingOf(ctx, h, callID)
	if err != nil {
		return err
	}
	release, err := s.gates(ctx, bookingGate(b.ID))
	if err != nil {
		return err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	owner := h.site.OwnerUserID
	now := s.now()
	if err := s.heldKey(fresh, h, now); err != nil {
		return err
	}
	current, err := s.bookingOf(fresh, h, b.ID)
	if err != nil {
		return err
	}
	room, err := s.bookedRoom(fresh, owner, *current)
	if err != nil {
		return err
	}
	switch standing(*current, room, now) {
	case callrules.BookingCancelled:
		return nil
	case callrules.BookingEnded:
		return callrules.Refuse(callrules.ErrEnded, callrules.ReasonCallEnded, "the call is over")
	case callrules.BookingStarted:
		return callrules.Refuse(callrules.ErrConflict, callrules.ReasonStarted, "a call that has started is not cancelled")
	}
	return s.store.updateBooking(fresh, owner, *current, map[string]any{"status": callrules.BookingCancelled}, memql.VersionTimeAfter(current.Version, now))
}

// heldKey asks again, under the gates, what keyHolderOf asked: that the session still
// opens anything, and that its key is the deployable's and active (keyOpensCalls).
func (s *Service) heldKey(ctx context.Context, h keyHolder, now time.Time) error {
	current, err := s.store.sessionByID(ctx, h.site.OwnerUserID, h.row.ID)
	if err != nil {
		return err
	}
	if !current.usable(now) {
		return fmt.Errorf("%w: the session ended", callrules.ErrNoSession)
	}
	opens, err := s.keyOpensCalls(ctx, h.site, h.row.KeyID)
	if err != nil {
		return err
	}
	if !opens {
		return callrules.Refuse(callrules.ErrForbidden, callrules.ReasonKeyNotActive, "the key books, moves and cancels no call")
	}
	return nil
}

// bookingOf is the booking a holder names by id: ErrNotFound when the deployable's owner
// holds none under it, ErrForbidden when it is another deployable's or another key's.
func (s *Service) bookingOf(ctx context.Context, h keyHolder, callID string) (*bookingRow, error) {
	callID = memql.BareShortId(strings.TrimSpace(callID))
	if callID == "" {
		return nil, fmt.Errorf("%w: no call is named", callrules.ErrInvalid)
	}
	b, err := s.store.bookingByID(ctx, h.site.OwnerUserID, callID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("%w: no call of that id is booked here", callrules.ErrNotFound)
	}
	if b.SiteID != h.site.SiteID || b.KeyID != h.row.KeyID {
		return nil, fmt.Errorf("%w: the call is booked on another key", callrules.ErrForbidden)
	}
	return b, nil
}

// upcoming is the spans a key's bookings hold at now, but the one named by except: what a
// booking or a move is checked against, and what counts toward the key's
// MaxUpcomingScheduled. The read returns only bookings that may still hold a slot
// (callsScheduledCallsForKey), so its size is bounded by that cap rather than by the
// key's history; heldSpan decides each from the service's own clock.
func (s *Service) upcoming(ctx context.Context, owner, keyID string, now time.Time, except string) ([]callrules.Interval, error) {
	rows, err := s.store.bookingsForKey(ctx, owner, keyID)
	if err != nil {
		return nil, err
	}
	var spans []callrules.Interval
	for _, b := range rows {
		if b.ID == except {
			continue
		}
		span, holds, err := s.heldSpan(ctx, owner, b, now)
		if err != nil {
			return nil, err
		}
		if holds {
			spans = append(spans, span)
		}
	}
	return spans, nil
}

// heldSpan is the span of the key's time a booking holds at now, and whether it holds
// any. A booking holds its booked span until its booked end, unless it was cancelled or
// ended. A STARTED booking holds its slot until its ROOM ENDS: a live call is never cut
// at its booked end, so while its room is open its span runs to the later of its booked
// end and now, and it counts against the key however long it runs. Once its room has
// ended it holds nothing. A booking marked started whose room was never written opened
// nothing (bookedRoom), and holds its booked span as a booking does.
//
// WHAT THE RUNNING SPAN DOES AND DOES NOT DO. Every booking starts at or after now
// (ValidateSchedule's first rule), and spans are half-open, so a span running to now
// overlaps no booking that can be made: what it changes is the count. A booking whose
// early window opens while the key's call still runs is one of the two overlaps a key's
// calls keep (the file note).
func (s *Service) heldSpan(ctx context.Context, owner string, b bookingRow, now time.Time) (callrules.Interval, bool, error) {
	span := callrules.Interval{Start: b.Start, End: b.End}
	switch b.status() {
	case callrules.BookingCancelled, callrules.BookingEnded:
		return span, false, nil
	case callrules.BookingStarted:
		room, err := s.bookedRoom(ctx, owner, b)
		if err != nil {
			return span, false, err
		}
		if room != nil {
			if !room.open() {
				return span, false, nil
			}
			if now.After(span.End) {
				span.End = now
			}
			return span, true, nil
		}
	}
	return span, now.Before(b.End), nil
}

// standing is where a booking stands for a move or a cancel: cancelled; ended -- it ended,
// its room ended, or its end passed before anyone opened it; started, while its room is
// open; or still booked. A booking marked started whose room was never written stands as
// booked (bookedRoom).
func standing(b bookingRow, room *roomRow, now time.Time) string {
	switch b.status() {
	case callrules.BookingCancelled, callrules.BookingEnded:
		return b.status()
	}
	if room != nil {
		if room.open() {
			return callrules.BookingStarted
		}
		return callrules.BookingEnded
	}
	if !now.Before(b.End) {
		return callrules.BookingEnded
	}
	return callrules.BookingScheduled
}

// bookedRoom is the room a booking opened, or nil when it opened none. A booking names
// its room once it is marked started, and one marked started whose room was never written
// opened none (the file note's write order). A room that does not name the booking back
// is not its room.
func (s *Service) bookedRoom(ctx context.Context, owner string, b bookingRow) (*roomRow, error) {
	if b.status() != callrules.BookingStarted || b.RoomID == "" {
		return nil, nil
	}
	room, err := s.store.roomByID(ctx, owner, b.RoomID)
	if err != nil || room == nil || room.ScheduledCallID != b.ID {
		return nil, err
	}
	return room, nil
}

// bookingGate serializes one booking: its moves, its cancel and the join that opens its
// room. It comes after the key's gate and before any room's (service.go's file note).
func bookingGate(callID string) string { return "calls.scheduled." + memql.BareShortId(callID) }

// ---------------------------------------------------------------------------
// the request, the calendar file
// ---------------------------------------------------------------------------

// booking is a request as a booking keeps it: the start to the second (the calendar
// file's resolution), the length, and the title on one line.
type booking struct {
	start   time.Time
	minutes int
	title   string
}

// checkBooking holds a request to what any booking can be, before anything is read: a
// length of a minute or more and a title of at most 80 characters.
func checkBooking(req callrules.ScheduleRequest) (booking, error) {
	title := tidyTitle(req.Title)
	if utf8.RuneCountInString(title) > callrules.MaxScheduledTitleRunes {
		return booking{}, callrules.Refuse(callrules.ErrInvalid, callrules.ReasonTitleTooLong,
			fmt.Sprintf("a title is at most %d characters", callrules.MaxScheduledTitleRunes))
	}
	if req.Minutes <= 0 {
		return booking{}, callrules.Refuse(callrules.ErrInvalid, callrules.ReasonMinutesInvalid, "a call is booked for a minute or more")
	}
	return booking{start: req.Start.UTC().Truncate(time.Second), minutes: req.Minutes, title: title}, nil
}

// span is the booked interval. A length past the policy's is held one minute past it, so
// the end cannot overflow and ValidateSchedule still answers too_long.
func (b booking) span(policy callrules.Policy) callrules.Interval {
	minutes := min(b.minutes, max(policy.MaxScheduledMinutes, 0)+1)
	return callrules.Interval{Start: b.start, End: b.start.Add(time.Duration(minutes) * time.Minute)}
}

// invalidBooking is ValidateSchedule's refusal of a span no state of the key could make
// acceptable -- in the past, beyond the horizon, too long -- asked before any gate is
// taken, or nil. The rest is the gated phase's to ask.
func invalidBooking(now time.Time, span callrules.Interval, policy callrules.Policy) error {
	err := callrules.ValidateSchedule(now, span, policy, nil)
	if errors.Is(err, callrules.ErrInPast) || errors.Is(err, callrules.ErrBeyondHorizon) || errors.Is(err, callrules.ErrTooLong) {
		return callrules.BookingRefusal(err)
	}
	return nil
}

// tidyTitle lays a title on one line: every run of spaces and control characters becomes
// one space, and the ends are trimmed. A calendar summary is one line, and nothing in a
// title means anything a line break or a control character could say.
func tidyTitle(title string) string {
	return strings.Join(strings.FieldsFunc(title, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
}

// scheduledCall is a booking as its scheduler is handed it: both links on the public host
// and the calendar file, which carries the guest link and never the host token.
func scheduledCall(callID string, span callrules.Interval, title string, links callrules.CallLinks, host, uid string, now time.Time) callrules.ScheduledCall {
	guest := callrules.GuestLink(host, links.GuestToken)
	summary := title
	if summary == "" {
		summary = defaultCallTitle
	}
	return callrules.ScheduledCall{
		ID: callID, Start: span.Start, End: span.End, Title: title,
		GuestToken: links.GuestToken, HostToken: links.HostToken,
		GuestLink: guest, HostLink: callrules.HostLink(host, links.GuestToken, links.HostToken),
		ICS: callrules.RenderICS(callrules.CalendarEvent{
			UID: uid, Summary: summary, Description: "Join the call: " + guest, URL: guest,
			ProductID: calendarProductID, Start: span.Start, End: span.End, Stamp: now,
		}),
	}
}

// ---------------------------------------------------------------------------
// joining a booked call
// ---------------------------------------------------------------------------

// joinBooking is a join by a link that found no room: a booked call's, whose first join
// inside its window opens the room (the file note). Every refusal before the room is open
// is answered at once and writes nothing.
func (s *Service) joinBooking(ctx context.Context, site callrules.SiteRef, sess *callrules.Session, held *sessionRow, ask joinAsk) (callrules.JoinResult, callrules.Session, error) {
	owner := site.OwnerUserID
	b, err := s.store.bookingByLink(ctx, owner, site.SiteID, callrules.Digest(ask.pepper, []byte(ask.link)), ask.asHost)
	if err != nil {
		return callrules.JoinResult{}, held.state(sess, site), err
	}
	if b == nil {
		return callrules.JoinResult{}, held.state(sess, site), errLinkOpensNoRoom
	}
	// Opened by a join that the room lookup raced: joined as any call is.
	if room, err := s.bookedRoom(ctx, owner, *b); err != nil || room != nil {
		if err != nil {
			return callrules.JoinResult{}, held.state(sess, site), err
		}
		return s.joinRoom(ctx, site, *room, sess, held, ask)
	}
	// The gated phase asks all of this again. Asking first, without the gates, is what
	// keeps a refused join from taking a lock or opening a media room.
	if refused, err := bookingJoinRefusal(*b, s.now()); err != nil {
		return refused, held.state(sess, site), err
	}
	// A BOOKED CALL'S FIRST JOIN DOES NOT ASK THE KEY'S OPEN ROOMS (oneCallPerKey false):
	// the booking held its slot, and the overlaps that leaves are the file note's two.
	if refused, err := s.openRefusal(ctx, site, b.KeyID, held, false); err != nil {
		return refused, held.state(sess, site), err
	}
	policy, err := s.store.PolicyFor(ctx, owner, site.SiteID)
	if err != nil {
		return callrules.JoinResult{}, held.state(sess, site), err
	}
	placed, refused, err := s.placeRoom(ctx, policy)
	if err != nil {
		return refused, held.state(sess, site), err
	}
	identity, err := callrules.NewToken(s.random, identityBytes)
	if err != nil {
		return callrules.JoinResult{}, held.state(sess, site), err
	}
	opened, err := s.openGated(ctx, site, *b, placed.server, policy, held, ask, placed.roomID, identity)
	if err != nil {
		return opened.result, opened.out, err
	}
	if opened.joinIn != nil {
		return s.joinRoom(ctx, site, *opened.joinIn, sess, held, ask)
	}
	if opened.result.Outcome != callrules.OutcomeAdmit {
		return opened.result, opened.out, nil
	}
	result, err := admitted(placed.server, placed.credential, placed.roomID, opened.seat, policy)
	if err != nil {
		return callrules.JoinResult{}, opened.out, err
	}
	return result, opened.out, nil
}

// bookingJoinRefusal is a join of a booking whose room is not open, refused by
// DecideBookingJoin at now with its reason beside the error, or nothing when the join
// opens the room. A booking marked started opened nothing when this is asked (bookedRoom),
// so it is decided as one still booked.
func bookingJoinRefusal(b bookingRow, now time.Time) (callrules.JoinResult, error) {
	status := b.status()
	if status == callrules.BookingStarted {
		status = callrules.BookingScheduled
	}
	decision := callrules.DecideBookingJoin(now, status, b.Start, b.End)
	if decision.Open {
		return callrules.JoinResult{}, nil
	}
	refused := refuseJoin(decision.Reason)
	switch decision.Reason {
	case callrules.ReasonNotYet:
		refused.RetryAfter = decision.RetryAfter
		return refused, fmt.Errorf("%w: the call opens %s before its start", callrules.ErrConflict, callrules.ScheduledCallOpensEarly)
	case callrules.ReasonCancelled:
		return refused, fmt.Errorf("%w: the call was cancelled", callrules.ErrEnded)
	}
	return refused, fmt.Errorf("%w: the call has ended", callrules.ErrEnded)
}

// opening is the opening join's answer: an admission without a pass (minted once the gates
// are released), a wait or a place in the queue, with the session the joiner now holds
// and their seat -- or the room another join opened meanwhile (joinIn), which the caller
// joins as any call once these gates are released.
type opening struct {
	result callrules.JoinResult
	out    callrules.Session
	seat   seatRow
	joinIn *roomRow
}

// openGated is a booked call's own checks under the booking's, the new room's and the
// server's gates -- the booking found again by its link, not opened meanwhile and still
// due, the joiner's session fresh, openRefusal again -- and then the opening every call
// shares (service.go openRoom), for a booked call: locked, its opener a host only by the
// host link, carrying the booking's id and digests, and the booking marked started just
// before the room is written.
func (s *Service) openGated(ctx context.Context, site callrules.SiteRef, b bookingRow, server MediaServer, policy callrules.Policy, held *sessionRow, ask joinAsk, roomID, identity string) (opening, error) {
	release, err := s.gates(ctx, bookingGate(b.ID), roomGate(roomID), serverGate(server.ID))
	if err != nil {
		return opening{out: held.stateOf(ask.token, site)}, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	owner := site.OwnerUserID
	now := s.now()

	// The booking as it stands, found again BY ITS LINK: a move since the lookup replaced
	// the link, and the old one opens nothing.
	current, err := s.store.bookingByLink(fresh, owner, site.SiteID, callrules.Digest(ask.pepper, []byte(ask.link)), ask.asHost)
	if err != nil {
		return opening{out: held.stateOf(ask.token, site)}, err
	}
	if current == nil || current.ID != b.ID {
		return opening{out: held.stateOf(ask.token, site)}, errLinkOpensNoRoom
	}
	if room, err := s.bookedRoom(fresh, owner, *current); err != nil || room != nil {
		return opening{out: held.stateOf(ask.token, site), joinIn: room}, err
	}
	if refused, err := bookingJoinRefusal(*current, now); err != nil {
		return opening{result: refused, out: held.stateOf(ask.token, site)}, err
	}
	if held != nil {
		if held, err = s.store.sessionByID(fresh, owner, held.ID); err != nil {
			return opening{}, err
		}
		if !held.usable(now) {
			held = nil
		}
	}
	out := held.stateOf(ask.token, site)
	if refused, err := s.openRefusal(fresh, site, current.KeyID, held, false); err != nil {
		return opening{result: refused, out: out}, err
	}
	guestDigest, hostDigest, err := s.store.bookingDigests(fresh, owner, current.ID)
	if err != nil {
		return opening{out: out}, err
	}
	// THE BOOKING IS MARKED STARTED, naming its room, after the seat and before the room is
	// written (the file note's write order), fenced on the version read under its gate.
	markStarted := func(ctx context.Context, now time.Time) error {
		return s.store.updateBooking(ctx, owner, *current, map[string]any{
			"status": callrules.BookingStarted, "roomId": roomID,
		}, memql.VersionTimeAfter(current.Version, now))
	}
	result, out, seat, err := s.openRoom(fresh, site, server, policy, held, ask, roomOpening{
		roomID: roomID, identity: identity, keyID: current.KeyID,
		// A host link's holder takes a host seat and passes the lock; the room opens LOCKED,
		// its waiting room on until its host lets people in; a full server queues the
		// opener as a booked call that is due.
		host: ask.asHost, locked: true, class: callrules.ClassScheduledDue,
		guestDigest: guestDigest, hostDigest: hostDigest, scheduledCallID: current.ID,
	}, out, markStarted, now)
	return opening{result: result, out: out, seat: seat}, err
}

// ---------------------------------------------------------------------------
// the rows
// ---------------------------------------------------------------------------

// bookingsAs is the context a site's bookings are read and written under: the site's
// owner, with internal origin. asOwner stamps the origin too; it is stamped again here
// because every booking construct is @serverOnly, and the server-only caller gate
// (test/dslconformance/server_only_callers_stamp_test.go) asks the file that renders one
// to stamp it itself. A blank owner answers false with the context unchanged.
func bookingsAs(ctx context.Context, owner string) (context.Context, bool) {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return ctx, false
	}
	return auth.ContextWithInternalOrigin(owned), true
}

// bookingRow is one v1:calls:scheduledCall. Its ids are BARE; its digests are filtered
// on and read back only by bookingDigests.
type bookingRow struct {
	ID, SiteID, KeyID, Title, Status, RoomID, CalendarUID string
	Start, End                                            time.Time
	Version                                               time.Time
}

// status is the concept's rule: an absent status reads as scheduled.
func (r bookingRow) status() string {
	if r.Status == "" {
		return callrules.BookingScheduled
	}
	return r.Status
}

func bookingFromRow(r map[string]any) bookingRow {
	return bookingRow{
		ID:          memql.BareShortId(rowString(r, "id")),
		SiteID:      memql.BareShortId(rowString(r, "siteId")),
		KeyID:       memql.BareShortId(rowString(r, "keyId")),
		Title:       rowString(r, "title"),
		Status:      rowString(r, "status"),
		RoomID:      memql.BareShortId(rowString(r, "roomId")),
		CalendarUID: rowString(r, "calendarUid"),
		Start:       rowTime(r, "start"),
		End:         rowTime(r, "end"),
		Version:     rowTime(r, "createdAt"),
	}
}

func (s *Store) readBookings(ctx context.Context, owner, what, call string, rendered error) ([]map[string]any, error) {
	owned, ok := bookingsAs(ctx, owner)
	if !ok {
		return nil, errNoOwner
	}
	return s.read(memql.ContextWithRootsOnlyRead(owned), what, call, rendered)
}

func (s *Store) oneBooking(ctx context.Context, owner, what, call string, rendered error) (*bookingRow, error) {
	rows, err := s.readBookings(ctx, owner, what, call, rendered)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	b := bookingFromRow(rows[0])
	return &b, nil
}

// bookingByID reads one booking by its id, as the site's owner. A miss is (nil, nil).
func (s *Store) bookingByID(ctx context.Context, owner, callID string) (*bookingRow, error) {
	call, err := langparser.RenderCall("callsScheduledCallById", map[string]any{"id": memql.BareShortId(callID)})
	return s.oneBooking(ctx, owner, "the booking", call, err)
}

// bookingByLink finds a booking by the digest of one of its links, on one site: the host
// link when host is true, the guest link otherwise. A miss is (nil, nil).
func (s *Store) bookingByLink(ctx context.Context, owner, siteID, digest string, host bool) (*bookingRow, error) {
	if host {
		call, err := langparser.RenderCall("callsScheduledCallByHostDigest", map[string]any{"siteId": memql.BareShortId(siteID), "hostDigest": digest})
		return s.oneBooking(ctx, owner, "a booking by its host link", call, err)
	}
	call, err := langparser.RenderCall("callsScheduledCallByGuestDigest", map[string]any{"siteId": memql.BareShortId(siteID), "guestDigest": digest})
	return s.oneBooking(ctx, owner, "a booking by its guest link", call, err)
}

// bookingsForKey is the bookings of one key that may still hold a slot: not cancelled,
// not ended, and ending after now or started, by the engine's clock
// (callsScheduledCallsForKey). The service decides each from its own (heldSpan).
func (s *Store) bookingsForKey(ctx context.Context, owner, keyID string) ([]bookingRow, error) {
	call, err := langparser.RenderCall("callsScheduledCallsForKey", map[string]any{"keyId": memql.BareShortId(keyID)})
	rows, err := s.readBookings(ctx, owner, "the key's bookings", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]bookingRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, bookingFromRow(r))
	}
	return out, nil
}

// bookingDigests reads a booking's two link digests, the one read that returns a digest:
// the room a booked call opens carries both (the file note).
func (s *Store) bookingDigests(ctx context.Context, owner, callID string) (guest, host string, err error) {
	call, err := langparser.RenderCall("callsScheduledCallDigestsById", map[string]any{"id": memql.BareShortId(callID)})
	rows, err := s.readBookings(ctx, owner, "the booking's link digests", call, err)
	if err != nil {
		return "", "", err
	}
	if len(rows) == 0 {
		return "", "", fmt.Errorf("calls: the booking %s is gone", callID)
	}
	guest, host = rowString(rows[0], "guestDigest"), rowString(rows[0], "hostDigest")
	if guest == "" || host == "" {
		return "", "", fmt.Errorf("calls: the booking %s carries no links", callID)
	}
	return guest, host, nil
}

// newBooking is what a booking's create writes: digests of its links, never the links.
type newBooking struct {
	ID, SiteID, KeyID, Title, GuestDigest, HostDigest, CalendarUID string
	Start, End                                                     time.Time
}

func (s *Store) createBooking(ctx context.Context, owner string, n newBooking, versionTime time.Time) error {
	args := map[string]any{
		"scheduledCallId": n.ID,
		"siteId":          n.SiteID,
		"keyId":           n.KeyID,
		"start":           versionStamp(n.Start),
		"end":             versionStamp(n.End),
		"guestDigest":     n.GuestDigest,
		"hostDigest":      n.HostDigest,
		"calendarUid":     n.CalendarUID,
		"versionTime":     versionStamp(versionTime),
	}
	// Absent rather than empty: a call booked with no title has none.
	if n.Title != "" {
		args["title"] = n.Title
	}
	call, err := langparser.RenderCall("createCallsScheduledCall", args)
	return s.writeBooking(ctx, owner, absent(scheduledCallConcept, n.ID), "the booking", call, err)
}

// updateBooking writes the named fields of a booking, against the version it was read at.
// Times are passed as time.Time and stamped here.
func (s *Store) updateBooking(ctx context.Context, owner string, prior bookingRow, fields map[string]any, versionTime time.Time) error {
	args := map[string]any{"scheduledCallId": prior.ID, "versionTime": versionStamp(versionTime)}
	for name, value := range fields {
		if stamp, isTime := value.(time.Time); isTime {
			value = versionStamp(stamp)
		}
		args[name] = value
	}
	call, err := langparser.RenderCall("updateCallsScheduledCall", args)
	return s.writeBooking(ctx, owner, at(scheduledCallConcept, prior.ID, prior.Version), "the booking", call, err)
}

// writeBooking is one of a site's booking writes, as the site's owner, under the fence it
// is handed.
func (s *Store) writeBooking(ctx context.Context, owner string, fence func(context.Context) (context.Context, error), what, call string, rendered error) error {
	owned, ok := bookingsAs(ctx, owner)
	if !ok {
		return errNoOwner
	}
	fenced, err := fence(owned)
	if err != nil {
		return err
	}
	return s.write(fenced, what, call, rendered)
}

// ---------------------------------------------------------------------------
// the deployable's public host
// ---------------------------------------------------------------------------

// publicHost is the host a deployable's links are built on: its live custom domain when it
// has one, the first by hostname of several, and otherwise its own hostname. The site is
// read as its owner and its custom domains as the cluster acting (the file note); both
// fresh, since the links handed out are the only copies there will be.
func (s *Store) publicHost(ctx context.Context, owner, siteID string) (string, error) {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return "", errNoOwner
	}
	call, err := langparser.RenderCall("siteById", map[string]any{"siteId": memql.BareShortId(siteID)})
	rows, err := s.read(memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(owned)), "the deployable", call, err)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("%w: the deployable cannot be read as its owner", callrules.ErrNotFound)
	}
	own := linkHost(rowString(rows[0], "hostname"))
	call, err = langparser.RenderCall("customDomainsForSite", map[string]any{"siteId": memql.BareShortId(siteID)})
	domains, err := s.readAsCluster(ctx, "the deployable's custom domains", call, err)
	if err != nil {
		return "", err
	}
	var live []string
	for _, d := range domains {
		host := linkHost(rowString(d, "hostname"))
		if rowString(d, "status") == liveDomain && host != "" && memql.BareShortId(rowString(d, "siteId")) == memql.BareShortId(siteID) {
			live = append(live, host)
		}
	}
	if len(live) > 0 {
		sort.Strings(live)
		return live[0], nil
	}
	if own == "" {
		return "", fmt.Errorf("calls: the deployable %s has no hostname to build its links on", siteID)
	}
	return own, nil
}

// linkHost is a stored hostname as a link carries it: trimmed, lowercase, and without the
// root's trailing dot.
func linkHost(hostname string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
}

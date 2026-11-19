package calls

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/core/id"
)

// schedule_db_test.go -- booked calls (schedule.go) against a real engine and a real
// database: booking a call with its two links and its calendar file, the limits a key's
// bookings are held to, moving and cancelling a booking, and the first join that opens a
// booked call's room. For service_db_test.go's reason the media server is the one fake.
//
// Postgres-gated: an unreachable database skips, and MEMQL_REQUIRE_DB=1 turns the skip
// into a failure.

// book books a call for a host session, starting `in` after the test's clock, at whole
// seconds (the resolution a booking keeps).
func (cw *callWorld) book(sess callrules.Session, in time.Duration, minutes int, title string) (callrules.ScheduledCall, error) {
	cw.t.Helper()
	return cw.svc.Schedule(context.Background(), sess, callrules.ScheduleRequest{
		Start: cw.clock.Now().Add(in).Truncate(time.Second), Minutes: minutes, Title: title,
	})
}

func (cw *callWorld) booked(sess callrules.Session, in time.Duration, minutes int, title string) callrules.ScheduledCall {
	cw.t.Helper()
	sc, err := cw.book(sess, in, minutes, title)
	require.NoError(cw.t, err)
	return sc
}

// moveTo reschedules a booking to start `in` after the test's clock.
func (cw *callWorld) moveTo(sess callrules.Session, callID string, in time.Duration, minutes int, title string) (callrules.ScheduledCall, error) {
	cw.t.Helper()
	return cw.svc.Reschedule(context.Background(), sess, callID, callrules.ScheduleRequest{
		Start: cw.clock.Now().Add(in).Truncate(time.Second), Minutes: minutes, Title: title,
	})
}

// siteHost is the site's own hostname, as newWorld wrote it.
func (cw *callWorld) siteHost() string { return cw.site + ".example.test" }

// bindDomain binds a client's hostname to the site at a status of the custom-domain
// walk, as the domain sweep's rows would stand.
func (cw *callWorld) bindDomain(hostname, status string) {
	cw.t.Helper()
	cw.insert("v1:platform:customDomain", "cl-domain-"+id.NewShortId(), map[string]any{
		"siteId": siteConcept + ":" + cw.site, "hostname": hostname, "token": "dns-proof", "status": status,
	})
}

// joinBy is a first join with one of a call's links and no session.
func (cw *callWorld) joinBy(body callrules.JoinBody) (callrules.JoinResult, callrules.Session, error) {
	cw.t.Helper()
	body.Confirm = confirmed
	if body.Name == "" {
		body.Name = "Guest"
	}
	return cw.svc.Join(context.Background(), cw.ref, nil, body)
}

// icsLines is a calendar file's logical lines: its folded lines joined again (RFC 5545
// section 3.1), so a check reads each property whole however long it is.
func icsLines(ics []byte) []string {
	unfolded := strings.ReplaceAll(string(ics), "\r\n ", "")
	return strings.Split(strings.TrimSuffix(unfolded, "\r\n"), "\r\n")
}

func icsStamp(at time.Time) string { return at.UTC().Format("20060102T150405Z") }

// requireReason checks a refusal's class and its reason.
func requireReason(t *testing.T, err error, class error, reason string) {
	t.Helper()
	require.ErrorIs(t, err, class, "refused with the wrong error: %v", err)
	require.Equal(t, reason, callrules.ReasonOf(err), "refused for the wrong reason: %v", err)
}

// writeBooking writes a booking on a session's key straight through the store, over the
// span the test names (the store checks nothing; Schedule is what checks), with the status
// it names, and answers its id.
func (cw *callWorld) writeBooking(sess callrules.Session, start, end time.Time, status string) string {
	cw.t.Helper()
	links, err := cw.svc.mintLinks()
	require.NoError(cw.t, err)
	callID := id.NewShortId()
	require.NoError(cw.t, cw.calls.store.createBooking(context.Background(), cw.owner, newBooking{
		ID: callID, SiteID: cw.site, KeyID: sess.KeyID, Start: start, End: end,
		GuestDigest: callrules.Digest(cw.pepper(), []byte(links.GuestToken)),
		HostDigest:  callrules.Digest(cw.pepper(), []byte(links.HostToken)),
		CalendarUID: callID + "@" + cw.siteHost(),
	}, time.Now()))
	if status != "" {
		cw.asOwnerMutate(scheduledCallConcept, callID, "updateCallsScheduledCall", map[string]any{"scheduledCallId": callID, "status": status})
	}
	return callID
}

// capUpcoming sets the site's MaxUpcomingScheduled.
func (cw *callWorld) capUpcoming(n int) {
	cw.t.Helper()
	p := callrules.DefaultPolicy()
	p.MaxUpcomingScheduled = n
	_, err := cw.calls.store.savePolicy(context.Background(), cw.owner, nil, cw.site, p, []string{"maxUpcomingScheduled"}, time.Now())
	require.NoError(cw.t, err)
}

// bookingRoom is the one room a booking opened, read off the table.
func (cw *callWorld) bookingRoom(callID string) (string, map[string]any) {
	cw.t.Helper()
	found := map[string]map[string]any{}
	for roomID, room := range cw.rowsOf(roomConcept) {
		if bare(room["scheduledCallId"]) == callID {
			found[roomID] = room
		}
	}
	require.Len(cw.t, found, 1, "the booking opened %d rooms", len(found))
	for roomID, room := range found {
		return roomID, room
	}
	return "", nil
}

// ---------------------------------------------------------------------------
// booking
// ---------------------------------------------------------------------------

// TestScheduleReturnsTwoLinksAndACalendarFile: a key holder books a call and is handed
// its id, its span, both link secrets, both links on the deployable's own hostname, and a
// calendar file whose UID is <id>@<host> and whose URL is the guest link, with the title
// as its summary, now as its stamp and a product-neutral PRODID, and no host token
// anywhere in it. The booking's row holds the two links' digests and never a link,
// belongs to the site's owner and names the key. Nothing is placed or opened until
// somebody joins.
func TestScheduleReturnsTwoLinksAndACalendarFile(t *testing.T) {
	cw := newCallWorld(t, "book", 50)
	if cw == nil {
		return
	}
	sess := cw.host()
	start := cw.clock.Now().Add(2 * time.Hour).Truncate(time.Second)

	sc, err := cw.svc.Schedule(context.Background(), sess, callrules.ScheduleRequest{Start: start, Minutes: 45, Title: "Design review"})
	require.NoError(t, err)
	require.NotEmpty(t, sc.ID)
	require.Equal(t, start, sc.Start.UTC())
	require.Equal(t, start.Add(45*time.Minute), sc.End.UTC())
	require.Equal(t, "Design review", sc.Title)
	require.Regexp(t, tokenShape, sc.GuestToken)
	require.Regexp(t, tokenShape, sc.HostToken)
	require.NotEqual(t, sc.GuestToken, sc.HostToken)
	guestLink := "https://" + cw.siteHost() + "/#j=" + sc.GuestToken
	require.Equal(t, guestLink, sc.GuestLink)
	require.Equal(t, guestLink+"&h="+sc.HostToken, sc.HostLink)

	lines := icsLines(sc.ICS)
	for _, want := range []string{
		"BEGIN:VCALENDAR", "PRODID:" + calendarProductID, "METHOD:PUBLISH", "BEGIN:VEVENT",
		"UID:" + sc.ID + "@" + cw.siteHost(),
		"DTSTAMP:" + icsStamp(cw.clock.Now()),
		"DTSTART:" + icsStamp(start),
		"DTEND:" + icsStamp(start.Add(45*time.Minute)),
		"SUMMARY:Design review",
		"URL:" + guestLink,
		"END:VEVENT", "END:VCALENDAR",
	} {
		require.Contains(t, lines, want, "the calendar file has no line %q", want)
	}
	require.NotContains(t, strings.Join(lines, "\n"), sc.HostToken, "the calendar file carries the host token")
	require.NotContains(t, string(sc.ICS), sc.HostToken, "the calendar file carries the host token")

	booking := cw.row(scheduledCallConcept, sc.ID)
	require.Equal(t, "scheduled", booking["status"])
	require.Equal(t, sess.KeyID, bare(booking["keyId"]))
	require.Equal(t, cw.owner, bare(booking["ownerUserId"]), "the booking belongs to the site's owner")
	require.Equal(t, "Design review", booking["title"])
	require.Equal(t, start, rowTime(booking, "start"))
	require.Equal(t, start.Add(45*time.Minute), rowTime(booking, "end"))
	require.Equal(t, callrules.Digest(cw.pepper(), []byte(sc.GuestToken)), booking["guestDigest"])
	require.Equal(t, callrules.Digest(cw.pepper(), []byte(sc.HostToken)), booking["hostDigest"])
	require.Equal(t, sc.ID+"@"+cw.siteHost(), booking["calendarUid"])
	require.Zero(t, cw.rowsContaining(sc.GuestToken)+cw.rowsContaining(sc.HostToken), "a stored row carries a link")
	require.Empty(t, cw.rowsOf(roomConcept), "booking a call opened a room")
	require.Empty(t, cw.media.rooms(), "booking a call opened a media room")

	// With no title the calendar file says what it is in neutral words, and a title is
	// kept on one line.
	untitled := cw.booked(sess, 4*time.Hour, 30, "")
	require.Empty(t, untitled.Title)
	require.Contains(t, icsLines(untitled.ICS), "SUMMARY:"+defaultCallTitle)
	tidy := cw.booked(sess, 6*time.Hour, 30, "  Weekly\n\tsync  ")
	require.Equal(t, "Weekly sync", tidy.Title)
	require.Contains(t, icsLines(tidy.ICS), "SUMMARY:Weekly sync")
}

// TestScheduledLinksUseTheSitesLiveCustomDomain: a deployable with a live custom domain
// hands out links on that domain, the first by hostname when it has several; a binding
// still on its walk is not the site's address yet. A call booked before the domain went
// live and moved after keeps the calendar UID it was booked under, so a calendar updates
// the entry it already has rather than adding a second.
func TestScheduledLinksUseTheSitesLiveCustomDomain(t *testing.T) {
	cw := newCallWorld(t, "domain", 50)
	if cw == nil {
		return
	}
	sess := cw.host()
	cw.bindDomain("pending-"+cw.suffix+".client.example.test", "pending_dns")
	before := cw.booked(sess, 2*time.Hour, 30, "Before")
	require.Equal(t, "https://"+cw.siteHost()+"/#j="+before.GuestToken, before.GuestLink, "a binding that is not live was used")

	alpha := "alpha-" + cw.suffix + ".client.example.test"
	cw.bindDomain("zeta-"+cw.suffix+".client.example.test", "live")
	cw.bindDomain(alpha, "live")
	after := cw.booked(sess, 4*time.Hour, 30, "After")
	require.Equal(t, "https://"+alpha+"/#j="+after.GuestToken, after.GuestLink, "the live custom domain was not used")
	require.Equal(t, "https://"+alpha+"/#j="+after.GuestToken+"&h="+after.HostToken, after.HostLink)
	lines := icsLines(after.ICS)
	require.Contains(t, lines, "UID:"+after.ID+"@"+alpha)
	require.Contains(t, lines, "URL:"+after.GuestLink)

	moved, err := cw.moveTo(sess, before.ID, 3*time.Hour, 30, "")
	require.NoError(t, err)
	require.Equal(t, "https://"+alpha+"/#j="+moved.GuestToken, moved.GuestLink, "a moved call's new links are not on the live domain")
	require.Contains(t, icsLines(moved.ICS), "UID:"+before.ID+"@"+cw.siteHost(), "moving the call changed its calendar UID")
}

// TestScheduleValidatesTheBookingAndWhoMayBook: a booking is refused, with ErrInvalid
// and a stable reason, when it starts in the past or beyond the horizon, is longer than
// the policy allows or has no length, or carries a title over 80 characters; nothing is
// written. Only a session a key opened books a call, and only while the key is active.
func TestScheduleValidatesTheBookingAndWhoMayBook(t *testing.T) {
	cw := newCallWorld(t, "bookrules", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	sess := cw.host()
	now := cw.clock.Now()
	soon := now.Add(time.Hour).Truncate(time.Second)
	for _, c := range []struct {
		name   string
		req    callrules.ScheduleRequest
		reason string
	}{
		{"in the past", callrules.ScheduleRequest{Start: now.Add(-time.Minute), Minutes: 30}, callrules.ReasonInPast},
		{"beyond the horizon", callrules.ScheduleRequest{Start: now.Add(31 * 24 * time.Hour), Minutes: 30}, callrules.ReasonBeyondHorizon},
		{"too long", callrules.ScheduleRequest{Start: soon, Minutes: 481}, callrules.ReasonTooLong},
		{"no length", callrules.ScheduleRequest{Start: soon, Minutes: 0}, callrules.ReasonMinutesInvalid},
		{"a negative length", callrules.ScheduleRequest{Start: soon, Minutes: -30}, callrules.ReasonMinutesInvalid},
		{"a long title", callrules.ScheduleRequest{Start: soon, Minutes: 30, Title: strings.Repeat("a", 81)}, callrules.ReasonTitleTooLong},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := cw.svc.Schedule(ctx, sess, c.req)
			requireReason(t, err, callrules.ErrInvalid, c.reason)
		})
	}
	require.Empty(t, cw.rowsOf(scheduledCallConcept), "a refused booking was written")

	// The limits hold at their edges: 480 minutes and 80 characters are allowed.
	long := cw.booked(sess, time.Hour, 480, strings.Repeat("e", 80))
	require.Equal(t, 480*time.Minute, long.End.Sub(long.Start))

	// Only a key holder books, and only with an active key.
	req := callrules.ScheduleRequest{Start: now.Add(20 * time.Hour), Minutes: 30}
	_, started := cw.started("Host")
	_, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	_, err = cw.svc.Schedule(ctx, guest, req)
	require.ErrorIs(t, err, callrules.ErrForbidden, "a guest's session booked a call")
	_, cohost, err := cw.joinBy(callrules.JoinBody{HostToken: started.Call.HostToken})
	require.NoError(t, err)
	_, err = cw.svc.Schedule(ctx, cohost, req)
	require.ErrorIs(t, err, callrules.ErrForbidden, "a host link's session, which holds no key, booked a call")
	frozen := cw.host()
	cw.asOwnerMutate(keyConcept, frozen.KeyID, "updateCallsKey", map[string]any{"keyId": frozen.KeyID, "status": "frozen"})
	_, err = cw.svc.Schedule(ctx, frozen, req)
	requireReason(t, err, callrules.ErrForbidden, callrules.ReasonKeyNotActive)
	_, err = cw.svc.Schedule(ctx, callrules.Session{Token: "not-a-token", Site: cw.ref}, req)
	require.ErrorIs(t, err, callrules.ErrNoSession)
	require.Len(t, cw.rowsOf(scheduledCallConcept), 1, "a refused booking was written")
}

// TestScheduleRefusesOverlapOnOneKey: a key's bookings never overlap as booked intervals,
// so a booking that overlaps another of the key's is refused as a conflict and writes
// nothing (what that does and does not promise once calls open is schedule.go's file
// note). Back-to-back
// bookings do not overlap, another key's bookings are its own, a move is checked against
// the key's other bookings and never against itself, and a cancelled or ended booking
// frees its slot. A key holds at most the policy's upcoming bookings.
func TestScheduleRefusesOverlapOnOneKey(t *testing.T) {
	cw := newCallWorld(t, "overlap", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	sess := cw.host()
	a := cw.booked(sess, 2*time.Hour, 60, "A") // 2:00 to 3:00

	_, err := cw.book(sess, 2*time.Hour+30*time.Minute, 60, "Overlapping its end")
	requireReason(t, err, callrules.ErrConflict, callrules.ReasonOverlaps)
	_, err = cw.book(sess, time.Hour+30*time.Minute, 60, "Overlapping its start")
	requireReason(t, err, callrules.ErrConflict, callrules.ReasonOverlaps)
	require.Len(t, cw.rowsOf(scheduledCallConcept), 1, "a refused booking was written")

	cw.booked(sess, 3*time.Hour, 60, "Right after A") // 3:00 to 4:00
	cw.booked(sess, time.Hour, 60, "Right before A")  // 1:00 to 2:00
	other := cw.host()
	cw.booked(other, 2*time.Hour+30*time.Minute, 60, "Another key's call")

	moved, err := cw.moveTo(sess, a.ID, 2*time.Hour+15*time.Minute, 30, "")
	require.NoError(t, err, "a move inside the call's own slot was refused")
	require.Equal(t, a.ID, moved.ID)
	_, err = cw.moveTo(sess, a.ID, 3*time.Hour+30*time.Minute, 60, "")
	requireReason(t, err, callrules.ErrConflict, callrules.ReasonOverlaps)

	require.NoError(t, cw.svc.Cancel(ctx, sess, a.ID))
	cw.booked(sess, 2*time.Hour, 60, "In A's old slot")

	p := callrules.DefaultPolicy()
	p.MaxUpcomingScheduled = 3
	_, err = cw.calls.store.savePolicy(ctx, cw.owner, nil, cw.site, p, []string{"maxUpcomingScheduled"}, time.Now())
	require.NoError(t, err)
	_, err = cw.book(sess, 10*time.Hour, 30, "One too many")
	requireReason(t, err, callrules.ErrConflict, callrules.ReasonTooManyUpcoming)
	cw.clock.Advance(2*time.Hour + time.Second) // "Right before A" is over
	cw.booked(sess, 10*time.Hour, 30, "In the place of one that ended")
}

// TestAKeysBookingsReadOnlyThoseThatHoldASlot: the read a booking is checked against,
// under the key's lock, returns only the bookings that may still hold a slot -- not
// cancelled, not ended, and ending after now or started -- so a key's past, cancelled and
// ended bookings never reach it. A key whose past, cancelled and ended bookings outnumber
// MaxUpcomingScheduled still books, up to the cap. A started booking is read whatever its
// end until its call ends and marks it ended (schedule.go's file note); this one opened no
// room before its end passed, so nothing ended it: it is read and holds nothing.
func TestAKeysBookingsReadOnlyThoseThatHoldASlot(t *testing.T) {
	cw := newCallWorld(t, "slots", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	sess := cw.host()
	cw.capUpcoming(2)

	// Wall-clock times, which the read compares against the engine's own clock.
	wall := time.Now().UTC().Truncate(time.Second)
	ahead := cw.writeBooking(sess, wall.Add(time.Hour), wall.Add(2*time.Hour), "")
	startedLongAgo := cw.writeBooking(sess, wall.Add(-2*time.Hour), wall.Add(-time.Hour), callrules.BookingStarted)
	for i := 0; i < 3; i++ {
		at := func(hours int) time.Time { return wall.Add(time.Duration(hours) * time.Hour) }
		cw.writeBooking(sess, at(-6-2*i), at(-5-2*i), "") // nobody opened it before its end
		cw.writeBooking(sess, at(10+2*i), at(11+2*i), callrules.BookingCancelled)
		cw.writeBooking(sess, at(20+2*i), at(21+2*i), callrules.BookingEnded)
	}
	require.Len(t, cw.rowsOf(scheduledCallConcept), 11)

	read, err := cw.calls.store.bookingsForKey(ctx, cw.owner, sess.KeyID)
	require.NoError(t, err)
	var ids []string
	for _, b := range read {
		ids = append(ids, b.ID)
	}
	require.ElementsMatch(t, []string{ahead, startedLongAgo}, ids, "the key's bookings read returned bookings that hold no slot")

	cw.booked(sess, 30*time.Hour, 30, "Within the cap")
	_, err = cw.book(sess, 40*time.Hour, 30, "Past the cap")
	requireReason(t, err, callrules.ErrConflict, callrules.ReasonTooManyUpcoming)
}

// TestARunningCallHoldsItsSlotUntilItsRoomEnds: a booked call that runs past its booked
// end still holds its key's slot, its span running to now, so it counts toward the key's
// MaxUpcomingScheduled however long it runs; once its room ends it holds nothing.
func TestARunningCallHoldsItsSlotUntilItsRoomEnds(t *testing.T) {
	cw := newCallWorld(t, "runover", 50)
	if cw == nil {
		return
	}
	sess := cw.host()
	cw.capUpcoming(1)
	late := cw.booked(sess, 5*time.Minute, 30, "Running late")
	in, _, err := cw.joinBy(callrules.JoinBody{HostToken: late.HostToken, Name: "Host"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
	roomID, _ := cw.bookingRoom(late.ID)

	cw.clock.Advance(45 * time.Minute) // ten minutes past its booked end, and still running
	_, err = cw.book(sess, time.Hour, 30, "While it runs over")
	requireReason(t, err, callrules.ErrConflict, callrules.ReasonTooManyUpcoming)

	cw.asOwnerMutate(roomConcept, roomID, "updateCallsRoom", map[string]any{"roomId": roomID, "status": "ended"})
	cw.booked(sess, time.Hour, 30, "Once it has ended")
}

// TestABackToBackBookingOpensEarlyWhileThePreviousCallRuns: back-to-back bookings on one
// key are allowed -- an agent books one interview per candidate, each ending as the next
// begins -- and the second opens in its early window while the first's room is still
// open, so the key has two calls at once. A booked opening does not look at the key's open
// rooms for exactly this case (schedule.go's file note); what an overlap costs comes with
// payments.
func TestABackToBackBookingOpensEarlyWhileThePreviousCallRuns(t *testing.T) {
	cw := newCallWorld(t, "backtoback", 50)
	if cw == nil {
		return
	}
	sess := cw.host()
	first := cw.booked(sess, 5*time.Minute, 30, "First interview")
	second := cw.booked(sess, 35*time.Minute, 30, "Second interview") // starts as the first ends

	in, _, err := cw.joinBy(callrules.JoinBody{HostToken: first.HostToken, Name: "Interviewer"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
	firstRoom, _ := cw.bookingRoom(first.ID)

	cw.clock.Advance(26 * time.Minute) // nine minutes before the second's start
	next, _, err := cw.joinBy(callrules.JoinBody{HostToken: second.HostToken, Name: "Next interviewer"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, next.Outcome, "a back-to-back booking did not open in its early window")
	secondRoom, room := cw.bookingRoom(second.ID)
	require.NotEqual(t, firstRoom, secondRoom)
	require.Equal(t, "open", cw.row(roomConcept, firstRoom)["status"], "the first call was cut")
	require.Equal(t, sess.KeyID, bare(room["keyId"]))
}

// TestRescheduleMintsNewLinksUnderTheSameUID: moving a booking keeps its id and its
// calendar UID and mints new links, handed out once with a new calendar file; the old
// links open nothing at all from then on, and the new ones open the call. A move with no
// title keeps the call's title, and one with a title replaces it.
func TestRescheduleMintsNewLinksUnderTheSameUID(t *testing.T) {
	cw := newCallWorld(t, "move", 50)
	if cw == nil {
		return
	}
	sess := cw.host()
	sc := cw.booked(sess, 2*time.Hour, 60, "Planning")

	moved, err := cw.moveTo(sess, sc.ID, 30*time.Minute, 45, "")
	require.NoError(t, err)
	require.Equal(t, sc.ID, moved.ID)
	start := cw.clock.Now().Add(30 * time.Minute).Truncate(time.Second)
	require.Equal(t, start, moved.Start.UTC())
	require.Equal(t, start.Add(45*time.Minute), moved.End.UTC())
	require.Equal(t, "Planning", moved.Title, "a move with no title lost the call's title")
	require.NotEqual(t, sc.GuestToken, moved.GuestToken)
	require.NotEqual(t, sc.HostToken, moved.HostToken)
	lines := icsLines(moved.ICS)
	require.Contains(t, lines, "UID:"+sc.ID+"@"+cw.siteHost(), "the moved call's calendar file has another UID")
	require.Contains(t, lines, "URL:"+moved.GuestLink)
	require.Contains(t, lines, "DTSTART:"+icsStamp(start))
	require.Contains(t, lines, "SUMMARY:Planning")

	booking := cw.row(scheduledCallConcept, sc.ID)
	require.Equal(t, callrules.Digest(cw.pepper(), []byte(moved.GuestToken)), booking["guestDigest"])
	require.Equal(t, callrules.Digest(cw.pepper(), []byte(moved.HostToken)), booking["hostDigest"])
	require.Equal(t, start, rowTime(booking, "start"))
	require.Equal(t, "scheduled", booking["status"])
	require.Len(t, cw.rowsOf(scheduledCallConcept), 1, "a move wrote a second booking")

	retitled, err := cw.moveTo(sess, sc.ID, 30*time.Minute, 45, "Planning, part two")
	require.NoError(t, err)
	require.Equal(t, "Planning, part two", retitled.Title)
	require.Equal(t, "Planning, part two", cw.row(scheduledCallConcept, sc.ID)["title"])

	// Inside the window: the old links find nothing, not even "not yet".
	cw.clock.Advance(25 * time.Minute)
	for _, old := range []callrules.ScheduledCall{sc, moved} {
		for _, body := range []callrules.JoinBody{{Token: old.GuestToken}, {HostToken: old.HostToken}} {
			_, out, err := cw.joinBy(body)
			require.ErrorIs(t, err, callrules.ErrNotFound, "a link a move replaced still opens the call")
			require.Empty(t, out.Token)
		}
	}
	res, _, err := cw.joinBy(callrules.JoinBody{HostToken: retitled.HostToken, Name: "Host"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, res.Outcome)
	require.True(t, res.Host)
}

// TestCancelKillsBothLinks: once a booking is cancelled, its guest link and its host
// link are answered at once that the call was cancelled, even inside the window in which
// they would open it, and nothing is written. Cancelling again changes nothing, a
// cancelled call cannot be moved, and only a session of the booking's own key acts on it.
// A call that has started is not cancelled, and one whose booked end passed unopened has
// ended.
func TestCancelKillsBothLinks(t *testing.T) {
	cw := newCallWorld(t, "cancel", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	sess := cw.host()
	sc := cw.booked(sess, 30*time.Minute, 60, "Called off")
	cw.clock.Advance(25 * time.Minute) // inside the window: either link would open the call
	versions := cw.versions(scheduledCallConcept, sc.ID)

	require.NoError(t, cw.svc.Cancel(ctx, sess, sc.ID))
	require.Equal(t, "cancelled", cw.row(scheduledCallConcept, sc.ID)["status"])
	require.Equal(t, versions+1, cw.versions(scheduledCallConcept, sc.ID))
	for _, body := range []callrules.JoinBody{{Token: sc.GuestToken}, {HostToken: sc.HostToken}} {
		res, out, err := cw.joinBy(body)
		requireRefused(t, res, err, callrules.ErrEnded, callrules.ReasonCancelled)
		require.Empty(t, out.Token, "a join to a cancelled call handed out a session")
	}
	require.Empty(t, cw.rowsOf(roomConcept), "a cancelled call's link opened a room")
	require.Empty(t, cw.rowsOf(seatConcept))
	require.Empty(t, cw.rowsOf(queueEntryConcept))
	require.Empty(t, cw.media.rooms())
	require.Len(t, cw.rowsOf(sessionConcept), 1, "a join to a cancelled call wrote a session")

	require.NoError(t, cw.svc.Cancel(ctx, sess, sc.ID), "cancelling twice is an error")
	require.Equal(t, versions+1, cw.versions(scheduledCallConcept, sc.ID), "a second cancel wrote the booking")
	_, err := cw.moveTo(sess, sc.ID, 2*time.Hour, 30, "")
	requireReason(t, err, callrules.ErrEnded, callrules.ReasonCancelled)

	kept := cw.booked(sess, 3*time.Hour, 30, "Kept")
	stranger := cw.host()
	require.ErrorIs(t, cw.svc.Cancel(ctx, stranger, kept.ID), callrules.ErrForbidden, "another key's session cancelled the call")
	_, err = cw.moveTo(stranger, kept.ID, 4*time.Hour, 30, "")
	require.ErrorIs(t, err, callrules.ErrForbidden, "another key's session moved the call")
	require.ErrorIs(t, cw.svc.Cancel(ctx, sess, id.NewShortId()), callrules.ErrNotFound)
	require.Equal(t, "scheduled", cw.row(scheduledCallConcept, kept.ID)["status"])

	live := cw.booked(sess, 5*time.Minute, 30, "Live")
	opened, _, err := cw.joinBy(callrules.JoinBody{Token: live.GuestToken})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, opened.Outcome)
	requireReason(t, cw.svc.Cancel(ctx, sess, live.ID), callrules.ErrConflict, callrules.ReasonStarted)
	_, err = cw.moveTo(sess, live.ID, 6*time.Hour, 30, "")
	requireReason(t, err, callrules.ErrConflict, callrules.ReasonStarted)
	require.Equal(t, "started", cw.row(scheduledCallConcept, live.ID)["status"])

	missed := cw.booked(sess, 40*time.Minute, 10, "Missed")
	cw.clock.Advance(time.Hour)
	requireReason(t, cw.svc.Cancel(ctx, sess, missed.ID), callrules.ErrEnded, callrules.ReasonCallEnded)
	_, err = cw.moveTo(sess, missed.ID, 6*time.Hour, 30, "")
	requireReason(t, err, callrules.ErrEnded, callrules.ReasonCallEnded)
}

// ---------------------------------------------------------------------------
// joining a booked call
// ---------------------------------------------------------------------------

// TestHostLinkGrantsHostAndCreatorNeedNotAttend: whoever schedules a call need not be in
// it. The first join inside the window opens the room, on the booking's key, LOCKED,
// carrying the booking and its two links' digests, and the booking is marked started
// with the room. A guest who opens it waits; whoever holds the host link is let in as a
// host though the call is locked, and their session holds no key, so it starts and books
// nothing. Later joins find the same room.
func TestHostLinkGrantsHostAndCreatorNeedNotAttend(t *testing.T) {
	cw := newCallWorld(t, "hostlink", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	scheduler := cw.host()
	sc := cw.booked(scheduler, 30*time.Minute, 60, "Interview")
	cw.clock.Advance(25 * time.Minute) // five minutes before the start

	res, guest, err := cw.join(sc.GuestToken, "Candidate")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, res.Outcome, "a booked call's guest was let in past its waiting room")
	require.Equal(t, "locked", res.Reason)
	require.Empty(t, res.Pass)
	require.Equal(t, callrules.SessionGuest, guest.Kind)

	roomID, room := cw.bookingRoom(sc.ID)
	require.Equal(t, true, room["locked"], "a booked call's room opened unlocked")
	require.Equal(t, "open", room["status"])
	require.Equal(t, scheduler.KeyID, bare(room["keyId"]), "the room is not on the booking's key")
	require.Equal(t, callrules.Digest(cw.pepper(), []byte(sc.GuestToken)), room["guestDigest"])
	require.Equal(t, callrules.Digest(cw.pepper(), []byte(sc.HostToken)), room["hostDigest"])
	require.Equal(t, roomID, guest.RoomID)
	booking := cw.row(scheduledCallConcept, sc.ID)
	require.Equal(t, "started", booking["status"])
	require.Equal(t, roomID, bare(booking["roomId"]))
	require.Equal(t, []fakeMediaRoom{{Server: BootstrapMediaServerID, Name: roomID, Max: 11}}, cw.media.rooms())
	seat := cw.row(seatConcept, guest.SeatID)
	require.Equal(t, "waiting", seat["status"])
	require.Equal(t, false, seat["host"])
	require.Equal(t, true, seat["confirmAdult"])
	require.Equal(t, true, seat["confirmNoMinors"])
	require.Equal(t, confirmed.Terms, seat["termsVersion"])
	require.EqualValues(t, 1, room["joinWindowCount"], "the opening join is not the room's first")

	hres, hsess, err := cw.joinBy(callrules.JoinBody{HostToken: sc.HostToken, Name: "Interviewer"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, hres.Outcome, "the host link waited in its own call")
	require.True(t, hres.Host)
	verifyPass(t, hres.Pass, testMediaConfig().APIKey, testAPISecret)
	require.Equal(t, callrules.SessionHost, hsess.Kind)
	require.Empty(t, hsess.KeyID, "a host link's session holds a key")
	require.Equal(t, roomID, hsess.RoomID)
	hostSeat := cw.row(seatConcept, hsess.SeatID)
	require.Equal(t, true, hostSeat["host"])
	require.Equal(t, "admitted", hostSeat["status"])

	resolved, err := cw.svc.Session(ctx, cw.ref, scheduler.Token)
	require.NoError(t, err)
	require.Empty(t, resolved.SeatID, "the scheduler was seated in a call they never joined")
	_, err = cw.svc.Start(ctx, hsess, callrules.StartBody{Name: "Interviewer", Confirm: confirmed})
	require.ErrorIs(t, err, callrules.ErrForbidden, "a host link's session started a call")
	_, err = cw.svc.Schedule(ctx, hsess, callrules.ScheduleRequest{Start: cw.clock.Now().Add(5 * time.Hour), Minutes: 30})
	require.ErrorIs(t, err, callrules.ErrForbidden, "a host link's session booked a call")

	polled, err := cw.svc.Queue(ctx, guest)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, polled.Outcome, "the waiting guest was let in without the host admitting them")
	second, _, err := cw.join(sc.GuestToken, "Second candidate")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, second.Outcome)
	cw.bookingRoom(sc.ID)
	// The host's pass was preceded by opening the media room again, as every pass is
	// (service.go ensureMediaRoom); every room opened is still this one.
	for _, opened := range cw.media.rooms() {
		require.Equal(t, roomID, opened.Name, "a later join opened a second media room")
	}
}

// TestJoinBeforeStartAfterCancelAfterEnd: a booked call's links are answered at once,
// never with a hang or a place in a queue. More than ten minutes before the start the
// answer is not yet, with the time until the call opens, and nothing is written; from
// ten minutes before, the first join opens the room. A cancelled call answers cancelled,
// and one whose booked end passed unopened answers that it ended. A call whose room is
// open stays open past its booked end, and one whose room ended answers that it ended.
func TestJoinBeforeStartAfterCancelAfterEnd(t *testing.T) {
	cw := newCallWorld(t, "booktimes", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	sess := cw.host()
	early := cw.booked(sess, time.Hour, 30, "Early")           // 1:00 to 1:30
	cancelled := cw.booked(sess, 2*time.Hour, 30, "Cancelled") // 2:00 to 2:30
	missed := cw.booked(sess, 3*time.Hour, 30, "Missed")       // 3:00 to 3:30

	// The booking keeps its start to the second, so the call opens a little under fifty
	// minutes from the test's clock.
	opens := early.Start.Add(-callrules.ScheduledCallOpensEarly)
	require.InDelta(t, float64(50*time.Minute), float64(opens.Sub(cw.clock.Now())), float64(time.Second))
	for _, body := range []callrules.JoinBody{{Token: early.GuestToken}, {HostToken: early.HostToken}} {
		res, out, err := cw.joinBy(body)
		requireRefused(t, res, err, callrules.ErrConflict, callrules.ReasonNotYet)
		require.Equal(t, opens.Sub(cw.clock.Now()), res.RetryAfter, "not yet does not say when the call opens")
		require.Empty(t, out.Token, "an early join handed out a session")
	}
	cw.clock.Advance(49 * time.Minute)
	res, _, err := cw.joinBy(callrules.JoinBody{Token: early.GuestToken})
	requireRefused(t, res, err, callrules.ErrConflict, callrules.ReasonNotYet)
	require.Equal(t, opens.Sub(cw.clock.Now()), res.RetryAfter)
	require.Empty(t, cw.rowsOf(roomConcept), "an early join opened a room")
	require.Empty(t, cw.rowsOf(seatConcept), "an early join wrote a seat")
	require.Len(t, cw.rowsOf(sessionConcept), 1, "an early join wrote a session")

	cw.clock.Advance(opens.Sub(cw.clock.Now())) // exactly ten minutes before the start
	opened, _, err := cw.joinBy(callrules.JoinBody{Token: early.GuestToken})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, opened.Outcome)
	earlyRoom, _ := cw.bookingRoom(early.ID)

	require.NoError(t, cw.svc.Cancel(ctx, sess, cancelled.ID))
	res, _, err = cw.joinBy(callrules.JoinBody{Token: cancelled.GuestToken})
	requireRefused(t, res, err, callrules.ErrEnded, callrules.ReasonCancelled)

	cw.clock.Advance(3 * time.Hour) // past every booked end
	for _, body := range []callrules.JoinBody{{Token: missed.GuestToken}, {HostToken: missed.HostToken}} {
		res, out, err := cw.joinBy(body)
		requireRefused(t, res, err, callrules.ErrEnded, callrules.ReasonCallEnded)
		require.Empty(t, out.Token)
	}
	require.Empty(t, cw.rowsOf(queueEntryConcept), "a refused join was queued")
	require.Len(t, cw.rowsOf(roomConcept), 1, "a refused join opened a room")

	late, _, err := cw.joinBy(callrules.JoinBody{HostToken: early.HostToken, Name: "Late host"})
	require.NoError(t, err, "a call running past its booked end was cut")
	require.Equal(t, callrules.OutcomeAdmit, late.Outcome)
	cw.asOwnerMutate(roomConcept, earlyRoom, "updateCallsRoom", map[string]any{"roomId": earlyRoom, "status": "ended"})
	res, _, err = cw.joinBy(callrules.JoinBody{Token: early.GuestToken})
	requireRefused(t, res, err, callrules.ErrEnded, callrules.ReasonCallEnded)
}

// TestABookedCallQueuesAheadOfNewCallsAndBehindRunningOnes: on a full media server, a
// booked call's first join is queued in the booked call's class: ahead of a new call's
// host, behind someone joining a call that is already running.
func TestABookedCallQueuesAheadOfNewCallsAndBehindRunningOnes(t *testing.T) {
	cw := newCallWorld(t, "dueclass", 2)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, a := cw.started("Host A") // seat 1 of 2
	_, _, err := cw.join(a.Call.GuestToken, "A1")
	require.NoError(t, err) // seat 2: the server is full

	newHost := cw.host()
	fresh, err := cw.svc.Start(ctx, newHost, callrules.StartBody{Name: "New host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, fresh.Join.Outcome)
	require.Equal(t, 1, fresh.Join.Position)
	newHost, err = cw.svc.Session(ctx, cw.ref, newHost.Token)
	require.NoError(t, err)

	sc := cw.booked(cw.host(), 5*time.Minute, 30, "Due")
	due, dueSess, err := cw.joinBy(callrules.JoinBody{HostToken: sc.HostToken, Name: "Booked host"})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, due.Outcome)
	require.Equal(t, 1, due.Position, "a booked call that is due queued behind a new call")
	behind, err := cw.svc.Queue(ctx, newHost)
	require.NoError(t, err)
	require.Equal(t, 2, behind.Position)

	running, _, err := cw.join(a.Call.GuestToken, "A2")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, running.Outcome)
	require.Equal(t, 1, running.Position, "someone joining a running call queued behind a booked call")
	dueNow, err := cw.svc.Queue(ctx, dueSess)
	require.NoError(t, err)
	require.Equal(t, 2, dueNow.Position)

	var class string
	for _, entry := range cw.rowsOf(queueEntryConcept) {
		if bare(entry["seatId"]) == dueSess.SeatID {
			class, _ = entry["class"].(string)
		}
	}
	require.Equal(t, "scheduled", class)
}

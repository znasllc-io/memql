package calls

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// rooms.go -- the rows a call is made of: sessions, rooms, seats and queue entries, and
// the one read that spans every call on a media server (loadOn). The decisions are the
// service's (service.go); this file reads and writes rows and decides nothing.
//
// # Which actor
//
// A call's rows are a SITE'S rows (store.go's file note), read and written as the
// site's owner with internal origin (rowsAs). The load of a media server is the one
// exception: a server's seats are shared by the calls of every deployable placed on it,
// so its rooms, seats and queue entries are read AS THE CLUSTER ACTING (clusterRowsAs),
// whose `actor.isClusterOwner` term is what admits every owner's rows. Nothing read that
// way leaves the service: it is counted, never returned.
//
// Both stamp internal origin here as well as in store.go, because every construct this
// file renders is @serverOnly and the server-only caller gate
// (test/dslconformance/server_only_callers_stamp_test.go) asks the file that renders one
// to stamp in the file.
//
// # Every write is fenced
//
// A create is fenced on its row's absence and an update on the version it was decided
// from (store.go's witnessed and witnessedAbsent), on top of the write gates the service
// holds. The gates serialize the replicas; the fence is what makes a write that lost its
// gate, or a session moved by a join to another room under that room's gate, fail
// rather than land on a row it did not read.

const (
	sessionConcept    = "v1:calls:session"
	roomConcept       = "v1:calls:room"
	seatConcept       = "v1:calls:seat"
	queueEntryConcept = "v1:calls:queueEntry"
)

// The seat statuses (v1:calls:seat.status). Admitted and in_call hold a media server's
// seat; waiting holds a place, in a queue or behind a lock, and no seat.
const (
	seatWaiting  = "waiting"
	seatAdmitted = "admitted"
	seatInCall   = "in_call"
	seatLeft     = "left"
	seatRemoved  = "removed"
)

// The queue entry statuses (v1:calls:queueEntry.status). An absent status reads as
// waiting, the concept's own rule.
const (
	entryWaiting  = "waiting"
	entryAdmitted = "admitted"
	entryExpired  = "expired"
)

// The queue classes as v1:calls:queueEntry.class stores them.
var queueClassNames = map[callrules.QueueClass]string{
	callrules.ClassRunningCall:  "running",
	callrules.ClassScheduledDue: "scheduled",
	callrules.ClassNewCall:      "new",
}

func queueClassOf(stored string) callrules.QueueClass {
	for class, name := range queueClassNames {
		if name == stored {
			return class
		}
	}
	// The concept's enum keeps anything else out; a value it never held drains last
	// rather than ahead of anybody.
	return callrules.ClassNewCall
}

// rowsAs is the context a site's call rows are read and written under: the site's owner,
// with internal origin (see the file note for why it is stamped here too). A blank owner
// answers false with the context unchanged, as asOwner's does.
func rowsAs(ctx context.Context, owner string) (context.Context, bool) {
	owned, ok := asOwner(ctx, owner)
	if !ok {
		return ctx, false
	}
	return auth.ContextWithInternalOrigin(owned), true
}

// clusterRowsAs is the context a media server's load is read under: the cluster acting,
// with internal origin, roots only and fresh, as readAsCluster's reads are.
func clusterRowsAs(ctx context.Context) (context.Context, error) {
	cluster, err := asCluster(ctx)
	if err != nil {
		return nil, err
	}
	return memql.ContextWithRootsOnlyRead(memql.ContextWithFreshRead(auth.ContextWithInternalOrigin(cluster))), nil
}

// readAs is one of a site's reads: roots only, as the site's owner. Every calls query is
// @cache(0), so it is answered by the database whatever the context says; a caller under
// a gate marks it fresh as well, which is the rule a reader under a gate keeps.
func (s *Store) readAs(ctx context.Context, owner, what, call string, rendered error) ([]map[string]any, error) {
	owned, ok := rowsAs(ctx, owner)
	if !ok {
		return nil, errNoOwner
	}
	return s.read(memql.ContextWithRootsOnlyRead(owned), what, call, rendered)
}

// writeAs is one of a site's writes, as the site's owner, under the fence it is handed:
// absent for a create, at for an update.
func (s *Store) writeAs(ctx context.Context, owner string, fence func(context.Context) (context.Context, error), what, call string, rendered error) error {
	owned, ok := rowsAs(ctx, owner)
	if !ok {
		return errNoOwner
	}
	fenced, err := fence(owned)
	if err != nil {
		return err
	}
	return s.write(fenced, what, call, rendered)
}

func absent(concept, rowID string) func(context.Context) (context.Context, error) {
	return func(ctx context.Context) (context.Context, error) { return witnessedAbsent(ctx, concept, rowID), nil }
}

func at(concept, rowID string, version time.Time) func(context.Context) (context.Context, error) {
	return func(ctx context.Context) (context.Context, error) { return witnessed(ctx, concept, rowID, version) }
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

// sessionRow is one v1:calls:session. Its ids are BARE; its digest is filtered on and
// never read back.
type sessionRow struct {
	ID, Kind, KeyID, SeatID, RoomID string
	ExpiresAt, EndedAt              time.Time
	Version                         time.Time
}

// usable reports whether the session still opens anything at now.
func (r *sessionRow) usable(now time.Time) bool {
	return r != nil && r.EndedAt.IsZero() && now.Before(r.ExpiresAt)
}

func sessionFromRow(r map[string]any) *sessionRow {
	return &sessionRow{
		ID:        memql.BareShortId(rowString(r, "id")),
		Kind:      rowString(r, "kind"),
		KeyID:     memql.BareShortId(rowString(r, "keyId")),
		SeatID:    memql.BareShortId(rowString(r, "seatId")),
		RoomID:    memql.BareShortId(rowString(r, "roomId")),
		ExpiresAt: rowTime(r, "expiresAt"),
		EndedAt:   rowTime(r, "endedAt"),
		Version:   rowTime(r, "createdAt"),
	}
}

// sessionByDigest finds a session by the digest of its token, on one site. A miss is
// (nil, nil), and that covers a session of another site.
func (s *Store) sessionByDigest(ctx context.Context, owner, siteID, digest string) (*sessionRow, error) {
	call, err := langparser.RenderCall("callsSessionByTokenDigest", map[string]any{"siteId": memql.BareShortId(siteID), "tokenDigest": digest})
	rows, err := s.readAs(ctx, owner, "a session by its digest", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return sessionFromRow(rows[0]), nil
}

func (s *Store) sessionByID(ctx context.Context, owner, sessionID string) (*sessionRow, error) {
	call, err := langparser.RenderCall("callsSessionById", map[string]any{"id": memql.BareShortId(sessionID)})
	rows, err := s.readAs(ctx, owner, "the session", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return sessionFromRow(rows[0]), nil
}

// newSession is what a session's create writes. The digest is all that is left of the
// token by the time it gets here.
type newSession struct {
	ID, SiteID, TokenDigest, Kind, KeyID, SeatID, RoomID string
	ExpiresAt                                            time.Time
}

func (s *Store) createSession(ctx context.Context, owner string, n newSession, versionTime time.Time) error {
	args := map[string]any{
		"sessionId":   n.ID,
		"siteId":      n.SiteID,
		"tokenDigest": n.TokenDigest,
		"kind":        n.Kind,
		"expiresAt":   versionStamp(n.ExpiresAt),
		"versionTime": versionStamp(versionTime),
	}
	// Absent rather than empty: a link's session was drawn on no key, and a session is
	// seated nowhere until it joins.
	for name, value := range map[string]string{"keyId": n.KeyID, "seatId": n.SeatID, "roomId": n.RoomID} {
		if value != "" {
			args[name] = value
		}
	}
	call, err := langparser.RenderCall("createCallsSession", args)
	return s.writeAs(ctx, owner, absent(sessionConcept, n.ID), "the session", call, err)
}

// seatSession records the seat a session holds, against the version it was read at.
func (s *Store) seatSession(ctx context.Context, owner string, prior *sessionRow, seatID, roomID string, versionTime time.Time) error {
	call, err := langparser.RenderCall("updateCallsSession", map[string]any{
		"sessionId":   prior.ID,
		"seatId":      seatID,
		"roomId":      roomID,
		"versionTime": versionStamp(versionTime),
	})
	return s.writeAs(ctx, owner, at(sessionConcept, prior.ID, prior.Version), "the session", call, err)
}

// ---------------------------------------------------------------------------
// keys, by id
// ---------------------------------------------------------------------------

// keyByID reads one key as the site's owner. A miss is (nil, nil).
func (s *Store) keyByID(ctx context.Context, owner, keyID string) (*Key, error) {
	call, err := langparser.RenderCall("callsKeyById", map[string]any{"id": memql.BareShortId(keyID)})
	rows, err := s.readAs(ctx, owner, "the key", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return &Key{
		ID:          memql.BareShortId(rowString(r, "id")),
		SiteID:      memql.BareShortId(rowString(r, "siteId")),
		OwnerUserID: memql.BareShortId(rowString(r, "ownerUserId")),
		AccountID:   memql.BareShortId(rowString(r, "accountId")),
		Label:       rowString(r, "label"),
		Kind:        rowString(r, "kind"),
		Status:      rowString(r, "status"),
		Version:     rowTime(r, "createdAt"),
	}, nil
}

// ---------------------------------------------------------------------------
// rooms
// ---------------------------------------------------------------------------

// roomRow is one v1:calls:room. Its ids are BARE and its digests are never read back.
type roomRow struct {
	ID, SiteID, OwnerUserID, KeyID, MediaServerID, MediaRoom, Status, HostSeatID string
	Locked                                                                       bool
	Window                                                                       callrules.JoinWindow
	Version                                                                      time.Time
}

// open is the concept's rule: an absent status reads as open.
func (r roomRow) open() bool { return r.Status != "ended" }

func roomFromRow(r map[string]any) roomRow {
	return roomRow{
		ID:            memql.BareShortId(rowString(r, "id")),
		SiteID:        memql.BareShortId(rowString(r, "siteId")),
		OwnerUserID:   memql.BareShortId(rowString(r, "ownerUserId")),
		KeyID:         memql.BareShortId(rowString(r, "keyId")),
		MediaServerID: memql.BareShortId(rowString(r, "mediaServerId")),
		MediaRoom:     rowString(r, "mediaRoom"),
		Status:        rowString(r, "status"),
		HostSeatID:    memql.BareShortId(rowString(r, "hostSeatId")),
		Locked:        rowBool(r, "locked"),
		Window:        callrules.JoinWindow{StartedAt: rowTime(r, "joinWindowStartedAt"), Count: rowInt(r, "joinWindowCount")},
		Version:       rowTime(r, "createdAt"),
	}
}

func (s *Store) oneRoom(ctx context.Context, owner, what, call string, rendered error) (*roomRow, error) {
	rows, err := s.readAs(ctx, owner, what, call, rendered)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	room := roomFromRow(rows[0])
	return &room, nil
}

func (s *Store) roomByID(ctx context.Context, owner, roomID string) (*roomRow, error) {
	call, err := langparser.RenderCall("callsRoomById", map[string]any{"id": memql.BareShortId(roomID)})
	return s.oneRoom(ctx, owner, "the room", call, err)
}

// roomByLink finds a room by the digest of one of its links, on one site: the host link
// when host is true, the guest link otherwise. A miss is (nil, nil).
func (s *Store) roomByLink(ctx context.Context, owner, siteID, digest string, host bool) (*roomRow, error) {
	if host {
		call, err := langparser.RenderCall("callsRoomByHostDigest", map[string]any{"siteId": memql.BareShortId(siteID), "hostDigest": digest})
		return s.oneRoom(ctx, owner, "a room by its host link", call, err)
	}
	call, err := langparser.RenderCall("callsRoomByGuestDigest", map[string]any{"siteId": memql.BareShortId(siteID), "guestDigest": digest})
	return s.oneRoom(ctx, owner, "a room by its guest link", call, err)
}

// openRoomsForKey is the open rooms one key opened.
func (s *Store) openRoomsForKey(ctx context.Context, owner, keyID string) ([]roomRow, error) {
	call, err := langparser.RenderCall("callsOpenRoomsForKey", map[string]any{"keyId": memql.BareShortId(keyID)})
	rows, err := s.readAs(ctx, owner, "the key's open rooms", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]roomRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, roomFromRow(r))
	}
	return out, nil
}

// newRoom is what a room's create writes: digests of its links, never the links.
type newRoom struct {
	ID, SiteID, Deployable, KeyID, MediaServerID, MediaRoom, HostSeatID string
	GuestDigest, HostDigest                                             string
	OpenedAt                                                            time.Time
	Window                                                              callrules.JoinWindow
}

func (s *Store) createRoom(ctx context.Context, owner string, n newRoom, versionTime time.Time) error {
	args := map[string]any{
		"roomId":              n.ID,
		"siteId":              n.SiteID,
		"keyId":               n.KeyID,
		"mediaServerId":       n.MediaServerID,
		"mediaRoom":           n.MediaRoom,
		"hostSeatId":          n.HostSeatID,
		"guestDigest":         n.GuestDigest,
		"hostDigest":          n.HostDigest,
		"openedAt":            versionStamp(n.OpenedAt),
		"joinWindowStartedAt": versionStamp(n.Window.StartedAt),
		"joinWindowCount":     n.Window.Count,
		"versionTime":         versionStamp(versionTime),
	}
	if n.Deployable != "" {
		args["deployable"] = n.Deployable
	}
	call, err := langparser.RenderCall("createCallsRoom", args)
	return s.writeAs(ctx, owner, absent(roomConcept, n.ID), "the room", call, err)
}

// countJoin writes a room's join window, against the version it was read at.
func (s *Store) countJoin(ctx context.Context, owner string, prior roomRow, w callrules.JoinWindow, versionTime time.Time) error {
	call, err := langparser.RenderCall("updateCallsRoom", map[string]any{
		"roomId":              prior.ID,
		"joinWindowStartedAt": versionStamp(w.StartedAt),
		"joinWindowCount":     w.Count,
		"versionTime":         versionStamp(versionTime),
	})
	return s.writeAs(ctx, owner, at(roomConcept, prior.ID, prior.Version), "the room's join count", call, err)
}

// ---------------------------------------------------------------------------
// seats
// ---------------------------------------------------------------------------

// seatRow is one v1:calls:seat. Its ids are BARE.
type seatRow struct {
	ID, RoomID, Identity, Kind, DisplayName, Status, TermsVersion string
	Host, ConfirmAdult, ConfirmNoMinors                           bool
	Version                                                       time.Time
}

// occupying reports whether the seat holds a media server's seat.
func (r seatRow) occupying() bool { return r.Status == seatAdmitted || r.Status == seatInCall }

// placed reports whether the seat is in its call or waiting to be: a seat a session may
// not leave behind for another call.
func (r seatRow) placed() bool { return r.occupying() || r.Status == seatWaiting }

func seatFromRow(r map[string]any) seatRow {
	return seatRow{
		ID:              memql.BareShortId(rowString(r, "id")),
		RoomID:          memql.BareShortId(rowString(r, "roomId")),
		Identity:        rowString(r, "identity"),
		Kind:            rowString(r, "kind"),
		DisplayName:     rowString(r, "displayName"),
		Status:          rowString(r, "status"),
		TermsVersion:    rowString(r, "termsVersion"),
		Host:            rowBool(r, "host"),
		ConfirmAdult:    rowBool(r, "confirmAdult"),
		ConfirmNoMinors: rowBool(r, "confirmNoMinors"),
		Version:         rowTime(r, "createdAt"),
	}
}

func (s *Store) seatByID(ctx context.Context, owner, seatID string) (*seatRow, error) {
	call, err := langparser.RenderCall("callsSeatById", map[string]any{"id": memql.BareShortId(seatID)})
	rows, err := s.readAs(ctx, owner, "the seat", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	seat := seatFromRow(rows[0])
	return &seat, nil
}

// newSeat is what a seat's create writes, the three confirmations included.
type newSeat struct {
	ID, SiteID, RoomID, Identity, Kind, DisplayName, Status string
	Host                                                    bool
	Confirm                                                 callrules.Confirmation
	JoinedAt                                                time.Time
}

func (s *Store) createSeat(ctx context.Context, owner string, n newSeat, versionTime time.Time) error {
	args := map[string]any{
		"seatId":          n.ID,
		"siteId":          n.SiteID,
		"roomId":          n.RoomID,
		"identity":        n.Identity,
		"kind":            n.Kind,
		"displayName":     n.DisplayName,
		"host":            n.Host,
		"status":          n.Status,
		"confirmAdult":    n.Confirm.Adult,
		"confirmNoMinors": n.Confirm.NoMinors,
		"termsVersion":    n.Confirm.Terms,
		"versionTime":     versionStamp(versionTime),
	}
	// A seat that waits has not been taken yet.
	if !n.JoinedAt.IsZero() {
		args["joinedAt"] = versionStamp(n.JoinedAt)
	}
	call, err := langparser.RenderCall("createCallsSeat", args)
	return s.writeAs(ctx, owner, absent(seatConcept, n.ID), "the seat", call, err)
}

// updateSeat writes the named fields of a seat, against the version it was read at.
// Times are passed as time.Time and stamped here.
func (s *Store) updateSeat(ctx context.Context, owner string, prior seatRow, fields map[string]any, versionTime time.Time) error {
	args := map[string]any{"seatId": prior.ID, "versionTime": versionStamp(versionTime)}
	for name, value := range fields {
		if stamp, isTime := value.(time.Time); isTime {
			value = versionStamp(stamp)
		}
		args[name] = value
	}
	call, err := langparser.RenderCall("updateCallsSeat", args)
	return s.writeAs(ctx, owner, at(seatConcept, prior.ID, prior.Version), "the seat", call, err)
}

// ---------------------------------------------------------------------------
// queue entries
// ---------------------------------------------------------------------------

// queueEntryRow is one v1:calls:queueEntry. Its ids are BARE.
type queueEntryRow struct {
	ID, RoomID, SeatID, Status string
	Class                      callrules.QueueClass
	EnqueuedAt, LastPollAt     time.Time
	Version                    time.Time
}

// waiting is the concept's rule: an absent status reads as waiting.
func (r queueEntryRow) waiting() bool { return r.Status == "" || r.Status == entryWaiting }

func (r queueEntryRow) rule() callrules.QueueEntry {
	return callrules.QueueEntry{ID: r.ID, Class: r.Class, EnqueuedAt: r.EnqueuedAt, LastPollAt: r.LastPollAt}
}

func queueEntryFromRow(r map[string]any) queueEntryRow {
	return queueEntryRow{
		ID:         memql.BareShortId(rowString(r, "id")),
		RoomID:     memql.BareShortId(rowString(r, "roomId")),
		SeatID:     memql.BareShortId(rowString(r, "seatId")),
		Status:     rowString(r, "status"),
		Class:      queueClassOf(rowString(r, "class")),
		EnqueuedAt: rowTime(r, "enqueuedAt"),
		LastPollAt: rowTime(r, "lastPollAt"),
		Version:    rowTime(r, "createdAt"),
	}
}

// newQueueEntry is what a queue entry's create writes.
type newQueueEntry struct {
	ID, SiteID, RoomID, SeatID, KeyID string
	Class                             callrules.QueueClass
	EnqueuedAt                        time.Time
}

func (s *Store) createQueueEntry(ctx context.Context, owner string, n newQueueEntry, versionTime time.Time) error {
	args := map[string]any{
		"queueEntryId": n.ID,
		"siteId":       n.SiteID,
		"roomId":       n.RoomID,
		"seatId":       n.SeatID,
		"class":        queueClassNames[n.Class],
		// An entry is polled when it is made: the request that made it is the waiter's.
		"enqueuedAt":  versionStamp(n.EnqueuedAt),
		"lastPollAt":  versionStamp(n.EnqueuedAt),
		"versionTime": versionStamp(versionTime),
	}
	if n.KeyID != "" {
		args["keyId"] = n.KeyID
	}
	call, err := langparser.RenderCall("createCallsQueueEntry", args)
	return s.writeAs(ctx, owner, absent(queueEntryConcept, n.ID), "the queue entry", call, err)
}

// updateQueueEntry writes an entry's status, its last poll, or both (a zero time and an
// empty status are left as they are), against the version it was read at.
func (s *Store) updateQueueEntry(ctx context.Context, owner string, prior queueEntryRow, status string, lastPollAt, versionTime time.Time) error {
	args := map[string]any{"queueEntryId": prior.ID, "versionTime": versionStamp(versionTime)}
	if status != "" {
		args["status"] = status
	}
	if !lastPollAt.IsZero() {
		args["lastPollAt"] = versionStamp(lastPollAt)
	}
	call, err := langparser.RenderCall("updateCallsQueueEntry", args)
	return s.writeAs(ctx, owner, at(queueEntryConcept, prior.ID, prior.Version), "the queue entry", call, err)
}

// ---------------------------------------------------------------------------
// a media server's load
// ---------------------------------------------------------------------------

// serverLoad is everything placed on one media server that admission counts: its open
// rooms, whichever deployable each is for, their seats and their queue entries, as one
// moment's reads. The service holds the server's write gate while it reads one it will
// decide from, so no other admission on the server can change it in between.
type serverLoad struct {
	server  MediaServer
	rooms   map[string]roomRow
	seats   []seatRow
	entries []queueEntryRow
}

// loadOn reads a media server's load as the cluster acting: its open rooms, then the
// seats and queue entries of all of them in one read each, so the cost does not grow
// with the number of calls on the server.
func (s *Store) loadOn(ctx context.Context, server MediaServer) (serverLoad, error) {
	load := serverLoad{server: server, rooms: map[string]roomRow{}}
	cluster, err := clusterRowsAs(ctx)
	if err != nil {
		return load, err
	}
	call, err := langparser.RenderCall("callsOpenRoomsOnServer", map[string]any{"mediaServerId": server.ID})
	rows, err := s.read(cluster, "the open rooms of the media server", call, err)
	if err != nil {
		return load, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		room := roomFromRow(r)
		// The filter asked for open rooms of this server; a row that says otherwise is
		// not counted rather than trusted.
		if room.ID == "" || !room.open() || room.MediaServerID != server.ID {
			continue
		}
		load.rooms[room.ID] = room
		ids = append(ids, room.ID)
	}
	// AN EMPTY LIST IS NOT ASKED: a membership term with no values cannot narrow the
	// read to an index, and the engine would enumerate every seat of the concept to
	// answer "none".
	if len(ids) == 0 {
		return load, nil
	}
	call, err = langparser.RenderCall("callsSeatsForRooms", map[string]any{"roomIds": ids})
	rows, err = s.read(cluster, "the seats of the media server's rooms", call, err)
	if err != nil {
		return load, err
	}
	for _, r := range rows {
		if seat := seatFromRow(r); load.has(seat.RoomID) {
			load.seats = append(load.seats, seat)
		}
	}
	call, err = langparser.RenderCall("callsQueueEntriesForRooms", map[string]any{"roomIds": ids})
	rows, err = s.read(cluster, "the queue of the media server", call, err)
	if err != nil {
		return load, err
	}
	for _, r := range rows {
		if entry := queueEntryFromRow(r); load.has(entry.RoomID) {
			load.entries = append(load.entries, entry)
		}
	}
	return load, nil
}

func (l serverLoad) has(roomID string) bool {
	_, ok := l.rooms[roomID]
	return ok
}

// free is the server's seats less every seat its open rooms occupy. It can go below
// zero (a server's seats lowered under a running call); a live call is never cut, and
// nobody new is admitted until it is back above.
func (l serverLoad) free() int {
	occupied := 0
	for _, seat := range l.seats {
		if seat.occupying() {
			occupied++
		}
	}
	return l.server.Seats - occupied
}

// roomState is one room as DecideJoin sees it: the people and agents in it now.
func (l serverLoad) roomState(room roomRow) callrules.RoomState {
	state := callrules.RoomState{Ended: !room.open(), Locked: room.Locked}
	for _, seat := range l.seats {
		if seat.RoomID != room.ID || !seat.occupying() {
			continue
		}
		switch callrules.SeatKind(seat.Kind) {
		case callrules.KindPerson:
			state.People++
		case callrules.KindAgent:
			state.Agents++
		}
	}
	return state
}

// queue splits the server's waiting entries into the live ones, in drain order, and
// those past their lifetime. An entry's lifetime is its OWN deployable's policy
// (ttlFor), since every call on the server shares the one queue.
func (l serverLoad) queue(now time.Time, ttlFor func(roomRow) time.Duration) (live []callrules.QueueEntry, expired []queueEntryRow) {
	byRoom := map[string][]callrules.QueueEntry{}
	rows := map[string]queueEntryRow{}
	for _, entry := range l.entries {
		if entry.waiting() {
			byRoom[entry.RoomID] = append(byRoom[entry.RoomID], entry.rule())
			rows[entry.ID] = entry
		}
	}
	for roomID, entries := range byRoom {
		gone := map[string]bool{}
		for _, entryID := range callrules.ExpiredEntries(entries, now, ttlFor(l.rooms[roomID])) {
			gone[entryID] = true
			expired = append(expired, rows[entryID])
		}
		for _, entry := range entries {
			if !gone[entry.ID] {
				live = append(live, entry)
			}
		}
	}
	return callrules.OrderQueue(live), expired
}

// waitingEntryOf is a seat's waiting queue entry, or nil. A seat has at most one: an
// entry is made only for a seat that has none (service.go).
func (l serverLoad) waitingEntryOf(seatID string) *queueEntryRow {
	for i := range l.entries {
		if l.entries[i].SeatID == seatID && l.entries[i].waiting() {
			return &l.entries[i]
		}
	}
	return nil
}

// arrival is when a new queue entry arrives: now, or just after the newest live entry
// when the clock says otherwise. Entries are made one at a time under the media
// server's gate, so this is the order they were made in whatever each replica's clock
// says. A tie would not do: calls.OrderQueue orders equal arrivals by id, which is
// random, and could put a newcomer ahead of an entry made before it.
func arrival(live []callrules.QueueEntry, now time.Time) time.Time {
	newest := time.Time{}
	for _, entry := range live {
		if entry.EnqueuedAt.After(newest) {
			newest = entry.EnqueuedAt
		}
	}
	return memql.VersionTimeAfter(newest, now)
}

// placeInLine is where entry stands among the live entries: its zero-based position,
// counting whoever drains before it. An entry not yet in the queue is placed as if it
// were.
func placeInLine(live []callrules.QueueEntry, entry callrules.QueueEntry) int {
	if position, ok := callrules.Position(live, entry.ID); ok {
		return position
	}
	position, _ := callrules.Position(callrules.OrderQueue(append(append([]callrules.QueueEntry(nil), live...), entry)), entry.ID)
	return position
}

// queueEntriesForRoom is every queue entry of one room, as the site's owner.
func (s *Store) queueEntriesForRoom(ctx context.Context, owner, roomID string) ([]queueEntryRow, error) {
	call, err := langparser.RenderCall("callsQueueEntriesForRoom", map[string]any{"roomId": memql.BareShortId(roomID)})
	rows, err := s.readAs(ctx, owner, "the room's queue entries", call, err)
	if err != nil {
		return nil, err
	}
	out := make([]queueEntryRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, queueEntryFromRow(r))
	}
	return out, nil
}

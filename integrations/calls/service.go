package calls

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/email"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// service.go -- the calls service: a key holder unlocks a session, starts a call and is
// seated as its host; people join by link, wait behind a lock or in the media server's
// queue, poll, and leave (design sections 7.7, 8 and 10). The rules are the pure
// module's (component/calls); this file reads the state they decide from, under the
// locks that make that state one replica's at a time, and writes what they decided. A
// host's controls and how a call ends are host.go's, booked calls schedule.go's, the
// media server's webhooks events.go's, and the pass that repairs every call against its
// media server reconcile.go's.
//
// # Bearer secrets, and what a stranger costs
//
// A key, a session token and a call's two links are bearer secrets. Each is stored
// only as calls.Digest under the keys' pepper and looked up within one deployable, so
// a secret of one deployable opens nothing at another. Original random secrets are
// handed out once; an active host can recover a separate, deterministic guest-only
// alias (invitation_link.go). No plain secret is stored or logged. A presented secret's SHAPE is checked before
// anything is read (a key must parse, calls.ParseKey; a token or a link must be exactly
// what NewToken makes of calls.TokenBytes, calls.TokenWellFormed), so a guess of the wrong
// shape costs no read. The calls route family asks the same two rules before it reads the
// site, so the shape a route refuses unread and the shape this service refuses are one
// rule.
//
// A SESSION IS READ AGAIN ON EVERY CALL. Start, Queue and Leave take the Session a
// caller holds and trust only its token and its deployable: the row the token names
// is read, and an ended or expired one is ErrNoSession, so a forged Session value
// opens nothing its token does not.
//
// # Multi-node: whose lock guards what
//
// Every replica runs this service over one database, so a decision is made under
// cross-replica write gates (memql.AcquireWriteGates, advisory locks), from reads
// answered by the database (memql.ContextWithFreshRead), and written at a version after
// the one it read (memql.VersionTimeAfter), fenced on that version (rooms.go). Each of
// the three alone was measured insufficient (memql#5431).
//
//   - "calls.room.<id>" serializes one call: its seats, its join count, its lock.
//   - "calls.server.<id>" serializes one media server: its seats are shared by every
//     call placed on it, whichever deployable each is for, and the queue for them is
//     the server's. Two calls have two room gates, so without this one two joins to
//     two calls could each take the server's last seat.
//   - "calls.key.<id>" serializes one key's calls and bookings: Start takes it so it opens
//     no call while any room of the key is open, and booking or moving a call takes it
//     because the overlap check spans every booking of the key. Where one call per key
//     holds, and where it does not, is schedule.go's file note.
//   - "calls.scheduled.<id>" serializes one booking: its moves, its cancel and the join
//     that opens its room, so two first joins on two replicas open one room, and the end
//     of that room, which marks the booking ended: a decision that may end a call -- a
//     leave, a host's end, reconciliation -- takes the booking's gate before the room's
//     (host.go endGates).
//
// Every write that takes or frees a server seat or changes a queue entry holds the
// room's gate and the server's together; so does every host control that writes, since
// a lock or an admission changes who stands in the server's queue. The ORDER IS FIXED --
// key, then booking, then room, then server -- and every caller takes its gates in one
// call in that order, which rules out two callers each holding a lock the other waits
// for. A room's server never changes after placement, so it is read before the gates are
// taken and the room is read again inside them.
//
// THE GATES TAKE A BOUNDED SHARE OF THE PROCESS'S GATE POOL (gates): at
// most callsGateShare calls decisions hold a write-gate connection at once in one process,
// and the rest wait in the process for a slot, so a burst of decisions behind one server's
// lock never holds the connections every other feature's write gates in the process need.
//
// # Capacity and the queue
//
// A server's free seats are its seats less the admitted and in-call seats of its open
// rooms (serverLoad.free). A new ask is handed DecideJoin's capacity as those free seats
// less the live waiters who drain before it (calls.OrderQueue: class, then arrival), so
// nobody jumps the line. An ask's class is its CALL'S STATE (serverLoad.classFor):
// joining a call somebody holds a seat in is ClassRunningCall, and otherwise it is the
// call's own class -- ClassScheduledDue for a booked call, ClassNewCall for an ad hoc
// one, a new call's host included -- so a call's guests never drain ahead of its own
// queued host. A waiter is promoted only on its OWN poll (Queue), when its
// place in line is inside the free seats, because only its poll's reply can carry its
// pass; Leave frees a seat and promotes nobody. A waiter that has not polled for its
// deployable's QueueEntryTTL is not counted, and its entry is marked expired when a
// request of its own call next holds the gates, or by reconciliation's pass, whichever
// comes first. A waiter in a locked call that does not pass the lock (no host seat, and
// not admitted by a host: seatRow.passesLock) is HELD: it keeps its entry and its arrival
// while it polls, but counts in nobody's place until its call is unlocked or a host
// admits it, so a lock never keeps a free seat from the rest of the line
// (serverLoad.queue).
//
// # Join attempts
//
// A room counts its join attempts on its own row (calls.CountJoinAttempt), under its
// gate, so every replica counts the same attempts. Every join that passes validation
// counts, a rejoin and a refused one included, and the count is checked before
// DecideJoin; the attempt past the limit is refused with the time until the window
// ends, and writes nothing. A host's Start is the room's first.
//
// # What survives a failure part-way
//
// Writes run in an order chosen so a failure part-way leaves nothing that blocks a
// retry. Opening a call -- Start, or a booked call's first join -- opens the media room
// before any row (placeRoom) and writes the room row LAST (openRoom, the one place a call's
// room is opened): a room row whose seat was never written would be an open call with
// nobody in it, and Start refuses its key while any room of the key is open. A join writes
// the room's count first (the attempt counts whatever follows), then the session, then the
// seat: a session pointing at a seat that was never written is seated nowhere, which the
// next join reads as free. Ending a call writes its seats, its queue entries and its
// booking before the room (host.go endRows), so an end cut short leaves an open call that
// the next attempt ends again.
//
// A MEDIA SERVER CAN LOSE A ROOM THE ROWS SAY IS OPEN -- its empty-room timeout, a
// restart -- so every pass into an open call is handed out only after the call's media
// room is opened again (ensureMediaRoom), after the gates. A failure there hands out no
// pass and leaves the admission written, and the seat's next ask opens the room again.

// tokenBytes is the randomness in a session token and in each link (calls.TokenBytes:
// 160 bits, spelled as 32 symbols by calls.NewToken).
const tokenBytes = callrules.TokenBytes

// identityBytes is the randomness in a seat's media identity. An identity is a
// participant's name on the media server, which every participant sees, so it is
// random rather than the seat's row id, and never the display name: clients label a
// participant by its kind and identity alone.
const identityBytes = callrules.MediaIdentityBytes

// The bounds on what a person types at a join: a name other participants see and the
// version of the terms they accept.
const (
	maxDisplayNameRunes  = 64
	maxTermsVersionRunes = 64
)

// Service is the calls service. New builds it beside the integration's operator acts,
// over the same store, gate pool, pepper, clock and randomness.
type Service struct {
	store            *Store
	gateDB           func() *sql.DB
	pepper           func(context.Context) ([]byte, error)
	random           io.Reader
	now              func() time.Time
	media            func(MediaServer, MediaCredential) (MediaBackend, error)
	logger           *slog.Logger
	invitationSender func() email.Sender
}

// ---------------------------------------------------------------------------
// unlock and sessions
// ---------------------------------------------------------------------------

// Unlock opens a host session with a key: the key must parse, be one of this
// deployable's, and be active. The session lives a day (calls.SessionLifetime) and is
// seated nowhere until it starts or joins a call.
func (s *Service) Unlock(ctx context.Context, site callrules.SiteRef, key string) (callrules.Session, error) {
	site, err := checkSite(site)
	if err != nil {
		return callrules.Session{}, err
	}
	raw, err := callrules.ParseKey(key)
	if err != nil {
		// ParseKey names the rule a spelling broke, never the key.
		return callrules.Session{}, fmt.Errorf("%w: %s", callrules.ErrInvalid, err.Error())
	}
	pepper, err := s.pepperOr(ctx, callrules.ErrNotFound)
	if err != nil {
		return callrules.Session{}, err
	}
	found, err := s.store.KeyByDigest(ctx, site.OwnerUserID, site.SiteID, callrules.Digest(pepper, raw[:]))
	if err != nil {
		return callrules.Session{}, err
	}
	if found == nil {
		return callrules.Session{}, fmt.Errorf("%w: no key of this deployable matches", callrules.ErrNotFound)
	}
	if found.UserHandle != "" || found.Kind == "guest" {
		return callrules.Session{}, callrules.Refuse(callrules.ErrForbidden, "passkey_required", "this key requires a passkey")
	}
	if !keyActive(found.Status) {
		// The holder proved the key, so saying why it opens nothing tells them nothing
		// they could not learn by holding it.
		return callrules.Session{}, fmt.Errorf("%w: the key is %s", callrules.ErrForbidden, found.Status)
	}
	token, err := callrules.NewToken(s.random, tokenBytes)
	if err != nil {
		return callrules.Session{}, err
	}
	now := s.now()
	row := newSession{
		ID: id.NewShortId(), SiteID: site.SiteID, TokenDigest: callrules.Digest(pepper, []byte(token)),
		Kind: callrules.SessionHost, KeyID: found.ID, ExpiresAt: now.Add(callrules.SessionLifetime),
	}
	if err := s.store.createSession(ctx, site.OwnerUserID, row, now); err != nil {
		return callrules.Session{}, err
	}
	return callrules.Session{Token: token, Kind: row.Kind, KeyID: row.KeyID, ExpiresAt: row.ExpiresAt, Site: site}, nil
}

// Session resolves a session token on one deployable. An unknown, ended or expired
// session, or a token of the wrong shape, is ErrNoSession.
func (s *Service) Session(ctx context.Context, site callrules.SiteRef, token string) (callrules.Session, error) {
	site, err := checkSite(site)
	if err != nil {
		return callrules.Session{}, fmt.Errorf("%w: %v", callrules.ErrNoSession, err)
	}
	row, _, err := s.session(ctx, site, token, s.now())
	if err != nil {
		return callrules.Session{}, err
	}
	return sessionOf(row, strings.TrimSpace(token), site), nil
}

// session reads the stored session a presented token names, refusing one of the wrong
// shape before anything is read, and an unknown, ended or expired one after.
func (s *Service) session(ctx context.Context, site callrules.SiteRef, token string, now time.Time) (*sessionRow, []byte, error) {
	token = strings.TrimSpace(token)
	if !callrules.TokenWellFormed(token) {
		return nil, nil, fmt.Errorf("%w: no session token of that shape is ever issued", callrules.ErrNoSession)
	}
	pepper, err := s.pepperOr(ctx, callrules.ErrNoSession)
	if err != nil {
		return nil, nil, err
	}
	row, err := s.store.sessionByDigest(ctx, site.OwnerUserID, site.SiteID, callrules.Digest(pepper, []byte(token)))
	if err != nil {
		return nil, nil, err
	}
	if !row.usable(now) {
		return nil, nil, fmt.Errorf("%w: the session is unknown, ended or expired", callrules.ErrNoSession)
	}
	return row, pepper, nil
}

func sessionOf(row *sessionRow, token string, site callrules.SiteRef) callrules.Session {
	return callrules.Session{
		Token: token, Kind: row.Kind, KeyID: row.KeyID, SeatID: row.SeatID, RoomID: row.RoomID,
		ExpiresAt: row.ExpiresAt, Site: site,
	}
}

// ---------------------------------------------------------------------------
// start
// ---------------------------------------------------------------------------

// Start opens a call with a key holder's session and seats its holder as the call's
// host: on the active media server with the most free seats, in a media room named by
// the room's short id with a place for every person, every agent and the sampler. The
// host is admitted or, when the server is full, queued behind everyone joining a call
// already running. Start opens no call while any room of the key is open, booked or not.
// It does not look at the key's bookings, though, and a booked call's room opens whatever
// else the key has open, so a key can still have two rooms open at once (schedule.go's
// file note, "One call per key").
func (s *Service) Start(ctx context.Context, sess callrules.Session, body callrules.StartBody) (result callrules.StartResult, resultErr error) {
	defer func() {
		if resultErr == nil && result.Call.ID != "" {
			s.observeSessionHistory(ctx, sess)
		}
	}()
	title, err := checkTitle(body.Title)
	if err != nil {
		return callrules.StartResult{}, err
	}
	name, confirm, err := checkJoiner(body.Name, body.Confirm)
	if err != nil {
		return callrules.StartResult{}, err
	}
	h, err := s.keyHolderOf(ctx, sess)
	if err != nil {
		return callrules.StartResult{}, err
	}
	policy, err := s.store.PolicyFor(ctx, h.site.OwnerUserID, h.site.SiteID)
	if err != nil {
		return callrules.StartResult{}, err
	}
	// The gated phase asks all of this again. Asking first, without the gates, is what
	// keeps a refused Start from taking a lock or opening a media room.
	if refused, err := s.openRefusal(ctx, h.site, h.row.KeyID, h.row, true); err != nil {
		return callrules.StartResult{Join: refused}, err
	}
	placed, refused, err := s.placeRoom(ctx, policy)
	if err != nil {
		return callrules.StartResult{Join: refused}, err
	}
	links, err := s.mintLinks()
	if err != nil {
		return callrules.StartResult{}, err
	}
	links.ID = placed.roomID
	identity, err := callrules.NewToken(s.random, identityBytes)
	if err != nil {
		return callrules.StartResult{}, err
	}
	ask := joinAsk{name: name, confirm: confirm, asHost: true, pepper: h.pepper, token: strings.TrimSpace(sess.Token)}
	host, seat, err := s.startGated(ctx, h, policy, placed.server, ask, roomOpening{
		roomID: placed.roomID, identity: identity, keyID: h.row.KeyID, host: true, class: callrules.ClassNewCall, title: title,
		guestDigest: callrules.Digest(h.pepper, []byte(links.GuestToken)), hostDigest: callrules.Digest(h.pepper, []byte(links.HostToken)),
	})
	if err != nil || host.Outcome == callrules.OutcomeRefuse {
		return callrules.StartResult{Join: host}, err
	}
	if host.Outcome == callrules.OutcomeAdmit {
		if host, err = admitted(placed.server, placed.credential, placed.roomID, seat, policy); err != nil {
			return callrules.StartResult{}, err
		}
	}
	return callrules.StartResult{Call: links, Join: host}, nil
}

// startGated is Start's own checks under the key's, the new room's and the server's
// gates -- the holder's session still opens anything, and openRefusal again, the key's
// open rooms included -- and then the opening every call shares (openRoom). It answers
// the host's admission without a pass (the caller mints it once the gates are released)
// and the host's seat.
func (s *Service) startGated(ctx context.Context, h keyHolder, policy callrules.Policy, server MediaServer, ask joinAsk, o roomOpening) (callrules.JoinResult, seatRow, error) {
	release, err := s.gates(ctx, keyGate(h.row.KeyID), roomGate(o.roomID), serverGate(server.ID))
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	current, err := s.store.sessionByID(fresh, h.site.OwnerUserID, h.row.ID)
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	if !current.usable(now) {
		return callrules.JoinResult{}, seatRow{}, fmt.Errorf("%w: the session ended", callrules.ErrNoSession)
	}
	if refused, err := s.openRefusal(fresh, h.site, current.KeyID, current, true); err != nil {
		return refused, seatRow{}, err
	}
	result, _, seat, err := s.openRoom(fresh, h.site, server, policy, current, ask, o, callrules.Session{}, nil, now)
	return result, seat, err
}

// ---------------------------------------------------------------------------
// opening a call's room, which Start and a booked call's first join share
// ---------------------------------------------------------------------------

// keyHolder is a session a key opened, on its deployable, with the pepper its call's links
// are digested under: who starts a call, and who books, moves or cancels one.
type keyHolder struct {
	site   callrules.SiteRef
	row    *sessionRow
	pepper []byte
}

// keyHolderOf resolves the session presented to start, book, move or cancel a call. Only
// its token and its deployable are trusted (the file note); the key's state is asked
// under the gates (openRefusal, heldKey).
func (s *Service) keyHolderOf(ctx context.Context, sess callrules.Session) (keyHolder, error) {
	site, err := checkSite(sess.Site)
	if err != nil {
		return keyHolder{}, fmt.Errorf("%w: %v", callrules.ErrNoSession, err)
	}
	row, pepper, err := s.session(ctx, site, sess.Token, s.now())
	if err != nil {
		return keyHolder{}, err
	}
	// A host-kind session and persistent key are necessary, not sufficient:
	// openRefusal also checks the key's hosting entitlement. A host invitation's
	// unfunded guest key does not acquire that entitlement from its seat authority.
	if row.Kind != callrules.SessionHost || row.KeyID == "" {
		return keyHolder{}, fmt.Errorf("%w: only a session opened with a key starts, books, moves or cancels a call", callrules.ErrForbidden)
	}
	return keyHolder{site: site, row: row, pepper: pepper}, nil
}

// openRefusal is what refuses opening a call's room before anything is placed, asked
// again under the gates: a key that is not the deployable's or not active (a frozen or
// banned key opens no new call, booked or not), and a session seated in a call still
// running (one browser is in one call at a time). With oneCallPerKey -- Start -- a key
// with a call open is refused too, and the key's state comes first, so a holder starting
// again in their own open call hears that the key is in a call. A booked call's first
// join does not ask it, because a back-to-back booking must be able to open while the
// previous call runs (schedule.go's file note).
func (s *Service) openRefusal(ctx context.Context, site callrules.SiteRef, keyID string, held *sessionRow, oneCallPerKey bool) (callrules.JoinResult, error) {
	opens, err := s.keyOpensCalls(ctx, site, keyID)
	if err != nil {
		return callrules.JoinResult{}, err
	}
	if !opens {
		return refuseJoin(callrules.ReasonKeyNotActive), fmt.Errorf("%w: the key opens no new call", callrules.ErrForbidden)
	}
	if oneCallPerKey {
		open, err := s.store.openRoomsForKey(ctx, site.OwnerUserID, keyID)
		if err != nil {
			return callrules.JoinResult{}, err
		}
		if len(open) > 0 {
			return refuseJoin(callrules.ReasonKeyInCall), fmt.Errorf("%w: the key already has a call open", callrules.ErrConflict)
		}
	}
	if held == nil {
		return callrules.JoinResult{}, nil
	}
	busy, err := s.seatedElsewhere(ctx, site.OwnerUserID, held, "")
	if err != nil {
		return callrules.JoinResult{}, err
	}
	if busy {
		return refuseJoin(callrules.ReasonInAnotherCall), fmt.Errorf("%w: the session is in another call", callrules.ErrConflict)
	}
	return callrules.JoinResult{}, nil
}

// keyOpensCalls reports whether a key is the deployable's and active: the one rule every
// act on a key's account asks, since a frozen or banned key starts, opens, books, moves
// and cancels no call.
func (s *Service) keyOpensCalls(ctx context.Context, site callrules.SiteRef, keyID string) (bool, error) {
	key, err := s.store.keyByID(ctx, site.OwnerUserID, keyID)
	if err != nil {
		return false, err
	}
	return key != nil && key.SiteID == site.SiteID && keyActive(key.Status) && key.Kind != "guest", nil
}

// mintLinks is a call's two link secrets, guest and host, each handed out once and stored
// only as its digest: a started call's (Start) and a booked call's (Schedule, and the
// Reschedule that replaces them).
func (s *Service) mintLinks() (callrules.CallLinks, error) {
	var links callrules.CallLinks
	var err error
	if links.GuestToken, err = callrules.NewToken(s.random, tokenBytes); err != nil {
		return links, err
	}
	if links.HostToken, err = callrules.NewToken(s.random, tokenBytes); err != nil {
		return links, err
	}
	return links, nil
}

// placedRoom is where a new call is placed: its media server, the server's credential,
// and the room's id, which names its media room too.
type placedRoom struct {
	server     MediaServer
	credential MediaCredential
	roomID     string
}

// placeRoom places a new call on the active media server with the most free seats and
// opens its media room there, named by the new room's id, with a place for every person,
// every agent and the sampler. No server taking new calls is a refusal, answered beside
// its error.
//
// THE MEDIA ROOM BEFORE ANY ROW, and outside the gates. Before any row so a room that could
// not be opened leaves nothing behind; outside the gates because the server's gate
// serializes every admission on the server, and a network call under it would hold all of
// them up. A gated check that refuses after this leaves an empty media room, which the
// server closes on its own.
func (s *Service) placeRoom(ctx context.Context, policy callrules.Policy) (placedRoom, callrules.JoinResult, error) {
	server, err := s.place(ctx)
	if err != nil {
		return placedRoom{}, callrules.JoinResult{}, err
	}
	if server == nil {
		return placedRoom{}, refuseJoin(callrules.ReasonNoMediaServer), fmt.Errorf("%w: no media server is taking new calls", callrules.ErrConflict)
	}
	credential, err := s.credential(ctx, *server)
	if err != nil {
		return placedRoom{}, callrules.JoinResult{}, err
	}
	backend, err := s.media(*server, credential)
	if err != nil {
		return placedRoom{}, callrules.JoinResult{}, fmt.Errorf("calls: reach media server %s: %w", server.ID, err)
	}
	placed := placedRoom{server: *server, credential: credential, roomID: id.NewShortId()}
	if err := backend.CreateRoom(ctx, placed.roomID, mediaRoomLimit(policy)); err != nil {
		return placedRoom{}, callrules.JoinResult{}, fmt.Errorf("calls: open the call's media room: %w", err)
	}
	return placed, callrules.JoinResult{}, nil
}

// roomOpening is a new call's room as the opening decides it: everything Start and a booked
// call's first join decide differently, and nothing else.
type roomOpening struct {
	roomID, identity        string               // the room, which names its media room; the opener's media identity
	keyID                   string               // the key the call runs on
	host, locked            bool                 // the opener takes a host seat; the room opens locked
	class                   callrules.QueueClass // the opener's place in the server's queue when it is full
	guestDigest, hostDigest string               // the room's links
	scheduledCallID         string               // the booking a booked call's room is opened for
	title                   string
}

// openRoom decides a new call's first seat and writes the call. The caller holds the new
// room's gate and its server's, behind its own first gate (a key's or a booking's), has
// read the opener's session fresh under them (held, nil for a joiner who brought none) and
// has asked what refuses its kind of opening. This is what every opening asks alike:
//
//   - the server again, since a drain between placement and the gate takes it out of
//     placement (the opener asks again and is placed elsewhere);
//   - the server's load, read under its gate, and the opener's place in its queue, in the
//     opening's class;
//   - the opener's seat as the room's first join attempt (calls.CountJoinAttempt);
//   - DecideJoin over an empty room, locked or not.
//
// THE WRITES RUN session, seat, queue entry, then whatever the caller writes before the
// room (beforeRoom: a booked call marks its booking started), and the ROOM LAST: a room
// row whose seat was never written would be an open call with nobody in it, refusing its
// key's Start while it stayed open.
// It answers the opener's admission without a pass (minted once the gates are released), a
// wait or a place in the queue, the session the opener now holds, and their seat.
func (s *Service) openRoom(ctx context.Context, site callrules.SiteRef, server MediaServer, policy callrules.Policy, held *sessionRow, ask joinAsk, o roomOpening, out callrules.Session, beforeRoom func(context.Context, time.Time) error, now time.Time) (callrules.JoinResult, callrules.Session, seatRow, error) {
	owner := site.OwnerUserID
	placed, err := s.store.MediaServerByID(ctx, server.ID)
	if err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	if placed == nil || placed.State != "active" {
		return refuseJoin(callrules.ReasonNoMediaServer), out, seatRow{}, fmt.Errorf("%w: the media server stopped taking new calls", callrules.ErrConflict)
	}
	load, err := s.store.loadOn(ctx, *placed)
	if err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	ttls, err := s.queueLifetimes(ctx, site, policy, load)
	if err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	live, heldBack, _ := load.queue(now, ttls)
	window, ok, retry := callrules.CountJoinAttempt(callrules.JoinWindow{}, now, policy.RoomJoinsPerMinute, callrules.JoinWindowLength)
	if !ok {
		return rateRefusal(retry), out, seatRow{}, fmt.Errorf("%w: the room takes no join attempts", callrules.ErrConflict)
	}
	arrives := arrival(now, live, heldBack)
	entry := callrules.QueueEntry{ID: id.NewShortId(), Class: o.class, EnqueuedAt: arrives, LastPollAt: arrives}
	position := placeInLine(live, entry)
	decision := callrules.DecideJoin(callrules.RoomState{Locked: o.locked}, callrules.JoinAsk{Kind: callrules.KindPerson, Host: o.host}, policy, load.free()-position)
	if decision.Outcome == callrules.OutcomeRefuse {
		// An empty room refuses its opener only under a policy with no people, which the
		// cluster's bounds forbid; refused rather than seated if it ever does.
		return refuseJoin(decision.Reason), out, seatRow{}, fmt.Errorf("%w: %s", callrules.ErrConflict, decision.Reason)
	}

	seat := seatRow{
		ID: id.NewShortId(), RoomID: o.roomID, Identity: o.identity, Kind: string(callrules.KindPerson),
		DisplayName: ask.name, Status: seatWaiting, Host: o.host, KeyID: held.keyHeld(), CredentialID: held.credentialHeld(),
	}
	joinedAt := time.Time{}
	if decision.Outcome == callrules.OutcomeAdmit {
		seat.Status, joinedAt = seatAdmitted, now
	}
	out, refused, err := s.seatJoiner(ctx, site, held, ask, out, seat.ID, o.roomID, now)
	if err != nil {
		return refused, out, seatRow{}, err
	}
	seat.KeyID = out.KeyID
	if err := s.store.createSeat(ctx, owner, newSeat{
		ID: seat.ID, SiteID: site.SiteID, RoomID: o.roomID, Identity: seat.Identity, Kind: seat.Kind,
		DisplayName: seat.DisplayName, Status: seat.Status, Host: seat.Host, Confirm: ask.confirm, JoinedAt: joinedAt,
		KeyID: seat.KeyID, CredentialID: seat.CredentialID,
	}, now); err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	if decision.Outcome == callrules.OutcomeQueue {
		if err := s.store.createQueueEntry(ctx, owner, newQueueEntry{
			ID: entry.ID, SiteID: site.SiteID, RoomID: o.roomID, SeatID: seat.ID, KeyID: o.keyID,
			Class: entry.Class, EnqueuedAt: entry.EnqueuedAt,
		}, now); err != nil {
			return callrules.JoinResult{}, out, seatRow{}, err
		}
	}
	if beforeRoom != nil {
		if err := beforeRoom(ctx, now); err != nil {
			return callrules.JoinResult{}, out, seatRow{}, err
		}
	}
	hostSeat := ""
	if seat.Host {
		hostSeat = seat.ID
	}
	if err := s.store.createRoom(ctx, owner, newRoom{
		ID: o.roomID, SiteID: site.SiteID, Deployable: site.Deployable, KeyID: o.keyID,
		MediaServerID: server.ID, MediaRoom: o.roomID, HostSeatID: hostSeat,
		GuestDigest: o.guestDigest, HostDigest: o.hostDigest, ScheduledCallID: o.scheduledCallID, Locked: o.locked,
		OpenedAt: now, Window: window, Title: callTitle(o.title, now),
	}, now); err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	switch decision.Outcome {
	case callrules.OutcomeQueue:
		return queued(decision.Reason, position, policy), out, seat, nil
	case callrules.OutcomeWait:
		return waited(decision.Reason, policy), out, seat, nil
	}
	return callrules.JoinResult{Outcome: callrules.OutcomeAdmit}, out, seat, nil
}

// place is the active media server with the most free seats, ties broken by id. Nil is
// no server taking new calls. The loads it reads are a hint, taken without the gates:
// the chosen server's load is read again under its gate before anything is decided.
func (s *Service) place(ctx context.Context) (*MediaServer, error) {
	servers, err := s.store.ActiveMediaServers(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(servers, func(a, b int) bool { return servers[a].ID < servers[b].ID })
	var best *MediaServer
	bestFree := 0
	for i := range servers {
		load, err := s.store.loadOn(ctx, servers[i])
		if err != nil {
			return nil, err
		}
		// Strictly more, so of two servers with the same free seats the first by id wins.
		if free := load.free(); best == nil || free > bestFree {
			best, bestFree = &servers[i], free
		}
	}
	return best, nil
}

// ---------------------------------------------------------------------------
// join
// ---------------------------------------------------------------------------

// Join seats a person in a call: by its guest link, by its host link (a host seat), or,
// with neither, in the call the presented session is seated in. A booked call's link
// that finds no room is answered from the booking, and its first join inside the
// booking's window opens the room (schedule.go). A session already seated in the call
// keeps its seat and its media identity; a person with no usable session gets one of
// their own, which the result carries so a route can set it.
//
// The answer is an admission with a pass, a wait behind a lock, a place in the media
// server's queue, or a refusal with its reason. A refusal returns its JoinResult with
// ErrEnded or ErrConflict; a wait and a queue are not errors.
func (s *Service) Join(ctx context.Context, site callrules.SiteRef, sess *callrules.Session, body callrules.JoinBody) (result callrules.JoinResult, session callrules.Session, resultErr error) {
	defer func() {
		if resultErr == nil && session.KeyID != "" {
			s.observeSessionHistory(ctx, session)
		}
	}()
	none := callrules.Session{}
	name, confirm, err := checkJoiner(body.Name, body.Confirm)
	if err != nil {
		return callrules.JoinResult{}, none, err
	}
	guestLink, hostLink := strings.TrimSpace(body.Token), strings.TrimSpace(body.HostToken)
	if guestLink != "" && hostLink != "" {
		return callrules.JoinResult{}, none, fmt.Errorf("%w: a join presents one link, not both", callrules.ErrInvalid)
	}
	link, asHost := guestLink, false
	if hostLink != "" {
		link, asHost = hostLink, true
	}
	presented := sess != nil && strings.TrimSpace(sess.Token) != ""
	if link == "" && !presented {
		return callrules.JoinResult{}, none, fmt.Errorf("%w: a join names its call, with a link or a session seated in it", callrules.ErrInvalid)
	}
	if link != "" && !callrules.TokenWellFormed(link) {
		return callrules.JoinResult{}, none, fmt.Errorf("%w: no link of that shape is ever issued", callrules.ErrNotFound)
	}
	site, err = checkSite(site)
	if err != nil {
		return callrules.JoinResult{}, none, err
	}
	miss := callrules.ErrNotFound
	if link == "" {
		miss = callrules.ErrNoSession
	}
	pepper, err := s.pepperOr(ctx, miss)
	if err != nil {
		return callrules.JoinResult{}, none, err
	}
	now := s.now()
	owner := site.OwnerUserID

	// The presented session, when it still opens anything. One that does not is no
	// session at all: with a link the join is a first join and makes a new one.
	var held *sessionRow
	if presented {
		token := strings.TrimSpace(sess.Token)
		if callrules.TokenWellFormed(token) {
			found, err := s.store.sessionByDigest(ctx, owner, site.SiteID, callrules.Digest(pepper, []byte(token)))
			if err != nil {
				return callrules.JoinResult{}, none, err
			}
			if found.usable(now) {
				held = found
			}
		}
	}
	policy, err := s.store.PolicyFor(ctx, owner, site.SiteID)
	if err != nil {
		return callrules.JoinResult{}, none, err
	}
	if policy.RequirePasskeys && (held == nil || held.CredentialID == "") {
		return callrules.JoinResult{}, none, callrules.Refuse(callrules.ErrNoSession, "passkey_required", "sign in before joining")
	}
	ask := joinAsk{name: name, confirm: confirm, asHost: asHost, pepper: pepper, link: link}
	if presented {
		ask.token = strings.TrimSpace(sess.Token)
	}
	room, err := s.roomFor(ctx, site, pepper, link, asHost, held)
	if errors.Is(err, errLinkOpensNoRoom) {
		// A LINK THAT FINDS NO ROOM MAY BE A BOOKED CALL'S, whose room its first join
		// opens (schedule.go).
		return s.joinBooking(ctx, site, sess, held, ask)
	}
	if err != nil {
		return callrules.JoinResult{}, held.state(sess, site), err
	}
	return s.joinRoom(ctx, site, *room, sess, held, ask)
}

// joinRoom is a join of a call whose room exists: refused once the call has ended, and
// otherwise decided and written under its gates, an admission's pass minted after them.
func (s *Service) joinRoom(ctx context.Context, site callrules.SiteRef, room roomRow, sess *callrules.Session, held *sessionRow, ask joinAsk) (callrules.JoinResult, callrules.Session, error) {
	if !room.open() {
		return refuseJoin(callrules.ReasonCallEnded), held.state(sess, site), fmt.Errorf("%w: the call has ended", callrules.ErrEnded)
	}
	policy, err := s.store.PolicyFor(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return callrules.JoinResult{}, held.state(sess, site), err
	}
	// The server's own row is read before the gates, fresh: no gate of this service
	// guards it (the bootstrap writes it under its own), so a read under them would be no
	// fresher. What the gates guard is the load on it, which is read under them.
	server, err := s.store.MediaServerByID(ctx, room.MediaServerID)
	if err != nil {
		return callrules.JoinResult{}, held.state(sess, site), err
	}
	if server == nil {
		return callrules.JoinResult{}, held.state(sess, site), fmt.Errorf("calls: the media server of call %s is gone", room.ID)
	}
	credential, err := s.credential(ctx, *server)
	if err != nil {
		return callrules.JoinResult{}, held.state(sess, site), err
	}
	result, out, seat, err := s.joinGated(ctx, site, room, *server, policy, held, ask)
	if err != nil || result.Outcome != callrules.OutcomeAdmit {
		return result, out, err
	}
	if err := s.ensureMediaRoom(ctx, *server, credential, room, policy); err != nil {
		return callrules.JoinResult{}, out, err
	}
	if result, err = admitted(*server, credential, room.MediaRoom, seat, policy); err != nil {
		return callrules.JoinResult{}, out, err
	}
	return result, out, nil
}

// ensureMediaRoom opens a call's media room on its server, with the call's limits, before
// EVERY pass into a call already open is handed out: a join's, a rejoin's, and the pass a
// poll hands a seat that is let in or already in. Start and a booked call's first join
// open the room as they place it (placeRoom), just before their own pass.
//
// WHY BEFORE EVERY PASS, AND NOT ONLY A SEAT ALONE IN THE CALL'S. A media server opens no
// room a connection names (deploy/media runs it with room.auto_create false, so nobody
// connects a room into being without the call's limits), and a room the call opened can
// be gone by the time a pass is used: the server closes a room nobody has been in for its
// empty-room timeout (a host queued that long, a call whose people all left while somebody
// waited), and a server that restarts forgets every room it held while everybody in the
// call reconnects at once, each with a fresh pass. The rule this replaces reopened the room
// only for a seat alone in the call, which left every seat of a restarted call refused,
// since none of them is alone, and left the second of two people let in one after the
// other into an empty room free to connect before the first one's reopening landed.
// Opening a room the server still holds is answered with that room unchanged
// (livekit-server v1.13.7 RoomManager.CreateRoom returns a room it holds before its
// allocator is reached), so a room still open costs one control call per pass, and a pass
// is handed out only when a seat is let in or comes back for a fresh one, never on a
// waiting seat's poll.
//
// OUTSIDE THE GATES, as every media call is: a media server slow to answer must not hold
// up its server's admissions. A failure hands out no pass and undoes nothing the gates
// wrote: the seat stays admitted, and its next ask, a poll or a rejoin, opens the room
// again before it is handed one.
func (s *Service) ensureMediaRoom(ctx context.Context, server MediaServer, credential MediaCredential, room roomRow, policy callrules.Policy) error {
	backend, err := s.media(server, credential)
	if err != nil {
		return fmt.Errorf("calls: reach media server %s: %w", server.ID, err)
	}
	if err := backend.CreateRoom(ctx, room.MediaRoom, mediaRoomLimit(policy)); err != nil {
		return fmt.Errorf("calls: open the call's media room: %w", err)
	}
	return nil
}

// mediaRoomLimit is how many connections a call's media room admits: every person, every
// agent and the sampler.
func mediaRoomLimit(policy callrules.Policy) uint32 {
	return uint32(max(policy.MaxPeople, 0) + max(policy.MaxAgents, 0) + 1)
}

// joinAsk is what a join brings to its gated phase.
type joinAsk struct {
	name    string
	confirm callrules.Confirmation
	asHost  bool   // the host link was presented
	link    string // the link presented, guest or host, or "" for a join by session
	token   string // the presented session's token, when one was presented
	pepper  []byte
}

// errLinkOpensNoRoom is a link that finds no room on the deployable: no call of this
// deployable has it, or it is a booked call's whose room nobody has opened yet.
var errLinkOpensNoRoom = fmt.Errorf("%w: the link opens no call on this deployable", callrules.ErrNotFound)

// roomFor is the call a join names: by its link, or the call the session is seated in.
func (s *Service) roomFor(ctx context.Context, site callrules.SiteRef, pepper []byte, link string, asHost bool, held *sessionRow) (*roomRow, error) {
	owner := site.OwnerUserID
	if link != "" {
		room, err := s.store.roomByLink(ctx, owner, site.SiteID, callrules.Digest(pepper, []byte(link)), asHost)
		if err != nil {
			return nil, err
		}
		if room == nil {
			return nil, errLinkOpensNoRoom
		}
		return room, nil
	}
	if held == nil {
		return nil, fmt.Errorf("%w: the session is unknown, ended or expired", callrules.ErrNoSession)
	}
	if held.SeatID == "" {
		return nil, fmt.Errorf("%w: the session is seated in no call", callrules.ErrNotFound)
	}
	seat, err := s.store.seatByID(ctx, owner, held.SeatID)
	if err != nil {
		return nil, err
	}
	if seat == nil {
		return nil, fmt.Errorf("%w: the session's seat is gone", callrules.ErrNotFound)
	}
	room, err := s.store.roomByID(ctx, owner, seat.RoomID)
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, fmt.Errorf("%w: the session's call is gone", callrules.ErrNotFound)
	}
	return room, nil
}

// joinGated is a join's decision and its writes, under the room's and the server's
// gates. It answers an admission without a pass (the caller opens the media room and
// mints the pass once the gates are released, ensureMediaRoom), the session the joiner
// now holds, and the seat.
//
// A HOST BACK IN THE CALL CLEARS THE COUNTDOWN. When the decision lets a host seat in --
// a host's re-entry, or anyone holding the host link -- and the room records that its
// last host left (hostLeftAt), the record is cleared in the same decision: the
// countdown runs only while nobody hosting the call is in it (host.go). A host who
// re-enters into the queue is not back yet, and clears nothing until their own poll lets
// them in (answerWaiting).
func (s *Service) joinGated(ctx context.Context, site callrules.SiteRef, room roomRow, server MediaServer, policy callrules.Policy, held *sessionRow, ask joinAsk) (callrules.JoinResult, callrules.Session, seatRow, error) {
	gateKeys := []string{roomGate(room.ID), serverGate(server.ID)}
	if held != nil && held.KeyID != "" {
		gateKeys = append([]string{accountGate(held.KeyID)}, gateKeys...)
	}
	release, err := s.gates(ctx, gateKeys...)
	if err != nil {
		return callrules.JoinResult{}, held.stateOf(ask.token, site), seatRow{}, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	owner := site.OwnerUserID
	now := s.now()

	current, err := s.store.roomByID(fresh, owner, room.ID)
	if err != nil {
		return callrules.JoinResult{}, held.stateOf(ask.token, site), seatRow{}, err
	}
	if current == nil {
		return callrules.JoinResult{}, held.stateOf(ask.token, site), seatRow{}, fmt.Errorf("%w: the call is gone", callrules.ErrNotFound)
	}
	if !current.open() {
		return refuseJoin("call_ended"), held.stateOf(ask.token, site), seatRow{}, fmt.Errorf("%w: the call has ended", callrules.ErrEnded)
	}
	// THE ATTEMPT IS COUNTED BEFORE ANYTHING IS DECIDED, and past the limit nothing is
	// written at all.
	window, ok, retry := callrules.CountJoinAttempt(current.Window, now, policy.RoomJoinsPerMinute, callrules.JoinWindowLength)
	if !ok {
		return rateRefusal(retry), held.stateOf(ask.token, site), seatRow{}, fmt.Errorf("%w: the call has taken its join attempts for this minute", callrules.ErrConflict)
	}
	counted := memql.VersionTimeAfter(current.Version, now)
	if err := s.store.countJoin(fresh, owner, *current, window, counted); err != nil {
		return callrules.JoinResult{}, held.stateOf(ask.token, site), seatRow{}, err
	}
	// The room as written: a second write to it in this decision (a host back in the
	// call) is fenced on this version, not on the one read.
	current.Version, current.Window = counted, window

	// The session and its seat, as they are now.
	var seat *seatRow
	if held != nil {
		if held, err = s.store.sessionByID(fresh, owner, held.ID); err != nil {
			return callrules.JoinResult{}, callrules.Session{}, seatRow{}, err
		}
		if !held.usable(now) {
			return callrules.JoinResult{}, callrules.Session{}, seatRow{}, callrules.Refuse(callrules.ErrConflict, "session_changed", "the session changed while joining")
		}
	}
	latestPolicy, err := s.store.PolicyFor(fresh, owner, site.SiteID)
	if err != nil {
		return callrules.JoinResult{}, callrules.Session{}, seatRow{}, err
	}
	if latestPolicy.RequirePasskeys && (held == nil || held.CredentialID == "") {
		return callrules.JoinResult{}, callrules.Session{}, seatRow{}, callrules.Refuse(callrules.ErrNoSession, "passkey_required", "sign in before joining")
	}
	if held != nil && held.SeatID != "" {
		if seat, err = s.store.seatByID(fresh, owner, held.SeatID); err != nil {
			return callrules.JoinResult{}, held.stateOf(ask.token, site), seatRow{}, err
		}
	}
	out := held.stateOf(ask.token, site)

	reentry := false
	switch {
	case seat != nil && seat.RoomID == current.ID:
		switch {
		case seat.Status == seatRemoved:
			return refuseJoin(callrules.ReasonRemoved), out, seatRow{}, fmt.Errorf("%w: the host removed this seat", callrules.ErrForbidden)
		case seat.occupying():
			// A REJOIN: the same seat and identity, a fresh pass, nothing counted against
			// the room's size or the server's seats.
			rejoined, err := s.recordJoin(fresh, owner, *seat, ask, nil, now)
			if err != nil {
				return callrules.JoinResult{}, out, seatRow{}, err
			}
			return callrules.JoinResult{Outcome: callrules.OutcomeAdmit}, out, rejoined, nil
		case seat.Status == seatWaiting:
			// A WAITING SEAT STAYS WAITING: it keeps its seat, its queue entry and its
			// place in line, and is answered exactly as its poll would answer it, so a
			// rejoin can neither skip the line nor lose a place the poll would keep.
			recorded, err := s.recordJoin(fresh, owner, *seat, ask, nil, now)
			if err != nil {
				return callrules.JoinResult{}, out, seatRow{}, err
			}
			result, after, err := s.answerWaiting(fresh, site, *current, server, policy, recorded, now)
			return result, out, after, err
		default:
			// A seat that left takes a counted join, on the same row.
			reentry = true
		}
	case seat != nil && seat.placed():
		other, err := s.store.roomByID(fresh, owner, seat.RoomID)
		if err != nil {
			return callrules.JoinResult{}, out, seatRow{}, err
		}
		if other != nil && other.open() {
			return refuseJoin(callrules.ReasonInAnotherCall), out, seatRow{}, fmt.Errorf("%w: the session is in another call", callrules.ErrConflict)
		}
	}

	// A NEW JOIN, decided against the room and the server's load.
	load, err := s.store.loadOn(fresh, server)
	if err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	remembered, err := s.recordAbandonedHost(fresh, *current, load, policy, now)
	if err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	*current = remembered
	ttls, err := s.queueLifetimes(fresh, site, policy, load)
	if err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	live, behindLocks, expired := load.queue(now, ttls)
	if err := s.expire(fresh, owner, current.ID, expired, now); err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	host := ask.asHost || (reentry && seat.Host)
	// A seat a host admitted passes the lock on its way back in as a host does
	// (seatRow.passesLock); it is not a host for anything else.
	passes := host || (reentry && seat.passesLock())
	arrives := arrival(now, live, behindLocks)
	entry := callrules.QueueEntry{ID: id.NewShortId(), Class: load.classFor(*current), EnqueuedAt: arrives, LastPollAt: arrives}
	position := placeInLine(live, entry)
	decision := callrules.DecideJoin(load.roomState(*current), callrules.JoinAsk{Kind: callrules.KindPerson, Host: passes}, policy, load.free()-position)
	if decision.Outcome == callrules.OutcomeRefuse {
		return refuseJoin(decision.Reason), out, seatRow{}, fmt.Errorf("%w: %s", callrules.ErrConflict, decision.Reason)
	}

	taken := seatRow{
		ID: id.NewShortId(), RoomID: current.ID, Kind: string(callrules.KindPerson),
		DisplayName: ask.name, Status: seatWaiting, Host: host, KeyID: held.keyHeld(), CredentialID: held.credentialHeld(),
	}
	if reentry {
		taken.ID, taken.Identity, taken.KeyID, taken.LetInAt = seat.ID, seat.Identity, seat.KeyID, seat.LetInAt
	} else if taken.Identity, err = callrules.NewToken(s.random, identityBytes); err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	joinedAt := time.Time{}
	if decision.Outcome == callrules.OutcomeAdmit {
		taken.Status, joinedAt = seatAdmitted, now
	}

	// The session first: a session pointing at a seat that was never written is seated
	// nowhere, which the next join reads as free.
	out, refused, err := s.seatJoiner(fresh, site, held, ask, out, taken.ID, current.ID, now)
	if err != nil {
		return refused, out, seatRow{}, err
	}
	taken.KeyID = out.KeyID
	if reentry {
		fields := map[string]any{"status": taken.Status, "host": taken.Host}
		if !joinedAt.IsZero() {
			fields["joinedAt"] = joinedAt
		}
		// A seat that left and comes back to wait begins waiting again now: a host's roster
		// says since when (host.go), and the first wait's time would be a stint ago.
		if taken.Status == seatWaiting {
			fields["waitingSince"] = now
		}
		if _, err := s.recordJoin(fresh, owner, *seat, ask, fields, now); err != nil {
			return callrules.JoinResult{}, out, seatRow{}, err
		}
	} else if err := s.store.createSeat(fresh, owner, newSeat{
		ID: taken.ID, SiteID: site.SiteID, RoomID: current.ID, Identity: taken.Identity, Kind: taken.Kind,
		DisplayName: taken.DisplayName, Status: taken.Status, Host: taken.Host, Confirm: ask.confirm, JoinedAt: joinedAt,
		KeyID: taken.KeyID, CredentialID: taken.CredentialID,
	}, now); err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	switch decision.Outcome {
	case callrules.OutcomeQueue:
		if err := s.store.createQueueEntry(fresh, owner, newQueueEntry{
			ID: entry.ID, SiteID: site.SiteID, RoomID: current.ID, SeatID: taken.ID, KeyID: current.KeyID,
			Class: entry.Class, EnqueuedAt: entry.EnqueuedAt,
		}, now); err != nil {
			return callrules.JoinResult{}, out, seatRow{}, err
		}
		return queued(decision.Reason, position, policy), out, taken, nil
	case callrules.OutcomeWait:
		return waited(decision.Reason, policy), out, taken, nil
	}
	if taken.Host {
		if err := s.hostIsBack(fresh, owner, *current, now); err != nil {
			return callrules.JoinResult{}, out, seatRow{}, err
		}
	}
	return callrules.JoinResult{Outcome: callrules.OutcomeAdmit}, out, taken, nil
}

// hostIsBack clears a room's record that its last host left (hostLeftAt), now that a host
// seat is in the call again, fenced on the version the caller holds. A room with no such
// record is not written.
func (s *Service) hostIsBack(ctx context.Context, owner string, room roomRow, now time.Time) error {
	if room.HostLeftAt.IsZero() {
		return nil
	}
	return s.store.updateRoom(ctx, owner, room, map[string]any{"hostLeftAt": ""}, memql.VersionTimeAfter(room.Version, now))
}

// seatJoiner records on the joiner's session the seat a join gave them: a new session
// for a joiner who brought none, with an unclaimed guest key and the invitation's
// guest/host seat authority, or their existing session fenced on its read version.
// It answers the session the joiner now holds; a session another join moved at the same
// moment is a refusal, which it answers beside the error.
func (s *Service) seatJoiner(ctx context.Context, site callrules.SiteRef, held *sessionRow, ask joinAsk, out callrules.Session, seatID, roomID string, now time.Time) (callrules.Session, callrules.JoinResult, error) {
	owner := site.OwnerUserID
	if held == nil {
		token, err := callrules.NewToken(s.random, tokenBytes)
		if err != nil {
			return out, callrules.JoinResult{}, err
		}
		kind := callrules.SessionGuest
		if ask.asHost {
			// A host link grants authority in this call. Its unfunded guest key
			// starts nothing (Start) and books nothing (Schedule).
			kind = callrules.SessionHost
		}
		// A successful anonymous invitation gets a stable zero-balance key.
		// Its placeholder spelling is undisclosed and cannot unlock anything;
		// only this HttpOnly session can later claim it with a passkey.
		keyID := id.NewShortId()
		raw, _, err := callrules.NewKey(s.random)
		if err != nil {
			return out, callrules.JoinResult{}, err
		}
		if err := s.store.createKey(ctx, owner, newKey{ID: keyID, SiteID: site.SiteID, Digest: callrules.Digest(ask.pepper, raw[:]), Kind: "guest", IssuedAt: now}, now); err != nil {
			return out, callrules.JoinResult{}, err
		}
		created := newSession{
			ID: id.NewShortId(), KeyID: keyID, SiteID: site.SiteID, TokenDigest: callrules.Digest(ask.pepper, []byte(token)),
			Kind: kind, SeatID: seatID, RoomID: roomID, ExpiresAt: now.Add(callrules.SessionLifetime),
		}
		if err := s.store.createSession(ctx, owner, created, now); err != nil {
			return out, callrules.JoinResult{}, err
		}
		return callrules.Session{Token: token, Kind: kind, KeyID: keyID, SeatID: seatID, RoomID: roomID, ExpiresAt: created.ExpiresAt, Site: site}, callrules.JoinResult{}, nil
	}
	// Written even when the seat is the session's own (a re-entry): the write is what
	// makes two joins moving one session at once meet on its version, under whichever
	// two room gates they hold.
	if err := s.store.seatSession(ctx, owner, held, seatID, roomID, memql.VersionTimeAfter(held.Version, now)); err != nil {
		if errors.Is(err, memql.ErrRowVersionChanged) {
			return out, refuseJoin(callrules.ReasonInAnotherCall), s.movedSession(err)
		}
		return out, callrules.JoinResult{}, err
	}
	// Rebuild from persisted authority: instant Start supplies an empty out, while
	// joins may carry an earlier snapshot. Both must keep the held key identity.
	out = held.stateOf(ask.token, site)
	out.SeatID, out.RoomID = seatID, roomID
	return out, callrules.JoinResult{}, nil
}

// recordJoin writes what a join confirmed onto a seat it keeps -- the name and the
// three confirmations, the host flag when the host link was presented -- together with
// any fields the caller sets, and only when something changed, so a rejoin with
// nothing new writes nothing. It answers the seat as written.
func (s *Service) recordJoin(ctx context.Context, owner string, seat seatRow, ask joinAsk, fields map[string]any, now time.Time) (seatRow, error) {
	if fields == nil {
		fields = map[string]any{}
	}
	if seat.DisplayName != ask.name {
		fields["displayName"], seat.DisplayName = ask.name, ask.name
	}
	if !seat.ConfirmAdult || !seat.ConfirmNoMinors || seat.TermsVersion != ask.confirm.Terms {
		fields["confirmAdult"], fields["confirmNoMinors"], fields["termsVersion"] = true, true, ask.confirm.Terms
		seat.ConfirmAdult, seat.ConfirmNoMinors, seat.TermsVersion = true, true, ask.confirm.Terms
	}
	if ask.asHost && !seat.Host {
		fields["host"], seat.Host = true, true
	}
	if status, ok := fields["status"].(string); ok {
		seat.Status = status
	}
	if host, ok := fields["host"].(bool); ok {
		seat.Host = host
	}
	if len(fields) == 0 {
		return seat, nil
	}
	version := memql.VersionTimeAfter(seat.Version, now)
	if err := s.store.updateSeat(ctx, owner, seat, fields, version); err != nil {
		return seatRow{}, err
	}
	// The seat as written, at the version written: a second write to it in the same
	// decision (a waiter's admission) is fenced on this one.
	seat.Version = version
	return seat, nil
}

// ---------------------------------------------------------------------------
// queue
// ---------------------------------------------------------------------------

// Queue is a waiting seat's poll. A seat whose turn has come -- its place in the media
// server's line inside the free seats, and nothing about its call refusing it -- is
// admitted with a pass; one that is still waiting is told its place and when to ask
// again. A seat already admitted is handed a fresh pass, which is how a waiter whose
// promotion reply was lost gets in, and a seat the host let in gets its pass.
func (s *Service) Queue(ctx context.Context, sess callrules.Session) (result callrules.JoinResult, resultErr error) {
	defer func() {
		if resultErr == nil && result.Outcome == callrules.OutcomeAdmit {
			s.observeSessionHistory(ctx, sess)
		}
	}()
	site, err := checkSite(sess.Site)
	if err != nil {
		return callrules.JoinResult{}, fmt.Errorf("%w: %v", callrules.ErrNoSession, err)
	}
	row, _, err := s.session(ctx, site, sess.Token, s.now())
	if err != nil {
		return callrules.JoinResult{}, err
	}
	seat, room, err := s.seatAndRoom(ctx, site.OwnerUserID, row)
	if err != nil {
		return callrules.JoinResult{}, err
	}
	if seat == nil || room == nil {
		return callrules.JoinResult{}, fmt.Errorf("%w: the session is waiting for no call", callrules.ErrNotFound)
	}
	if !room.open() {
		return refuseJoin("call_ended"), fmt.Errorf("%w: the call has ended", callrules.ErrEnded)
	}
	policy, err := s.store.PolicyFor(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return callrules.JoinResult{}, err
	}
	// Before the gates, for Join's reason.
	server, err := s.store.MediaServerByID(ctx, room.MediaServerID)
	if err != nil {
		return callrules.JoinResult{}, err
	}
	if server == nil {
		return callrules.JoinResult{}, fmt.Errorf("calls: the media server of call %s is gone", room.ID)
	}
	credential, err := s.credential(ctx, *server)
	if err != nil {
		return callrules.JoinResult{}, err
	}
	result, after, err := s.pollGated(ctx, site, *room, *server, policy, seat.ID)
	if err != nil || result.Outcome != callrules.OutcomeAdmit {
		return result, err
	}
	if err := s.ensureMediaRoom(ctx, *server, credential, *room, policy); err != nil {
		return callrules.JoinResult{}, err
	}
	return admitted(*server, credential, room.MediaRoom, after, policy)
}

// pollGated is a poll's decision and its writes, under the room's and the server's
// gates. It answers as joinGated does: an admission without a pass, and the seat.
func (s *Service) pollGated(ctx context.Context, site callrules.SiteRef, room roomRow, server MediaServer, policy callrules.Policy, seatID string) (callrules.JoinResult, seatRow, error) {
	release, err := s.gates(ctx, roomGate(room.ID), serverGate(server.ID))
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	owner := site.OwnerUserID
	now := s.now()
	current, err := s.store.roomByID(fresh, owner, room.ID)
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	if current == nil {
		return callrules.JoinResult{}, seatRow{}, fmt.Errorf("%w: the call is gone", callrules.ErrNotFound)
	}
	if !current.open() {
		return refuseJoin("call_ended"), seatRow{}, fmt.Errorf("%w: the call has ended", callrules.ErrEnded)
	}
	seat, err := s.store.seatByID(fresh, owner, seatID)
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	switch {
	case seat == nil:
		return callrules.JoinResult{}, seatRow{}, fmt.Errorf("%w: the seat is gone", callrules.ErrNotFound)
	case seat.Status == seatRemoved:
		return refuseJoin(callrules.ReasonRemoved), seatRow{}, fmt.Errorf("%w: the host removed this seat", callrules.ErrForbidden)
	case seat.occupying():
		return callrules.JoinResult{Outcome: callrules.OutcomeAdmit}, *seat, nil
	case seat.Status != seatWaiting:
		return callrules.JoinResult{}, seatRow{}, fmt.Errorf("%w: the seat has left the call", callrules.ErrNotFound)
	}
	return s.answerWaiting(fresh, site, *current, server, policy, *seat, now)
}

// answerWaiting decides a waiting seat's turn and writes what came of it. The caller
// holds the room's and the server's gates and read the room and the seat fresh under
// them; the server's load is read here, under the same gates.
//
// A seat whose queue entry is still waiting -- in line, or held behind its call's lock
// (serverLoad.queue) -- keeps it: it asks with its own place in line, and its poll keeps
// the entry alive, so a waiter held through a lock is back in its place when the lock
// lifts. One without -- it waited behind a lock from the start, or its entry outlived
// its lifetime -- asks as a newcomer at the back of the line, in the class its call's
// state gives it now (serverLoad.classFor), as any new ask does. A seat a host admitted
// (host.go) passes the lock and is decided exactly so: admitted when the server has a
// seat for it after the line ahead, queued otherwise; its admission never handed out a
// pass, so this poll's reply is the only one that carries it.
func (s *Service) answerWaiting(ctx context.Context, site callrules.SiteRef, room roomRow, server MediaServer, policy callrules.Policy, seat seatRow, now time.Time) (callrules.JoinResult, seatRow, error) {
	owner := site.OwnerUserID
	load, err := s.store.loadOn(ctx, server)
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	room, err = s.recordAbandonedHost(ctx, room, load, policy, now)
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	ttls, err := s.queueLifetimes(ctx, site, policy, load)
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	live, held, expired := load.queue(now, ttls)
	if err := s.expire(ctx, owner, room.ID, expired, now); err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	stored := load.waitingEntryOf(seat.ID)
	_, inLine := callrules.Position(live, entryID(stored))
	_, heldBack := callrules.Position(held, entryID(stored))
	kept := inLine || heldBack
	arrives := arrival(now, live, held)
	entry := callrules.QueueEntry{ID: id.NewShortId(), Class: load.classFor(room), EnqueuedAt: arrives, LastPollAt: arrives}
	if kept {
		entry = stored.rule()
	}
	// A held entry is placed by its own arrival among those in line: the place it takes
	// back when the lock lifts. While the lock holds, DecideJoin answers it "locked".
	position := placeInLine(live, entry)
	// A host seat, and a seat a host admitted, pass the lock (seatRow.passesLock).
	decision := callrules.DecideJoin(load.roomState(room), callrules.JoinAsk{Kind: callrules.SeatKind(seat.Kind), Host: seat.passesLock()}, policy, load.free()-position)

	switch decision.Outcome {
	case callrules.OutcomeAdmit:
		admittedSeat := seat
		admittedSeat.Status = seatAdmitted
		if err := s.store.updateSeat(ctx, owner, seat, map[string]any{"status": seatAdmitted, "joinedAt": now}, memql.VersionTimeAfter(seat.Version, now)); err != nil {
			return callrules.JoinResult{}, seatRow{}, err
		}
		if kept {
			if err := s.store.updateQueueEntry(ctx, owner, *stored, entryAdmitted, time.Time{}, memql.VersionTimeAfter(stored.Version, now)); err != nil {
				return callrules.JoinResult{}, seatRow{}, err
			}
		}
		if seat.Host {
			// A host is back in the call: the countdown stops (joinGated's note).
			if err := s.hostIsBack(ctx, owner, room, now); err != nil {
				return callrules.JoinResult{}, seatRow{}, err
			}
		}
		return callrules.JoinResult{Outcome: callrules.OutcomeAdmit}, admittedSeat, nil
	case callrules.OutcomeQueue, callrules.OutcomeWait:
		if kept {
			if err := s.refreshPoll(ctx, owner, *stored, policy, now); err != nil {
				return callrules.JoinResult{}, seatRow{}, err
			}
		} else if decision.Outcome == callrules.OutcomeQueue {
			// A lock-waiter let through into a full server, or a waiter back after its
			// entry expired: the back of the line.
			if err := s.store.createQueueEntry(ctx, owner, newQueueEntry{
				ID: entry.ID, SiteID: site.SiteID, RoomID: room.ID, SeatID: seat.ID, KeyID: room.KeyID,
				Class: entry.Class, EnqueuedAt: entry.EnqueuedAt,
			}, now); err != nil {
				return callrules.JoinResult{}, seatRow{}, err
			}
		}
		if decision.Outcome == callrules.OutcomeWait {
			return waited(decision.Reason, policy), seat, nil
		}
		return queued(decision.Reason, position, policy), seat, nil
	}
	// The call can no longer take this seat (it filled while they waited): the seat
	// leaves, and its place in line goes to whoever is next.
	if err := s.store.updateSeat(ctx, owner, seat, map[string]any{"status": seatLeft, "leftAt": now}, memql.VersionTimeAfter(seat.Version, now)); err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	if kept {
		if err := s.store.updateQueueEntry(ctx, owner, *stored, entryExpired, time.Time{}, memql.VersionTimeAfter(stored.Version, now)); err != nil {
			return callrules.JoinResult{}, seatRow{}, err
		}
	}
	return refuseJoin(decision.Reason), seatRow{}, fmt.Errorf("%w: %s", callrules.ErrConflict, decision.Reason)
}

// refreshPoll records a waiter's poll on its entry -- at most once a minute, and twice
// within the entry's lifetime when the lifetime is shorter than two minutes -- so a
// waiter polling every few seconds writes one row version a minute rather than twenty,
// and an entry is never left to expire under a waiter who is still polling.
func (s *Service) refreshPoll(ctx context.Context, owner string, entry queueEntryRow, policy callrules.Policy, now time.Time) error {
	every := min(time.Minute, policy.QueueEntryTTL/2)
	seen := entry.LastPollAt
	if seen.IsZero() {
		seen = entry.EnqueuedAt
	}
	if now.Sub(seen) < every {
		return nil
	}
	return s.store.updateQueueEntry(ctx, owner, entry, "", now, memql.VersionTimeAfter(entry.Version, now))
}

// expire marks a call's queue entries that have outlived their lifetime. Only the call
// whose gate is held: another call's entries are left to a request of that call, and
// counted by nobody meanwhile (serverLoad.queue).
func (s *Service) expire(ctx context.Context, owner, roomID string, expired []queueEntryRow, now time.Time) error {
	for _, entry := range expired {
		if entry.RoomID != roomID {
			continue
		}
		if err := s.store.updateQueueEntry(ctx, owner, entry, entryExpired, time.Time{}, memql.VersionTimeAfter(entry.Version, now)); err != nil {
			return err
		}
	}
	return nil
}

// queueLifetimes is how long each waiting entry on a server lives without a poll: its
// OWN deployable's QueueEntryTTL, read once per deployable, since every call on the
// server shares the one queue and each deployable sets its own limits.
func (s *Service) queueLifetimes(ctx context.Context, site callrules.SiteRef, policy callrules.Policy, load serverLoad) (func(roomRow) time.Duration, error) {
	bySite := map[string]time.Duration{site.SiteID: policy.QueueEntryTTL}
	for _, entry := range load.entries {
		room := load.rooms[entry.RoomID]
		if !entry.waiting() || room.SiteID == "" {
			continue
		}
		if _, known := bySite[room.SiteID]; known {
			continue
		}
		theirs, err := s.store.PolicyFor(ctx, room.OwnerUserID, room.SiteID)
		if err != nil {
			return nil, err
		}
		bySite[room.SiteID] = theirs.QueueEntryTTL
	}
	return func(room roomRow) time.Duration {
		if ttl, ok := bySite[room.SiteID]; ok {
			return ttl
		}
		return callrules.DefaultPolicy().QueueEntryTTL
	}, nil
}

func entryID(entry *queueEntryRow) string {
	if entry == nil {
		return ""
	}
	return entry.ID
}

// ---------------------------------------------------------------------------
// leave
// ---------------------------------------------------------------------------

// Leave takes a session's seat out of its call: the seat is left and its queue entry,
// if it was waiting, expired. It frees the seat and promotes nobody, since only a
// waiter's own poll can carry its pass. A seat that held a media server's seat has its
// identity disconnected, so a client that left without hanging up does not stay in the
// call on a seat the server counts as free. Leaving a seat that is gone already, or
// with a session seated nowhere, is not an error: the state asked for holds.
//
// THE LAST PERSON LEAVING ENDS THE CALL: when nobody is left in it and nobody waits to
// come in, it ends at once, empty, its booking with it (host.go endRows), and its media
// room is deleted -- a booked call only after its booked end, since before that an emptied
// booked call waits for its host (host.go endsEmpty). Otherwise, when the seat was the last
// host seat in the call, the room records when the host left (hostLeftAt), and the
// host-left countdown reconciliation applies runs from then -- a booked call's from its
// booked start when that is later (host.go's file note).
//
// THIS ROUTE IS THE HANG-UP, and the only thing that closes a seat at once. A media
// server's participant_left is not a leave: a page refreshed and a tab closed disconnect
// as a hang-up does, so a disconnect only records the seat's last sign of life, and
// reconciliation closes the seat if it stays gone (events.go).
func (s *Service) Leave(ctx context.Context, sess callrules.Session) error {
	site, err := checkSite(sess.Site)
	if err != nil {
		return fmt.Errorf("%w: %v", callrules.ErrNoSession, err)
	}
	row, _, err := s.session(ctx, site, sess.Token, s.now())
	if err != nil {
		return err
	}
	seat, room, err := s.seatAndRoom(ctx, site.OwnerUserID, row)
	if err != nil || seat == nil || room == nil {
		return err
	}
	left, ended, err := s.leaveGated(ctx, site.OwnerUserID, *room, seat.ID)
	if err != nil || left == nil {
		return err
	}
	// After the gates: a media server slow to answer must not hold up the server's
	// admissions. Best effort, and logged by the room and seat only.
	if left.occupying() {
		if err := s.disconnect(ctx, *room, left.Identity); err != nil {
			s.logger.Warn("calls: a seat left but its media identity could not be disconnected", "component", pluginName,
				"room", room.ID, "seat", left.ID, "reason", failureReason(err))
		}
	}
	// Nobody is in an empty call by the rows' account, so its media room is cleared up
	// rather than ended: best effort, as a media server closes an empty room itself.
	if ended {
		if err := s.deleteMediaRoom(ctx, *room); err != nil {
			s.logger.Warn("calls: a call ended empty but its media room could not be deleted", "component", pluginName,
				"room", room.ID, "reason", failureReason(err))
		}
	}
	return nil
}

// leaveGated writes a seat's leaving under its call's gates (host.go endGates, since the
// leave may end the call), and answers the seat as it was, or nil when it had left
// already, and whether the call ended with it.
func (s *Service) leaveGated(ctx context.Context, owner string, room roomRow, seatID string) (*seatRow, bool, error) {
	release, err := s.gates(ctx, endGates(room)...)
	if err != nil {
		return nil, false, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	seat, err := s.store.seatByID(fresh, owner, seatID)
	if err != nil || seat == nil || !seat.placed() {
		return nil, false, err
	}
	if err := s.store.updateSeat(fresh, owner, *seat, map[string]any{"status": seatLeft, "leftAt": now}, memql.VersionTimeAfter(seat.Version, now)); err != nil {
		return nil, false, err
	}
	if seat.Status == seatWaiting {
		if err := s.giveUpPlace(fresh, owner, room.ID, seat.ID, now); err != nil {
			return nil, false, err
		}
	}
	current, err := s.store.roomByID(fresh, owner, room.ID)
	if err != nil || current == nil || !current.open() {
		return seat, false, err
	}
	// The call as the leave left it: the seat just written is out of it.
	v, err := s.viewRoom(fresh, *current)
	if err != nil {
		return seat, false, err
	}
	ends, err := s.endsEmpty(fresh, v, now)
	if err != nil {
		return seat, false, err
	}
	if ends {
		if err := s.endRows(fresh, v, callrules.EndReasonEmpty, now); err != nil {
			return seat, false, err
		}
		return seat, true, nil
	}
	// An ad hoc host abandoning the queue starts the same grace as a host leaving
	// media, once no other host is still in line. A later requeue must not restart it.
	if seat.Host && !v.hostPresent() && current.HostLeftAt.IsZero() &&
		(seat.occupying() || (current.ScheduledCallID == "" && !v.hostComing(now))) {
		if err := s.store.updateRoom(fresh, owner, *current, map[string]any{"hostLeftAt": now}, memql.VersionTimeAfter(current.Version, now)); err != nil {
			return seat, false, err
		}
	}
	return seat, false, nil
}

// disconnect asks a call's media server to drop one identity from its media room.
func (s *Service) disconnect(ctx context.Context, room roomRow, identity string) error {
	backend, err := s.backendFor(ctx, room)
	if err != nil || backend == nil {
		return err
	}
	return backend.RemoveParticipant(ctx, room.MediaRoom, identity)
}

// ---------------------------------------------------------------------------
// shared by the acts
// ---------------------------------------------------------------------------

// seatAndRoom is a session's seat and that seat's call, or nils for a session seated
// nowhere or a seat or call that is gone.
func (s *Service) seatAndRoom(ctx context.Context, owner string, row *sessionRow) (*seatRow, *roomRow, error) {
	if row.SeatID == "" {
		return nil, nil, nil
	}
	seat, err := s.store.seatByID(ctx, owner, row.SeatID)
	if err != nil || seat == nil {
		return nil, nil, err
	}
	room, err := s.store.roomByID(ctx, owner, seat.RoomID)
	if err != nil || room == nil {
		return nil, nil, err
	}
	return seat, room, nil
}

// seatedElsewhere reports whether a session holds a seat, in a call other than roomID,
// that is still in its call or waiting to be, in a call still open: one browser is in
// one call at a time.
func (s *Service) seatedElsewhere(ctx context.Context, owner string, row *sessionRow, roomID string) (bool, error) {
	if row.SeatID == "" {
		return false, nil
	}
	seat, err := s.store.seatByID(ctx, owner, row.SeatID)
	if err != nil || seat == nil || seat.RoomID == roomID || !seat.placed() {
		return false, err
	}
	other, err := s.store.roomByID(ctx, owner, seat.RoomID)
	if err != nil || other == nil {
		return false, err
	}
	return other.open(), nil
}

// movedSession is the answer to a session whose version changed under a join that held
// its call's gates: another join, under another call's gates, seated it at the same
// moment.
func (s *Service) movedSession(err error) error {
	return fmt.Errorf("%w: the session was seated in another call at the same moment: %w", callrules.ErrConflict, err)
}

// gates takes the write gates of one decision, in the order given, which every caller
// keeps as account, key, booking, room, server (see the file note), within the calls share of the
// process's gate pool.
//
// A BOUNDED SHARE OF THE POOL. The gate pool is the process's, every
// write gate of every feature takes its connection there (component/database gate_pool.go:
// four per process), and a gate holds its connection from the moment it asks for its
// locks, waiting included. Calls decisions serialize on a few locks -- every join, poll and
// leave of one media server's calls on calls.server.<id>, across both replicas -- so a
// burst of them waiting behind one lock would hold the whole pool, and every other
// feature's write gate in the process (an artifact upload, a campaign link) would wait
// out its six seconds and answer busy. So at most callsGateShare calls decisions hold a
// gate connection at once, and the rest wait HERE, in the process, for a slot, rather
// than in the pool.
//
// THE WAIT IS THE GATE'S OWN, SHARED. A decision waits callsGateWait in all, a slot first
// and then its locks with what is left, so it waits no longer than a gate alone would; one
// that finds no slot in time answers memql.ErrWriteGateBusy, exactly as a gate that could
// not be taken answers: nothing attempted, try again. The slot is given back with the
// gates. COST: under a burst, calls decisions queue a little longer in the process; the
// share is per pool and at most half of a pool of two or more (gateSlotsFor).
func (s *Service) gates(ctx context.Context, keys ...string) (func(), error) {
	pool, err := memql.ResolveWriteGatePool(s.gateDB, "calls: a call's admission", "the calls plug-in takes it from PluginContext.GateDB")
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(callsGateWait)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	wait, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	slots := gateSlotsFor(pool)
	select {
	case slots <- struct{}{}:
	case <-wait.Done():
		return nil, fmt.Errorf("%w: calls decisions hold their share of this process's write gates (%d), and none was given back in time: %w",
			memql.ErrWriteGateBusy, cap(slots), wait.Err())
	}
	left := time.Until(deadline)
	if left <= 0 {
		<-slots
		return nil, fmt.Errorf("%w: the wait for a calls write gate ran out: %w", memql.ErrWriteGateBusy, context.DeadlineExceeded)
	}
	release, err := memql.AcquireWriteGatesWithin(ctx, pool, left, keys...)
	if err != nil {
		<-slots
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			release()
			<-slots
		})
	}, nil
}

// callsGateShare is how many of one process's write-gate connections calls decisions may
// hold at once: two of the bff's four (database.DefaultGatePoolSize).
const callsGateShare = 2

// callsGateWait is how long a calls decision waits, in all, for a slot of the share and
// then for its gates: the six seconds a write gate waits by default (component/memql
// write_gate.go writeGateWait).
const callsGateWait = 6 * time.Second

// callsGateSlots is each gate pool's calls share: one counting channel per pool.
var callsGateSlots sync.Map // *sql.DB -> chan struct{}

// gateSlotsFor is the calls share of one gate pool, made the first time the pool is seen:
// callsGateShare slots, and at most half the pool, so a pool configured smaller than the
// bff's keeps a connection for everything else. A pool of one connection
// (MEMQL_DB_GATE_POOL_SIZE=1) gets one slot: holding its only connection back from calls
// would refuse every call, so there calls take their turn on it with everything else.
func gateSlotsFor(pool *sql.DB) chan struct{} {
	if slots, ok := callsGateSlots.Load(pool); ok {
		return slots.(chan struct{})
	}
	share := callsGateShare
	if size := pool.Stats().MaxOpenConnections; size > 0 {
		share = max(1, min(share, size/2))
	}
	slots, _ := callsGateSlots.LoadOrStore(pool, make(chan struct{}, share))
	return slots.(chan struct{})
}

func keyGate(keyID string) string       { return "calls.key." + memql.BareShortId(keyID) }
func roomGate(roomID string) string     { return "calls.room." + memql.BareShortId(roomID) }
func serverGate(serverID string) string { return "calls.server." + memql.BareShortId(serverID) }

// pepperOr is the key pepper, or miss for a cluster whose media bootstrap never ran:
// with no pepper no key, session or link was ever issued, so whatever was presented
// opens nothing.
func (s *Service) pepperOr(ctx context.Context, miss error) ([]byte, error) {
	pepper, err := s.pepper(ctx)
	if errors.Is(err, errNoPepper) {
		return nil, fmt.Errorf("%w: calls are not set up on this cluster", miss)
	}
	return pepper, err
}

// credential is the API key pair a media server's passes are signed with and its
// control API is called with, read from the sealed secret its row names.
func (s *Service) credential(ctx context.Context, server MediaServer) (MediaCredential, error) {
	if strings.TrimSpace(server.CredentialSecret) == "" {
		return MediaCredential{}, fmt.Errorf("calls: media server %s names no credential", server.ID)
	}
	plaintext, err := s.store.resolveSecret(ctx, server.CredentialSecret)
	if err != nil {
		return MediaCredential{}, quietly("calls: read the credential of media server "+server.ID, err)
	}
	return ParseMediaCredential(plaintext)
}

// admitted is an admission's reply: the server's public address, a pass for the seat's
// identity in the call's media room, and the pass's lifetime.
func admitted(server MediaServer, credential MediaCredential, mediaRoom string, seat seatRow, policy callrules.Policy) (callrules.JoinResult, error) {
	pass, err := MintPass(credential.Key, credential.Secret, callrules.SeatKind(seat.Kind), mediaRoom, seat.Identity, seat.DisplayName, nil, policy.PassTTL)
	if err != nil {
		return callrules.JoinResult{}, err
	}
	return callrules.JoinResult{
		Outcome: callrules.OutcomeAdmit, URL: server.PublicURL, Pass: pass, Identity: seat.Identity,
		Host: seat.Host, ExpiresIn: policy.PassTTL,
	}, nil
}

func refuseJoin(reason string) callrules.JoinResult {
	return callrules.JoinResult{Outcome: callrules.OutcomeRefuse, Reason: reason}
}

func rateRefusal(retryAfter time.Duration) callrules.JoinResult {
	refused := refuseJoin(callrules.ReasonRate)
	refused.RetryAfter = retryAfter
	return refused
}

// queued is a place in line, counted from one.
func queued(reason string, position int, policy callrules.Policy) callrules.JoinResult {
	return callrules.JoinResult{Outcome: callrules.OutcomeQueue, Reason: reason, Position: position + 1, RetryAfter: policy.QueuePoll}
}

func waited(reason string, policy callrules.Policy) callrules.JoinResult {
	return callrules.JoinResult{Outcome: callrules.OutcomeWait, Reason: reason, RetryAfter: policy.QueuePoll}
}

// state is the session a join answers when it decided nothing: the one presented, if
// it was usable, and none otherwise.
func (r *sessionRow) state(presented *callrules.Session, site callrules.SiteRef) callrules.Session {
	if r == nil || presented == nil {
		return callrules.Session{}
	}
	return sessionOf(r, strings.TrimSpace(presented.Token), site)
}

func (r *sessionRow) stateOf(token string, site callrules.SiteRef) callrules.Session {
	if r == nil {
		return callrules.Session{}
	}
	return sessionOf(r, token, site)
}

// keyHeld is the key a session was opened with, or "" for a link's session or none: what
// a seat the session takes records (seatRow.KeyID), from the session row and never from
// the request.
func (r *sessionRow) keyHeld() string {
	if r == nil {
		return ""
	}
	return r.KeyID
}

// keyActive is the key concept's rule: an absent status reads as active.
func keyActive(status string) bool { return status == "" || status == "active" }

// checkSite holds a SiteRef to what every act needs: a deployable, and the owner its
// calls rows belong to. Ids are taken bare, as the calls rows and queries compare them.
// A deployable with no owner holds no calls rows, so nothing on it can be found.
func checkSite(site callrules.SiteRef) (callrules.SiteRef, error) {
	site.SiteID = memql.BareShortId(strings.TrimSpace(site.SiteID))
	site.OwnerUserID = memql.BareShortId(strings.TrimSpace(site.OwnerUserID))
	site.Deployable = strings.TrimSpace(site.Deployable)
	if site.SiteID == "" {
		return site, fmt.Errorf("%w: no deployable is named", callrules.ErrInvalid)
	}
	if site.OwnerUserID == "" {
		return site, fmt.Errorf("%w: the deployable has no owner", callrules.ErrNotFound)
	}
	return site, nil
}

// checkJoiner holds what a person brings to a seat: a name other participants can read,
// and all three confirmations every seat records -- 18 or older, no one under 18 on
// their camera or screen, and the terms version they accept. Anything missing is
// ErrInvalid, before anything is read or written.
func checkJoiner(name string, c callrules.Confirmation) (string, callrules.Confirmation, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxDisplayNameRunes || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", c, fmt.Errorf("%w: a name of 1 to %d characters is needed", callrules.ErrInvalid, maxDisplayNameRunes)
	}
	if !c.Adult {
		return "", c, fmt.Errorf("%w: the person has not confirmed they are 18 or older", callrules.ErrInvalid)
	}
	if !c.NoMinors {
		return "", c, fmt.Errorf("%w: the person has not confirmed that no one under 18 will appear on their camera or screen", callrules.ErrInvalid)
	}
	c.Terms = strings.TrimSpace(c.Terms)
	if c.Terms == "" || utf8.RuneCountInString(c.Terms) > maxTermsVersionRunes || strings.IndexFunc(c.Terms, unicode.IsControl) >= 0 {
		return "", c, fmt.Errorf("%w: the terms version the person accepts is needed", callrules.ErrInvalid)
	}
	return name, c, nil
}

func (r *sessionRow) credentialHeld() string {
	if r == nil {
		return ""
	}
	return r.CredentialID
}

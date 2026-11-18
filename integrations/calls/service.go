package calls

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// service.go -- the calls service: a key holder unlocks a session, starts a call and is
// seated as its host; people join by link, wait behind a lock or in the media server's
// queue, poll, and leave (design sections 7.7, 8 and 10). The rules are the pure
// module's (component/calls); this file reads the state they decide from, under the
// locks that make that state one replica's at a time, and writes what they decided.
//
// # Bearer secrets, and what a stranger costs
//
// A key, a session token and a call's two links are bearer secrets. Each is stored
// only as calls.Digest under the keys' pepper and looked up within one deployable, so
// a secret of one deployable opens nothing at another, and a plain secret is handed
// out once and never stored or logged. A presented secret's SHAPE is checked before
// anything is read (a key must parse; a token or a link is exactly what NewToken makes
// of tokenBytes), so a guess of the wrong shape costs no read.
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
//   - "calls.key.<id>" serializes one key's calls, which Start takes so a key holds one
//     call at a time.
//
// Every write that takes or frees a server seat or changes a queue entry holds the
// room's gate and the server's together. The ORDER IS FIXED -- key, then room, then
// server -- and every caller takes its gates in one call in that order, which rules out
// two callers each holding a lock the other waits for. A room's server never changes
// after placement, so it is read before the gates are taken and the room is read again
// inside them.
//
// # Capacity and the queue
//
// A server's free seats are its seats less the admitted and in-call seats of its open
// rooms (serverLoad.free). A new ask is handed DecideJoin's capacity as those free seats
// less the live waiters who drain before it (calls.OrderQueue: class, then arrival), so
// nobody jumps the line: someone joining a running call is ClassRunningCall and a new
// call's host ClassNewCall. A waiter is promoted only on its OWN poll (Queue), when its
// place in line is inside the free seats, because only its poll's reply can carry its
// pass; Leave frees a seat and promotes nobody. A waiter that has not polled for its
// deployable's QueueEntryTTL is not counted, and its entry is marked expired when a
// request of its own call next holds the gates. A non-host waiter in a locked call is
// HELD: it keeps its entry and its arrival while it polls, but counts in nobody's place
// until its call is unlocked, so a lock never keeps a free seat from the rest of the
// line (serverLoad.queue).
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
// retry. Start opens the media room before any row and writes the room row LAST: a
// room row whose seat was never written would hold its key's one call open with nobody
// in it. A join writes the room's count first (the attempt counts whatever follows),
// then the session, then the seat: a session pointing at a seat that was never written
// is seated nowhere, which the next join reads as free.

// tokenBytes is the randomness in a session token and in each link: 160 bits, which no
// guessing at any request rate reaches, spelled as 32 symbols by calls.NewToken.
const tokenBytes = 20

// identityBytes is the randomness in a seat's media identity. An identity is a
// participant's name on the media server, which every participant sees, so it is
// random rather than the seat's row id, and never the display name: clients label a
// participant by its kind and identity alone.
const identityBytes = 10

// The bounds on what a person types at a join: a name other participants see and the
// version of the terms they accept.
const (
	maxDisplayNameRunes  = 64
	maxTermsVersionRunes = 64
)

// issuedToken is every token and link this service hands out (calls.NewToken over
// tokenBytes).
var issuedToken = regexp.MustCompile(`^[a-z2-7]{32}$`)

// Service is the calls service. New builds it beside the integration's operator acts,
// over the same store, gate pool, pepper, clock and randomness.
type Service struct {
	store  *Store
	gateDB func() *sql.DB
	pepper func(context.Context) ([]byte, error)
	random io.Reader
	now    func() time.Time
	media  func(MediaServer, MediaCredential) (MediaBackend, error)
	logger *slog.Logger
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
	if !issuedToken.MatchString(token) {
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
// already running. A key holds one call at a time.
func (s *Service) Start(ctx context.Context, sess callrules.Session, body callrules.StartBody) (callrules.StartResult, error) {
	name, confirm, err := checkJoiner(body.Name, body.Confirm)
	if err != nil {
		return callrules.StartResult{}, err
	}
	site, err := checkSite(sess.Site)
	if err != nil {
		return callrules.StartResult{}, fmt.Errorf("%w: %v", callrules.ErrNoSession, err)
	}
	row, pepper, err := s.session(ctx, site, sess.Token, s.now())
	if err != nil {
		return callrules.StartResult{}, err
	}
	// ONLY A SESSION A KEY OPENED STARTS A CALL. A session a call's host link opened is
	// that call's host and holds no key, so it starts nothing on the key's account; a
	// guest's starts nothing at all.
	if row.Kind != callrules.SessionHost || row.KeyID == "" {
		return callrules.StartResult{}, fmt.Errorf("%w: only a session opened with a key starts a call", callrules.ErrForbidden)
	}
	policy, err := s.store.PolicyFor(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return callrules.StartResult{}, err
	}
	// The gated phase asks all of this again. Asking first, without the gates, is what
	// keeps a refused Start from taking a lock or opening a media room.
	if refused, err := s.startRefusal(ctx, site, row); err != nil {
		return callrules.StartResult{Join: refused}, err
	}
	server, err := s.place(ctx)
	if err != nil {
		return callrules.StartResult{}, err
	}
	if server == nil {
		return callrules.StartResult{Join: refuseJoin(callrules.ReasonNoMediaServer)},
			fmt.Errorf("%w: no media server is taking new calls", callrules.ErrConflict)
	}
	credential, err := s.credential(ctx, *server)
	if err != nil {
		return callrules.StartResult{}, err
	}
	backend, err := s.media(*server, credential)
	if err != nil {
		return callrules.StartResult{}, fmt.Errorf("calls: reach media server %s: %w", server.ID, err)
	}
	call := startedCall{roomID: id.NewShortId(), name: name, confirm: confirm}
	// THE MEDIA ROOM BEFORE ANY ROW, and outside the gates. Before any row so a room
	// that could not be opened leaves nothing behind; outside the gates because the
	// server's gate serializes every admission on the server, and a network call under
	// it would hold all of them up. A gated check that refuses after this leaves an
	// empty media room, which the server closes on its own.
	if err := backend.CreateRoom(ctx, call.roomID, uint32(policy.MaxPeople+policy.MaxAgents+1)); err != nil {
		return callrules.StartResult{}, fmt.Errorf("calls: open the call's media room: %w", err)
	}
	links := callrules.CallLinks{ID: call.roomID}
	if links.GuestToken, err = callrules.NewToken(s.random, tokenBytes); err != nil {
		return callrules.StartResult{}, err
	}
	if links.HostToken, err = callrules.NewToken(s.random, tokenBytes); err != nil {
		return callrules.StartResult{}, err
	}
	if call.identity, err = callrules.NewToken(s.random, identityBytes); err != nil {
		return callrules.StartResult{}, err
	}
	call.guestDigest = callrules.Digest(pepper, []byte(links.GuestToken))
	call.hostDigest = callrules.Digest(pepper, []byte(links.HostToken))

	host, seat, err := s.startGated(ctx, site, row, policy, *server, call)
	if err != nil || host.Outcome == callrules.OutcomeRefuse {
		return callrules.StartResult{Join: host}, err
	}
	if host.Outcome == callrules.OutcomeAdmit {
		if host, err = admitted(*server, credential, call.roomID, seat, policy); err != nil {
			return callrules.StartResult{}, err
		}
	}
	return callrules.StartResult{Call: links, Join: host}, nil
}

// startedCall is what Start decided before its gates: the room's id, its links'
// digests and the host's seat.
type startedCall struct {
	roomID, guestDigest, hostDigest, identity, name string
	confirm                                         callrules.Confirmation
}

// startRefusal is what refuses a Start before anything is placed: a key that is not
// active, a key with a call open, and a session seated in a call still running. The
// key comes first so a holder starting again in their own open call hears that the key
// is in a call.
func (s *Service) startRefusal(ctx context.Context, site callrules.SiteRef, row *sessionRow) (callrules.JoinResult, error) {
	key, err := s.store.keyByID(ctx, site.OwnerUserID, row.KeyID)
	if err != nil {
		return callrules.JoinResult{}, err
	}
	if key == nil || key.SiteID != site.SiteID || !keyActive(key.Status) {
		return refuseJoin(callrules.ReasonKeyNotActive), fmt.Errorf("%w: the key opens no new call", callrules.ErrForbidden)
	}
	open, err := s.store.openRoomsForKey(ctx, site.OwnerUserID, key.ID)
	if err != nil {
		return callrules.JoinResult{}, err
	}
	if len(open) > 0 {
		return refuseJoin(callrules.ReasonKeyInCall), fmt.Errorf("%w: the key already has a call open", callrules.ErrConflict)
	}
	busy, err := s.seatedElsewhere(ctx, site.OwnerUserID, row, "")
	if err != nil {
		return callrules.JoinResult{}, err
	}
	if busy {
		return refuseJoin(callrules.ReasonInAnotherCall), fmt.Errorf("%w: the session is in another call", callrules.ErrConflict)
	}
	return callrules.JoinResult{}, nil
}

// startGated is Start's decision and its writes, under the key's, the new room's and
// the server's gates. It answers the host's admission without a pass (the caller mints
// it once the gates are released) and the host's seat.
func (s *Service) startGated(ctx context.Context, site callrules.SiteRef, row *sessionRow, policy callrules.Policy, server MediaServer, call startedCall) (callrules.JoinResult, seatRow, error) {
	release, err := s.gates(ctx, keyGate(row.KeyID), roomGate(call.roomID), serverGate(server.ID))
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	owner := site.OwnerUserID
	now := s.now()

	current, err := s.store.sessionByID(fresh, owner, row.ID)
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	if !current.usable(now) {
		return callrules.JoinResult{}, seatRow{}, fmt.Errorf("%w: the session ended", callrules.ErrNoSession)
	}
	if refused, err := s.startRefusal(fresh, site, current); err != nil {
		return refused, seatRow{}, err
	}
	// A drain between placement and the gate takes the server out of placement; the
	// holder asks again and is placed elsewhere.
	placed, err := s.store.MediaServerByID(fresh, server.ID)
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	if placed == nil || placed.State != "active" {
		return refuseJoin(callrules.ReasonNoMediaServer), seatRow{}, fmt.Errorf("%w: the media server stopped taking new calls", callrules.ErrConflict)
	}
	load, err := s.store.loadOn(fresh, *placed)
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	ttls, err := s.queueLifetimes(fresh, site, policy, load)
	if err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	live, held, _ := load.queue(now, ttls)
	// The host's seat is the room's first join attempt.
	window, ok, retry := callrules.CountJoinAttempt(callrules.JoinWindow{}, now, policy.RoomJoinsPerMinute, callrules.JoinWindowLength)
	if !ok {
		return rateRefusal(retry), seatRow{}, fmt.Errorf("%w: the room takes no join attempts", callrules.ErrConflict)
	}
	arrives := arrival(now, live, held)
	entry := callrules.QueueEntry{ID: id.NewShortId(), Class: callrules.ClassNewCall, EnqueuedAt: arrives, LastPollAt: arrives}
	position := placeInLine(live, entry)
	decision := callrules.DecideJoin(callrules.RoomState{}, callrules.JoinAsk{Kind: callrules.KindPerson, Host: true}, policy, load.free()-position)
	if decision.Outcome != callrules.OutcomeAdmit && decision.Outcome != callrules.OutcomeQueue {
		// An empty room refuses a host only under a policy with no people, which the
		// cluster's bounds forbid; refused rather than seated if it ever does.
		return refuseJoin(decision.Reason), seatRow{}, fmt.Errorf("%w: %s", callrules.ErrConflict, decision.Reason)
	}

	seat := seatRow{
		ID: id.NewShortId(), RoomID: call.roomID, Identity: call.identity, Kind: string(callrules.KindPerson),
		DisplayName: call.name, Status: seatAdmitted, Host: true,
	}
	joinedAt := now
	if decision.Outcome == callrules.OutcomeQueue {
		seat.Status, joinedAt = seatWaiting, time.Time{}
	}
	// The order is the file note's: the session, the seat, its queue entry, and the room
	// last.
	if err := s.store.seatSession(fresh, owner, current, seat.ID, call.roomID, memql.VersionTimeAfter(current.Version, now)); err != nil {
		if errors.Is(err, memql.ErrRowVersionChanged) {
			return refuseJoin(callrules.ReasonInAnotherCall), seatRow{}, s.movedSession(err)
		}
		return callrules.JoinResult{}, seatRow{}, err
	}
	if err := s.store.createSeat(fresh, owner, newSeat{
		ID: seat.ID, SiteID: site.SiteID, RoomID: call.roomID, Identity: seat.Identity, Kind: seat.Kind,
		DisplayName: seat.DisplayName, Status: seat.Status, Host: true, Confirm: call.confirm, JoinedAt: joinedAt,
	}, now); err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	if decision.Outcome == callrules.OutcomeQueue {
		if err := s.store.createQueueEntry(fresh, owner, newQueueEntry{
			ID: entry.ID, SiteID: site.SiteID, RoomID: call.roomID, SeatID: seat.ID, KeyID: current.KeyID,
			Class: entry.Class, EnqueuedAt: entry.EnqueuedAt,
		}, now); err != nil {
			return callrules.JoinResult{}, seatRow{}, err
		}
	}
	if err := s.store.createRoom(fresh, owner, newRoom{
		ID: call.roomID, SiteID: site.SiteID, Deployable: site.Deployable, KeyID: current.KeyID,
		MediaServerID: server.ID, MediaRoom: call.roomID, HostSeatID: seat.ID,
		GuestDigest: call.guestDigest, HostDigest: call.hostDigest, OpenedAt: now, Window: window,
	}, now); err != nil {
		return callrules.JoinResult{}, seatRow{}, err
	}
	if decision.Outcome == callrules.OutcomeQueue {
		return queued(decision.Reason, position, policy), seat, nil
	}
	return callrules.JoinResult{Outcome: callrules.OutcomeAdmit}, seat, nil
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
// with neither, in the call the presented session is seated in. A session already
// seated in the call keeps its seat and its media identity; a person with no usable
// session gets one of their own, which the result carries so a route can set it.
//
// The answer is an admission with a pass, a wait behind a lock, a place in the media
// server's queue, or a refusal with its reason. A refusal returns its JoinResult with
// ErrEnded or ErrConflict; a wait and a queue are not errors.
func (s *Service) Join(ctx context.Context, site callrules.SiteRef, sess *callrules.Session, body callrules.JoinBody) (callrules.JoinResult, callrules.Session, error) {
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
	if link != "" && !issuedToken.MatchString(link) {
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
		if issuedToken.MatchString(token) {
			found, err := s.store.sessionByDigest(ctx, owner, site.SiteID, callrules.Digest(pepper, []byte(token)))
			if err != nil {
				return callrules.JoinResult{}, none, err
			}
			if found.usable(now) {
				held = found
			}
		}
	}
	room, err := s.roomFor(ctx, site, pepper, link, asHost, held)
	if err != nil {
		return callrules.JoinResult{}, held.state(sess, site), err
	}
	if !room.open() {
		return refuseJoin("call_ended"), held.state(sess, site), fmt.Errorf("%w: the call has ended", callrules.ErrEnded)
	}
	policy, err := s.store.PolicyFor(ctx, owner, site.SiteID)
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
	ask := joinAsk{name: name, confirm: confirm, asHost: asHost, pepper: pepper}
	if presented {
		ask.token = strings.TrimSpace(sess.Token)
	}
	result, out, seat, err := s.joinGated(ctx, site, *room, *server, policy, held, ask)
	if err != nil || result.Outcome != callrules.OutcomeAdmit {
		return result, out, err
	}
	if result, err = admitted(*server, credential, room.MediaRoom, seat, policy); err != nil {
		return callrules.JoinResult{}, out, err
	}
	return result, out, nil
}

// joinAsk is what a join brings to its gated phase.
type joinAsk struct {
	name    string
	confirm callrules.Confirmation
	asHost  bool   // the host link was presented
	token   string // the presented session's token, when one was presented
	pepper  []byte
}

// roomFor is the call a join names: by its link, or the call the session is seated in.
func (s *Service) roomFor(ctx context.Context, site callrules.SiteRef, pepper []byte, link string, asHost bool, held *sessionRow) (*roomRow, error) {
	owner := site.OwnerUserID
	if link != "" {
		room, err := s.store.roomByLink(ctx, owner, site.SiteID, callrules.Digest(pepper, []byte(link)), asHost)
		if err != nil {
			return nil, err
		}
		if room == nil {
			return nil, fmt.Errorf("%w: the link opens no call on this deployable", callrules.ErrNotFound)
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
// gates. It answers an admission without a pass (the caller mints it once the gates
// are released), the session the joiner now holds, and the seat.
func (s *Service) joinGated(ctx context.Context, site callrules.SiteRef, room roomRow, server MediaServer, policy callrules.Policy, held *sessionRow, ask joinAsk) (callrules.JoinResult, callrules.Session, seatRow, error) {
	release, err := s.gates(ctx, roomGate(room.ID), serverGate(server.ID))
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
	if err := s.store.countJoin(fresh, owner, *current, window, memql.VersionTimeAfter(current.Version, now)); err != nil {
		return callrules.JoinResult{}, held.stateOf(ask.token, site), seatRow{}, err
	}

	// The session and its seat, as they are now.
	var seat *seatRow
	if held != nil {
		if held, err = s.store.sessionByID(fresh, owner, held.ID); err != nil {
			return callrules.JoinResult{}, callrules.Session{}, seatRow{}, err
		}
		if !held.usable(now) {
			held = nil
		}
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
	ttls, err := s.queueLifetimes(fresh, site, policy, load)
	if err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	live, behindLocks, expired := load.queue(now, ttls)
	if err := s.expire(fresh, owner, current.ID, expired, now); err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	host := ask.asHost || (reentry && seat.Host)
	arrives := arrival(now, live, behindLocks)
	entry := callrules.QueueEntry{ID: id.NewShortId(), Class: callrules.ClassRunningCall, EnqueuedAt: arrives, LastPollAt: arrives}
	position := placeInLine(live, entry)
	decision := callrules.DecideJoin(load.roomState(*current), callrules.JoinAsk{Kind: callrules.KindPerson, Host: host}, policy, load.free()-position)
	if decision.Outcome == callrules.OutcomeRefuse {
		return refuseJoin(decision.Reason), out, seatRow{}, fmt.Errorf("%w: %s", callrules.ErrConflict, decision.Reason)
	}

	taken := seatRow{
		ID: id.NewShortId(), RoomID: current.ID, Kind: string(callrules.KindPerson),
		DisplayName: ask.name, Status: seatWaiting, Host: host,
	}
	if reentry {
		taken.ID, taken.Identity = seat.ID, seat.Identity
	} else if taken.Identity, err = callrules.NewToken(s.random, identityBytes); err != nil {
		return callrules.JoinResult{}, out, seatRow{}, err
	}
	joinedAt := time.Time{}
	if decision.Outcome == callrules.OutcomeAdmit {
		taken.Status, joinedAt = seatAdmitted, now
	}

	// The session first: a session pointing at a seat that was never written is seated
	// nowhere, which the next join reads as free.
	if held == nil {
		token, err := callrules.NewToken(s.random, tokenBytes)
		if err != nil {
			return callrules.JoinResult{}, out, seatRow{}, err
		}
		kind := callrules.SessionGuest
		if ask.asHost {
			// A host link's session is that call's host and holds no key: it starts
			// nothing (Start).
			kind = callrules.SessionHost
		}
		created := newSession{
			ID: id.NewShortId(), SiteID: site.SiteID, TokenDigest: callrules.Digest(ask.pepper, []byte(token)),
			Kind: kind, SeatID: taken.ID, RoomID: current.ID, ExpiresAt: now.Add(callrules.SessionLifetime),
		}
		if err := s.store.createSession(fresh, owner, created, now); err != nil {
			return callrules.JoinResult{}, out, seatRow{}, err
		}
		out = callrules.Session{Token: token, Kind: kind, SeatID: taken.ID, RoomID: current.ID, ExpiresAt: created.ExpiresAt, Site: site}
	} else {
		// Written even when the seat is the session's own (a re-entry): the write is
		// what makes two joins moving one session at once meet on its version, under
		// whichever two room gates they hold.
		if err := s.store.seatSession(fresh, owner, held, taken.ID, current.ID, memql.VersionTimeAfter(held.Version, now)); err != nil {
			if errors.Is(err, memql.ErrRowVersionChanged) {
				return refuseJoin(callrules.ReasonInAnotherCall), out, seatRow{}, s.movedSession(err)
			}
			return callrules.JoinResult{}, out, seatRow{}, err
		}
		out.SeatID, out.RoomID = taken.ID, current.ID
	}
	if reentry {
		fields := map[string]any{"status": taken.Status, "host": taken.Host}
		if !joinedAt.IsZero() {
			fields["joinedAt"] = joinedAt
		}
		if _, err := s.recordJoin(fresh, owner, *seat, ask, fields, now); err != nil {
			return callrules.JoinResult{}, out, seatRow{}, err
		}
	} else if err := s.store.createSeat(fresh, owner, newSeat{
		ID: taken.ID, SiteID: site.SiteID, RoomID: current.ID, Identity: taken.Identity, Kind: taken.Kind,
		DisplayName: taken.DisplayName, Status: taken.Status, Host: taken.Host, Confirm: ask.confirm, JoinedAt: joinedAt,
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
	return callrules.JoinResult{Outcome: callrules.OutcomeAdmit}, out, taken, nil
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
func (s *Service) Queue(ctx context.Context, sess callrules.Session) (callrules.JoinResult, error) {
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
	return admitted(*server, credential, room.MediaRoom, after, policy)
}

// pollGated is a poll's decision and its writes, under the room's and the server's
// gates.
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
// its lifetime -- asks as a newcomer at the back of the line, in the class it last
// waited in.
func (s *Service) answerWaiting(ctx context.Context, site callrules.SiteRef, room roomRow, server MediaServer, policy callrules.Policy, seat seatRow, now time.Time) (callrules.JoinResult, seatRow, error) {
	owner := site.OwnerUserID
	load, err := s.store.loadOn(ctx, server)
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
	entry := callrules.QueueEntry{ID: id.NewShortId(), Class: callrules.ClassRunningCall, EnqueuedAt: arrives, LastPollAt: arrives}
	if stored != nil {
		entry.Class = stored.Class
		if kept {
			entry = stored.rule()
		}
	}
	// A held entry is placed by its own arrival among those in line: the place it takes
	// back when the lock lifts. While the lock holds, DecideJoin answers it "locked".
	position := placeInLine(live, entry)
	decision := callrules.DecideJoin(load.roomState(room), callrules.JoinAsk{Kind: callrules.SeatKind(seat.Kind), Host: seat.Host}, policy, load.free()-position)

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
	left, err := s.leaveGated(ctx, site.OwnerUserID, *room, seat.ID)
	if err != nil || left == nil || !left.occupying() {
		return err
	}
	// After the gates: a media server slow to answer must not hold up the server's
	// admissions. Best effort, and logged by the room and seat only.
	if err := s.disconnect(ctx, *room, left.Identity); err != nil {
		s.logger.Warn("calls: a seat left but its media identity could not be disconnected", "component", pluginName,
			"room", room.ID, "seat", left.ID, "reason", failureReason(err))
	}
	return nil
}

// leaveGated writes a seat's leaving under its room's and its server's gates, and
// answers the seat as it was, or nil when it had left already.
func (s *Service) leaveGated(ctx context.Context, owner string, room roomRow, seatID string) (*seatRow, error) {
	release, err := s.gates(ctx, roomGate(room.ID), serverGate(room.MediaServerID))
	if err != nil {
		return nil, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	now := s.now()
	seat, err := s.store.seatByID(fresh, owner, seatID)
	if err != nil || seat == nil || !seat.placed() {
		return nil, err
	}
	if err := s.store.updateSeat(fresh, owner, *seat, map[string]any{"status": seatLeft, "leftAt": now}, memql.VersionTimeAfter(seat.Version, now)); err != nil {
		return nil, err
	}
	if seat.Status == seatWaiting {
		entries, err := s.store.queueEntriesForRoom(fresh, owner, room.ID)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.SeatID != seat.ID || !entry.waiting() {
				continue
			}
			if err := s.store.updateQueueEntry(fresh, owner, entry, entryExpired, time.Time{}, memql.VersionTimeAfter(entry.Version, now)); err != nil {
				return nil, err
			}
		}
	}
	return seat, nil
}

// disconnect asks a call's media server to drop one identity from its media room.
func (s *Service) disconnect(ctx context.Context, room roomRow, identity string) error {
	server, err := s.store.MediaServerByID(ctx, room.MediaServerID)
	if err != nil || server == nil {
		return err
	}
	credential, err := s.credential(ctx, *server)
	if err != nil {
		return err
	}
	backend, err := s.media(*server, credential)
	if err != nil {
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
// keeps as key, room, server (see the file note).
func (s *Service) gates(ctx context.Context, keys ...string) (func(), error) {
	pool, err := memql.ResolveWriteGatePool(s.gateDB, "calls: a call's admission", "the calls plug-in takes it from PluginContext.GateDB")
	if err != nil {
		return nil, err
	}
	return memql.AcquireWriteGates(ctx, pool, keys...)
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

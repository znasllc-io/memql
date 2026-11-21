package calls

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	lkauth "github.com/livekit/protocol/auth"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	callrules "github.com/znasllc-io/memql/component/calls"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// service_db_test.go -- the calls service (service.go) against a real engine and a real
// database: unlocking a key, starting a call, joining, waiting, queueing and leaving.
//
// WHY A REAL ENGINE, for operator_db_test.go's reason: every claim below is about what
// the rows hold afterwards and what the @serverWritten and @serverOnly gates let the
// service's own stamps through. The media server is the one fake (fakeMedia): what the
// service asks of a media server's control API is recorded, and a pass is verified with
// the secret of the server it was minted for, through LiveKit's own verifier.
//
// Postgres-gated: an unreachable database skips, and MEMQL_REQUIRE_DB=1 turns the skip
// into a failure.

// testClock is the service's clock, moved by the test.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeMedia stands in for every media server's control API and records, per server,
// what each was asked. held is the rooms each server holds now, as livekit-server v1.13.7
// keeps them: opening a room it holds returns that room unchanged, its limits the ones it
// was opened with, deleting one forgets it, and a restart forgets them all (restart).
// present is who each media room shows connected, by room name, as a test sets it
// (setPresent): deleting a room disconnects everybody in it and removing a participant
// disconnects them, as a media server does.
type fakeMedia struct {
	mu         sync.Mutex
	created    []fakeMediaRoom
	held       map[fakeMediaRef]fakeMediaRoom
	removed    []fakeMediaRemoval
	deleted    []fakeMediaRef
	muted      []fakeMediaMute
	present    map[string][]MediaParticipant
	failCreate error
	failDelete error
}

type fakeMediaRoom struct {
	Server, Name string
	Max          uint32
}

type fakeMediaRemoval struct {
	Server, Room, Identity string
}

type fakeMediaRef struct {
	Server, Room string
}

type fakeMediaMute struct {
	Server, Room, Identity, Track string
}

// setPresent is who a media room shows connected from now on.
func (f *fakeMedia) setPresent(room string, people ...MediaParticipant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.present == nil {
		f.present = map[string][]MediaParticipant{}
	}
	f.present[room] = slices.Clone(people)
}

func (f *fakeMedia) deletions() []fakeMediaRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deleted)
}

// restart is every media server restarting: each forgets the rooms it held and everybody
// connected to them, and opens none again until it is asked.
func (f *fakeMedia) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held, f.present = nil, nil
}

// holding is the room a media server holds by that name now, and whether it holds one.
func (f *fakeMedia) holding(server, name string) (fakeMediaRoom, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	room, ok := f.held[fakeMediaRef{Server: server, Room: name}]
	return room, ok
}

func (f *fakeMedia) mutes() []fakeMediaMute {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.muted)
}

// person is a participant as a media server shows one: the seat's identity, a
// person's kind, and the tracks given.
func person(identity string, tracks ...MediaTrack) MediaParticipant {
	return MediaParticipant{Identity: identity, Kind: string(callrules.KindPerson), Tracks: tracks}
}

func (f *fakeMedia) backend(server MediaServer, credential MediaCredential) (MediaBackend, error) {
	if credential.Key == "" || credential.Secret == "" {
		return nil, errors.New("fake media: no credential")
	}
	return &fakeMediaServer{media: f, server: server.ID}, nil
}

func (f *fakeMedia) rooms() []fakeMediaRoom {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.created)
}

func (f *fakeMedia) removals() []fakeMediaRemoval {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}

type fakeMediaServer struct {
	media  *fakeMedia
	server string
}

func (b *fakeMediaServer) CreateRoom(_ context.Context, name string, max uint32) error {
	b.media.mu.Lock()
	defer b.media.mu.Unlock()
	if b.media.failCreate != nil {
		return b.media.failCreate
	}
	room := fakeMediaRoom{Server: b.server, Name: name, Max: max}
	b.media.created = append(b.media.created, room)
	if b.media.held == nil {
		b.media.held = map[fakeMediaRef]fakeMediaRoom{}
	}
	if _, open := b.media.held[fakeMediaRef{Server: b.server, Room: name}]; !open {
		b.media.held[fakeMediaRef{Server: b.server, Room: name}] = room
	}
	return nil
}

func (b *fakeMediaServer) DeleteRoom(_ context.Context, name string) error {
	b.media.mu.Lock()
	defer b.media.mu.Unlock()
	if b.media.failDelete != nil {
		return b.media.failDelete
	}
	b.media.deleted = append(b.media.deleted, fakeMediaRef{Server: b.server, Room: name})
	delete(b.media.held, fakeMediaRef{Server: b.server, Room: name})
	delete(b.media.present, name)
	return nil
}

func (b *fakeMediaServer) Participants(_ context.Context, room string) ([]MediaParticipant, error) {
	b.media.mu.Lock()
	defer b.media.mu.Unlock()
	return slices.Clone(b.media.present[room]), nil
}

func (b *fakeMediaServer) RemoveParticipant(_ context.Context, room, identity string) error {
	b.media.mu.Lock()
	defer b.media.mu.Unlock()
	b.media.removed = append(b.media.removed, fakeMediaRemoval{Server: b.server, Room: room, Identity: identity})
	if people, ok := b.media.present[room]; ok {
		b.media.present[room] = slices.DeleteFunc(slices.Clone(people), func(p MediaParticipant) bool { return p.Identity == identity })
	}
	return nil
}

func (b *fakeMediaServer) MuteTrack(_ context.Context, room, identity, trackSID string) error {
	b.media.mu.Lock()
	defer b.media.mu.Unlock()
	b.media.muted = append(b.media.muted, fakeMediaMute{Server: b.server, Room: room, Identity: identity, Track: trackSID})
	return nil
}

// callWorld is one test's deployable with the calls service over it: its own
// integration, clock and media fake, sharing the engine and the database.
type callWorld struct {
	*world
	clock   *testClock
	media   *fakeMedia
	calls   *Integration
	svc     *Service
	ref     callrules.SiteRef
	servers []string // media servers this test added, besides the bootstrap one
	// mintedKeys is every key this test minted, by key id, as its holder would type it.
	mintedKeys map[string]string
}

var confirmed = callrules.Confirmation{Adult: true, NoMinors: true, Terms: "2026-11-17"}

// tokenShape is what calls.NewToken makes of twenty random bytes.
var tokenShape = regexp.MustCompile(`^[a-z2-7]{32}$`)

// newCallWorld is a world whose ONLY active media server is the bootstrap one, with
// the given seats, so placement and capacity are the test's to decide.
func newCallWorld(t *testing.T, name string, seats int) *callWorld {
	t.Helper()
	w := newWorld(t, name)
	if w == nil {
		return nil
	}
	resetBootstrap(t, w.db)
	cfg := testMediaConfig()
	cfg.Seats = seats
	_, err := w.in.Bootstrap(context.Background(), cfg)
	require.NoError(t, err)
	clock := &testClock{now: time.Now().UTC().Truncate(time.Microsecond)}
	media := &fakeMedia{}
	in := New(Deps{
		Engine:              liveEngine{w.eng},
		GateDB:              func() *sql.DB { return sharedBoot.gates },
		ResolveSystemSecret: w.eng.ResolveSystemSecret,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:                 clock.Now,
		NewMediaBackend:     media.backend,
	})
	cw := &callWorld{
		world: w, clock: clock, media: media, calls: in, svc: in.Service(),
		ref:        callrules.SiteRef{SiteID: w.site, OwnerUserID: w.owner, Deployable: "desk-" + w.suffix},
		mintedKeys: map[string]string{},
	}
	t.Cleanup(cw.sweepCalls)
	servers, err := in.store.ActiveMediaServers(context.Background())
	require.NoError(t, err)
	require.Len(t, servers, 1, "a media server another test left active would take this test's rooms: %+v", servers)
	return cw
}

// sweepCalls takes away every calls row of this test's sites and the media servers it
// added: the world's own sweep knows only keys and limits.
func (cw *callWorld) sweepCalls() {
	ctx := context.Background()
	concepts := []string{familyConcept, commerceConcept, sessionConcept, roomConcept, seatConcept, queueEntryConcept, scheduledCallConcept, invitationLinkConcept}
	for _, site := range cw.sites {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).
			Where("concept IN (?)", bun.In(concepts)).Where("payload->>'siteId' = ?", siteConcept+":"+site).Exec(ctx)
	}
	for _, server := range cw.servers {
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", mediaServerConcept).
			Where("id = ?", mediaServerConcept+":"+server).Exec(ctx)
		_, _ = cw.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", globalSecretConcept).
			Where("payload->>'name' = ?", "calls.media."+server).Exec(ctx)
	}
}

// addServer records a second active media server with its own credential.
func (cw *callWorld) addServer(seats int, apiSecret string) MediaServer {
	cw.t.Helper()
	ctx := context.Background()
	serverID := "cl-media-" + cw.suffix
	secretName := "calls.media." + serverID
	cw.servers = append(cw.servers, serverID)
	sealed, err := MediaCredential{Key: "APIsecondkey", Secret: apiSecret}.seal()
	require.NoError(cw.t, err)
	require.NoError(cw.t, cw.calls.store.sealSecret(ctx, secretName, sealed, "a second media server for a test"))
	server := MediaServer{
		ID: serverID, Provider: "docker-local", APIURL: "http://second.media.example.test:7880",
		PublicURL: "wss://second.media.example.test", Seats: seats, CredentialSecret: secretName,
	}
	require.NoError(cw.t, cw.calls.store.createMediaServer(ctx, server, time.Now()))
	return server
}

// mintKey writes a key for the site as its owner, with the given status, and answers
// the key as its holder would type it.
func (cw *callWorld) mintKey(status string) string {
	cw.t.Helper()
	raw, key, err := callrules.NewKey(rand.Reader)
	require.NoError(cw.t, err)
	keyID := id.NewShortId()
	row := newKey{ID: keyID, SiteID: cw.site, Digest: callrules.Digest(cw.pepper(), raw[:]), Kind: "test"}
	require.NoError(cw.t, cw.calls.store.createKey(context.Background(), cw.owner, row, time.Now()))
	cw.mintedKeys[keyID] = key
	if status != "active" {
		cw.asOwnerMutate(keyConcept, keyID, "updateCallsKey", map[string]any{"keyId": keyID, "status": status})
	}
	return key
}

// host unlocks a new active key and answers its host session.
func (cw *callWorld) host() callrules.Session {
	cw.t.Helper()
	sess, err := cw.svc.Unlock(context.Background(), cw.ref, cw.mintKey("active"))
	require.NoError(cw.t, err)
	return sess
}

// started is a call started by a new key's host, who was admitted.
func (cw *callWorld) started(name string) (callrules.Session, callrules.StartResult) {
	cw.t.Helper()
	sess := cw.host()
	res, err := cw.svc.Start(context.Background(), sess, callrules.StartBody{Name: name, Confirm: confirmed})
	require.NoError(cw.t, err)
	require.Equal(cw.t, callrules.OutcomeAdmit, res.Join.Outcome, "the host was not admitted: %+v", res.Join)
	sess, err = cw.svc.Session(context.Background(), cw.ref, sess.Token)
	require.NoError(cw.t, err)
	return sess, res
}

// join is a guest's first join by the guest link, with no session.
func (cw *callWorld) join(link, name string) (callrules.JoinResult, callrules.Session, error) {
	cw.t.Helper()
	return cw.svc.Join(context.Background(), cw.ref, nil, callrules.JoinBody{Token: link, Name: name, Confirm: confirmed})
}

// rejoin is a join that brings a session and, when given, a guest link.
func (cw *callWorld) rejoin(sess callrules.Session, link string) (callrules.JoinResult, callrules.Session, error) {
	cw.t.Helper()
	return cw.svc.Join(context.Background(), cw.ref, &sess, callrules.JoinBody{Token: link, Name: "Again", Confirm: confirmed})
}

// rowsOf is the newest version of every row of a calls concept on this test's site,
// straight off the table.
func (cw *callWorld) rowsOf(concept string) map[string]map[string]any {
	cw.t.Helper()
	var nodes []memorynodes.MemoryNode
	err := cw.db.NewRaw(`SELECT DISTINCT ON (id) * FROM "MemoryNodes" WHERE concept = ? AND payload->>'siteId' = ? ORDER BY id, "createdAt" DESC`,
		concept, siteConcept+":"+cw.site).Scan(context.Background(), &nodes)
	require.NoError(cw.t, err)
	out := map[string]map[string]any{}
	for _, node := range nodes {
		var payload map[string]any
		require.NoError(cw.t, json.Unmarshal(node.Payload, &payload))
		out[memql.BareShortId(node.ID)] = payload
	}
	return out
}

func (cw *callWorld) row(concept, rowID string) map[string]any {
	cw.t.Helper()
	payload, _ := latestPayload(cw.t, cw.db, concept, concept+":"+rowID)
	return payload
}

// versions is how many versions a row has.
func (cw *callWorld) versions(concept, rowID string) int {
	cw.t.Helper()
	return stateWhere(cw.t, cw.db, `concept = ? AND id = ?`, concept, concept+":"+rowID).Versions
}

// seatsIn is the newest version of every seat of one room, by id.
func (cw *callWorld) seatsIn(roomID string) map[string]map[string]any {
	cw.t.Helper()
	out := map[string]map[string]any{}
	for seatID, seat := range cw.rowsOf(seatConcept) {
		if bare(seat["roomId"]) == roomID {
			out[seatID] = seat
		}
	}
	return out
}

func (cw *callWorld) admittedIn(roomID string) int {
	n := 0
	for _, seat := range cw.seatsIn(roomID) {
		if seat["status"] == "admitted" || seat["status"] == "in_call" {
			n++
		}
	}
	return n
}

// asOwnerMutate writes a calls row as the site's owner, as the host controls will, at a
// version after the row's newest, whichever clock wrote that one.
func (cw *callWorld) asOwnerMutate(concept, rowID, name string, args map[string]any) {
	cw.t.Helper()
	owned, ok := asOwner(context.Background(), cw.owner)
	require.True(cw.t, ok)
	newest := stateWhere(cw.t, cw.db, `concept = ? AND id = ?`, concept, concept+":"+rowID).Newest
	args["versionTime"] = versionStamp(memql.VersionTimeAfter(newest, cw.clock.Now()))
	cw.mutate(owned, name, args)
}

// verifyPass checks a pass with the secret of the server it should have been minted
// for, through LiveKit's own verifier, and answers its grants and lifetime.
func verifyPass(t *testing.T, token, apiKey, apiSecret string) (*lkauth.ClaimGrants, time.Duration) {
	t.Helper()
	verifier, err := lkauth.ParseAPIToken(token)
	require.NoError(t, err, "the pass does not parse")
	require.Equal(t, apiKey, verifier.APIKey(), "the pass names another server's key")
	claims, grants, err := verifier.Verify(apiSecret)
	require.NoError(t, err, "the pass does not verify with the secret of the server it is for")
	require.NotNil(t, claims.ExpiresAt)
	require.NotNil(t, claims.NotBefore)
	return grants, claims.ExpiresAt.Sub(claims.NotBefore.Time)
}

func requireRefused(t *testing.T, res callrules.JoinResult, err error, sentinel error, reason string) {
	t.Helper()
	require.ErrorIs(t, err, sentinel, "refused with the wrong error: %v", err)
	require.Equal(t, callrules.OutcomeRefuse, res.Outcome, "a refusal answered %+v", res)
	require.Equal(t, reason, res.Reason)
	require.Empty(t, res.Pass, "a refusal carries a pass")
}

// ---------------------------------------------------------------------------
// unlock and sessions
// ---------------------------------------------------------------------------

// TestUnlockOpensAHostSessionForAnActiveKey: an active key opens a host session that
// lives a day. The session row holds the token's digest and never the token, and the
// token resolves on its own deployable only. A key that does not parse is invalid, a
// key nobody minted here is not found, and a frozen or banned key opens nothing.
func TestUnlockOpensAHostSessionForAnActiveKey(t *testing.T) {
	cw := newCallWorld(t, "unlock", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	key := cw.mintKey("active")

	sess, err := cw.svc.Unlock(ctx, cw.ref, key)
	require.NoError(t, err)
	require.Equal(t, callrules.SessionHost, sess.Kind)
	require.Regexp(t, tokenShape, sess.Token)
	require.NotEmpty(t, sess.KeyID)
	require.Empty(t, sess.SeatID, "a new host session is seated nowhere")
	require.Empty(t, sess.RoomID)
	require.Equal(t, cw.clock.Now().Add(callrules.SessionLifetime), sess.ExpiresAt.UTC())
	require.Equal(t, cw.ref, sess.Site)

	sessions := cw.rowsOf(sessionConcept)
	require.Len(t, sessions, 1)
	for _, stored := range sessions {
		require.Equal(t, callrules.Digest(cw.pepper(), []byte(sess.Token)), stored["tokenDigest"], "the row does not hold the token's digest")
		require.Equal(t, cw.owner, bare(stored["ownerUserId"]), "the session belongs to the site's owner")
		require.Equal(t, "host", stored["kind"])
		require.Equal(t, sess.KeyID, bare(stored["keyId"]))
	}
	require.Zero(t, cw.rowsContaining(sess.Token), "a stored row carries the session token")

	// Every spelling of the key opens the same thing, and the token resolves on its own
	// deployable and nowhere else.
	again, err := cw.svc.Unlock(ctx, cw.ref, strings.ToUpper(key))
	require.NoError(t, err)
	require.Equal(t, sess.KeyID, again.KeyID)
	resolved, err := cw.svc.Session(ctx, cw.ref, sess.Token)
	require.NoError(t, err)
	require.Equal(t, sess.KeyID, resolved.KeyID)
	require.Equal(t, callrules.SessionHost, resolved.Kind)
	_, err = cw.svc.Session(ctx, callrules.SiteRef{SiteID: cw.second, OwnerUserID: cw.owner}, sess.Token)
	require.ErrorIs(t, err, callrules.ErrNoSession, "a session of one deployable resolved at another")
	_, err = cw.svc.Session(ctx, cw.ref, "not-a-token")
	require.ErrorIs(t, err, callrules.ErrNoSession)

	_, err = cw.svc.Unlock(ctx, cw.ref, "ck_not-a-key")
	require.ErrorIs(t, err, callrules.ErrInvalid)
	_, stranger, err := callrules.NewKey(rand.Reader)
	require.NoError(t, err)
	_, err = cw.svc.Unlock(ctx, cw.ref, stranger)
	require.ErrorIs(t, err, callrules.ErrNotFound, "a key nobody minted opened a session")
	_, err = cw.svc.Unlock(ctx, callrules.SiteRef{SiteID: cw.second, OwnerUserID: cw.owner}, key)
	require.ErrorIs(t, err, callrules.ErrNotFound, "a key of one deployable opened a session at another")
	for _, status := range []string{"frozen", "banned"} {
		_, err = cw.svc.Unlock(ctx, cw.ref, cw.mintKey(status))
		require.ErrorIs(t, err, callrules.ErrForbidden, "a %s key opened a session", status)
	}
	require.Len(t, cw.rowsOf(sessionConcept), 2, "a refused unlock wrote a session")

	// A session opens nothing after its day.
	cw.clock.Advance(callrules.SessionLifetime)
	_, err = cw.svc.Session(ctx, cw.ref, sess.Token)
	require.ErrorIs(t, err, callrules.ErrNoSession, "an expired session still resolved")
}

// ---------------------------------------------------------------------------
// start
// ---------------------------------------------------------------------------

// TestStartOpensOneCallPerKey: a host session starts a call on the media server with
// the most free seats, in a media room named by the room's short id with a place for
// every person, every agent and the sampler, and the host is seated and admitted with
// all three confirmations recorded. While that call is open, a second Start on the same
// key, from the same session or another, is a conflict and writes nothing.
func TestStartOpensOneCallPerKey(t *testing.T) {
	cw := newCallWorld(t, "start", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	sess := cw.host()

	res, err := cw.svc.Start(ctx, sess, callrules.StartBody{Name: "Host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, res.Join.Outcome)
	require.True(t, res.Join.Host)
	require.NotEmpty(t, res.Join.Pass)
	require.Regexp(t, tokenShape, res.Call.GuestToken)
	require.Regexp(t, tokenShape, res.Call.HostToken)
	require.NotEqual(t, res.Call.GuestToken, res.Call.HostToken)
	roomID := res.Call.ID
	p := callrules.DefaultPolicy()
	require.Equal(t, []fakeMediaRoom{{Server: BootstrapMediaServerID, Name: roomID, Max: uint32(p.MaxPeople + p.MaxAgents + 1)}}, cw.media.rooms())

	room := cw.row(roomConcept, roomID)
	require.Equal(t, "open", room["status"])
	require.Equal(t, sess.KeyID, bare(room["keyId"]))
	require.Equal(t, BootstrapMediaServerID, bare(room["mediaServerId"]))
	require.Equal(t, roomID, room["mediaRoom"], "the media room is not named by the room's short id")
	require.Equal(t, cw.ref.Deployable, room["deployable"])
	require.Equal(t, callrules.Digest(cw.pepper(), []byte(res.Call.GuestToken)), room["guestDigest"])
	require.Equal(t, callrules.Digest(cw.pepper(), []byte(res.Call.HostToken)), room["hostDigest"])
	require.EqualValues(t, 1, room["joinWindowCount"], "the host's seat is the room's first join")
	require.Zero(t, cw.rowsContaining(res.Call.GuestToken)+cw.rowsContaining(res.Call.HostToken), "a stored row carries a link")

	hostSeat := cw.row(seatConcept, bare(room["hostSeatId"]))
	require.Equal(t, roomID, bare(hostSeat["roomId"]))
	require.Equal(t, sess.KeyID, bare(hostSeat["keyId"]), "an instant host seat must retain the authenticated key identity")
	require.Empty(t, hostSeat["credentialId"], "a legacy host seat must not invent a passkey credential")
	require.Equal(t, true, hostSeat["host"])
	require.Equal(t, "admitted", hostSeat["status"])
	require.Equal(t, "person", hostSeat["kind"])
	require.Equal(t, "Host", hostSeat["displayName"])
	require.Equal(t, true, hostSeat["confirmAdult"])
	require.Equal(t, true, hostSeat["confirmNoMinors"])
	require.Equal(t, confirmed.Terms, hostSeat["termsVersion"])
	require.Equal(t, res.Join.Identity, hostSeat["identity"])
	resolved, err := cw.svc.Session(ctx, cw.ref, sess.Token)
	require.NoError(t, err)
	require.Equal(t, bare(room["hostSeatId"]), resolved.SeatID)
	require.Equal(t, roomID, resolved.RoomID)

	// One call at a time per KEY: the same session, and a second session of the key.
	versionsBeforeRefusal := cw.versions(roomConcept, roomID)
	second, err := cw.svc.Start(ctx, resolved, callrules.StartBody{Name: "Host", Confirm: confirmed})
	requireRefused(t, second.Join, err, callrules.ErrConflict, callrules.ReasonKeyInCall)
	other, err := cw.svc.Unlock(ctx, cw.ref, cw.keyOf(sess))
	require.NoError(t, err)
	third, err := cw.svc.Start(ctx, other, callrules.StartBody{Name: "Other", Confirm: confirmed})
	requireRefused(t, third.Join, err, callrules.ErrConflict, callrules.ReasonKeyInCall)
	require.Len(t, cw.rowsOf(roomConcept), 1, "a refused Start wrote a room")
	require.Len(t, cw.media.rooms(), 1, "a refused Start opened a media room")
	require.Equal(t, versionsBeforeRefusal, cw.versions(roomConcept, roomID), "a refused Start wrote the open room")

	// Without its three confirmations a Start is invalid and writes nothing.
	fresh := cw.host()
	for _, c := range []callrules.Confirmation{{NoMinors: true, Terms: "t"}, {Adult: true, Terms: "t"}, {Adult: true, NoMinors: true}} {
		_, err := cw.svc.Start(ctx, fresh, callrules.StartBody{Name: "Host", Confirm: c})
		require.ErrorIs(t, err, callrules.ErrInvalid, "a Start without %+v was accepted", c)
	}
	_, err = cw.svc.Start(ctx, fresh, callrules.StartBody{Name: "  ", Confirm: confirmed})
	require.ErrorIs(t, err, callrules.ErrInvalid, "a Start with no name was accepted")
	require.Len(t, cw.rowsOf(roomConcept), 1)

	// A guest's session is not a key holder's and starts nothing.
	_, guest, err := cw.join(res.Call.GuestToken, "Guest")
	require.NoError(t, err)
	_, err = cw.svc.Start(ctx, guest, callrules.StartBody{Name: "Guest", Confirm: confirmed})
	require.ErrorIs(t, err, callrules.ErrForbidden, "a guest's session started a call")

	// A frozen key starts nothing, though its session was opened while it was active.
	frozenKey := cw.mintKey("active")
	frozen, err := cw.svc.Unlock(ctx, cw.ref, frozenKey)
	require.NoError(t, err)
	cw.asOwnerMutate(keyConcept, frozen.KeyID, "updateCallsKey", map[string]any{"keyId": frozen.KeyID, "status": "frozen"})
	refused, err := cw.svc.Start(ctx, frozen, callrules.StartBody{Name: "Host", Confirm: confirmed})
	requireRefused(t, refused.Join, err, callrules.ErrForbidden, callrules.ReasonKeyNotActive)

	// A media server that will not open the room leaves nothing behind.
	cw.media.mu.Lock()
	cw.media.failCreate = errors.New("the media server is down")
	cw.media.mu.Unlock()
	_, err = cw.svc.Start(ctx, fresh, callrules.StartBody{Name: "Host", Confirm: confirmed})
	require.Error(t, err)
	cw.media.mu.Lock()
	cw.media.failCreate = nil
	cw.media.mu.Unlock()
	require.Len(t, cw.rowsOf(roomConcept), 1, "a room was written for a media room that was never opened")
	resolvedFresh, err := cw.svc.Session(ctx, cw.ref, fresh.Token)
	require.NoError(t, err)
	require.Empty(t, resolvedFresh.SeatID, "a failed Start seated the host")

	// With no active media server there is nothing to place a call on.
	cluster, err := asCluster(ctx)
	require.NoError(t, err)
	cw.mutate(cluster, "updateCallsMediaServer", map[string]any{
		"mediaServerId": BootstrapMediaServerID, "state": "draining", "versionTime": versionStamp(time.Now().Add(time.Minute)),
	})
	none, err := cw.svc.Start(ctx, fresh, callrules.StartBody{Name: "Host", Confirm: confirmed})
	requireRefused(t, none.Join, err, callrules.ErrConflict, callrules.ReasonNoMediaServer)
	require.Len(t, cw.rowsOf(roomConcept), 1)
}

// keyOf is the key a host session was unlocked with, found again by its id: the test
// minted it, so it re-mints nothing and reads the key's digest-free row only to find
// the key it handed out.
func (cw *callWorld) keyOf(sess callrules.Session) string {
	cw.t.Helper()
	key, ok := cw.mintedKeys[sess.KeyID]
	require.True(cw.t, ok, "no key minted for %s", sess.KeyID)
	return key
}

// ---------------------------------------------------------------------------
// join
// ---------------------------------------------------------------------------

// TestJoinRequiresAllThreeConfirmations: every join, a rejoin included, confirms that
// the person is 18 or older, that no one under 18 will appear on their camera or
// screen, and which terms version they accept. Without any of the three, or with no
// name, the join is invalid and writes nothing: no seat, no session, no count.
func TestJoinRequiresAllThreeConfirmations(t *testing.T) {
	cw := newCallWorld(t, "confirm", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	hostSess, started := cw.started("Host")
	roomID := started.Call.ID
	roomVersions := cw.versions(roomConcept, roomID)

	for _, c := range []struct {
		name string
		body callrules.JoinBody
	}{
		{"not 18 or older", callrules.JoinBody{Token: started.Call.GuestToken, Name: "Guest", Confirm: callrules.Confirmation{NoMinors: true, Terms: "2026-11-17"}}},
		{"a minor on camera", callrules.JoinBody{Token: started.Call.GuestToken, Name: "Guest", Confirm: callrules.Confirmation{Adult: true, Terms: "2026-11-17"}}},
		{"no terms version", callrules.JoinBody{Token: started.Call.GuestToken, Name: "Guest", Confirm: callrules.Confirmation{Adult: true, NoMinors: true}}},
		{"a blank terms version", callrules.JoinBody{Token: started.Call.GuestToken, Name: "Guest", Confirm: callrules.Confirmation{Adult: true, NoMinors: true, Terms: "   "}}},
		{"no name", callrules.JoinBody{Token: started.Call.GuestToken, Name: " ", Confirm: confirmed}},
		{"both links", callrules.JoinBody{Token: started.Call.GuestToken, HostToken: started.Call.HostToken, Name: "Guest", Confirm: confirmed}},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, sess, err := cw.svc.Join(ctx, cw.ref, nil, c.body)
			require.ErrorIs(t, err, callrules.ErrInvalid)
			require.Empty(t, res.Pass)
			require.Empty(t, sess.Token, "an invalid join handed out a session")
		})
	}
	// The host's own rejoin needs them too.
	_, _, err := cw.svc.Join(ctx, cw.ref, &hostSess, callrules.JoinBody{Name: "Host", Confirm: callrules.Confirmation{Adult: true, NoMinors: true}})
	require.ErrorIs(t, err, callrules.ErrInvalid, "a rejoin without the terms version was accepted")

	require.Len(t, cw.seatsIn(roomID), 1, "an invalid join wrote a seat")
	require.Len(t, cw.rowsOf(sessionConcept), 1, "an invalid join wrote a session")
	require.Equal(t, roomVersions, cw.versions(roomConcept, roomID), "an invalid join was counted on the room")
}

// TestJoinAdmitsAndMintsAPassForThePlacedServer: with two media servers, a call is
// placed on the one with the most free seats, and a guest's pass is for THAT server: its
// address, signed with its own credential, for the room's media room, under a media
// identity that is stable for the seat and never the display name, with the person's
// grants and the policy's lifetime. The guest's first join creates the guest's own
// session.
func TestJoinAdmitsAndMintsAPassForThePlacedServer(t *testing.T) {
	cw := newCallWorld(t, "admit", 10)
	if cw == nil {
		return
	}
	ctx := context.Background()
	const secondSecret = "second-server-secret-0123456789abcdef"
	second := cw.addServer(40, secondSecret)

	_, started := cw.started("Host")
	require.Equal(t, []fakeMediaRoom{{Server: second.ID, Name: started.Call.ID, Max: 11}}, cw.media.rooms(),
		"the call was not placed on the server with the most free seats")
	require.Equal(t, second.PublicURL, started.Join.URL)
	require.Equal(t, second.ID, bare(cw.row(roomConcept, started.Call.ID)["mediaServerId"]))

	res, guest, err := cw.join(started.Call.GuestToken, "Guest Person")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, res.Outcome)
	require.Equal(t, second.PublicURL, res.URL, "the pass is not for the placed server")
	require.False(t, res.Host)
	require.Equal(t, callrules.DefaultPolicy().PassTTL, res.ExpiresIn)
	require.NotEmpty(t, res.Identity)
	require.NotEqual(t, "Guest Person", res.Identity, "the media identity is the display name")

	grants, ttl := verifyPass(t, res.Pass, "APIsecondkey", secondSecret)
	require.Equal(t, 120*time.Second, ttl)
	require.Equal(t, res.Identity, grants.Identity)
	require.Equal(t, "Guest Person", grants.Name)
	require.Equal(t, string(callrules.KindPerson), grants.Attributes[KindAttribute])
	require.NotNil(t, grants.Video)
	require.Equal(t, started.Call.ID, grants.Video.Room)
	require.True(t, grants.Video.RoomJoin)
	require.False(t, grants.Video.GetCanUpdateOwnMetadata())
	spec, err := callrules.GrantFor(callrules.KindPerson)
	require.NoError(t, err)
	require.ElementsMatch(t, spec.Sources, grants.Video.CanPublishSources)
	_, err = lkauth.ParseAPIToken(res.Pass)
	require.NoError(t, err)
	if verifier, err := lkauth.ParseAPIToken(res.Pass); err == nil {
		_, _, err = verifier.Verify(testAPISecret)
		require.Error(t, err, "a pass for the second server verifies with the bootstrap server's secret")
	}

	require.Equal(t, callrules.SessionGuest, guest.Kind)
	require.Regexp(t, tokenShape, guest.Token)
	require.Equal(t, started.Call.ID, guest.RoomID)
	seat := cw.row(seatConcept, guest.SeatID)
	require.Equal(t, "admitted", seat["status"])
	require.Equal(t, false, seat["host"])
	require.Equal(t, res.Identity, seat["identity"])
	require.Equal(t, "Guest Person", seat["displayName"])
	require.Equal(t, true, seat["confirmAdult"])
	require.Equal(t, true, seat["confirmNoMinors"])
	require.Equal(t, confirmed.Terms, seat["termsVersion"])
	stored := cw.rowsOf(sessionConcept)
	var guestRow map[string]any
	for _, s := range stored {
		if bare(s["seatId"]) == guest.SeatID {
			guestRow = s
		}
	}
	require.NotNil(t, guestRow, "the guest's session was not stored")
	require.Equal(t, "guest", guestRow["kind"])
	require.Equal(t, callrules.Digest(cw.pepper(), []byte(guest.Token)), guestRow["tokenDigest"])
	require.Zero(t, cw.rowsContaining(guest.Token), "a stored row carries the guest's session token")

	// A link nobody issued, or one of another deployable, opens nothing.
	_, _, err = cw.join("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "Guest")
	require.ErrorIs(t, err, callrules.ErrNotFound)
	_, _, err = cw.svc.Join(ctx, callrules.SiteRef{SiteID: cw.second, OwnerUserID: cw.owner}, nil,
		callrules.JoinBody{Token: started.Call.GuestToken, Name: "Guest", Confirm: confirmed})
	require.ErrorIs(t, err, callrules.ErrNotFound, "a link of one deployable opened a call at another")
}

// TestPlacementBreaksATieById: of two media servers with the same free seats, a call is
// placed on the first by id, so two replicas placing at once choose alike.
func TestPlacementBreaksATieById(t *testing.T) {
	cw := newCallWorld(t, "tie", 40)
	if cw == nil {
		return
	}
	second := cw.addServer(40, "tie-server-secret-0123456789abcdefghij")
	require.Less(t, BootstrapMediaServerID, second.ID, "the fixture needs the bootstrap server first by id")
	_, started := cw.started("Host")
	require.Equal(t, BootstrapMediaServerID, bare(cw.row(roomConcept, started.Call.ID)["mediaServerId"]))
	require.Equal(t, testMediaConfig().PublicURL, started.Join.URL)
}

// TestLockedCallWaitsUntilAdmitted: in a locked call a guest waits, seated and with a
// session of their own but with no pass, and keeps waiting while they poll; the host
// and a holder of the host link are let in. When the call is unlocked, the waiter's
// next poll admits them with a pass.
func TestLockedCallWaitsUntilAdmitted(t *testing.T) {
	cw := newCallWorld(t, "locked", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	hostSess, started := cw.started("Host")
	roomID := started.Call.ID
	cw.asOwnerMutate(roomConcept, roomID, "updateCallsRoom", map[string]any{"roomId": roomID, "locked": true})

	res, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, res.Outcome)
	require.Equal(t, "locked", res.Reason)
	require.Equal(t, callrules.DefaultPolicy().QueuePoll, res.RetryAfter)
	require.Empty(t, res.Pass)
	require.NotEmpty(t, guest.Token, "a waiting guest has no session to poll with")
	require.Equal(t, "waiting", cw.row(seatConcept, guest.SeatID)["status"])
	require.Empty(t, cw.rowsOf(queueEntryConcept), "a lock is not a capacity queue")

	polled, err := cw.svc.Queue(ctx, guest)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, polled.Outcome, "a waiter in a locked call was let in")
	require.Empty(t, polled.Pass)

	// The host, and whoever holds the host link, are let in though the call is locked.
	back, _, err := cw.svc.Join(ctx, cw.ref, &hostSess, callrules.JoinBody{Name: "Host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, back.Outcome)
	require.True(t, back.Host)
	cohost, _, err := cw.svc.Join(ctx, cw.ref, nil, callrules.JoinBody{HostToken: started.Call.HostToken, Name: "Co-host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, cohost.Outcome, "the host link waited in a locked call")
	require.True(t, cohost.Host)

	cw.asOwnerMutate(roomConcept, roomID, "updateCallsRoom", map[string]any{"roomId": roomID, "locked": false})
	admitted, err := cw.svc.Queue(ctx, guest)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, admitted.Outcome, "the waiter was not let in once the call was unlocked")
	require.NotEmpty(t, admitted.Pass)
	require.Equal(t, "admitted", cw.row(seatConcept, guest.SeatID)["status"])
	verifyPass(t, admitted.Pass, testMediaConfig().APIKey, testAPISecret)
}

// TestFullCapacityQueuesAndPromotesInOrder: a media server's seats are shared by every
// call on it. When they run out a joiner is queued with their place in line; a seat
// that frees up goes to whoever is first in line, on their own poll, and nobody behind
// them takes it first. Someone joining a running call is ahead of a new call's host. A
// waiter who stops polling loses their place after the queue entry's lifetime.
func TestFullCapacityQueuesAndPromotesInOrder(t *testing.T) {
	cw := newCallWorld(t, "queue", 3)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, started := cw.started("Host") // seat 1 of 3
	link := started.Call.GuestToken

	_, g1, err := cw.join(link, "First")
	require.NoError(t, err) // seat 2
	_, g2, err := cw.join(link, "Second")
	require.NoError(t, err) // seat 3

	q3, g3, err := cw.join(link, "Third")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, q3.Outcome)
	require.Equal(t, "capacity", q3.Reason)
	require.Equal(t, 1, q3.Position)
	require.Equal(t, callrules.DefaultPolicy().QueuePoll, q3.RetryAfter)
	require.Empty(t, q3.Pass)
	q4, g4, err := cw.join(link, "Fourth")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, q4.Outcome)
	require.Equal(t, 2, q4.Position)

	// A new call's host waits behind everyone joining a running call.
	newHost := cw.host()
	newCall, err := cw.svc.Start(ctx, newHost, callrules.StartBody{Name: "New host", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, newCall.Join.Outcome)
	require.Equal(t, 3, newCall.Join.Position)
	newHost, err = cw.svc.Session(ctx, cw.ref, newHost.Token)
	require.NoError(t, err)

	// A seat frees up. The fourth polls first, and must not take it from the third.
	require.NoError(t, cw.svc.Leave(ctx, g2))
	early, err := cw.svc.Queue(ctx, g4)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, early.Outcome, "a waiter took a seat from the one ahead of them")
	require.Equal(t, 2, early.Position)
	hostEarly, err := cw.svc.Queue(ctx, newHost)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, hostEarly.Outcome, "a new call took a seat from a running call's waiter")

	promoted, err := cw.svc.Queue(ctx, g3)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, promoted.Outcome, "the first in line was not let in")
	require.NotEmpty(t, promoted.Pass)
	require.Equal(t, "admitted", cw.row(seatConcept, g3.SeatID)["status"])
	next, err := cw.svc.Queue(ctx, g4)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, next.Outcome)
	require.Equal(t, 1, next.Position)
	require.Equal(t, 3, cw.admittedIn(started.Call.ID), "the server holds more people than it has seats")

	// The next seat goes to the fourth, before the new call's host.
	require.NoError(t, cw.svc.Leave(ctx, g1))
	hostStill, err := cw.svc.Queue(ctx, newHost)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, hostStill.Outcome)
	fourth, err := cw.svc.Queue(ctx, g4)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, fourth.Outcome)

	// A poll refreshes the waiter's entry at most once a minute.
	entries := cw.rowsOf(queueEntryConcept)
	var hostEntry string
	for entryID, entry := range entries {
		if bare(entry["seatId"]) == newHost.SeatID {
			hostEntry = entryID
		}
	}
	require.NotEmpty(t, hostEntry)
	before := cw.versions(queueEntryConcept, hostEntry)
	for i := 0; i < 3; i++ {
		cw.clock.Advance(3 * time.Second)
		_, err := cw.svc.Queue(ctx, newHost)
		require.NoError(t, err)
	}
	require.Equal(t, before, cw.versions(queueEntryConcept, hostEntry), "a poll within the minute wrote the entry")
	cw.clock.Advance(time.Minute)
	_, err = cw.svc.Queue(ctx, newHost)
	require.NoError(t, err)
	require.Equal(t, before+1, cw.versions(queueEntryConcept, hostEntry), "a poll after a minute did not refresh the entry")

	// A waiter who goes quiet for the entry's lifetime loses their place: the fifth
	// stops polling and the sixth keeps at it, so the seventh queues right behind the
	// sixth, and the fifth's entry is marked expired.
	q5, g5, err := cw.join(link, "Fifth")
	require.NoError(t, err)
	require.Equal(t, 1, q5.Position)
	q6, g6, err := cw.join(link, "Sixth")
	require.NoError(t, err)
	require.Equal(t, 2, q6.Position)
	ttl := callrules.DefaultPolicy().QueueEntryTTL
	cw.clock.Advance(ttl - time.Minute)
	still, err := cw.svc.Queue(ctx, g6)
	require.NoError(t, err)
	require.Equal(t, 2, still.Position, "a waiter who is inside the lifetime lost their place")
	cw.clock.Advance(time.Minute + time.Second)
	q7, _, err := cw.join(link, "Seventh")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, q7.Outcome)
	require.Equal(t, 2, q7.Position, "a waiter who stopped polling still holds a place in line")
	fifthEntry := ""
	for entryID, entry := range cw.rowsOf(queueEntryConcept) {
		if bare(entry["seatId"]) == g5.SeatID {
			fifthEntry = entryID
		}
	}
	require.NotEmpty(t, fifthEntry)
	require.Equal(t, "expired", cw.row(queueEntryConcept, fifthEntry)["status"], "the quiet waiter's entry is not marked expired")
	resumed, err := cw.svc.Queue(ctx, g6)
	require.NoError(t, err)
	require.Equal(t, 1, resumed.Position, "the waiter behind an expired one did not move up")
}

// TestAGuestDoesNotQueueAheadOfItsQueuedHost: a call whose host is still queued is not
// running yet, so a guest joining it queues in the new call's class behind its own host,
// by arrival, and never ahead of them as someone joining a running call would. When a
// seat frees up it goes to the host, not to a guest who would sit in a call with nobody
// hosting it.
func TestAGuestDoesNotQueueAheadOfItsQueuedHost(t *testing.T) {
	cw := newCallWorld(t, "hostfirst", 2)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, a := cw.started("Host A") // seat 1 of 2
	_, a1, err := cw.join(a.Call.GuestToken, "A1")
	require.NoError(t, err) // seat 2: the server is full

	bHost := cw.host()
	b, err := cw.svc.Start(ctx, bHost, callrules.StartBody{Name: "Host B", Confirm: confirmed})
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, b.Join.Outcome)
	require.Equal(t, 1, b.Join.Position)
	bHost, err = cw.svc.Session(ctx, cw.ref, bHost.Token)
	require.NoError(t, err)

	queued, bGuest, err := cw.join(b.Call.GuestToken, "B1")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, queued.Outcome)
	require.Equal(t, 2, queued.Position, "a guest queued ahead of their own call's queued host")
	var class string
	for _, entry := range cw.rowsOf(queueEntryConcept) {
		if bare(entry["seatId"]) == bGuest.SeatID {
			class, _ = entry["class"].(string)
		}
	}
	require.Equal(t, "new", class, "a guest of a call nobody is in yet queued as joining a running call")

	require.NoError(t, cw.svc.Leave(ctx, a1)) // a seat frees up
	guestPoll, err := cw.svc.Queue(ctx, bGuest)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, guestPoll.Outcome, "a guest took the seat ahead of their own queued host")
	require.Equal(t, 2, guestPoll.Position)
	hostPoll, err := cw.svc.Queue(ctx, bHost)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, hostPoll.Outcome, "the queued host was not let in first")
}

// TestALockedCallDoesNotHoldUpTheServersQueue: a waiter in a call its host has locked
// cannot be let in, so it holds no place in the media server's line that keeps a free
// seat from anyone else: with B's call locked, A2 behind B1 is admitted to the seat A1
// frees. B1 keeps its entry and its arrival while it polls through the lock, and once
// B's call is unlocked it is back in its place, ahead of everyone who queued after it.
func TestALockedCallDoesNotHoldUpTheServersQueue(t *testing.T) {
	cw := newCallWorld(t, "lockedline", 3)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, a := cw.started("Host A") // seat 1 of 3
	_, b := cw.started("Host B") // seat 2
	_, a1, err := cw.join(a.Call.GuestToken, "A1")
	require.NoError(t, err) // seat 3
	qb1, b1, err := cw.join(b.Call.GuestToken, "B1")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, qb1.Outcome)
	require.Equal(t, 1, qb1.Position)
	qa2, a2, err := cw.join(a.Call.GuestToken, "A2")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, qa2.Outcome)
	require.Equal(t, 2, qa2.Position)

	// The lock is written to the row as the host's lock control writes it (host.go), so the
	// test is about the queue and not the control (host_db_test.go holds the control).
	cw.asOwnerMutate(roomConcept, b.Call.ID, "updateCallsRoom", map[string]any{"roomId": b.Call.ID, "locked": true})
	require.NoError(t, cw.svc.Leave(ctx, a1)) // a seat frees up

	admitted, err := cw.svc.Queue(ctx, a2)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, admitted.Outcome,
		"a waiter held behind a lock kept the free seat from the one behind it: %+v", admitted)
	held, err := cw.svc.Queue(ctx, b1)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeWait, held.Outcome)
	require.Equal(t, "locked", held.Reason)

	// Through the lock, B1 keeps polling and keeps its entry, and it is counted in nobody
	// else's place: A3, who queues now, is first in line.
	q3, a3, err := cw.join(a.Call.GuestToken, "A3")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, q3.Outcome)
	require.Equal(t, 1, q3.Position, "a waiter held behind a lock is counted in the line")
	cw.clock.Advance(callrules.DefaultPolicy().QueueEntryTTL - time.Minute)
	_, err = cw.svc.Queue(ctx, b1)
	require.NoError(t, err)
	_, err = cw.svc.Queue(ctx, a3)
	require.NoError(t, err)
	cw.clock.Advance(2 * time.Minute) // past the lifetime of an entry nobody polled

	cw.asOwnerMutate(roomConcept, b.Call.ID, "updateCallsRoom", map[string]any{"roomId": b.Call.ID, "locked": false})
	back, err := cw.svc.Queue(ctx, b1)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, back.Outcome)
	require.Equal(t, 1, back.Position, "the waiter lost its place to the lock")
	behind, err := cw.svc.Queue(ctx, a3)
	require.NoError(t, err)
	require.Equal(t, 2, behind.Position, "a waiter who queued during the lock went ahead of one who queued before it")

	// The next seat goes to B1, before A3.
	require.NoError(t, cw.svc.Leave(ctx, a2))
	early, err := cw.svc.Queue(ctx, a3)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, early.Outcome, "a waiter took the seat of the one back in line ahead of them")
	in, err := cw.svc.Queue(ctx, b1)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, in.Outcome)
}

// TestRejoinReusesTheSeat: a person who joins again keeps their seat. While seated
// they refresh the same media identity; after leaving they take the same seat with
// a new media identity, and a removed seat stays shut. A rejoin is NEVER REFUSED AS
// FULL, in a call holding the policy's people or on a media server with no free seat,
// because the person already holds the seat. A session seated in one open call cannot
// join another.
func TestRejoinReusesTheSeat(t *testing.T) {
	cw := newCallWorld(t, "rejoin", 50)
	if cw == nil {
		return
	}
	ctx := context.Background()
	_, started := cw.started("Host")
	roomID := started.Call.ID

	first, guest, err := cw.join(started.Call.GuestToken, "Guest")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, first.Outcome)

	again, sess, err := cw.rejoin(guest, started.Call.GuestToken)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, again.Outcome)
	require.Equal(t, guest.SeatID, sess.SeatID, "a rejoin took a new seat")
	require.Equal(t, guest.Token, sess.Token, "a rejoin replaced the session")
	require.Equal(t, first.Identity, again.Identity, "a rejoin changed the media identity")
	verifyPass(t, again.Pass, testMediaConfig().APIKey, testAPISecret)
	noLink, _, err := cw.rejoin(guest, "")
	require.NoError(t, err, "a seated session could not rejoin without its link")
	require.Equal(t, first.Identity, noLink.Identity)
	require.Len(t, cw.seatsIn(roomID), 2, "a rejoin wrote a seat")

	// Leaving frees the seat and disconnects the identity; coming back takes it again.
	require.NoError(t, cw.svc.Leave(ctx, guest))
	left := cw.row(seatConcept, guest.SeatID)
	require.Equal(t, "left", left["status"])
	require.NotEmpty(t, left["leftAt"])
	require.Contains(t, cw.media.removals(), fakeMediaRemoval{Server: BootstrapMediaServerID, Room: roomID, Identity: first.Identity})
	require.NoError(t, cw.svc.Leave(ctx, guest), "leaving twice is not an error")
	back, backSess, err := cw.rejoin(guest, started.Call.GuestToken)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, back.Outcome)
	require.Equal(t, guest.SeatID, backSess.SeatID, "coming back after leaving took a new seat")
	require.NotEqual(t, first.Identity, back.Identity, "a new admission reused a retired media identity")
	require.Equal(t, "admitted", cw.row(seatConcept, guest.SeatID)["status"])
	require.Len(t, cw.seatsIn(roomID), 2)

	// A session seated in an open call cannot join another.
	_, other := cw.started("Other host")
	busy, _, err := cw.rejoin(guest, other.Call.GuestToken)
	requireRefused(t, busy, err, callrules.ErrConflict, callrules.ReasonInAnotherCall)
	require.Len(t, cw.seatsIn(other.Call.ID), 1, "a session seated in one call took a seat in another")

	// A rejoin to a call holding the policy's people: anyone new is refused as full, and
	// the seated guest is let back onto the same seat.
	p := callrules.DefaultPolicy()
	for n := cw.admittedIn(roomID); n < p.MaxPeople; n++ {
		res, _, err := cw.join(started.Call.GuestToken, "Filler")
		require.NoError(t, err)
		require.Equal(t, callrules.OutcomeAdmit, res.Outcome)
	}
	require.Equal(t, p.MaxPeople, cw.admittedIn(roomID))
	tooMany, _, err := cw.join(started.Call.GuestToken, "One too many")
	requireRefused(t, tooMany, err, callrules.ErrConflict, "room_full")
	seats := len(cw.seatsIn(roomID))
	inFull, keptSess, err := cw.rejoin(guest, started.Call.GuestToken)
	require.NoError(t, err, "a rejoin to a full call was refused")
	require.Equal(t, callrules.OutcomeAdmit, inFull.Outcome)
	require.Equal(t, guest.SeatID, keptSess.SeatID, "a rejoin to a full call took a new seat")
	require.Equal(t, back.Identity, inFull.Identity, "a rejoin to a full call changed the media identity")
	require.Len(t, cw.seatsIn(roomID), seats, "a rejoin to a full call wrote a seat")

	// A rejoin on a media server with no free seat, in a call that is not full: the other
	// call holds its host and a guest, the server's seats are cut to the people it
	// carries, anyone new to that call is queued, and its seated guest is let back on.
	firstB, guestB, err := cw.join(other.Call.GuestToken, "Guest B")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, firstB.Outcome)
	cut := testMediaConfig()
	cut.Seats = cw.admittedIn(roomID) + cw.admittedIn(other.Call.ID)
	_, err = cw.in.Bootstrap(ctx, cut)
	require.NoError(t, err)
	queuedB, _, err := cw.join(other.Call.GuestToken, "Waiter")
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeQueue, queuedB.Outcome, "the media server still has a free seat")
	seatsB := len(cw.seatsIn(other.Call.ID))
	onFull, keptB, err := cw.rejoin(guestB, other.Call.GuestToken)
	require.NoError(t, err, "a rejoin on a full media server was refused")
	require.Equal(t, callrules.OutcomeAdmit, onFull.Outcome, "a rejoin on a full media server was not let back on: %+v", onFull)
	require.Equal(t, guestB.SeatID, keptB.SeatID, "a rejoin on a full media server took a new seat")
	require.Equal(t, firstB.Identity, onFull.Identity)
	require.Len(t, cw.seatsIn(other.Call.ID), seatsB, "a rejoin on a full media server wrote a seat")

	// A removed seat stays shut.
	cw.asOwnerMutate(seatConcept, guest.SeatID, "updateCallsSeat", map[string]any{"seatId": guest.SeatID, "status": "removed"})
	shut, _, err := cw.rejoin(guest, started.Call.GuestToken)
	requireRefused(t, shut, err, callrules.ErrForbidden, callrules.ReasonRemoved)
	require.Equal(t, "removed", cw.row(seatConcept, guest.SeatID)["status"])
}

// TestRoomJoinRateIsLimited: a room takes at most RoomJoinsPerMinute join attempts a
// minute, counted on the room so every replica counts the same attempts. The host's
// Start is the first, rejoins and refused attempts count, and the attempt past the limit
// is refused with the time until the window ends, writing nothing.
func TestRoomJoinRateIsLimited(t *testing.T) {
	cw := newCallWorld(t, "rate", 50)
	if cw == nil {
		return
	}
	_, started := cw.started("Host") // attempt 1
	roomID := started.Call.ID
	_, guest, err := cw.join(started.Call.GuestToken, "Guest") // attempt 2
	require.NoError(t, err)
	for i := 3; i <= 30; i++ {
		cw.clock.Advance(time.Second)
		res, _, err := cw.rejoin(guest, started.Call.GuestToken)
		require.NoError(t, err, "attempt %d", i)
		require.Equal(t, callrules.OutcomeAdmit, res.Outcome, "attempt %d", i)
	}
	roomVersions := cw.versions(roomConcept, roomID)
	seatVersions := cw.versions(seatConcept, guest.SeatID)

	cw.clock.Advance(time.Second)
	res, _, err := cw.rejoin(guest, started.Call.GuestToken) // attempt 31
	requireRefused(t, res, err, callrules.ErrConflict, callrules.ReasonRate)
	require.Greater(t, res.RetryAfter, time.Duration(0))
	require.LessOrEqual(t, res.RetryAfter, time.Minute)
	stranger, strangerSess, err := cw.join(started.Call.GuestToken, "Stranger")
	requireRefused(t, stranger, err, callrules.ErrConflict, callrules.ReasonRate)
	require.Empty(t, strangerSess.Token, "a refused first join handed out a session")
	require.Equal(t, roomVersions, cw.versions(roomConcept, roomID), "a refused attempt was written")
	require.Equal(t, seatVersions, cw.versions(seatConcept, guest.SeatID))
	require.Len(t, cw.seatsIn(roomID), 2)
	require.Len(t, cw.rowsOf(sessionConcept), 2)

	// A new window admits again, and refused attempts count in it too.
	cw.clock.Advance(res.RetryAfter)
	ok, _, err := cw.rejoin(guest, started.Call.GuestToken)
	require.NoError(t, err)
	require.Equal(t, callrules.OutcomeAdmit, ok.Outcome)
	cw.asOwnerMutate(seatConcept, guest.SeatID, "updateCallsSeat", map[string]any{"seatId": guest.SeatID, "status": "removed"})
	for i := 2; i <= 30; i++ {
		res, _, err := cw.rejoin(guest, started.Call.GuestToken)
		requireRefused(t, res, err, callrules.ErrForbidden, callrules.ReasonRemoved)
	}
	res, _, err = cw.rejoin(guest, started.Call.GuestToken)
	requireRefused(t, res, err, callrules.ErrConflict, callrules.ReasonRate)
}

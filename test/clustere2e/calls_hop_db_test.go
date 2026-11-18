package clustere2e

// calls_hop_db_test.go -- a call's limits hold when its joins land on different
// replicas at once.
//
// # WHAT IS AT STAKE
//
// Every replica runs its own calls service over one database. A join reads the room,
// its seats and the media server's load, decides, and writes a seat; two replicas doing
// that at the same moment each see the same room and each admit, and a room of eight
// holds nine. A lock inside the service, or a read served from a replica's own cache,
// would look correct on one node and fail here.
//
// # WHAT THIS PROVES
//
// Two engines over one Postgres, each with its own pool, cache, gate pool and calls
// integration (only the media server's control API is a fake, shared, because both
// replicas talk to the same server). A host starts a call on replica A; then:
//
//   - twelve people join at once, alternating replicas: the room ends with exactly the
//     policy's eight people (the host and seven), and the other five are refused as
//     full -- nobody is admitted past the limit and no join fails some other way;
//   - on a media server with six seats and four calls on it, twelve people join the four
//     calls at once from both replicas: exactly two more are admitted, so the server
//     holds its six, and the other ten are queued. Only the media server's gate stands
//     between those calls, because each has its own room gate.
//
// # HOW TO CONFIRM IT IS LOAD-BEARING
//
// Replace the advisory gates in integrations/calls service.go with a mutex held by the
// Service (a single node's lock): the replicas race, and joins fail on the room's
// version fence or a room takes a ninth person (measured: every run of both tests
// failed). Take the media server's key out of a join's gates and the four calls' first
// joins each take one of the two free seats (measured: the second test failed).

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/calls"
)

// callsHopMasterKey seals the media credentials this test's bootstrap writes.
const callsHopMasterKey = "5c1d1e2c3b4a59687f6e5d4c3b2a19087f6e5d4c3b2a19081726354453627180"

const callsHopAPISecret = "hop-api-secret-0123456789abcdefghijklmn"

// callsEngine is the narrow engine the calls integration asks for.
type callsEngine struct{ e *memql.MemQLEngine }

func (c callsEngine) Execute(ctx context.Context, q string) (any, error) { return c.e.Execute(ctx, q) }

// hopMedia is the media server both replicas talk to: it opens any room it is asked.
type hopMedia struct{}

func (hopMedia) CreateRoom(context.Context, string, uint32) error { return nil }
func (hopMedia) DeleteRoom(context.Context, string) error         { return nil }
func (hopMedia) Participants(context.Context, string) ([]calls.MediaParticipant, error) {
	return nil, nil
}
func (hopMedia) RemoveParticipant(context.Context, string, string) error { return nil }
func (hopMedia) MuteTrack(context.Context, string, string, string) error { return nil }

// callsReplica is one node: its own pool, engine, result cache, gate pool and calls
// integration.
type callsReplica struct {
	db  *bun.DB
	eng *memql.MemQLEngine
	in  *calls.Integration
}

func newCallsReplica(t *testing.T, dsn string) *callsReplica {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "calls admission across replicas", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	eng, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = quiet
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	gates, err := database.NewGatePool(dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gates.Close() })
	in := calls.New(calls.Deps{
		Engine:              callsEngine{eng},
		GateDB:              func() *sql.DB { return gates },
		ResolveSystemSecret: eng.ResolveSystemSecret,
		Logger:              quiet,
		NewMediaBackend: func(calls.MediaServer, calls.MediaCredential) (calls.MediaBackend, error) {
			return hopMedia{}, nil
		},
	})
	return &callsReplica{db: db, eng: eng, in: in}
}

// callsHopWorld is the deployable both replicas serve, and the rows this test wrote.
type callsHopWorld struct {
	t      *testing.T
	a, b   *callsReplica
	owner  string
	site   string
	ref    callrules.SiteRef
	pepper []byte
}

func newCallsHopWorld(t *testing.T, seats int) *callsHopWorld {
	t.Helper()
	dsn := armingDSN(t)
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.EnvMasterKey, callsHopMasterKey)
	a := newCallsReplica(t, dsn)
	b := newCallsReplica(t, dsn)
	if a == nil || b == nil {
		return nil
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	w := &callsHopWorld{t: t, a: a, b: b, owner: "calls-hop-owner-" + suffix, site: "calls-hop-site-" + suffix}
	w.ref = callrules.SiteRef{SiteID: w.site, OwnerUserID: w.owner, Deployable: "desk-" + suffix}
	w.wipeMedia()
	t.Cleanup(w.sweep)

	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.SystemActor("calls-hop-test")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "calls-hop-test"})
	w.exec(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"displayName":"Calls host","primaryEmail":%s,"role":"writer","active":true})`,
		langparser.QuoteString(w.owner), langparser.QuoteString(w.owner+"@example.test")))
	w.exec(system, fmt.Sprintf(`insert("v1:platform:site", id=%s, payload={"hostname":%s,"kind":"spa","status":"draft","bundleRef":%s,"ownerUserId":%s})`,
		langparser.QuoteString(w.site), langparser.QuoteString(w.site+".example.test"),
		langparser.QuoteString("blob://sites/"+w.site+"/v1/"), langparser.QuoteString("v1:identity:user:"+w.owner)))

	if _, err := a.in.Bootstrap(context.Background(), calls.MediaConfig{
		Provider: "docker-local", APIURL: "http://media.example.test:7880", PublicURL: "wss://media.example.test",
		Seats: seats, APIKey: "APIhopkey", APISecret: callsHopAPISecret,
		WebhookKey: "APIhopwebhook", WebhookSecret: "hop-webhook-secret-0123456789abcdefghij",
	}); err != nil {
		t.Fatalf("bootstrap the media server: %v", err)
	}
	stored, err := a.eng.ResolveSystemSecret(memql.ContextWithFreshRead(context.Background()), calls.SecretKeyPepper)
	if err != nil {
		t.Fatalf("read the key pepper: %v", err)
	}
	if w.pepper, err = hex.DecodeString(stored); err != nil {
		t.Fatalf("the key pepper is not hex: %v", err)
	}
	return w
}

func (w *callsHopWorld) exec(ctx context.Context, query string) {
	w.t.Helper()
	if _, err := w.a.eng.Execute(ctx, query); err != nil {
		w.t.Fatalf("%s: %v", query, err)
	}
}

// wipeMedia takes the bootstrap server and its three secrets away, so this test's
// server is the only one rooms are placed on and its seats are the ones it set.
func (w *callsHopWorld) wipeMedia() {
	ctx := context.Background()
	if _, err := w.a.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", "v1:calls:mediaServer").Exec(ctx); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.a.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", "v1:platform:globalSecret").
		Where("payload->>'name' IN (?)", bun.In([]string{calls.SecretMediaBootstrap, calls.SecretMediaWebhook, calls.SecretKeyPepper})).Exec(ctx); err != nil {
		w.t.Fatal(err)
	}
}

func (w *callsHopWorld) sweep() {
	ctx := context.Background()
	_, _ = w.a.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("payload->>'siteId' = ?", "v1:platform:site:"+w.site).Exec(ctx)
	_, _ = w.a.db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id IN (?)", bun.In([]string{
		"v1:platform:site:" + w.site, "v1:identity:user:" + w.owner,
	})).Exec(ctx)
	w.wipeMedia()
}

// startCall mints a key, unlocks it on replica A and starts a call there.
func (w *callsHopWorld) startCall() callrules.StartResult {
	w.t.Helper()
	ctx := context.Background()
	raw, key, err := callrules.NewKey(rand.Reader)
	if err != nil {
		w.t.Fatal(err)
	}
	owner := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, w.owner))
	call, err := langparser.RenderCall("createCallsKey", map[string]any{
		"keyId": id.NewShortId(), "siteId": w.site, "digest": callrules.Digest(w.pepper, raw[:]), "kind": "test",
	})
	if err != nil {
		w.t.Fatal(err)
	}
	w.exec(owner, "mutation "+call)
	sess, err := w.a.in.Service().Unlock(ctx, w.ref, key)
	if err != nil {
		w.t.Fatalf("unlock on replica A: %v", err)
	}
	started, err := w.a.in.Service().Start(ctx, sess, callrules.StartBody{Name: "Host", Confirm: hopConfirmed})
	if err != nil || started.Join.Outcome != callrules.OutcomeAdmit {
		w.t.Fatalf("start on replica A: %+v, %v", started.Join, err)
	}
	return started
}

// joinAtOnce makes every join at the same moment, alternating replicas, and answers
// each one's outcome.
func (w *callsHopWorld) joinAtOnce(links []string) ([]callrules.JoinResult, []error) {
	results := make([]callrules.JoinResult, len(links))
	errs := make([]error, len(links))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, link := range links {
		replica := w.a
		if i%2 == 1 {
			replica = w.b
		}
		wg.Add(1)
		go func(i int, replica *callsReplica, link string) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			results[i], _, errs[i] = replica.in.Service().Join(ctx, w.ref, nil, callrules.JoinBody{
				Token: link, Name: fmt.Sprintf("Guest %d", i), Confirm: hopConfirmed,
			})
		}(i, replica, link)
	}
	close(start)
	wg.Wait()
	return results, errs
}

// admittedIn counts, straight off the table, the seats of one room whose newest
// version is admitted.
func (w *callsHopWorld) admittedIn(roomID string) int {
	w.t.Helper()
	var payloads []json.RawMessage
	err := w.a.db.NewRaw(`SELECT payload FROM (SELECT DISTINCT ON (id) payload FROM "MemoryNodes"
		WHERE concept = 'v1:calls:seat' AND payload->>'roomId' = ? ORDER BY id, "createdAt" DESC) latest`,
		"v1:calls:room:"+roomID).Scan(context.Background(), &payloads)
	if err != nil {
		w.t.Fatal(err)
	}
	n := 0
	for _, raw := range payloads {
		var seat map[string]any
		if err := json.Unmarshal(raw, &seat); err != nil {
			w.t.Fatal(err)
		}
		if seat["status"] == "admitted" {
			n++
		}
	}
	return n
}

var hopConfirmed = callrules.Confirmation{Adult: true, NoMinors: true, Terms: "2026-11-17"}

// TestConcurrentJoinsAcrossReplicasNeverExceedThePolicy: twelve people join one call at
// once through two replicas. The room ends with exactly the policy's eight people, the
// host among them, and every join past that is refused as full rather than failing.
func TestConcurrentJoinsAcrossReplicasNeverExceedThePolicy(t *testing.T) {
	w := newCallsHopWorld(t, 50)
	if w == nil {
		return
	}
	started := w.startCall()
	links := make([]string, 12)
	for i := range links {
		links[i] = started.Call.GuestToken
	}
	results, errs := w.joinAtOnce(links)

	maxPeople := callrules.DefaultPolicy().MaxPeople
	admitted, full := 0, 0
	for i, res := range results {
		switch {
		case errs[i] == nil && res.Outcome == callrules.OutcomeAdmit:
			admitted++
		case errors.Is(errs[i], callrules.ErrConflict) && res.Reason == "room_full":
			full++
		default:
			t.Errorf("join %d (replica %s) answered %+v, %v: every join is either admitted or refused as full", i, map[bool]string{true: "A", false: "B"}[i%2 == 0], res, errs[i])
		}
	}
	if admitted != maxPeople-1 || full != len(links)-(maxPeople-1) {
		t.Fatalf("%d joins were admitted and %d refused as full, want %d and %d: the host holds the eighth place",
			admitted, full, maxPeople-1, len(links)-(maxPeople-1))
	}
	if got := w.admittedIn(started.Call.ID); got != maxPeople {
		t.Fatalf("the room holds %d admitted people, want exactly the policy's %d", got, maxPeople)
	}
}

// TestConcurrentJoinsAcrossReplicasNeverExceedTheServer: four calls share a media
// server of six seats, each holding its host, so two seats are free. Twelve people join
// the four calls at once through two replicas, three to each call and each call's from
// both replicas: exactly two are admitted, so the server carries its six, and the other
// ten are queued. Each call has its own room gate, so the four first joins run side by
// side and only the media server's gate stands between them and the two free seats.
func TestConcurrentJoinsAcrossReplicasNeverExceedTheServer(t *testing.T) {
	w := newCallsHopWorld(t, 6)
	if w == nil {
		return
	}
	calls := []callrules.StartResult{w.startCall(), w.startCall(), w.startCall(), w.startCall()}
	links := make([]string, 12)
	for i := range links {
		// Joins i and i+1 go to one call through replicas A and B, and the next pair to
		// the next call, so all four calls are joined at once from both replicas.
		links[i] = calls[(i/2)%len(calls)].Call.GuestToken
	}
	results, errs := w.joinAtOnce(links)
	admitted, queued := 0, 0
	for i, res := range results {
		switch {
		case errs[i] == nil && res.Outcome == callrules.OutcomeAdmit:
			admitted++
		case errs[i] == nil && res.Outcome == callrules.OutcomeQueue:
			queued++
		default:
			t.Errorf("join %d answered %+v, %v: every join is either admitted or queued", i, res, errs[i])
		}
	}
	if admitted != 2 || queued != 10 {
		t.Fatalf("%d joins were admitted and %d queued, want 2 and 10: the server has six seats and the four hosts hold four", admitted, queued)
	}
	carried := 0
	for _, call := range calls {
		carried += w.admittedIn(call.Call.ID)
	}
	if carried != 6 {
		t.Fatalf("the server carries %d admitted people, want exactly its 6 seats", carried)
	}
}

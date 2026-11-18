package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/integrations/calls"
)

// calls_surface_db_test.go -- a call through the real edge: the edge's resolver, switch,
// stamp and cookie rule in front of the real bff mount, the real calls service, the real
// engine and a real Postgres, with what a browser holding somebody else's credentials would
// send. Everything the bff half's own tests do by stamping a request themselves happens
// here because a browser asked the edge.
//
// THE MEDIA SERVER IS THE ONE FAKE (callsEdgeMedia): media never passes through MemQL, and
// what is on trial here is everything between a browser and the rows.
//
// WITH NO BUILD TAG, for access_surface_db_test.go's reason: the db-gated lane compiles
// this package untagged. Postgres-gated: a skip is what an unreachable database produces,
// and MEMQL_REQUIRE_DB=1 turns it into a failure.

// callsEdgeMedia is the media server's control API: it opens any room it is asked for.
type callsEdgeMedia struct{}

func (callsEdgeMedia) CreateRoom(context.Context, string, uint32) error { return nil }
func (callsEdgeMedia) DeleteRoom(context.Context, string) error         { return nil }
func (callsEdgeMedia) Participants(context.Context, string) ([]calls.MediaParticipant, error) {
	return nil, nil
}
func (callsEdgeMedia) RemoveParticipant(context.Context, string, string) error { return nil }
func (callsEdgeMedia) MuteTrack(context.Context, string, string, string) error { return nil }

// callsEdgeEngine is the narrow engine the calls integration asks for.
type callsEdgeEngine struct{ engine *memql.MemQLEngine }

func (e callsEdgeEngine) Execute(ctx context.Context, q string) (any, error) {
	return e.engine.Execute(ctx, q)
}

// callsEdgeBundle is the deployable's bundle: one page.
type callsEdgeBundle struct{}

func (callsEdgeBundle) Open(string) (fs.FS, error) {
	return fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>A call</title>")}}, nil
}

// callsEdgeBFF is the bff behind the edge, and what it was last handed.
type callsEdgeBFF struct {
	handler http.Handler
	mu      sync.Mutex
	count   int
	last    http.Header
}

func (b *callsEdgeBFF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.count++
	b.last = r.Header.Clone()
	b.mu.Unlock()
	b.handler.ServeHTTP(w, r)
}

func (b *callsEdgeBFF) seen() (int, http.Header) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count, b.last
}

var callsEdgeCookie = regexp.MustCompile(`^__Host-memql_calls=([a-z2-7]{32}); Path=/; HttpOnly; Secure; SameSite=Strict$`)

// callsEdgeConfirm is the three confirmations every seat records.
const callsEdgeConfirm = `"confirm":{"adult":true,"noMinors":true,"terms":"2026-11-17"}`

func TestACallThroughTheRealEdge(t *testing.T) {
	eng, db := workTemplateDBEngineAndDB(t)
	// Built here, never written down: the master key seals the media credentials and the
	// key pepper this test's bootstrap writes.
	t.Setenv(secret.EnvMasterKey, strings.Repeat("6c", 32))
	t.Setenv(edge.CallsMediaHostSuffixEnv, "media.example.test")
	ctx := context.Background()

	// THE MEDIA SERVER THIS CLUSTER HAS, and only it: what another test left is taken away
	// now and when this one ends, so placement and the pepper are this test's own.
	wipeMedia := func() {
		_, err := db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", "v1:calls:mediaServer").Exec(ctx)
		require.NoError(t, err)
		_, err = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", "v1:platform:globalSecret").
			Where("payload->>'name' IN (?)", bun.In([]string{calls.SecretMediaBootstrap, calls.SecretMediaWebhook, calls.SecretKeyPepper})).Exec(ctx)
		require.NoError(t, err)
	}
	wipeMedia()
	t.Cleanup(wipeMedia)

	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = gates.Close() })
	integration := calls.New(calls.Deps{
		Engine:              callsEdgeEngine{eng},
		GateDB:              func() *sql.DB { return gates },
		ResolveSystemSecret: eng.ResolveSystemSecret,
		Logger:              slog.New(slog.DiscardHandler),
		NewMediaBackend: func(calls.MediaServer, calls.MediaCredential) (calls.MediaBackend, error) {
			return callsEdgeMedia{}, nil
		},
	})
	require.NoError(t, eng.RegisterIntegration(integration))
	_, err = integration.Bootstrap(ctx, calls.MediaConfig{
		Provider: "docker-local", APIURL: "http://m1.media.example.test:7880", PublicURL: "wss://m1.media.example.test",
		Seats: 50, APIKey: "APIedgekey", APISecret: strings.Repeat("k", 40),
		WebhookKey: "APIedgewebhook", WebhookSecret: strings.Repeat("w", 40),
	})
	require.NoError(t, err)

	// The deployable: live, owned, in an organization, and with calls OFF.
	w := newAccessWorld(t, &accessSurfaceRig{eng: eng, db: db})
	site := "cs-edge-" + w.suffix
	host := site + ".example.test"
	w.insert("v1:platform:site", site, map[string]any{
		"hostname": host, "kind": "spa", "status": "live", "bundleRef": "blob://sites/" + site + "/v1/",
		"ownerUserId": "v1:identity:user:" + w.owner, "accountId": w.account,
		"packageId": "v1:platform:package:" + w.pkg, "packageDeployableName": "meetings",
	})
	t.Cleanup(func() {
		_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).
			Where("payload->>'siteId' IN (?)", bun.In([]string{site, "v1:platform:site:" + site})).Exec(context.Background())
	})

	// The bff as the bff node mounts it: the real middleware ahead of the real mux.
	a := &App{mux: http.NewServeMux(), engine: eng, Logger: slog.New(slog.DiscardHandler), identityVerifier: &verifier.Verifier{}}
	a.mountCallsSurface()
	bff := &callsEdgeBFF{handler: a.identityHTTPMiddleware()(a.mux)}
	bffServer := httptest.NewServer(bff)
	t.Cleanup(bffServer.Close)

	// The edge as the edge node builds it: the real resolver over the real engine, which
	// reads the switch off the row, in front of the bff.
	resolver := edge.NewResolver(edge.NewEngineExecutor(&EdgeEngineAdapter{Engine: eng}), time.Hour)
	front := httptest.NewServer(edge.NewHandler(edge.Options{
		Resolver: resolver, Opener: callsEdgeBundle{}, Logger: slog.New(slog.DiscardHandler), APITarget: bffServer.URL,
	}))
	t.Cleanup(front.Close)

	// WHAT A BROWSER ON THIS ORIGIN SENDS ALONG, whatever the request is about: a sign-in
	// cookie, an access session, the deployable's own cookie and a bearer. The bff sees none.
	const passengers = "memql_auth=a-revoked-sign-in; __Host-memql_access=mql_acs_somebody-elses; theme=dark"
	send := func(method, path, body, callsCookie string) *http.Response {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, front.URL+path, reader)
		require.NoError(t, err)
		req.Host = host
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "https://"+host)
		}
		cookie := passengers
		if callsCookie != "" {
			cookie += "; " + callsCookie
		}
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Authorization", "Bearer not-a-credential-this-surface-reads")
		res, err := front.Client().Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = res.Body.Close() })
		return res
	}
	read := func(res *http.Response) string {
		t.Helper()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return string(body)
	}
	cookieOf := func(res *http.Response) string {
		t.Helper()
		match := callsEdgeCookie.FindStringSubmatch(res.Header.Get("Set-Cookie"))
		require.NotNil(t, match, "Set-Cookie = %q", res.Header.Get("Set-Cookie"))
		return "__Host-memql_calls=" + match[1]
	}
	operator := func(name string, args map[string]any) map[string]any {
		t.Helper()
		return w.builtin(name, args)
	}

	// WITH THE SWITCH OFF THE EDGE ANSWERS, and the bff never hears of it.
	res := send(http.MethodPost, "/_memql/calls/unlock", `{"key":"not-yet"}`, "")
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.Equal(t, `{"error":"not_found"}`, read(res))
	count, _ := bff.seen()
	require.Zero(t, count, "a deployable with calls off forwarded a calls request")

	// The operator turns calls on and adds a key, as a person does in Deployables. The
	// edge's copy of the site is dropped as the change feed drops it in a cluster.
	operator("callsSiteEnable", map[string]any{"siteId": site, "on": true})
	resolver.Invalidate(host)
	key, _ := operator("callsTestKeyCreate", map[string]any{"siteId": site, "label": "Through the edge"})["key"].(string)
	require.NotEmpty(t, key)

	// THE PAGE: its policy names the media servers and lets it ask for the camera.
	res = send(http.MethodGet, "/", "", "")
	require.Equal(t, http.StatusOK, res.StatusCode, read(res))
	policy := res.Header.Get("Content-Security-Policy")
	require.Contains(t, policy, " wss://*.media.example.test:* ")
	require.Equal(t, "geolocation=(), microphone=(self), camera=(self)", res.Header.Get("Permissions-Policy"))

	// A STRANGER COSTS THE DATABASE NOTHING: no session, nothing read, nothing set.
	statements := &accessStatements{}
	db.AddQueryHook(statements)
	require.Empty(t, statements.during(func() { res = send(http.MethodGet, "/_memql/calls/session", "", "") }),
		"a stranger's session check reached the database through the edge")
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	require.JSONEq(t, `{"error":"no_session"}`, read(res))
	require.Empty(t, res.Header.Values("Set-Cookie"))

	// Unlock: the key for a host session, which only the cookie carries.
	res = send(http.MethodPost, "/_memql/calls/unlock", `{"key":"`+key+`"}`, "")
	require.Equal(t, http.StatusOK, res.StatusCode)
	hostCookie := cookieOf(res)
	require.NotContains(t, read(res), strings.TrimPrefix(hostCookie, "__Host-memql_calls="))

	// WHAT THE BFF WAS HANDED: the edge's stamp and the calls cookie, and nothing else a
	// browser sent.
	res = send(http.MethodGet, "/_memql/calls/session", "", hostCookie)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Contains(t, read(res), `"kind":"host"`, "a key's session is a host's")
	_, handed := bff.seen()
	require.Equal(t, []string{hostCookie}, handed.Values("Cookie"))
	require.Empty(t, handed.Values("Authorization"))
	require.Equal(t, []string{site}, handed.Values(memql.CallsStampSiteHeader))
	require.Len(t, handed.Values(memql.CallsStampOwnerHeader), 1)
	require.Equal(t, w.owner, memql.BareShortId(handed.Get(memql.CallsStampOwnerHeader)),
		"the stamped owner is not the owner the row names")
	require.Empty(t, handed.Values(memql.AccessStampSiteHeader))

	// Start: the host is seated, and the media server the pass names is one the page may
	// reach.
	res = send(http.MethodPost, "/_memql/calls/start", `{"name":"Host",`+callsEdgeConfirm+`}`, hostCookie)
	body := read(res)
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	var started struct {
		Call struct {
			ID         string `json:"id"`
			GuestToken string `json:"guestToken"`
		} `json:"call"`
		Join struct {
			Outcome string `json:"outcome"`
			URL     string `json:"url"`
			Pass    string `json:"pass"`
			Host    bool   `json:"host"`
		} `json:"join"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &started))
	require.Equal(t, "admit", started.Join.Outcome)
	require.True(t, started.Join.Host)
	require.NotEmpty(t, started.Join.Pass)
	require.Equal(t, "wss://m1.media.example.test", started.Join.URL)
	require.True(t, strings.HasSuffix(strings.TrimPrefix(started.Join.URL, "wss://"), ".media.example.test"),
		"the media server the pass names is not under the suffix the page policy admits")

	// A guest joins by the link, with no session, and is handed a seat's.
	res = send(http.MethodPost, "/_memql/calls/join", `{"token":"`+started.Call.GuestToken+`","name":"Guest",`+callsEdgeConfirm+`}`, "")
	body = read(res)
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	require.Contains(t, body, `"outcome":"admit"`)
	guestCookie := cookieOf(res)
	require.NotEqual(t, hostCookie, guestCookie)

	// The host's session lists the guest, which is how the host's page learns who is in.
	res = send(http.MethodGet, "/_memql/calls/session", "", hostCookie)
	body = read(res)
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	require.Contains(t, body, `"displayName":"Guest"`)

	// The guest hangs up.
	res = send(http.MethodPost, "/_memql/calls/leave", `{}`, guestCookie)
	require.Equal(t, http.StatusNoContent, res.StatusCode, read(res))

	// THE BFF RE-CHECKS THE SWITCH. Turned off while the edge still holds its old copy of the
	// site, a request is forwarded, and the bff, reading the row afresh, refuses it.
	operator("callsSiteEnable", map[string]any{"siteId": site, "on": false})
	before, _ := bff.seen()
	res = send(http.MethodGet, "/_memql/calls/session", "", hostCookie)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.Equal(t, `{"error":"not_found"}`, read(res))
	after, _ := bff.seen()
	require.Equal(t, before+1, after, "the edge's stale copy should still have forwarded this one")

	// And once the edge's copy is current, the edge answers it alone.
	resolver.Invalidate(host)
	res = send(http.MethodGet, "/_memql/calls/session", "", hostCookie)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.Equal(t, `{"error":"not_found"}`, read(res))
	last, _ := bff.seen()
	require.Equal(t, after, last, "a deployable with calls off forwarded a calls request")
}

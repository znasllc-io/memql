package shopify

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	identityhttp "github.com/znasllc-io/memql/component/identity/http"
	"github.com/znasllc-io/memql/component/identity/refresh"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// connect_hop_db_test.go -- THE CROSS-NODE HOP for Connect Shopify (design
// 12.9; CLAUDE.md "Multi-node is the DEFAULT").
//
// Begin runs on the bff and the callback on the identity node, and those are
// two processes with one database between them. So this runs begin through
// ENGINE A's builtin and the callback through the identity node's real route on
// a server wired to a SEPARATE ENGINE B over the same Postgres: separate engine
// memory, separate StoreRegistry caches, one database. It lives here, in a
// db-gated package the db-tests lane runs, rather than only in test/clustere2e,
// for memql#4352's reason: a gate skipped by default cannot stand between a
// feature and the bug it exists to prevent.
//
// Each engine keeps its own result cache, as each node does. In a cluster the
// mesh carries ONE broadcast between them for it, cache.invalidate.*
// (component/node/routing.go), and this test carries exactly that broadcast
// between the two engines and nothing else -- no graph events, no automations.
//
// TO CONFIRM IT IS LOAD-BEARING: make the callback read the store or the saved
// app through B's StoreRegistry (warmed below, before A's changes), or keep any
// of begin's state in process memory on A, and a round here fails.
//
// Postgres-gated; MEMQL_REQUIRE_DB=1 turns the skip into a failure.

// recordingEngine is a real engine that remembers every statement handed to it,
// so the test can say what no statement carried.
type recordingEngine struct {
	*memql.MemQLEngine
	mu    stdsync.Mutex
	stmts []string
}

func (r *recordingEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	r.mu.Lock()
	r.stmts = append(r.stmts, q)
	r.mu.Unlock()
	return r.MemQLEngine.Execute(ctx, q)
}

func (r *recordingEngine) statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stmts...)
}

// lockedBuffer is a log sink two servers can write at once.
type lockedBuffer struct {
	mu  stdsync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// meshBetween gives each engine a bus and its result-cache evictor, and relays
// cache.invalidate.* from each bus to the other as the mesh's broadcast rule
// does, marking a relayed event remote so it is not relayed back. It answers a
// settle func: delivery is asynchronous on a bus, as it is over the mesh.
func meshBetween(t *testing.T, a, b *memql.MemQLEngine) func() {
	t.Helper()
	quiet := events.WithLogger(slog.New(slog.DiscardHandler))
	busA, busB := events.NewBus(quiet), events.NewBus(quiet)
	a.SetEventBus(busA)
	b.SetEventBus(busB)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.StartCacheInvalidationSubscriber(ctx)
	b.StartCacheInvalidationSubscriber(ctx)
	relay := func(from, to *events.Bus, origin string) {
		t.Cleanup(from.Subscribe("cache.invalidate.#", func(ev events.Event) {
			if ev.IsRemote() {
				return
			}
			ev.OriginNodeId = origin
			to.Publish(ev)
		}))
	}
	relay(busA, busB, "bff")
	relay(busB, busA, "identity")
	// ponytail: a fixed wait for two goroutine hops (relay, then evictor);
	// a delivery-tracking bus would make it exact if this ever flakes.
	return func() { time.Sleep(200 * time.Millisecond) }
}

func TestConnectShopifyBeginsOnOneEngineAndFinishesOnAnother(t *testing.T) {
	engA, rawA := authorityEngine(t)
	if engA == nil {
		return
	}
	engB, rawB := authorityEngine(t)
	const identityBase = "https://identity.hop.example.test"
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ef", 32))
	t.Setenv("MEMQL_IDENTITY_BASE_URL", identityBase)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)

	dev := "hop-dev-" + suffix
	shop := "hop-" + suffix + ".myshopify.com"
	storeID := "hop-" + suffix
	pkg := "v1:platform:package:hop-" + suffix
	site := "hop-sf-" + suffix
	secret1, secret2 := "hop-app-secret-one-"+suffix, "hop-app-secret-two-"+suffix
	admin1, admin2 := "shpat_hop_admin_one_"+suffix, "shpat_hop_admin_two_"+suffix
	minted1, minted2 := "hop-minted-one-"+suffix, "hop-minted-two-"+suffix
	credentials := []string{secret1, secret2, admin1, admin2, minted1, minted2, "hop-code-"}

	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	settle := meshBetween(t, engA, engB)

	// Engine A: the bff, where the Store panel's builtins run.
	recA := &recordingEngine{MemQLEngine: engA}
	connA := NewConnector(recA, logger, NewStoreRegistry(recA, engA.ResolveSystemSecret), NewAdminClient())
	connA.WithDatabase(func() *sql.DB { return rawA })
	if err := recA.RegisterIntegration(NewIntegration(connA)); err != nil {
		t.Fatalf("register the shopify integration on A: %v", err)
	}

	// Engine B: the identity node, its own connector and its own registry.
	recB := &recordingEngine{MemQLEngine: engB}
	connB := NewConnector(recB, logger, NewStoreRegistry(recB, engB.ResolveSystemSecret), NewAdminClient())
	connB.WithDatabase(func() *sql.DB { return rawB })
	token := newFakeTokenEndpoint(t)
	admin := newFakeAdmin(t)
	connB.connectTokenURL = func(string) string { return token.server.URL + "/admin/oauth/access_token" }
	connB.admin.endpoint = func(Store) string { return admin.server.URL + "/graphql" }
	connB.admin.sleep = func(context.Context, time.Duration) error { return nil }
	connB.deliver = func(s Store) string { return "https://api.hop.example.test/inbound/shopify-" + s.ID }
	// Step 15 inline, so a round's webhook job cannot land inside the next.
	connB.background = func(f func()) { f() }
	admin.reply("ShopifyConnectPlan", map[string]any{"shop": map[string]any{"plan": map[string]any{"publicDisplayName": "Basic"}}})
	admin.reply("ShopifyWebhookSubscriptions", map[string]any{"webhookSubscriptions": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{},
	}})
	admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{
		"webhookSubscription": map[string]any{"id": "gid://shopify/WebhookSubscription/1"}, "userErrors": []any{},
	}})
	mintAs := func(value string) {
		admin.reply("ShopifyStorefrontTokenCreate", map[string]any{"storefrontAccessTokenCreate": map[string]any{
			"storefrontAccessToken": map[string]any{"accessToken": value}, "userErrors": []any{},
		}})
	}
	issueAs := func(value string) {
		token.mu.Lock()
		token.body = `{"access_token":"` + value + `","scope":"` + strings.Join(ConnectScopes(), ",") + `"}`
		token.mu.Unlock()
	}
	srvB := &identityhttp.Server{
		Cfg:            componentIdentity.Config{BaseURL: identityBase},
		Store:          &componentIdentity.Store{Engine: recB, Logger: logger, DirectDB: func() *sql.DB { return rawB }},
		Audit:          &componentIdentity.SlogAuditLogger{Logger: logger, DB: &componentIdentity.EngineAuditSink{Engine: engB, Logger: logger}},
		Logger:         logger,
		ShopifyConnect: connB,
	}
	mux := http.NewServeMux()
	srvB.Mount(mux)

	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range []string{"v1:identity:user:" + dev, "v1:platform:site:" + site, "v1:platform:packageDeployment:hop-run-" + suffix, pkg, "v1:identity:authSession:" + sessionFor(dev)} {
			_, _ = rawA.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id = $1`, id)
		}
		_, _ = rawA.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept IN ('v1:platform:globalSecret','v1:platform:globalVariable') AND payload->>'name' LIKE $1`,
			"SHOPIFY_HOP-"+suffix+"%")
		_, _ = rawA.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`, storeID)
		_, _ = rawA.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:githubConnectState' AND payload->>'userId' LIKE $1`, "%"+dev)
		_, _ = rawA.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND payload->>'domain' = $1`, shop)
	})
	seed := func(concept, id string, payload map[string]any) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		q := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, langparser.QuoteString(concept), langparser.QuoteString(id), string(body))
		if _, err := engA.Execute(seedCtx(), q); err != nil {
			t.Fatalf("seed %s %s: %v", concept, id, err)
		}
	}
	seed("v1:identity:user", dev, map[string]any{
		"displayName": "Hop Dev", "primaryEmail": dev + "@hop.example.test", "role": string(auth.RoleDeveloper), "active": true,
	})
	// The browser the developer signed in with (D16): its session is the one
	// every call below names (actorCtx's sid), and its refresh cookie is what
	// the callback proves that session with.
	browserCookie := "hop-browser-refresh-" + suffix
	seed("v1:identity:authSession", sessionFor(dev), map[string]any{
		"userId": dev, "subject": dev, "tokenHash": "hop-token-hash-" + suffix, "source": "bff_exchange",
		"expiresAt": time.Now().UTC().Add(time.Hour).Format(time.RFC3339), "refreshTokenHash": refresh.HashRefreshToken(browserCookie),
	})
	seed("v1:platform:site", site, map[string]any{
		"ownerUserId": dev, "title": site, "hostname": site + ".hop.example.test", "status": "draft",
		"kind": "shopify_storefront", "bundleRef": "blob://sites/" + site + "/v1/",
		"packageId": pkg, "packageDeployableName": "storefront",
	})
	// The package the run names: a packageDeployment's packageId must resolve
	// to a package of the same organization (memql#5598).
	seed("v1:platform:package", strings.TrimPrefix(pkg, "v1:platform:package:"), map[string]any{
		"ownerUserId": dev, "name": "storefront", "sourceKind": "artifact", "status": "active",
	})
	seed("v1:platform:packageDeployment", "hop-run-"+suffix, map[string]any{
		"ownerUserId": dev, "packageId": pkg, "status": "succeeded",
		"report": map[string]any{"deployables": []any{map[string]any{
			"name": "storefront", "kind": "shopify_storefront", "binding": map[string]any{"store": shop},
		}}},
		"deployables": []any{map[string]any{"name": "storefront", "siteId": "v1:platform:site:" + site}},
	})

	devCtx := actorCtx(dev, auth.RoleDeveloper)
	call := func(ctx context.Context, format string, args ...any) map[string]any {
		t.Helper()
		out := builtinReply(t, engA, ctx, fmt.Sprintf(format, args...))
		if out["reason"] != connectReasonOK {
			t.Fatalf("%s: %v", fmt.Sprintf(format, args...), out)
		}
		return out
	}
	save := func(clientID, secret string) time.Time {
		call(devCtx, `builtin shopifyStoreAppSave(siteId: %s, clientId: %s, clientSecret: %s)`,
			langparser.QuoteString(site), langparser.QuoteString(clientID), langparser.QuoteString(secret))
		return time.Now()
	}
	// begin runs Begin on A and answers the plaintext state Shopify will echo.
	begin := func(wantClient string) string {
		out := call(devCtx, `builtin shopifyConnectBegin(siteId: %s, returnPath: "/?open=store")`, langparser.QuoteString(site))
		u, err := url.Parse(out["authorizeUrl"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if u.Host != shop || u.Query().Get("client_id") != wantClient || u.Query().Get("redirect_uri") != identityBase+githubconnect.ShopifyCallbackPath {
			t.Fatalf("authorize URL %s, want %s's approve page for %s coming back to the identity node", u, shop, wantClient)
		}
		return u.Query().Get("state")
	}
	// callbackFrom is Shopify sending a browser to B's identity route; callback
	// is the browser that began.
	callbackFrom := func(cookie, secret, state, code string) url.Values {
		t.Helper()
		params := map[string]string{"code": code, "shop": shop, "state": state, "timestamp": fmt.Sprint(time.Now().Unix())}
		r := httptest.NewRequest(http.MethodGet, identityBase+"/auth/shopify/complete"+"?"+signedQuery(secret, params).Encode(), nil)
		r.Header.Set("X-Forwarded-Proto", "https")
		r.AddCookie(&http.Cookie{Name: "memql_refresh", Value: cookie})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("callback answered %d: %s", rec.Code, rec.Body.String())
		}
		loc := rec.Header().Get("Location")
		for _, c := range credentials {
			if strings.Contains(loc, c) {
				t.Errorf("the redirect carries a credential: %s", loc)
			}
		}
		u, err := url.Parse(loc)
		if err != nil {
			t.Fatal(err)
		}
		return u.Query()
	}
	callback := func(secret, state, code string) url.Values {
		t.Helper()
		return callbackFrom(browserCookie, secret, state, code)
	}
	ownerRead := func(q string) map[string]any {
		t.Helper()
		res, err := engA.Execute(seedCtx(), q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		rows := memql.MaterializeRows(res)
		if len(rows) != 1 {
			t.Fatalf("%s answered %d rows", q, len(rows))
		}
		return rows[0]
	}
	storeRow := func() Store {
		t.Helper()
		s, ok := storeFromRow(ownerRead(renderCall("storeById", map[string]any{"storeId": storeID})))
		if !ok {
			t.Fatal("the store row does not read as a store")
		}
		return s
	}
	secretOf := func(name string) string {
		t.Helper()
		v, err := engA.ResolveSystemSecret(context.Background(), name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		return v
	}
	storeRows := func() int {
		t.Helper()
		var n int
		if err := rawA.QueryRowContext(context.Background(),
			`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND payload->>'domain' = $1`, shop).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	audits := func(action string) int {
		t.Helper()
		var n int
		if err := rawA.QueryRowContext(context.Background(),
			`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1 AND payload->>'action' = $2`,
			storeID, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	spent := func(state string) bool {
		row, err := (&componentIdentity.Store{Engine: engA}).LookupGithubConnectState(context.Background(), componentIdentity.HashConnectState(state))
		if err != nil || row == nil {
			t.Fatalf("lookup the state: %v %v", row, err)
		}
		return !row.ConsumedAt.IsZero()
	}
	boundStore := func() (string, string) {
		row := ownerRead(renderCall("siteById", map[string]any{"siteId": site}))
		binding, _ := rowValue(row, "binding").(map[string]any)
		return memql.BareShortId(mapString(binding, "storeId")), mapString(row, "status")
	}
	adminName := storeSecretName(storeID, suffixAdminToken)
	webhookName := storeSecretName(storeID, suffixWebhookSecret)

	// ---- Round 1: a first Connect. B's registry is warm -- and empty -- before
	// the Save on A, and the callback lands on B inside its 30-second TTL.
	if _, err := connB.stores.Stores(context.Background()); err != nil {
		t.Fatalf("warm B's registry: %v", err)
	}
	issueAs(admin1)
	mintAs(minted1)
	savedAt := save("hop-client-one", secret1)
	state1 := begin("hop-client-one")
	settle()
	// The approval opened in ANOTHER browser (D16): Shopify signed it, the
	// state is live, and it is not this browser's Connect -- nothing is spent,
	// nothing is written, and the browser that began can still finish.
	if got := callbackFrom("another-browser-"+suffix, secret1, state1, "hop-code-elsewhere"); got.Get("shopify") != "connect_state_invalid" || got.Get("site") != "" {
		t.Fatalf("a callback from another browser came back %v, want connect_state_invalid naming nothing", got)
	}
	if spent(state1) || storeRows() != 0 || token.count() != 0 {
		t.Fatalf("another browser's callback: spent=%v stores=%d exchanges=%d", spent(state1), storeRows(), token.count())
	}
	got := callback(secret1, state1, "hop-code-one")
	settle()
	if elapsed := time.Since(savedAt); elapsed >= 30*time.Second {
		t.Fatalf("the Save and the callback were %s apart, outside the registry's 30-second window: the hop was not measured", elapsed)
	}
	if got.Get("shopify") != connectResultConnected || got.Get("site") != site {
		t.Fatalf("round 1 came back %v, want connected for %s", got, site)
	}
	s := storeRow()
	if s.AppClientID != "hop-client-one" || s.AdminTokenRef != adminName || s.WebhookSecretRef != webhookName ||
		s.StorefrontTokenRef == "" || memql.BareShortId(s.OwnerUserID) != dev || s.Plan != "Basic" || len(s.ScopesGranted) != len(ConnectScopes()) {
		t.Fatalf("round 1 store row: %+v", s)
	}
	if secretOf(adminName) != admin1 || secretOf(webhookName) != secret1 || secretOf(s.StorefrontTokenRef) != minted1 {
		t.Fatal("round 1 sealed the wrong values")
	}
	if pending, err := connA.pendingClientID(context.Background(), storeID); err != nil || pending != "" {
		t.Fatalf("the pending app survived its promotion: %q %v", pending, err)
	}
	if bound, status := boundStore(); bound != storeID || status != "draft" {
		t.Fatalf("round 1 binding %q status %q, want %s and still a draft", bound, status, storeID)
	}
	if !spent(state1) || storeRows() != 1 || audits("shopify_connected") != 1 {
		t.Fatalf("round 1: spent=%v stores=%d audits=%d", spent(state1), storeRows(), audits("shopify_connected"))
	}

	// ---- Round 2: a reconnect with a NEW app, with changes made on A seconds
	// before the callback on B, and B's registry warmed before them.
	if _, err := connB.stores.Stores(context.Background()); err != nil {
		t.Fatalf("warm B's registry: %v", err)
	}
	// An owner points the privacy exports at somebody else: Connect must
	// never move them back.
	if _, err := engA.Execute(seedCtx(), renderCall("updateStore", map[string]any{"storeId": storeID, "ownerUserId": "hop-merchant-" + suffix})); err != nil {
		t.Fatalf("set the owner: %v", err)
	}
	// The developer clears the Storefront token on A, so the next Connect mints.
	if out := builtinReply(t, engA, devCtx, fmt.Sprintf(`builtin shopifyStorefrontTokenSet(siteId: %s, token: "")`, langparser.QuoteString(site))); out["reason"] != connectReasonOK {
		t.Fatalf("clear the token: %v", out)
	}
	save("hop-client-two", secret2)
	// A Save never changes a connected store's live credentials.
	if s := storeRow(); s.AppClientID != "hop-client-one" || s.WebhookSecretRef != webhookName || secretOf(webhookName) != secret1 {
		t.Fatalf("a Save moved the live credentials: %+v", s)
	}
	issueAs(admin2)
	mintAs(minted2)
	state2 := begin("hop-client-two")
	settle()
	if got := callback(secret2, state2, "hop-code-two"); got.Get("shopify") != connectResultReconnected {
		t.Fatalf("round 2 came back %v, want reconnected", got)
	}
	settle()
	s = storeRow()
	if storeRows() != 1 {
		t.Fatalf("a reconnect made %d store rows", storeRows())
	}
	if s.AppClientID != "hop-client-two" || secretOf(webhookName) != secret2 || secretOf(adminName) != admin2 {
		t.Fatalf("round 2 did not promote the approved app: %+v", s)
	}
	if memql.BareShortId(s.OwnerUserID) != "hop-merchant-"+suffix {
		t.Fatalf("a reconnect moved the privacy owner to %q", s.OwnerUserID)
	}
	if s.StorefrontTokenRef == "" || secretOf(s.StorefrontTokenRef) != minted2 || admin.countOp("ShopifyStorefrontTokenCreate") != 2 {
		t.Fatalf("the token A cleared was not minted again on B (ref %q, mints %d)", s.StorefrontTokenRef, admin.countOp("ShopifyStorefrontTokenCreate"))
	}
	if !spent(state2) || audits("shopify_reconnected") != 1 {
		t.Fatalf("round 2: spent=%v audits=%d", spent(state2), audits("shopify_reconnected"))
	}

	// ---- Round 3: Connect again with the CURRENT app -- no Save -- verified on
	// B from the store row A's round left. Nothing is minted twice.
	state3 := begin("hop-client-two")
	settle()
	if got := callback(secret2, state3, "hop-code-three"); got.Get("shopify") != connectResultReconnected {
		t.Fatalf("round 3 came back %v, want reconnected", got)
	}
	settle()
	if storeRows() != 1 || admin.countOp("ShopifyStorefrontTokenCreate") != 2 || storeRow().AppClientID != "hop-client-two" {
		t.Fatalf("round 3: stores=%d mints=%d", storeRows(), admin.countOp("ShopifyStorefrontTokenCreate"))
	}
	if bound, _ := boundStore(); bound != storeID {
		t.Fatalf("the binding moved to %q", bound)
	}

	// ---- No credential anywhere: no statement either engine was handed, no
	// log line, no audit row.
	for _, rec := range []*recordingEngine{recA, recB} {
		for _, stmt := range rec.statements() {
			for _, c := range credentials {
				if strings.Contains(stmt, c) {
					t.Errorf("a statement carries a credential: %s", stmt)
				}
			}
		}
	}
	for _, c := range credentials {
		if strings.Contains(logs.String(), c) {
			t.Errorf("a credential reached the log")
		}
	}
	rows, err := rawA.QueryContext(context.Background(), `SELECT payload::text FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`, storeID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		for _, c := range credentials {
			if strings.Contains(payload, c) {
				t.Errorf("an audit row carries a credential: %s", payload)
			}
		}
	}
}

package shopify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// connect_begin_db_test.go -- shopifyConnectBegin (design 12.4) against a real
// engine over a real Postgres: the store part asked at the builtin, the state
// row admitted by the @serverOnly create (which only internal origin reaches),
// its new fields read back by the lookup the callback consumes through, and the
// row spendable by its own flow alone.
//
// Postgres-gated; MEMQL_REQUIRE_DB=1 turns the skip into a failure.

func TestConnectBeginWritesTheStateAgainstARealEngine(t *testing.T) {
	eng, raw := authorityEngine(t)
	if eng == nil {
		return
	}
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("cd", 32))
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "https://identity.connect.example.test")
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)

	conn := NewConnector(eng, slog.New(slog.DiscardHandler), NewStoreRegistry(eng, eng.ResolveSystemSecret), NewAdminClient())
	if err := eng.RegisterIntegration(NewIntegration(conn)); err != nil {
		t.Fatalf("register the shopify integration: %v", err)
	}

	dev := "begin-dev-" + suffix
	stranger := "begin-stranger-" + suffix
	writer := "begin-writer-" + suffix
	shop := "begin-" + suffix + ".myshopify.com"
	storeID := "begin-" + suffix
	pkg := "v1:platform:package:begin-" + suffix
	mine := "begin-sf-" + suffix
	theirs := "begin-theirs-" + suffix

	var cleanup []string
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range cleanup {
			_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id = $1`, id)
		}
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept IN ('v1:platform:globalSecret','v1:platform:globalVariable') AND payload->>'name' LIKE $1`,
			"SHOPIFY_BEGIN-%"+suffix+"%")
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`, storeID)
		_, _ = raw.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:githubConnectState' AND payload->>'userId' LIKE $1`, "%begin-%"+suffix)
	})
	seedWith := func(ctx context.Context, concept, id string, payload map[string]any) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		q := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, langparser.QuoteString(concept), langparser.QuoteString(id), string(body))
		if _, err := eng.Execute(ctx, q); err != nil {
			t.Fatalf("seed %s %s: %v", concept, id, err)
		}
		cleanup = append(cleanup, concept+":"+id)
	}
	seed := func(concept, id string, payload map[string]any) {
		t.Helper()
		seedWith(seedCtx(), concept, id, payload)
	}
	for id, owner := range map[string]string{mine: dev, theirs: stranger} {
		ctx := seedCtx()
		if owner == stranger {
			ctx = untiedSeedCtx()
		}
		seedWith(ctx, "v1:platform:site", id, map[string]any{
			"ownerUserId": owner, "title": id, "hostname": id + ".connect.example.test", "status": "draft",
			"kind": "shopify_storefront", "bundleRef": "blob://sites/" + id + "/v1/",
			"packageId": pkg, "packageDeployableName": "storefront",
		})
	}
	// The package the run names: a packageDeployment's packageId must resolve
	// to a package of the same organization (memql#5598).
	seed("v1:platform:package", strings.TrimPrefix(pkg, "v1:platform:package:"), map[string]any{
		"ownerUserId": dev, "name": "storefront", "sourceKind": "artifact", "status": "active",
	})
	seed("v1:platform:packageDeployment", "begin-run-"+suffix, map[string]any{
		"ownerUserId": dev, "packageId": pkg, "status": "succeeded",
		"report": map[string]any{"deployables": []any{map[string]any{
			"name": "storefront", "kind": "shopify_storefront", "binding": map[string]any{"store": shop},
		}}},
		"deployables": []any{map[string]any{"name": "storefront", "siteId": "v1:platform:site:" + mine}},
	})

	states := func() int {
		t.Helper()
		var n int
		if err := raw.QueryRowContext(context.Background(),
			`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept = 'v1:identity:githubConnectState' AND payload->>'userId' LIKE $1`,
			"%begin-%"+suffix).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	beginCall := func(siteId, returnPath string) string {
		return fmt.Sprintf(`builtin shopifyConnectBegin(siteId: %s, returnPath: %s)`, langparser.QuoteString(siteId), langparser.QuoteString(returnPath))
	}
	devCtx := actorCtx(dev, auth.RoleDeveloper)

	// Below the store part the builtin gate refuses before the handler runs.
	if _, err := eng.Execute(actorCtx(writer, auth.RoleWriter), beginCall(mine, "/")); err == nil || !strings.Contains(err.Error(), "capability_not_held") {
		t.Fatalf("a writer's begin was not refused by the store part: %v", err)
	}
	// A resolution refusal and "nothing to connect" both write no state.
	if got := builtinReply(t, eng, devCtx, beginCall(theirs, "/"))["reason"]; got != connectReasonSiteNotWritable {
		t.Fatalf("begin on a stranger's storefront: reason %v", got)
	}
	if got := builtinReply(t, eng, devCtx, beginCall(mine, "/"))["reason"]; got != connectReasonShopifyAppNotSaved {
		t.Fatalf("begin with no app saved: reason %v", got)
	}
	if n := states(); n != 0 {
		t.Fatalf("%d state rows after three refusals, want 0", n)
	}

	// The reachable positive: save an app, then begin.
	clientID := "begin-client-" + suffix
	if _, err := eng.Execute(devCtx, fmt.Sprintf(`builtin shopifyStoreAppSave(siteId: %s, clientId: %s, clientSecret: "begin-secret")`,
		langparser.QuoteString(mine), langparser.QuoteString(clientID))); err != nil {
		t.Fatalf("save: %v", err)
	}
	out := builtinReply(t, eng, devCtx, beginCall(mine, "/deployables"))
	if out["reason"] != connectReasonOK {
		t.Fatalf("begin: %v", out)
	}
	u, err := url.Parse(out["authorizeUrl"].(string))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "https" || u.Host != shop || u.Path != "/admin/oauth/authorize" || q.Get("client_id") != clientID ||
		q.Get("redirect_uri") != "https://identity.connect.example.test/auth/shopify/callback" || q.Get("state") == "" {
		t.Fatalf("authorizeUrl = %s", u)
	}
	if n := states(); n != 1 {
		t.Fatalf("%d state rows after one begin, want 1", n)
	}

	// Read back by the lookup the callback consumes through.
	// The identity node's store as production wires it: the consume's advisory
	// lock on the direct connection, which main's gate refuses without.
	store := &componentIdentity.Store{Engine: eng, DirectDB: func() *sql.DB { return raw }}
	digest := componentIdentity.HashConnectState(q.Get("state"))
	row, err := store.LookupGithubConnectState(context.Background(), digest)
	if err != nil || row == nil {
		t.Fatalf("lookup: row=%v err=%v", row, err)
	}
	if row.Purpose != githubconnect.PurposeShopifyConnect || !strings.HasSuffix(row.UserId, dev) ||
		row.ShopDomain != shop || row.SiteID != mine || row.ClientID != clientID ||
		row.CredentialSource != credentialSourcePending || row.ReturnPath != "/deployables" {
		t.Fatalf("the state row: %+v", row)
	}
	if life := row.ExpiresAt.Sub(row.CreatedAt); life < 9*time.Minute || life > 11*time.Minute {
		t.Fatalf("the state lives %v, want ten minutes", life)
	}

	// Its own flow alone spends it, under the identity callback's own actor: the
	// GitHub consume answers the unknown-state sentence and leaves it unspent,
	// and the Shopify consume spends it.
	if err := asIdentityCallback(func(ctx context.Context) error {
		_, err := store.ConsumeGithubConnectState(ctx, digest, "")
		return err
	}); !errors.Is(err, componentIdentity.ErrGithubConnectStateNotFound) {
		t.Fatalf("the GitHub callback's consume of a Shopify state: %v", err)
	}
	if err := asIdentityCallback(func(ctx context.Context) error {
		_, err := store.ConsumeGithubConnectStateFor(ctx, digest, "", githubconnect.PurposeShopifyConnect)
		return err
	}); err != nil {
		t.Fatalf("the Shopify consume refused its own state: %v", err)
	}
}

// asIdentityCallback runs fn under the context an identity callback route
// runs under (the system actor its mount wraps every route in).
func asIdentityCallback(fn func(context.Context) error) error {
	var err error
	componentIdentity.SystemActorMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		err = fn(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	return err
}

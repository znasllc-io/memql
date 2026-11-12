package datasync

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/inbound"
	languageparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// shopify_privacy_delivery_db_test.go -- a privacy request that says the same
// thing a second time (ruling 96).
//
// # Why this goes through the real receiver and the real dispatcher
//
// The fault was in neither the connector nor the runtime alone. The RECEIVER
// names a staged delivery by the digest of its signed body, and staging keeps
// a stored row's status; the DISPATCHER skips a row that is not waiting to be
// worked. A shop's erasure request says the shop's id and its domain and
// nothing else, so a second one, after a reinstall and a second uninstall, has
// the first one's body, landed on the first one's row, already processed, and
// never reached the connector: the second erasure was never made. What makes
// the same words a new delivery is the CONNECTOR deleting the deliveries of a
// request it has finished with. So the three are tested together: an HTTP
// request signed with the app's secret, the receiver's handler, the staged
// row, the dispatcher, the connector's own job and its runner, on a real
// engine and a real Postgres.

const (
	managedClientIDName     = "SHOPIFY_CONNECT_CLIENT_ID"
	managedClientSecretName = "SHOPIFY_CONNECT_CLIENT_SECRET"
	managedClientSecret     = "managed-secret-of-the-test"
)

// privacyDeliveries is one store connected through the shared app, the
// receiver, the dispatcher and the connector.
type privacyDeliveries struct {
	t        *testing.T
	eng      *memql.MemQLEngine
	db       *sql.DB
	internal context.Context
	store    string
	domain   string
	receiver *inbound.Handler
	d        *Dispatcher
	conn     *shopify.Connector
	sent     int
}

// receiverEngine is the engine as the receiver's handler takes it.
type receiverEngine struct{ eng *memql.MemQLEngine }

func (e receiverEngine) Execute(ctx context.Context, query string) (any, error) {
	return e.eng.Execute(ctx, query)
}

func newPrivacyDeliveries(t *testing.T) *privacyDeliveries {
	t.Helper()
	eng, db := healthEngine(t)
	if eng == nil {
		return nil
	}
	run := fmt.Sprintf("dbtest-pd-%d", time.Now().UnixNano())
	f := &privacyDeliveries{t: t, eng: eng, db: db, store: run, domain: run + ".myshopify.com"}
	claims := map[string]any{"sub": "dbtest-owner", "role": "owner"}
	owner := auth.ContextWithToken(auth.ContextWithClaims(context.Background(), claims), auth.BuildTokenInfo(claims))
	f.internal = auth.ContextWithInternalOrigin(auth.ContextWithAccess(owner, &auth.AccessContext{UserId: "dbtest-owner", Role: auth.RoleOwner}))

	clean := func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:shopify:%' AND (payload->>'storeId' = $1 OR id = 'v1:shopify:store:' || $1)`,
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`,
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'partitionKey' = $1`,
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'body' LIKE '%' || $1 || '%'`,
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'headersJson' LIKE '%' || $1 || '%'`,
		} {
			_, _ = db.ExecContext(ctx, q, f.store)
		}
		_, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept IN ('v1:platform:globalVariable', 'v1:platform:globalSecret') AND payload->>'name' IN ($1, $2)`, managedClientIDName, managedClientSecretName)
	}
	clean()
	t.Cleanup(clean)

	// The shared app, as the connector reads it: its client id, and a sealed
	// secret under its name. What the seal holds is the registry's to say.
	f.raw("v1:platform:globalVariable:var-global-shopify-connect-client-id", "v1:platform:globalVariable",
		map[string]any{"name": managedClientIDName, "value": "managed-client"})
	f.raw("v1:platform:globalSecret:"+run+"-managed-secret", "v1:platform:globalSecret",
		map[string]any{"name": managedClientSecretName, "encryptedValue": "sealed-by-the-test"})
	f.exec(fmt.Sprintf(`mutation createStore(storeId: %s, domain: %s, name: "Example Bicycles", appClientId: "managed-client", apiVersion: "2026-07", ownerUserId: "dbtest-owner")`,
		languageparser.QuoteString(f.store), languageparser.QuoteString(f.domain)))

	logger := slog.New(slog.DiscardHandler)
	registry := shopify.NewStoreRegistry(eng, func(_ context.Context, ref string) (string, error) {
		if ref == managedClientSecretName {
			return managedClientSecret, nil
		}
		return "token-" + ref, nil
	})
	f.conn = shopify.NewConnector(redactionEngine{IntegrationEngineAccess: eng, store: f.store}, logger, registry, shopify.NewAdminClient()).
		WithDatabase(func() *sql.DB { return db }).
		WithMaintenanceSession(func(ctx context.Context) (*sql.Conn, func(), error) {
			return database.OpenMaintenanceSessionDSN(ctx, dbtest.DSN())
		})
	// The receiver finds a connector's source through the binding, as a node's
	// does.
	if err := memqlsync.Bind(f.conn); err != nil {
		t.Fatalf("bind the connector: %v", err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(f.conn.Name()) })
	f.receiver = inbound.NewHandler(inbound.Config{Enabled: true, MaxBodyBytes: 1 << 20, Tolerance: 5 * time.Minute}, receiverEngine{eng}, logger)

	syncStore := NewStore(engineSeam{eng})
	f.d = NewDispatcher(syncStore, NewApplier(syncStore, NewEngineMirrorWriter(engineSeam{eng})))
	f.d.lookup = func(_ context.Context, name string) (memqlsync.Connector, bool) {
		return f.conn, name == shopify.ConnectorName || strings.HasPrefix(name, shopify.ConnectorName+"-")
	}
	return f
}

func (f *privacyDeliveries) exec(statement string) {
	f.t.Helper()
	if _, err := f.eng.Execute(f.internal, statement); err != nil {
		f.t.Fatalf("%s: %v", statement, err)
	}
}

// raw writes one row as the table holds it.
func (f *privacyDeliveries) raw(id, concept string, payload map[string]any) {
	f.t.Helper()
	body, _ := json.Marshal(payload)
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, concept, type, "createdBy", "createdAt", payload, schema, metadata, provenance)
		 VALUES ($1, $2, 'object', 'dbtest-owner', now(), $3::jsonb, '{}'::jsonb, '{}'::jsonb, '{"kind":"system","name":"test"}'::jsonb)`,
		id, concept, string(body)); err != nil {
		f.t.Fatalf("write %s: %v", id, err)
	}
}

func (f *privacyDeliveries) rows(query string, args ...any) []string {
	f.t.Helper()
	rows, err := f.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, v.String)
	}
	return out
}

// delivers sends one webhook to the app-level endpoint as Shopify sends it,
// signed with the app's secret, and has the dispatcher work whatever the
// receiver staged. It answers the staged row's id and whether the connector
// was handed the delivery: a row that was already processed is skipped.
func (f *privacyDeliveries) delivers(topic, body string) (id string, worked bool) {
	f.t.Helper()
	f.sent++
	mac := hmac.New(sha256.New, []byte(managedClientSecret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, inbound.RoutePrefix+shopify.ConnectorName, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Hmac-Sha256", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Shopify-Topic", topic)
	req.Header.Set("X-Shopify-Shop-Domain", f.domain)
	req.Header.Set("X-Shopify-Webhook-Id", fmt.Sprintf("%s-webhook-%d", f.store, f.sent))
	answer := httptest.NewRecorder()
	f.receiver.ServeHTTP(answer, req)
	if answer.Code != http.StatusAccepted {
		f.t.Fatalf("the receiver answered %d to a signed %s: %s", answer.Code, topic, answer.Body.String())
	}
	ids := f.rows(`SELECT DISTINCT id FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'source' = $1 AND payload->>'body' = $2`, shopify.ConnectorName, body)
	if len(ids) != 1 {
		f.t.Fatalf("the receiver staged %d row(s) for this body, want one: %v", len(ids), ids)
	}
	id = strings.TrimPrefix(ids[0], "v1:platform:inboundRequest:")
	staged, found, err := f.d.Staged(context.Background(), id)
	if err != nil || !found {
		f.t.Fatalf("the staged row %s: found %v, %v", id, found, err)
	}
	was := f.status(id)
	if _, err := f.d.DispatchStaged(context.Background(), staged); err != nil {
		f.t.Fatalf("the dispatcher on %s: %v", id, err)
	}
	return id, was == "received" && f.status(id) == "processed"
}

// status is a staged delivery's status now, or "" when the row is gone.
func (f *privacyDeliveries) status(id string) string {
	got := f.rows(`SELECT payload->>'status' FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND id = $1 ORDER BY "createdAt" DESC LIMIT 1`, "v1:platform:inboundRequest:"+id)
	if len(got) == 0 {
		return ""
	}
	return got[0]
}

// job is the privacy job of a topic for the store as the table holds it now.
func (f *privacyDeliveries) job(topic string) map[string]any {
	f.t.Helper()
	got := f.rows(`SELECT payload::text FROM "MemoryNodes" WHERE concept = 'v1:shopify:complianceJob' AND payload->>'storeId' = $1 AND payload->>'topic' = $2 ORDER BY "createdAt" DESC LIMIT 1`, f.store, topic)
	if len(got) == 0 {
		f.t.Fatalf("the store has no %s job", topic)
	}
	row := map[string]any{}
	if err := json.Unmarshal([]byte(got[0]), &row); err != nil {
		f.t.Fatal(err)
	}
	return row
}

// due makes the store's job of a topic due now: its hold, or the hour between
// its two passes, has elapsed.
func (f *privacyDeliveries) due(topic string) {
	f.t.Helper()
	ids := f.rows(`SELECT DISTINCT id FROM "MemoryNodes" WHERE concept = 'v1:shopify:complianceJob' AND payload->>'storeId' = $1 AND payload->>'topic' = $2`, f.store, topic)
	if len(ids) != 1 {
		f.t.Fatalf("the store has %d %s job(s)", len(ids), topic)
	}
	f.exec(fmt.Sprintf(`mutation recordComplianceJob(jobId: %s, status: "pending", dueAt: %s)`,
		languageparser.QuoteString(strings.TrimPrefix(ids[0], "v1:shopify:complianceJob:")), languageparser.QuoteString(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))))
	// A request is older than a pass that starts a full second after it.
	time.Sleep(1100 * time.Millisecond)
}

// pass runs the privacy jobs that are due and requires the store's one to
// have run.
func (f *privacyDeliveries) pass(what string) {
	f.t.Helper()
	ran, err := f.conn.RunDueComplianceJobs(context.Background())
	if err != nil || ran != 1 {
		f.t.Fatalf("%s: ran %d, %v", what, ran, err)
	}
}

// storeNow is one value of the store's row as the table holds it now: a field
// of its payload, or with health set a key of its health.
func (f *privacyDeliveries) storeNow(field string, health bool) string {
	f.t.Helper()
	path := "payload->>$2"
	if health {
		path = "payload->'health'->>$2"
	}
	got := f.rows(`SELECT `+path+` FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND id = 'v1:shopify:store:' || $1 ORDER BY "createdAt" DESC LIMIT 1`, f.store, field)
	if len(got) == 0 {
		f.t.Fatalf("the store has no row")
	}
	return got[0]
}

func (f *privacyDeliveries) mirror() []string {
	return f.rows(`SELECT DISTINCT payload->>'gid' FROM "MemoryNodes" WHERE concept = 'v1:shopify:product' AND payload->>'storeId' = $1 ORDER BY 1`, f.store)
}

func (f *privacyDeliveries) product(n string) {
	f.raw("v1:shopify:product:"+f.store+"-"+n, "v1:shopify:product",
		map[string]any{"storeId": f.store, "gid": "gid://shopify/Product/" + n, "title": "Road bicycle", "deleted": false})
}

// Two shop erasure requests with the same body for one store on the shared
// app, with a reinstall between them: both run, and the second purges what
// was recorded after the reinstall.
func TestASecondShopErasureWithTheSameBodyIsMade(t *testing.T) {
	f := newPrivacyDeliveries(t)
	if f == nil {
		return
	}
	body := `{"shop_id":954889,"shop_domain":"` + f.domain + `"}`
	uninstall := func() {
		f.exec(fmt.Sprintf(`mutation markStoreUninstalled(storeId: %s, uninstalledAt: %s)`, languageparser.QuoteString(f.store), languageparser.QuoteString(time.Now().UTC().Format(time.RFC3339))))
	}
	f.product("7001")
	uninstall()

	first, worked := f.delivers(shopify.TopicShopRedact, body)
	if !worked {
		t.Fatalf("the first shop/redact was not handed to the connector (row %s is %q)", first, f.status(first))
	}
	// A redelivery while the job waits lands on the same row, and is not new.
	again, worked := f.delivers(shopify.TopicShopRedact, body)
	if again != first || worked {
		t.Errorf("a redelivery of a request whose job is waiting is row %s, handed to the connector: %v; want the first one's row %s, skipped", again, worked, first)
	}
	if job := f.job(shopify.TopicShopRedact); job["status"] != "pending" || fmt.Sprint(job["deliveryIds"]) != "["+first+"]" && fmt.Sprint(job["deliveryIds"]) != "[v1:platform:inboundRequest:"+first+"]" {
		t.Fatalf("after the redelivery the job is %v and remembers %v, want it waiting, remembering the one delivery", job["status"], job["deliveryIds"])
	}
	f.due(shopify.TopicShopRedact)
	f.pass("the first purge")
	if left := f.mirror(); len(left) != 0 {
		t.Fatalf("the first purge left %v", left)
	}
	if f.status(first) == "" {
		t.Error("the delivery was deleted before the job ended: a redelivery would be a new delivery of a request still being worked")
	}
	f.due(shopify.TopicShopRedact)
	f.pass("the second purge")
	if job := f.job(shopify.TopicShopRedact); job["status"] != "done" {
		t.Fatalf("after both purges the job is %v: %v", job["status"], job["lastError"])
	}
	if got := f.status(first); got != "" {
		t.Errorf("the delivery of a request the connector has finished with is still a row, %s", got)
	}

	// Installed again, recorded again, uninstalled again; and Shopify asks
	// again, in the same words.
	f.exec(fmt.Sprintf(`mutation markStoreReconnected(storeId: %s, status: "configured")`, languageparser.QuoteString(f.store)))
	f.product("7002")
	uninstall()
	second, worked := f.delivers(shopify.TopicShopRedact, body)
	if !worked {
		t.Fatalf("the second shop/redact, with the first one's body, was not handed to the connector: row %s is %q", second, f.status(second))
	}
	if job := f.job(shopify.TopicShopRedact); job["status"] != "pending" {
		t.Fatalf("the second request left the job %v, want it waiting again", job["status"])
	}
	f.due(shopify.TopicShopRedact)
	f.pass("the first purge of the second erasure")
	if left := f.mirror(); len(left) != 0 {
		t.Errorf("the second erasure left what was recorded after the reinstall: %v", left)
	}
	f.due(shopify.TopicShopRedact)
	f.pass("the second purge of the second erasure")
	if job := f.job(shopify.TopicShopRedact); job["status"] != "done" {
		t.Errorf("the second erasure ends %v: %v", job["status"], job["lastError"])
	}
}

// A second identical customer erasure after the first is done runs both
// passes again: the same customer, the same orders, the same email and phone.
func TestASecondCustomerErasureWithTheSameBodyIsMade(t *testing.T) {
	f := newPrivacyDeliveries(t)
	if f == nil {
		return
	}
	body := `{"shop_id":954889,"shop_domain":"` + f.domain + `","customer":{"id":191167,"email":"ada@example.com","phone":"+15550100"},"orders_to_redact":[299938]}`
	first, worked := f.delivers(shopify.TopicRedact, body)
	if !worked {
		t.Fatalf("the first customers/redact was not handed to the connector (row %s is %q)", first, f.status(first))
	}
	f.due(shopify.TopicRedact)
	f.pass("the first pass")
	f.due(shopify.TopicRedact)
	f.pass("the second pass")
	if job := f.job(shopify.TopicRedact); job["status"] != "done" {
		t.Fatalf("after both passes the job is %v: %v", job["status"], job["lastError"])
	}
	if got := f.status(first); got != "" {
		t.Fatalf("the delivery of an erasure that is done is still a row, %s", got)
	}

	second, worked := f.delivers(shopify.TopicRedact, body)
	if !worked {
		t.Fatalf("the second customers/redact, with the first one's body, was not handed to the connector: row %s is %q", second, f.status(second))
	}
	job := f.job(shopify.TopicRedact)
	if scrubbed, present := job["scrubbedAt"]; job["status"] != "pending" || (present && scrubbed != nil && scrubbed != "") {
		t.Fatalf("the second request left the job %v with scrubbedAt %v, want it waiting for both passes again", job["status"], job["scrubbedAt"])
	}
	f.due(shopify.TopicRedact)
	f.pass("the first pass of the second erasure")
	if job := f.job(shopify.TopicRedact); job["status"] != "pending" || job["scrubbedAt"] == nil || job["scrubbedAt"] == "" {
		t.Fatalf("after its first pass the second erasure is %v with scrubbedAt %v", job["status"], job["scrubbedAt"])
	}
	f.due(shopify.TopicRedact)
	f.pass("the second pass of the second erasure")
	if job := f.job(shopify.TopicRedact); job["status"] != "done" {
		t.Errorf("the second erasure ends %v: %v", job["status"], job["lastError"])
	}
}

// A second identical uninstall. A shop's details do not change between two
// uninstalls, so the second app/uninstalled has the first one's body and is
// given the first one's row. The store remembers the delivery that carried
// its uninstall, and the delivery goes with the store's purge; so after a
// reinstall the same notice is a new delivery, and disconnects the store
// again.
//
// The other way the remembered delivery goes, at the reconnect of a store
// that was not purged, is the Connect write's, which asks the origin and
// cannot be driven from here; the connector's own package tests it
// (TestTheUninstallsDeliveryIsRememberedAndDeletedAtReconnect). Here the
// reinstall is what that write records on the store's row.
func TestASecondUninstallWithTheSameBodyDisconnectsTheStoreAgain(t *testing.T) {
	f := newPrivacyDeliveries(t)
	if f == nil {
		return
	}
	notice := `{"id":954889,"name":"Example Bicycles","myshopify_domain":"` + f.domain + `"}`
	erase := `{"shop_id":954889,"shop_domain":"` + f.domain + `"}`
	f.product("7101")

	first, worked := f.delivers(shopify.TopicAppUninstalled, notice)
	if !worked {
		t.Fatalf("the first app/uninstalled was not handed to the connector (row %s is %q)", first, f.status(first))
	}
	if f.storeNow("uninstalledAt", false) == "" {
		t.Fatal("the first uninstall did not disconnect the store")
	}
	if got := f.storeNow("uninstallDeliveryIds", true); got != `["`+first+`"]` {
		t.Fatalf("the store remembers the deliveries %s for its uninstall, want the one row the receiver staged, %s", got, first)
	}
	// Delivered again while the store is uninstalled, it lands on the same
	// row and is not worked again.
	if again, worked := f.delivers(shopify.TopicAppUninstalled, notice); again != first || worked {
		t.Errorf("a redelivery of the uninstall is row %s, handed to the connector: %v; want the first one's row %s, skipped", again, worked, first)
	}

	// Shopify asks for the shop's erasure, and both purges run.
	if _, worked := f.delivers(shopify.TopicShopRedact, erase); !worked {
		t.Fatal("the shop/redact was not handed to the connector")
	}
	f.due(shopify.TopicShopRedact)
	f.pass("the first purge")
	f.due(shopify.TopicShopRedact)
	f.pass("the second purge")
	if job := f.job(shopify.TopicShopRedact); job["status"] != "done" {
		t.Fatalf("after both purges the job is %v: %v", job["status"], job["lastError"])
	}
	if got := f.status(first); got != "" {
		t.Fatalf("the delivery that carried the uninstall is still a row, %s, after the store was purged: the same notice a second time would land on it", got)
	}

	// Installed again, and uninstalled again: the same shop, the same notice.
	f.exec(fmt.Sprintf(`mutation markStoreReconnected(storeId: %s, status: "configured")`, languageparser.QuoteString(f.store)))
	if at := f.storeNow("uninstalledAt", false); at != "" {
		t.Fatalf("the reinstalled store still reads uninstalledAt %q", at)
	}
	second, worked := f.delivers(shopify.TopicAppUninstalled, notice)
	if second != first {
		t.Fatalf("the second identical uninstall was staged as %s, want the name its body gives it, %s", second, first)
	}
	if !worked {
		t.Fatalf("the second identical uninstall was not handed to the connector: row %s is %q", second, f.status(second))
	}
	if f.storeNow("uninstalledAt", false) == "" {
		t.Error("the second identical uninstall did not disconnect the store again")
	}
}

// An older privacy delivery, as the receiver itself staged it. Nothing
// remembers it: its job is from before a job kept the deliveries that asked
// for it, and an erasure has blanked its body since. It is the shop's by the
// headers the receiver put on its row, and it is deleted at the store's
// first erasure, which then says on the store's row that this ran.
func TestAnOlderPrivacyDeliveryTheReceiverStagedIsDeletedAtTheStoresFirstErasure(t *testing.T) {
	f := newPrivacyDeliveries(t)
	if f == nil {
		return
	}
	older, worked := f.delivers(shopify.TopicRedact,
		`{"shop_id":954889,"shop_domain":"`+f.domain+`","customer":{"id":191168,"email":"bob@example.com","phone":"+15550101"},"orders_to_redact":[]}`)
	if !worked {
		t.Fatalf("the older customers/redact was not handed to the connector (row %s is %q)", older, f.status(older))
	}
	// As an earlier release left it: no job remembers the delivery, and its
	// body is blank.
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:complianceJob' AND payload->>'storeId' = $1`, f.store); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{body}', '""'::jsonb, true) WHERE concept = 'v1:platform:inboundRequest' AND id = $1`,
		"v1:platform:inboundRequest:"+older); err != nil {
		t.Fatal(err)
	}
	if got := f.status(older); got != "processed" {
		t.Fatalf("the older delivery is %q, want it processed and nobody's", got)
	}

	// The store's first erasure under this release.
	own, worked := f.delivers(shopify.TopicRedact,
		`{"shop_id":954889,"shop_domain":"`+f.domain+`","customer":{"id":191167,"email":"ada@example.com","phone":"+15550100"},"orders_to_redact":[299938]}`)
	if !worked {
		t.Fatalf("the customers/redact was not handed to the connector (row %s is %q)", own, f.status(own))
	}
	f.due(shopify.TopicRedact)
	f.pass("the first pass")
	if got := f.status(older); got != "" {
		t.Errorf("the older privacy delivery the receiver staged is still a row, %s: it was not found by what its row records", got)
	}
	if f.status(own) == "" {
		t.Error("the job's own delivery went with the older ones, before the job ended")
	}
	if f.storeNow("oldErasureRepairedAt", true) == "" {
		t.Error("the store's row does not say its once-only erasure work ran")
	}
	if job := f.job(shopify.TopicRedact); !strings.Contains(fmt.Sprint(job["outcome"]), "older privacy and uninstall deliveries") {
		t.Errorf("the outcome does not say the older delivery went: %v", job["outcome"])
	}
}

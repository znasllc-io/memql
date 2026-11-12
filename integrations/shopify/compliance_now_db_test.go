package shopify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/library"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// compliance_now_db_test.go -- a privacy job decides on what is true NOW
// (fix round 1 of the erasure: rulings 90 and 92, and the review's findings on
// the runner).
//
// # Why these need the real engine and a real Postgres
//
// Each is about two writers of one row, or about what a table holds when a
// statement runs. A request that lands on a job while the runner works it; a
// store that reconnects between two jobs of one run; a delivery's body in
// every version of its row. A fake engine has one writer and no versions.

// hookEngine runs a function once, just BEFORE the first call that matches is
// executed: what another writer does in the moment between the runner's
// decision and its write.
type hookEngine struct {
	memql.IntegrationEngineAccess
	match func(query string) bool
	fire  func()
	fired bool
}

func (e *hookEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if !e.fired && e.match(query) {
		e.fired = true
		e.fire()
	}
	return e.IntegrationEngineAccess.Execute(ctx, query)
}

// queuedBackfills records the backfills a connector asks the sync runtime to
// queue, and answers each as queued. The runtime is component/datasync's, and
// is not in this package's engine; what is asked of it is this package's.
type queuedBackfills struct {
	memql.IntegrationEngineAccess
	calls []string
}

func (e *queuedBackfills) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(query, "builtin datasyncQueueBackfill(") {
		e.calls = append(e.calls, query)
		return resultWithRows([]map[string]any{}), nil
	}
	return e.IntegrationEngineAccess.Execute(ctx, query)
}

// withTheSyncRuntime puts the sync runtime's stand-in under the fixture's own
// connector and answers it: what the connector asks the runtime to queue is
// recorded there, and answered as queued. A test that expects backfills to be
// queued asks for it. Without it the builtin has nothing to answer it in this
// package's engine and a run that queues fails, which is how every other test
// says that it expects none.
func (r *runnerFixture) withTheSyncRuntime() *queuedBackfills {
	r.t.Helper()
	queued := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
	r.conn = r.connectorOn(queued, nil)
	return queued
}

// connectorOn is the fixture's connector over another engine: the same store
// registry, the same origin and the same clock, and an Admin client that does
// not wait between its tries.
func (r *runnerFixture) connectorOn(engine memql.IntegrationEngineAccess, tune func(*AdminClient)) *Connector {
	r.t.Helper()
	registry := NewStoreRegistry(r.eng, func(context.Context, string) (string, error) { return "shpat_test", nil })
	registry.ttl = 0
	client := NewAdminClient()
	client.endpoint = func(Store) string { return r.admin.server.URL + "/graphql" }
	client.sleep = func(context.Context, time.Duration) error { return nil }
	if tune != nil {
		tune(client)
	}
	conn := NewConnector(engine, nil, registry, client).
		WithDatabase(func() *sql.DB { return r.db }).
		WithMaintenanceSession(privacySessionOf(r.db))
	conn.now = func() time.Time { return r.at }
	return conn
}

// narrowed is the real engine with the due read narrowed to the fixture's
// store, as the fixture's own connector reads it.
func (r *runnerFixture) narrowed() memql.IntegrationEngineAccess {
	return narrowedEngine{IntegrationEngineAccess: r.eng, store: r.store}
}

// delivers hands the connector one privacy delivery for the fixture's store,
// as the dispatcher does: a customer by the number Shopify sends, and the
// orders the request names by theirs.
func (r *runnerFixture) delivers(conn *Connector, topic, customer string, orders ...string) {
	r.t.Helper()
	r.deliversAs(conn, "", "", topic, customer, orders...)
}

// deliversAs is delivers for a delivery that is staged: source is where it
// was staged and id its row's id there, which the connector is told as the
// dispatcher tells it. The row is written first, with the body the connector
// is handed, and stamped processed as the dispatcher stamps it afterwards.
func (r *runnerFixture) deliversAs(conn *Connector, source, id, topic, customer string, orders ...string) {
	r.t.Helper()
	domain := r.store + ".example.com"
	body := map[string]any{"shop_domain": domain}
	if topic != TopicShopRedact {
		body["customer"] = map[string]any{"id": json.Number(customer), "email": adaEmail, "phone": adaPhone}
		named := []any{}
		for _, order := range orders {
			named = append(named, json.Number(order))
		}
		if topic == TopicDataRequest {
			body["orders_requested"], body["data_request"] = named, map[string]any{"id": json.Number("4242" + customer)}
		} else {
			body["orders_to_redact"] = named
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		r.t.Fatal(err)
	}
	if id != "" {
		r.mutate("stageInboundRequest", map[string]any{
			"requestId": id, "source": source, "medium": "webhook", "body": string(raw), "contentType": "application/json",
			"headersJson": `{"x-shopify-topic":"` + topic + `"}`, "signatureVerified": true, "verifiedBy": "connector", "receivedAt": r.at.Format(time.RFC3339),
		})
		r.mutate("updateInboundRequestStatus", map[string]any{"requestId": id, "status": "processing", "attempts": 1})
	}
	if err := conn.enqueueComplianceJob(context.Background(), Store{ID: r.store, Domain: domain}, topic, memqlsync.InboundRequest{RequestId: id, Source: source, Body: raw, ReceivedAt: r.at}); err != nil {
		r.t.Fatalf("the %s delivery was not recorded: %v", topic, err)
	}
	if id != "" {
		r.mutate("updateInboundRequestStatus", map[string]any{"requestId": id, "status": "processed", "attempts": 1, "processedAt": r.at.Format(time.RFC3339)})
	}
}

// deliveryBodies is the body of every stored version of one staged delivery,
// oldest first.
func (r *runnerFixture) deliveryBodies(id string) []string {
	r.t.Helper()
	rows, err := queryRows(context.Background(), r.db,
		`SELECT COALESCE(payload->>'body', '<no body key>') FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND id = $1 ORDER BY "createdAt"`,
		"v1:platform:inboundRequest:"+id)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, row := range rows {
		out = append(out, row[0])
	}
	return out
}

// onTheAppSource names deliveries of this test on the app-level source, where
// a managed store's privacy deliveries arrive, and removes them afterwards.
func (r *runnerFixture) onTheAppSource(names ...string) []string {
	r.t.Helper()
	tag := strings.TrimPrefix(r.store, "dbtest-era-")
	var ids []string
	for _, name := range names {
		ids = append(ids, tag+"-"+name)
	}
	clean := func() {
		for _, id := range ids {
			_, _ = r.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND id = $1`, "v1:platform:inboundRequest:"+id)
		}
	}
	clean()
	r.t.Cleanup(clean)
	return ids
}

func ordersOf(row map[string]any) string {
	var out []string
	list, _ := row["orderGids"].([]any)
	for _, v := range list {
		out = append(out, strings.TrimPrefix(fmt.Sprint(v), "gid://shopify/Order/"))
	}
	return strings.Join(out, ",")
}

// noFirstPass reports whether a job's row records no first pass.
func noFirstPass(row map[string]any) bool {
	at, present := row["scrubbedAt"]
	return !present || at == nil || at == ""
}

// ---- Item 3: a second request runs (ruling 92) ----

// A customer who asks to be erased a second time is erased a second time. The
// job's id is derived from the store, the topic and the customer, so the
// second request lands on the first one's row, which is done; it used to stay
// done, and the second erasure never ran.
func TestASecondErasureOfTheSameCustomerRunsBothPassesAgain(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("3101")
	r.write("customer", adaCustomer("3101", "2026-10-01T10:00:00Z"), seen)

	first := r.at
	r.delivers(r.conn, TopicRedact, "3101")
	if job := r.job(TopicRedact, customer); job["status"] != "pending" || !instant(t, job, "dueAt").Equal(first.Add(24*time.Hour)) || !instant(t, job, "requestedAt").Equal(first) {
		t.Fatalf("the first request left %v, want it waiting for its hold, requested when it was recorded", job)
	}
	r.at = first.Add(25 * time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the first pass: ran %d", ran)
	}
	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the second pass: ran %d", ran)
	}
	if job := r.job(TopicRedact, customer); job["status"] != "done" {
		t.Fatalf("after both passes the job is %v", job["status"])
	}

	// She buys again: the origin's rows come back with her on them.
	r.write("customer", adaCustomer("3101", "2026-12-01T10:00:00Z"), seen.Add(60*24*time.Hour))
	r.write("order", bikeOrder("53101", customer, "2026-12-01T10:00:00Z"), seen.Add(60*24*time.Hour))
	if !strings.Contains(fmt.Sprint(r.newest("customer", customer)["defaultEmailAddress"]), adaEmail) {
		t.Fatal("the fixture did not write her back")
	}

	// And asks again.
	second := r.at.Add(40 * 24 * time.Hour)
	r.at = second
	r.delivers(r.conn, TopicRedact, "3101", "53101")
	job := r.job(TopicRedact, customer)
	if job["status"] != "pending" || !noFirstPass(job) || !instant(t, job, "dueAt").Equal(second.Add(24*time.Hour)) || ordersOf(job) != "53101" || !instant(t, job, "requestedAt").Equal(second) {
		t.Fatalf("the second request left the job %v with scrubbedAt %v, due %v, orders %q; want it waiting again for both passes, after the hold of the new request", job["status"], job["scrubbedAt"], job["dueAt"], ordersOf(job))
	}
	if ran := r.run(r.conn); ran != 0 {
		t.Fatalf("the second erasure ran inside its hold: %d", ran)
	}
	r.at = second.Add(25 * time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the first pass of the second erasure: ran %d", ran)
	}
	r.holdsNoWordOf("customer", customer, adaWords)
	r.holdsNoWordOf("order", eraOrderGID("53101"), adaWords)
	if job := r.job(TopicRedact, customer); job["status"] != "pending" || !instant(t, job, "scrubbedAt").Equal(r.at) {
		t.Fatalf("after its first pass the second erasure is %v with scrubbedAt %v", job["status"], job["scrubbedAt"])
	}
	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the second pass of the second erasure: ran %d", ran)
	}
	if job := r.job(TopicRedact, customer); job["status"] != "done" {
		t.Errorf("the second erasure ends %v, want done", job["status"])
	}
}

// A shop that is uninstalled, purged, installed again and uninstalled again
// is purged again: the second shop/redact lands on the job of the first.
func TestASecondErasureOfTheSameShopPurgesAgain(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	r.mutate("markStoreUninstalled", map[string]any{"storeId": r.store, "uninstalledAt": r.at.Format(time.RFC3339)})
	r.write("product", map[string]any{"id": "gid://shopify/Product/3201", "updatedAt": "2026-10-01T10:00:00Z", "title": "Road bicycle"}, seen)
	r.delivers(r.conn, TopicShopRedact, "")
	r.at = r.at.Add(ShopRedactHold + time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the first purge: ran %d", ran)
	}
	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the second purge: ran %d", ran)
	}
	if job := r.job(TopicShopRedact, ""); job["status"] != "done" {
		t.Fatalf("after both purges the job is %v", job["status"])
	}
	if left := r.allTexts(); len(left) != 0 {
		t.Fatalf("the first erasure left %d row(s)", len(left))
	}

	// Installed again: the reconnect reads the store's rows again.
	r.mutate("markStoreReconnected", map[string]any{"storeId": r.store, "status": StatusConfigured})
	r.mutate("updateStore", map[string]any{"storeId": r.store, "adminTokenRef": "EXAMPLE_ADMIN"})
	r.write("product", map[string]any{"id": "gid://shopify/Product/3202", "updatedAt": "2026-12-01T10:00:00Z", "title": "City bicycle"}, seen.Add(60*24*time.Hour))
	// Uninstalled again, and Shopify asks again.
	second := r.at.Add(30 * 24 * time.Hour)
	r.at = second
	r.mutate("markStoreUninstalled", map[string]any{"storeId": r.store, "uninstalledAt": second.Format(time.RFC3339)})
	r.delivers(r.conn, TopicShopRedact, "")
	job := r.job(TopicShopRedact, "")
	if job["status"] != "pending" || !noFirstPass(job) || !instant(t, job, "dueAt").Equal(second.Add(ShopRedactHold)) {
		t.Fatalf("the second shop/redact left the job %v with scrubbedAt %v, due %v; want it waiting again for both purges, after the hold", job["status"], job["scrubbedAt"], job["dueAt"])
	}
	r.at = second.Add(ShopRedactHold + time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the first purge of the second erasure: ran %d", ran)
	}
	if left := r.allTexts(); len(left) != 0 {
		t.Errorf("the second erasure left %d row(s) of the store's mirror", len(left))
	}
	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the second purge of the second erasure: ran %d", ran)
	}
	if job := r.job(TopicShopRedact, ""); job["status"] != "done" {
		t.Errorf("the second erasure of the shop ends %v, want done", job["status"])
	}
}

// A request for an erasure that is waiting, or failed, adds its orders to the
// job's and moves nothing: not the hold of a job waiting for it, not the hour
// of a job waiting for its second pass. A redelivery used to move the second
// pass a day on.
func TestARequestForAWaitingErasureAddsItsOrdersAndDoesNotMoveItsHold(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("3301")
	r.write("customer", adaCustomer("3301", "2026-10-01T10:00:00Z"), seen)
	first := r.at
	r.delivers(r.conn, TopicRedact, "3301", "53301")

	// Waiting for its hold.
	r.at = first.Add(2 * time.Hour)
	r.delivers(r.conn, TopicRedact, "3301", "53302", "53301")
	job := r.job(TopicRedact, customer)
	if ordersOf(job) != "53301,53302" || !instant(t, job, "dueAt").Equal(first.Add(24*time.Hour)) || job["status"] != "pending" || !instant(t, job, "requestedAt").Equal(r.at) {
		t.Fatalf("a request for a job inside its hold left orders %q, due %v, status %v, requested %v; want the orders of both, the hold where it was", ordersOf(job), job["dueAt"], job["status"], job["requestedAt"])
	}

	// Waiting for its second pass.
	r.at = first.Add(25 * time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the first pass: ran %d", ran)
	}
	firstPass := r.at
	r.at = r.at.Add(10 * time.Minute)
	r.delivers(r.conn, TopicRedact, "3301", "53303")
	job = r.job(TopicRedact, customer)
	if ordersOf(job) != "53301,53302,53303" || !instant(t, job, "dueAt").Equal(firstPass.Add(ErasureSecondPassAfter)) || !instant(t, job, "scrubbedAt").Equal(firstPass) || job["status"] != "pending" {
		t.Fatalf("a request for a job waiting for its second pass left orders %q, due %v, scrubbedAt %v, status %v; want the orders added and the second pass due when it was", ordersOf(job), job["dueAt"], job["scrubbedAt"], job["status"])
	}

	// Failed.
	r.at = firstPass.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.broken); ran != 0 {
		t.Fatalf("the broken runner ran %d", ran)
	}
	due := instant(t, r.job(TopicRedact, customer), "dueAt")
	r.delivers(r.conn, TopicRedact, "3301", "53304")
	job = r.job(TopicRedact, customer)
	if job["status"] != "failed" || ordersOf(job) != "53301,53302,53303,53304" || !instant(t, job, "dueAt").Equal(due) {
		t.Errorf("a request for a failed job left status %v, orders %q, due %v; want it failed still, with the orders added and its dueAt where it was", job["status"], ordersOf(job), job["dueAt"])
	}
}

// A request that reaches a job while its LAST pass is running is not lost.
// The pass read the job before the request named its order; were the job then
// recorded done, the order would never be erased, and no later run would look.
func TestARequestThatLandsWhileTheLastPassRunsIsNotLost(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("3401")
	r.write("customer", adaCustomer("3401", "2026-10-01T10:00:00Z"), seen)
	// An order of hers that names no customer: only a request can name it.
	guest := bikeOrder("53401", "", "2026-10-01T10:00:00Z")
	guest["customer"] = nil
	r.write("order", guest, seen)
	// The job is waiting for its second pass.
	id := r.queue(TopicRedact, customer, nil, r.at.Add(-2*time.Hour))
	r.mutate("recordComplianceJob", map[string]any{
		"jobId": id, "status": "pending", "outcome": "first pass", "scrubbedAt": r.at.Add(-time.Hour).Format(time.RFC3339), "dueAt": r.at.Add(-time.Minute).Format(time.RFC3339),
	})

	hook := &hookEngine{IntegrationEngineAccess: r.narrowed()}
	hook.match = func(q string) bool {
		return strings.HasPrefix(q, "recordComplianceJob(") && strings.Contains(q, `status: "done"`)
	}
	hook.fire = func() { r.delivers(r.conn, TopicRedact, "3401", "53401") }
	if ran := r.run(r.connectorOn(hook, nil)); ran != 1 || !hook.fired {
		t.Fatalf("the last pass: ran %d, and the request landed during it: %v", ran, hook.fired)
	}
	job := r.job(TopicRedact, customer)
	if job["status"] != "pending" || !noFirstPass(job) || ordersOf(job) != "53401" {
		t.Fatalf("the job is %v with scrubbedAt %v and orders %q after a request landed during its last pass; want it waiting for both passes again, naming the order", job["status"], job["scrubbedAt"], ordersOf(job))
	}
	if !strings.Contains(fmt.Sprint(r.newest("order", eraOrderGID("53401"))["email"]), adaEmail) {
		t.Fatal("the premise does not hold: the pass the request missed erased its order")
	}

	// The next run makes the erasure again, and this time it names the order.
	r.at = r.at.Add(time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the next run: ran %d", ran)
	}
	r.holdsNoWordOf("order", eraOrderGID("53401"), adaWords)
	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("its second pass: ran %d", ran)
	}
	if job := r.job(TopicRedact, customer); job["status"] != "done" {
		t.Errorf("the job ends %v, want done", job["status"])
	}
}

// The same, when the request is recorded by a node whose clock is behind: it
// lands while the last pass runs, and the time it writes is no later than the
// job's last request. Two clocks cannot say which came first; the row can. A
// write of the row since the pass read it, with the job where the pass found
// it, is a request, whatever time it carries.
func TestARequestRecordedByALateClockDuringTheLastPassIsNotLost(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("3501")
	r.write("customer", adaCustomer("3501", "2026-10-01T10:00:00Z"), seen)
	guest := bikeOrder("53501", "", "2026-10-01T10:00:00Z")
	guest["customer"] = nil
	r.write("order", guest, seen)
	// The job is waiting for its second pass, last requested half an hour ago.
	requested := r.at.Add(-30 * time.Minute)
	id := r.queue(TopicRedact, customer, nil, r.at.Add(-2*time.Hour))
	r.mutate("recordComplianceJob", map[string]any{
		"jobId": id, "status": "pending", "outcome": "first pass", "scrubbedAt": r.at.Add(-time.Hour).Format(time.RFC3339), "dueAt": r.at.Add(-time.Minute).Format(time.RFC3339),
	})
	r.mutate("extendComplianceJob", map[string]any{"jobId": id, "orderGids": []string{}, "requestedAt": requested.Format(time.RFC3339)})

	// The node that records the request believes it is half an hour ago.
	late := r.connectorOn(r.narrowed(), nil)
	late.now = func() time.Time { return requested }
	hook := &hookEngine{IntegrationEngineAccess: r.narrowed()}
	hook.match = func(q string) bool {
		return strings.HasPrefix(q, "recordComplianceJob(") && strings.Contains(q, `status: "done"`)
	}
	hook.fire = func() { r.delivers(late, TopicRedact, "3501", "53501") }
	if ran := r.run(r.connectorOn(hook, nil)); ran != 1 || !hook.fired {
		t.Fatalf("the last pass: ran %d, and the request landed during it: %v", ran, hook.fired)
	}
	job := r.job(TopicRedact, customer)
	if !instant(t, job, "requestedAt").Equal(requested) {
		t.Fatalf("the premise does not hold: the late node recorded the request at %v, want the time of the job's last one", job["requestedAt"])
	}
	if job["status"] != "pending" || !noFirstPass(job) || ordersOf(job) != "53501" {
		t.Fatalf("the job is %v with scrubbedAt %v and orders %q; want it waiting for both passes again, naming the order", job["status"], job["scrubbedAt"], ordersOf(job))
	}
	r.at = r.at.Add(time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the next run: ran %d", ran)
	}
	r.holdsNoWordOf("order", eraOrderGID("53501"), adaWords)
}

// The runner records a job done only if its last request is older than the
// start of the pass that finished (ruling 92). A request recorded in the very
// second the pass began is not older: the erasure is made again.
func TestAJobRequestedAsItsLastPassBeganIsNotDone(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("3601")
	r.write("customer", adaCustomer("3601", "2026-10-01T10:00:00Z"), seen)
	id := r.queue(TopicRedact, customer, nil, r.at.Add(-2*time.Hour))
	r.mutate("recordComplianceJob", map[string]any{
		"jobId": id, "status": "pending", "outcome": "first pass", "scrubbedAt": r.at.Add(-time.Hour).Format(time.RFC3339), "dueAt": r.at.Add(-time.Minute).Format(time.RFC3339),
	})
	r.delivers(r.conn, TopicRedact, "3601")
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("ran %d", ran)
	}
	if job := r.job(TopicRedact, customer); job["status"] != "pending" || !noFirstPass(job) {
		t.Fatalf("a job requested in the second its last pass began is %v with scrubbedAt %v, want it waiting for both passes again", job["status"], job["scrubbedAt"])
	}
	// A request older than the pass does not hold the job open.
	r.at = r.at.Add(time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the first pass again: ran %d", ran)
	}
	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the second pass again: ran %d", ran)
	}
	if job := r.job(TopicRedact, customer); job["status"] != "done" {
		t.Errorf("the job ends %v, want done", job["status"])
	}
}

// Two runs that work the same job do not write over each other. A write of
// the job's row is refused when the row was written since the pass read it;
// if the job has MOVED meanwhile -- another run ended it -- the row is left
// as that run left it.
func TestARunDoesNotWriteOverAJobAnotherRunMoved(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("3701")
	r.write("customer", adaCustomer("3701", "2026-10-01T10:00:00Z"), seen)
	id := r.queue(TopicRedact, customer, nil, r.at.Add(-2*time.Hour))
	r.mutate("recordComplianceJob", map[string]any{
		"jobId": id, "status": "pending", "outcome": "first pass", "scrubbedAt": r.at.Add(-time.Hour).Format(time.RFC3339), "dueAt": r.at.Add(-time.Minute).Format(time.RFC3339),
	})
	const theirs = "second pass: made by another run"
	hook := &hookEngine{IntegrationEngineAccess: r.narrowed()}
	hook.match = func(q string) bool { return strings.HasPrefix(q, "recordComplianceJob(") }
	hook.fire = func() {
		r.mutate("recordComplianceJob", map[string]any{"jobId": id, "status": "done", "outcome": theirs, "completedAt": r.at.Format(time.RFC3339)})
	}
	r.run(r.connectorOn(hook, nil))
	if !hook.fired {
		t.Fatal("the other run did not write")
	}
	if job := r.job(TopicRedact, customer); job["status"] != "done" || job["outcome"] != theirs {
		t.Errorf("the job is %v: %q, want it as the other run left it", job["status"], job["outcome"])
	}
}

// ---- Item 2: a purge decides on what is true now ----

// uninstalledWithAProduct is a store whose uninstall is older than the hold,
// one row of its mirror, and its shop/redact due.
func (r *runnerFixture) uninstalledWithAProduct(product string) {
	r.t.Helper()
	r.hasStore()
	r.mutate("markStoreUninstalled", map[string]any{"storeId": r.store, "uninstalledAt": r.at.Add(-49 * time.Hour).Format(time.RFC3339)})
	r.write("product", map[string]any{"id": product, "updatedAt": "2026-10-01T10:00:00Z", "title": "Road bicycle"}, seen)
	r.queue(TopicShopRedact, "", nil, r.at.Add(-time.Minute))
}

func (r *runnerFixture) mirrorKeys() []string {
	r.t.Helper()
	var keys []string
	for key := range r.allTexts() {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// A store that reconnects while a run of the privacy jobs is under way is not
// purged by that run. The stores were read once, when the run began, so the
// run's shop/redact decided on a row from before the reconnect: it deleted
// everything the reconnect had just read and stamped the store redacted.
func TestAStoreThatReconnectsDuringARunIsNotPurged(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	const before, after = "gid://shopify/Product/9201", "gid://shopify/Product/9202"
	r.uninstalledWithAProduct(before)
	// An earlier job of the same run.
	r.queue(TopicRedact, eraCustomerGID("9201"), nil, r.at.Add(-2*time.Hour))

	queued := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
	hook := &hookEngine{IntegrationEngineAccess: queued}
	hook.match = func(q string) bool { return strings.HasPrefix(q, "recordComplianceJob(") }
	hook.fire = func() {
		// The owner reconnects the store while the first job is being
		// recorded: the uninstall is cleared, the grant is back, the store
		// answers, and a row is read again.
		r.mutate("markStoreReconnected", map[string]any{"storeId": r.store, "status": StatusConfigured})
		r.mutate("updateStore", map[string]any{"storeId": r.store, "adminTokenRef": "EXAMPLE_ADMIN"})
		r.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
		r.write("product", map[string]any{"id": after, "updatedAt": "2026-10-01T10:00:00Z", "title": "City bicycle"}, seen)
	}
	conn := r.connectorOn(hook, nil)
	if ran := r.run(conn); ran != 2 || !hook.fired {
		t.Fatalf("ran %d job(s), and the reconnect landed between them: %v", ran, hook.fired)
	}
	if got := strings.Join(r.mirrorKeys(), ", "); got != "product "+before+", product "+after {
		t.Errorf("after the run the mirror holds [%s], want both products: the store came back before its purge", got)
	}
	// The purge is skipped, and the skip queues what the store has not
	// started to read: the fixture's reconnect queued nothing itself.
	want := fmt.Sprintf("skipped: the store is reachable again; queued %d backfill(s) of the store that had not started", len(queued.calls))
	if job := r.job(TopicShopRedact, ""); job["status"] != "done" || len(queued.calls) == 0 || job["outcome"] != want {
		t.Errorf("the shop's erasure is %v: %v, want it skipped, saying %q", job["status"], job["outcome"], want)
	}
	fresh, found, err := conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if err != nil || !found || fresh.RedactedAt != "" || !fresh.Ingests() {
		t.Errorf("the reconnected store reads redactedAt %q, ingests %v (%v, %v); want it connected and never stamped redacted", fresh.RedactedAt, fresh.Ingests(), found, err)
	}
}

// A store that does not ANSWER is not purged. Any error used to read as "not
// reachable", so a timeout from the origin deleted the mirror of a store whose
// row says it is connected. Only a clear refusal says the app is gone.
func TestAStoreThatDoesNotAnswerIsNotPurged(t *testing.T) {
	for name, origin := range map[string]func(r *runnerFixture, client *AdminClient){
		"the origin answers 503": func(r *runnerFixture, _ *AdminClient) {
			r.admin.httpStatus("ShopifyReachable", http.StatusServiceUnavailable)
		},
		"the origin cannot be reached": func(_ *runnerFixture, client *AdminClient) {
			client.endpoint = func(Store) string { return "http://127.0.0.1:1/graphql" }
		},
		"the origin answers nothing a reader can use": func(r *runnerFixture, _ *AdminClient) {
			r.admin.httpStatus("ShopifyReachable", http.StatusForbidden)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRunnerFixture(t)
			if r == nil {
				return
			}
			const product = "gid://shopify/Product/9301"
			r.hasStore()
			r.write("product", map[string]any{"id": product, "updatedAt": "2026-10-01T10:00:00Z", "title": "Road bicycle"}, seen)
			r.queue(TopicShopRedact, "", nil, r.at.Add(-time.Minute))
			conn := r.connectorOn(r.narrowed(), func(client *AdminClient) { origin(r, client) })

			if ran := r.run(conn); ran != 0 {
				t.Fatalf("ran %d job(s), want the purge failed", ran)
			}
			if got := strings.Join(r.mirrorKeys(), ", "); got != "product "+product {
				t.Errorf("the mirror holds [%s] after the store did not answer, want its row untouched", got)
			}
			job := r.job(TopicShopRedact, "")
			if job["status"] != "failed" || job["lastError"] != failReach {
				t.Errorf("the job is %v: %q, want it failed for the store's silence and run again", job["status"], job["lastError"])
			}
			for _, action := range r.audits() {
				if strings.Contains(action, "shopify_shop_redacted") {
					t.Errorf("a purge that did not happen was audited as %s", action)
				}
			}
			fresh, _, _ := conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
			if fresh.RedactedAt != "" {
				t.Errorf("the store was stamped redacted at %q", fresh.RedactedAt)
			}
		})
	}
}

// A store the origin REFUSES the app for is purged: the row says it ingests,
// and the origin says the app is not installed there. That is the uninstall
// whose notice never arrived.
func TestAStoreTheOriginRefusesTheAppForIsPurged(t *testing.T) {
	for name, status := range map[string]int{"the token is refused": http.StatusUnauthorized, "there is no such shop": http.StatusNotFound} {
		t.Run(name, func(t *testing.T) {
			r := newRunnerFixture(t)
			if r == nil {
				return
			}
			r.hasStore()
			r.write("product", map[string]any{"id": "gid://shopify/Product/9401", "updatedAt": "2026-10-01T10:00:00Z", "title": "Road bicycle"}, seen)
			r.queue(TopicShopRedact, "", nil, r.at.Add(-time.Minute))
			r.admin.httpStatus("ShopifyReachable", status)
			if ran := r.run(r.connectorOn(r.narrowed(), nil)); ran != 1 {
				t.Fatalf("ran %d job(s), want the purge", ran)
			}
			if left := r.mirrorKeys(); len(left) != 0 {
				t.Errorf("the mirror still holds %v", left)
			}
			if job := r.job(TopicShopRedact, ""); job["status"] != "pending" || noFirstPass(job) {
				t.Errorf("the job is %v with scrubbedAt %v, want its first purge made", job["status"], job["scrubbedAt"])
			}
		})
	}
}

// A purge names its store. With an empty id the delete would match every row
// that carries none.
func TestAPurgeRefusesAnEmptyStoreId(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	// A row that carries no store id, as a fault elsewhere could leave one.
	const gid = "gid://shopify/Product/990099009900"
	id := generated.ConceptID("product") + ":" + MirrorRowID("", gid)
	clean := func() { _, _ = r.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id = $1`, id) }
	clean()
	t.Cleanup(clean)
	storeless := mapObject(generated.Types["product"], "", map[string]any{"id": gid, "updatedAt": "2026-10-01T10:00:00Z", "title": "of no store"}, "", seen)
	if err := r.conn.writeMirror(context.Background(), storeless[0]); err != nil {
		t.Fatalf("write a row with no store id: %v", err)
	}
	_, err := r.conn.PurgeStore(context.Background(), Store{})
	if err == nil || !strings.Contains(err.Error(), "store's id") {
		t.Errorf("a purge with no store id answered %v, want it refused for want of the id", err)
	}
	var left int
	if err := r.db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id = $1`, id).Scan(&left); err != nil || left != 1 {
		t.Errorf("a purge with no store id deleted the row that carries none (%d left, %v)", left, err)
	}
}

// ---- Item 4: the runner ----

// stopsTheClockAfterOneJob is a connector whose clock moves past the run's
// budget when the run records its first job.
func (r *runnerFixture) pastItsBudgetAfterOneJob() *Connector {
	hook := &hookEngine{IntegrationEngineAccess: r.narrowed()}
	hook.match = func(q string) bool { return strings.HasPrefix(q, "recordComplianceJob(") }
	hook.fire = func() { r.at = r.at.Add(privacyRunBudget + time.Minute) }
	return r.connectorOn(hook, nil)
}

// A run takes jobs for its budget and no longer: what is left waits for the
// next run, which takes them. One job of a large store can take minutes, and
// the runs are single, so a run that went through every due job whatever the
// time would hold the next one off for as long as it took.
func TestARunStopsTakingJobsWhenItsBudgetIsSpent(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	for i, n := range []string{"7101", "7102", "7103"} {
		r.write("customer", adaCustomer(n, "2026-10-01T10:00:00Z"), seen)
		r.queue(TopicRedact, eraCustomerGID(n), nil, r.at.Add(-time.Duration(3-i)*time.Hour))
	}
	if ran := r.run(r.pastItsBudgetAfterOneJob()); ran != 1 {
		t.Fatalf("a run whose budget was spent after its first job ran %d", ran)
	}
	if first := r.job(TopicRedact, eraCustomerGID("7101")); noFirstPass(first) {
		t.Error("the oldest due job was not the one the run took")
	}
	for _, n := range []string{"7102", "7103"} {
		if job := r.job(TopicRedact, eraCustomerGID(n)); job["status"] != "pending" || !noFirstPass(job) {
			t.Errorf("the job of customer %s is %v with scrubbedAt %v, want it left as it was for the next run", n, job["status"], job["scrubbedAt"])
		}
	}
	if ran := r.run(r.conn); ran != 2 {
		t.Errorf("the next run ran %d, want the two the first left", ran)
	}
}

// New requests are worked before repeats of old ones. An erasure an earlier
// version recorded done is made again, and was due long ago: read oldest due
// first, thousands of them would hold back a request that arrived yesterday.
func TestNewRequestsAreWorkedBeforeRepeatsOfOldOnes(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	longAgo := r.at.Add(-30 * 24 * time.Hour)
	old := r.queue(TopicRedact, eraCustomerGID("7201"), nil, longAgo)
	r.mutate("recordComplianceJob", map[string]any{"jobId": old, "status": "done", "outcome": "scrubbed 2 row version(s)", "completedAt": longAgo.Format(time.RFC3339)})
	r.queue(TopicRedact, eraCustomerGID("7202"), nil, r.at.Add(-time.Minute))

	if ran := r.run(r.pastItsBudgetAfterOneJob()); ran != 1 {
		t.Fatalf("a run whose budget was spent after its first job ran %d", ran)
	}
	if fresh := r.job(TopicRedact, eraCustomerGID("7202")); noFirstPass(fresh) {
		t.Error("the new request was not the job the run took first")
	}
	if repeat := r.job(TopicRedact, eraCustomerGID("7201")); repeat["status"] != "done" || !noFirstPass(repeat) || !instant(t, repeat, "dueAt").Equal(longAgo) {
		t.Errorf("the old erasure is %v with scrubbedAt %v, due %v; want it untouched until a run reaches it", repeat["status"], repeat["scrubbedAt"], repeat["dueAt"])
	}
}

// An old erasure being made again is due from when it is first taken. Its
// row says it was due a month ago, so its first failure used to read as a
// privacy request failing for a month: every store with an old erasure would
// need attention the moment one repeat failed once.
func TestAnOldErasureThatFailsOnceIsNotAFailureOfADay(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("7301")
	longAgo := r.at.Add(-30 * 24 * time.Hour)
	old := r.queue(TopicRedact, customer, nil, longAgo)
	r.mutate("recordComplianceJob", map[string]any{"jobId": old, "status": "done", "outcome": "scrubbed 2 row version(s)", "completedAt": longAgo.Format(time.RFC3339)})

	taken := r.at
	if ran := r.run(r.broken); ran != 0 {
		t.Fatalf("the broken runner ran %d", ran)
	}
	job := r.job(TopicRedact, customer)
	if job["status"] != "failed" || !instant(t, job, "dueAt").Equal(taken) {
		t.Fatalf("the old erasure that failed is %v, due %v; want it failed and due from when it was taken (%v)", job["status"], job["dueAt"], taken)
	}
	if got, err := r.conn.privacyRequestsFailing(context.Background(), r.store); err != nil || got.Count != 0 {
		t.Fatalf("a repeat that failed once reads as %+v, %v; want no request failing for a day", got, err)
	}
	// It goes on failing: a day later it is a failure of a day, counted from
	// when it was first taken and not from each try.
	r.at = taken.Add(12 * time.Hour)
	r.run(r.broken)
	r.at = taken.Add(25 * time.Hour)
	r.run(r.broken)
	if job := r.job(TopicRedact, customer); !instant(t, job, "dueAt").Equal(taken) {
		t.Errorf("a later failure moved the job's dueAt to %v", job["dueAt"])
	}
	if got, err := r.conn.privacyRequestsFailing(context.Background(), r.store); err != nil || got.Count != 1 || !got.Since.Equal(taken) {
		t.Errorf("after a day of failing the read answers %+v, %v; want the one request, since it was first taken", got, err)
	}
}

// A pass that failed is not audited as an erasure that ran. The scrub used to
// be audited before the store's backfills were settled, so a pass that then
// failed left "redacted" on the trail for an erasure an old export could
// still undo -- once for every run that failed.
func TestAPassThatFailedIsNotAuditedAsRun(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("7401")
	r.write("customer", adaCustomer("7401", "2026-10-01T10:00:00Z"), seen)
	r.queue(TopicRedact, customer, nil, r.at.Add(-time.Minute))
	// The store's order backfill is running, and cannot be started again.
	stateID := memqlsync.StateID(generated.ConceptID("order"), ConnectorName, string(memqlsync.DirectionInbound), r.store)
	r.mutate("upsertSyncState", map[string]any{
		"stateId": stateID, "conceptId": generated.ConceptID("order"), "connector": ConnectorName, "direction": "inbound",
		"partitionKey": r.store, "backfillStatus": "running",
	})
	refusing := &refusingEngine{IntegrationEngineAccess: r.narrowed(), prefix: "builtin datasyncQueueBackfill("}
	var logged strings.Builder
	failing := r.connectorOn(refusing, nil)
	failing.logger = slog.New(slog.NewTextHandler(&logged, nil))
	if ran := r.run(failing); ran != 0 || refusing.refused == 0 {
		t.Fatalf("ran %d job(s) with %d restart(s) refused, want the pass failed on its restart", ran, refusing.refused)
	}
	// The log says what failed, in the job's own sentence, and of what class
	// the error was. Never the error's text: this one quotes the statement it
	// refused and an address, and a failed export's can hold the whole export.
	if log := logged.String(); !strings.Contains(log, failRestart) || !strings.Contains(log, "class=") {
		t.Errorf("the failed job's log line does not carry its sentence and the error's class:\n%s", log)
	} else if strings.Contains(log, adaEmail) || strings.Contains(log, "refused:") || strings.Contains(log, "datasyncQueueBackfill(") {
		t.Errorf("the failed job's log line carries the cause's own text:\n%s", log)
	}
	if job := r.job(TopicRedact, customer); job["status"] != "failed" || job["lastError"] != failRestart {
		t.Fatalf("the job is %v: %q", job["status"], job["lastError"])
	}
	for _, action := range r.audits() {
		if strings.HasPrefix(action, "shopify_customer_redacted") {
			t.Errorf("a pass that failed was audited as %s", action)
		}
	}
	// The pass that runs is audited, once.
	if ran := r.run(r.connectorOn(&queuedBackfills{IntegrationEngineAccess: r.narrowed()}, nil)); ran != 1 {
		t.Fatalf("the next run ran %d", ran)
	}
	redacted := 0
	for _, action := range r.audits() {
		if action == "shopify_customer_redacted" {
			redacted++
		}
	}
	if redacted != 1 {
		t.Errorf("the pass that ran was audited %d time(s), want once", redacted)
	}
}

// refusingEngine refuses every call with a prefix, with an error whose text
// holds what a real one can: the statement, and a person's address in it.
type refusingEngine struct {
	memql.IntegrationEngineAccess
	prefix  string
	refused int
}

func (e *refusingEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(query, e.prefix) {
		e.refused++
		return nil, fmt.Errorf("refused: %s for %s", query, adaEmail)
	}
	return e.IntegrationEngineAccess.Execute(ctx, query)
}

// ---- Item 5: what the old erasure left ----

// oldErasureOf is a store with customer 123, erased by the old erasure, and
// customer 1234 with an order: the old erasure matched the gid as a substring
// of a row's text, so it marked 1234's rows as well.
func (r *runnerFixture) oldErasureOf123() (theirs []string) {
	r.t.Helper()
	r.hasStore()
	r.write("customer", adaCustomer("123", "2026-10-01T10:00:00Z"), seen)
	other := adaCustomer("1234", "2026-10-01T10:00:00Z")
	other["firstName"], other["lastName"], other["displayName"] = "Bob", "Sample", "Bob Sample"
	other["lastOrder"], other["orders"] = nil, nil
	r.write("customer", other, seen)
	r.write("order", bikeOrder("51234", eraCustomerGID("1234"), "2026-10-01T10:00:00Z"), seen)
	r.leftByTheOldErasure(eraCustomerGID("123"))
	if o := r.newest("order", eraOrderGID("51234")); o["billingAddress"] != RedactionMarker {
		r.t.Fatalf("the fixture is not what the old erasure left on another customer's order: %v", o["billingAddress"])
	}
	return []string{"customer " + eraCustomerGID("1234"), "order " + eraOrderGID("51234")}
}

func (r *runnerFixture) oldDone(customer string) {
	r.t.Helper()
	longAgo := r.at.Add(-30 * 24 * time.Hour)
	id := r.queue(TopicRedact, customer, nil, longAgo)
	r.mutate("recordComplianceJob", map[string]any{"jobId": id, "status": "done", "outcome": "scrubbed 2 row version(s)", "completedAt": longAgo.Format(time.RFC3339)})
}

// A row of ANOTHER customer that the old erasure marked is repaired, and its
// kind is queued to be read again. The repair makes the row writable: the
// marker in an address made every later write of the row fail. It cannot
// bring back what the old erasure wrongly scrubbed there -- the order's
// number is a string, and a string that reads "[redacted]" is a valid one --
// so the row has to be read from the origin again.
func TestARowOfAnotherCustomerTheOldErasureMarkedIsRepairedAndReadAgain(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.oldErasureOf123()
	r.oldDone(eraCustomerGID("123"))
	later := mapObject(generated.Types["order"], r.store, map[string]any{
		"id": eraOrderGID("51234"), "updatedAt": "2026-11-01T10:00:00Z", "name": "#51234", "displayFinancialStatus": "REFUNDED", "totalPriceSet": euros("1299.00"),
	}, "", seen.Add(31*24*time.Hour))
	if err := r.conn.writeMirror(context.Background(), later[0]); err == nil {
		t.Fatal("the premise does not hold: the mirror can write the other customer's order the old erasure marked")
	}

	queued := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
	if ran := r.run(r.connectorOn(queued, nil)); ran != 1 {
		t.Fatalf("ran %d", ran)
	}
	o := r.newest("order", eraOrderGID("51234"))
	if _, present := o["billingAddress"]; present {
		t.Errorf("the other customer's order still holds %v in its billing address", o["billingAddress"])
	}
	if o["customerGid"] != eraCustomerGID("1234") || o["currencyCode"] != "EUR" {
		t.Errorf("the repair changed more than the marker: %v", o)
	}
	if err := r.conn.writeMirror(context.Background(), later[0]); err != nil {
		t.Errorf("after the repair the mirror still cannot write the other customer's order: %v", err)
	}
	var kinds []string
	for _, call := range queued.calls {
		if !strings.Contains(call, "restart: true") || !strings.Contains(call, r.store) {
			t.Errorf("a kind was queued without the restart flag, or for another store: %s", call)
		}
		for _, kind := range []string{"customer", "order"} {
			if strings.Contains(call, `"`+generated.ConceptID(kind)+`"`) {
				kinds = append(kinds, kind)
			}
		}
	}
	sort.Strings(kinds)
	if strings.Join(kinds, ",") != "customer,order" {
		t.Errorf("queued %v (%d call(s)), want the customers and the orders read again from nothing, once each", kinds, len(queued.calls))
	}
	if job := r.job(TopicRedact, eraCustomerGID("123")); !strings.Contains(fmt.Sprint(job["outcome"]), "an earlier erasure") {
		t.Errorf("the job's outcome does not say what was repaired: %v", job["outcome"])
	}
}

// A store the old erasure never touched has nothing to repair, and nothing of
// it is read again: the repair runs at the first pass of every erasure, and
// must cost a store nothing but the look.
func TestAStoreWithNoOldMarkerQueuesNothing(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	r.write("customer", adaCustomer("7501", "2026-10-01T10:00:00Z"), seen)
	r.write("order", bikeOrder("57501", eraCustomerGID("7501"), "2026-10-01T10:00:00Z"), seen)
	other := bikeOrder("57502", eraCustomerGID("7502"), "2026-10-01T10:00:00Z")
	r.write("order", other, seen)
	before := r.allTexts()["order "+eraOrderGID("57502")]
	r.queue(TopicRedact, eraCustomerGID("7501"), nil, r.at.Add(-time.Minute))

	queued := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
	if ran := r.run(r.connectorOn(queued, nil)); ran != 1 {
		t.Fatalf("ran %d", ran)
	}
	if len(queued.calls) != 0 {
		t.Errorf("a store with no old marker had %d backfill(s) queued: %v", len(queued.calls), queued.calls)
	}
	if after := r.allTexts()["order "+eraOrderGID("57502")]; after != before {
		t.Error("another customer's order was changed by an erasure with nothing to repair")
	}
	if job := r.job(TopicRedact, eraCustomerGID("7501")); strings.Contains(fmt.Sprint(job["outcome"]), "an earlier erasure") {
		t.Errorf("the outcome speaks of a repair that did not happen: %v", job["outcome"])
	}
}

// Two old erasures of one store repair the store once, in one run.
func TestTwoOldErasuresOfOneStoreRepairItOnce(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.oldErasureOf123()
	r.write("customer", adaCustomer("7601", "2026-10-01T10:00:00Z"), seen)
	r.oldDone(eraCustomerGID("123"))
	r.oldDone(eraCustomerGID("7601"))

	queued := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
	if ran := r.run(r.connectorOn(queued, nil)); ran != 2 {
		t.Fatalf("ran %d, want both old erasures", ran)
	}
	if len(queued.calls) != 2 {
		t.Errorf("the two erasures queued %d backfill(s), want the customers and the orders once each: %v", len(queued.calls), queued.calls)
	}
	repaired := 0
	for _, customer := range []string{"123", "7601"} {
		if strings.Contains(fmt.Sprint(r.job(TopicRedact, eraCustomerGID(customer))["outcome"]), "an earlier erasure") {
			repaired++
		}
	}
	if repaired != 1 {
		t.Errorf("%d of the two jobs say they repaired the store, want the first of them and not the second", repaired)
	}
}

// The repair runs once a store, ever. The store's row says that it ran, and
// no erasure after that looks through the store again: what the old erasure
// left cannot be left anew, and the look reads every row of the store in
// every kind. So damage of the old kind that appears afterwards (here planted
// by hand) is not repaired: nothing reads for it.
func TestTheRepairRunsOnceAStoreEver(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.oldErasureOf123()
	r.oldDone(eraCustomerGID("123"))
	queued := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
	conn := r.connectorOn(queued, nil)
	if ran := r.run(conn); ran != 1 {
		t.Fatalf("ran %d", ran)
	}
	first := len(queued.calls)
	if _, present := r.newest("order", eraOrderGID("51234"))["billingAddress"]; present || first == 0 {
		t.Fatalf("a store never repaired was not repaired at its first erasure (%d backfill(s) queued)", first)
	}
	store, found, err := conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if err != nil || !found || mapString(store.Health, healthOldErasureRepaired) == "" {
		t.Fatalf("the store's row does not say its repair ran: %v (%v, %v)", store.Health, found, err)
	}

	// Later: the same damage on another customer's order, and another erasure.
	other := adaCustomer("8801", "2026-10-01T10:00:00Z")
	other["lastOrder"], other["orders"] = nil, nil
	r.write("customer", other, seen)
	r.write("order", bikeOrder("58801", eraCustomerGID("8801"), "2026-10-01T10:00:00Z"), seen)
	r.leftByTheOldErasure(eraCustomerGID("8801"))
	r.write("customer", adaCustomer("8802", "2026-10-01T10:00:00Z"), seen)
	r.queue(TopicRedact, eraCustomerGID("8802"), nil, r.at.Add(-time.Minute))
	r.at = r.at.Add(2 * time.Hour)
	if ran := r.run(conn); ran < 1 {
		t.Fatalf("the later run ran %d", ran)
	}
	if got := r.newest("order", eraOrderGID("58801"))["billingAddress"]; got != RedactionMarker {
		t.Errorf("a later erasure looked through a store that was repaired once: the planted marker reads %v", got)
	}
	if len(queued.calls) != first {
		t.Errorf("a later erasure queued %d more backfill(s) for a repair", len(queued.calls)-first)
	}
	if job := r.job(TopicRedact, eraCustomerGID("8802")); strings.Contains(fmt.Sprint(job["outcome"]), "an earlier erasure") {
		t.Errorf("a later erasure's outcome speaks of a repair: %v", job["outcome"])
	}
}

// ---- Item 6: what an erasure also holds elsewhere (ruling 90) ----

// staged stages one delivery under a source as the receiver does, and stamps
// it as the dispatcher would have by now.
func (r *runnerFixture) staged(source, id, status string, attempts int) {
	r.t.Helper()
	body := `{"id":820982911946154508,"email":"` + adaEmail + `","customer":{"first_name":"Ada","last_name":"Example"},"note":"delivery ` + id + `"}`
	r.mutate("stageInboundRequest", map[string]any{
		"requestId": id, "source": source, "medium": "webhook", "body": body, "contentType": "application/json",
		"headersJson": `{"x-shopify-topic":"orders/create"}`, "signatureVerified": true, "verifiedBy": "connector",
		"receivedAt": r.at.Add(-time.Hour).Format(time.RFC3339),
	})
	if status == "received" {
		return
	}
	r.mutate("updateInboundRequestStatus", map[string]any{"requestId": id, "status": "processing", "attempts": attempts})
	stamp := map[string]any{"requestId": id, "status": status, "attempts": attempts}
	if status == "processed" {
		stamp["processedAt"] = r.at.Add(-30 * time.Minute).Format(time.RFC3339)
	} else {
		stamp["lastError"] = "the origin did not answer"
		if attempts < 4 {
			stamp["nextAttemptAt"] = r.at.Add(time.Minute).Format(time.RFC3339)
		}
	}
	r.mutate("updateInboundRequestStatus", stamp)
}

// bodies is the body of every stored version of the deliveries under a
// source, by the delivery's id, oldest version first.
func (r *runnerFixture) bodies(source string) map[string][]string {
	r.t.Helper()
	rows, err := queryRows(context.Background(), r.db,
		`SELECT id, COALESCE(payload->>'body', '<no body key>') FROM "MemoryNodes"
		  WHERE concept = 'v1:platform:inboundRequest' AND payload->>'source' = $1 ORDER BY id, "createdAt"`, source)
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string][]string{}
	for _, row := range rows {
		id := strings.TrimPrefix(row[0], "v1:platform:inboundRequest:")
		out[id] = append(out[id], row[1])
	}
	return out
}

// withDeliveries stages the fixture store's deliveries and another store's,
// and removes them when the test ends.
func (r *runnerFixture) withDeliveries() (own, other string) {
	r.t.Helper()
	own, other = storeInboundSource(r.store), storeInboundSource(r.store+"-other")
	clean := func() {
		_, _ = r.db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'source' IN ($1, $2)`, own, other)
	}
	clean()
	r.t.Cleanup(clean)
	tag := strings.TrimPrefix(r.store, "dbtest-era-")
	r.staged(own, tag+"-done-0", "processed", 1)
	r.staged(own, tag+"-done-1", "processed", 1)
	r.staged(own, tag+"-done-2", "processed", 2)
	r.staged(own, tag+"-held", "failed", 1)
	r.staged(own, tag+"-given-up", "failed", 4)
	r.staged(own, tag+"-new", "received", 0)
	r.staged(other, tag+"-theirs", "processed", 1)
	return own, other
}

// A customer's erasure blanks the body of every processed delivery of the
// store, in every version of its row. A webhook's raw body is kept on its
// staged row, and an order's or a customer's carries the person's name, email
// and address; the mirror was scrubbed and the deliveries were not. A
// delivery that is not processed keeps its body: a later try needs it.
func TestACustomersErasureBlanksTheStoresProcessedDeliveries(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	own, other := r.withDeliveries()
	tag := strings.TrimPrefix(r.store, "dbtest-era-")
	// Two deliveries to a read and one to a statement, so that the read is
	// made again from where it stopped and the blanking goes batch by batch.
	page, batch := deliveryFindPage, deliveryBlankBatch
	deliveryFindPage, deliveryBlankBatch = 2, 1
	t.Cleanup(func() { deliveryFindPage, deliveryBlankBatch = page, batch })
	before := r.bodies(own)
	if len(before[tag+"-done-1"]) < 3 || !strings.Contains(before[tag+"-done-1"][0], adaEmail) {
		t.Fatalf("the fixture's processed delivery is %d version(s): %v", len(before[tag+"-done-1"]), before[tag+"-done-1"])
	}
	customer := eraCustomerGID("7701")
	r.write("customer", adaCustomer("7701", "2026-10-01T10:00:00Z"), seen)
	r.queue(TopicRedact, customer, nil, r.at.Add(-time.Minute))

	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("ran %d", ran)
	}
	after := r.bodies(own)
	for _, id := range []string{tag + "-done-0", tag + "-done-1", tag + "-done-2"} {
		for i, body := range after[id] {
			if body != "" {
				t.Errorf("version %d of the processed delivery %s still holds its body: %s", i, id, body)
			}
		}
		if len(after[id]) != len(before[id]) {
			t.Errorf("the delivery %s has %d version(s), had %d: a body is blanked where it is, not by a new version", id, len(after[id]), len(before[id]))
		}
	}
	for _, id := range []string{tag + "-held", tag + "-given-up", tag + "-new"} {
		if strings.Join(after[id], "\n") != strings.Join(before[id], "\n") || !strings.Contains(strings.Join(after[id], "\n"), adaEmail) {
			t.Errorf("the delivery %s is not processed and lost its body: %v", id, after[id])
		}
	}
	if theirs := r.bodies(other)[tag+"-theirs"]; len(theirs) == 0 || !strings.Contains(theirs[0], adaEmail) {
		t.Errorf("another store's processed delivery was blanked: %v", theirs)
	}
	// A blanked delivery is a row the engine still reads and writes.
	r.mutate("updateInboundRequestStatus", map[string]any{"requestId": tag + "-done-1", "status": "processed"})
	if bodies := r.bodies(own)[tag+"-done-1"]; bodies[len(bodies)-1] != "" {
		t.Errorf("a write of a blanked delivery brought its body back: %q", bodies[len(bodies)-1])
	}
	if job := r.job(TopicRedact, customer); !strings.Contains(fmt.Sprint(job["outcome"]), "processed deliveries") {
		t.Errorf("the outcome does not say the deliveries were blanked: %v", job["outcome"])
	}

	// The second pass takes a delivery processed since the first.
	r.staged(own, tag+"-done-3", "processed", 1)
	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the second pass ran %d", ran)
	}
	for i, body := range r.bodies(own)[tag+"-done-3"] {
		if body != "" {
			t.Errorf("version %d of a delivery processed between the passes still holds its body", i)
		}
	}
}

// A shop's erasure deletes every staged delivery of the store's own source,
// whatever its status, and no other source's.
func TestAShopsErasureDeletesTheStoresStagedDeliveries(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.uninstalledWithAProduct("gid://shopify/Product/7801")
	own, other := r.withDeliveries()
	tag := strings.TrimPrefix(r.store, "dbtest-era-")
	if len(r.bodies(own)) != 6 {
		t.Fatalf("the fixture staged %d deliveries", len(r.bodies(own)))
	}
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("ran %d", ran)
	}
	if left := r.bodies(own); len(left) != 0 {
		t.Errorf("after the shop's erasure %d of its deliveries remain: %v", len(left), left)
	}
	if theirs := r.bodies(other)[tag+"-theirs"]; len(theirs) == 0 {
		t.Error("another store's delivery was deleted")
	}
	// The second purge takes one staged since the first.
	r.staged(own, tag+"-late", "processed", 1)
	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the second purge ran %d", ran)
	}
	if left := r.bodies(own); len(left) != 0 {
		t.Errorf("after the second purge %d of its deliveries remain", len(left))
	}
}

// ---- Ruling 95: the deliveries that asked ----

// A privacy job remembers the deliveries that asked for it, and their bodies
// are blanked when it has done its work. A managed store's privacy deliveries
// arrive on the app-level source, which blanking "the store's own source"
// never reaches; and the body of customers/redact holds the customer's email
// address and her phone number. So the delivery that asks for an erasure kept
// both after it.
func TestAnErasureBlanksTheDeliveriesThatAskedForIt(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("9601")
	r.write("customer", adaCustomer("9601", "2026-10-01T10:00:00Z"), seen)
	ids := r.onTheAppSource("redact-1", "redact-2", "somebody-elses")
	first, second, theirs := ids[0], ids[1], ids[2]
	// Another store's privacy delivery on the same source, which no job of
	// this store remembers.
	r.staged(ConnectorName, theirs, "processed", 1)

	r.deliversAs(r.conn, ConnectorName, first, TopicRedact, "9601")
	if held := r.deliveryBodies(first); len(held) < 3 || !strings.Contains(held[0], adaEmail) || !strings.Contains(held[0], adaPhone) {
		t.Fatalf("the fixture's delivery is %v, want her email and phone in every version", held)
	}
	// She asks again while the first request waits: its delivery is remembered too.
	r.at = r.at.Add(time.Hour)
	r.deliversAs(r.conn, ConnectorName, second, TopicRedact, "9601", "59601")
	job := r.job(TopicRedact, customer)
	if got := fmt.Sprint(job["deliveryIds"]); !strings.Contains(got, first) || !strings.Contains(got, second) {
		t.Fatalf("the job remembers the deliveries %v, want both that asked for it", job["deliveryIds"])
	}

	r.at = r.at.Add(25 * time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the first pass: ran %d", ran)
	}
	for _, id := range []string{first, second} {
		for i, body := range r.deliveryBodies(id) {
			if body != "" {
				t.Errorf("version %d of the delivery %s that asked for the erasure still holds its body: %s", i, id, body)
			}
		}
	}
	if held := r.deliveryBodies(theirs); len(held) == 0 || !strings.Contains(held[0], adaEmail) {
		t.Errorf("a delivery no job of this store remembers was blanked: %v", held)
	}
	if job := r.job(TopicRedact, customer); !strings.Contains(fmt.Sprint(job["outcome"]), "that asked for it") {
		t.Errorf("the outcome does not say the deliveries that asked were blanked: %v", job["outcome"])
	}
}

// A data request's delivery holds the customer's email and phone as well, and
// is blanked when the request ends done: there is no pass of an erasure to do
// it.
func TestADataRequestThatEndsDoneBlanksItsDelivery(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("9701")
	r.write("customer", adaCustomer("9701", "2026-10-01T10:00:00Z"), seen)
	asked := r.onTheAppSource("data-request")[0]
	r.deliversAs(r.conn, ConnectorName, asked, TopicDataRequest, "9701")
	t.Cleanup(func() { r.removeLibraryRowsOf("dbtest-owner") })
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("ran %d", ran)
	}
	if job := r.job(TopicDataRequest, customer); job["status"] != "done" {
		t.Fatalf("the data request is %v: %v", job["status"], job["lastError"])
	}
	for i, body := range r.deliveryBodies(asked) {
		if body != "" {
			t.Errorf("version %d of the data request's delivery still holds its body: %s", i, body)
		}
	}
}

// A shop's erasure deals with the delivery that asked for it as well: it is
// blanked at the first purge, and deleted when the job ends because the store
// came back (ruling 96). The purge deletes the deliveries of the store's own
// source; a managed store's shop/redact is on the app-level source, where
// only its row id finds it.
func TestAShopsErasureBlanksTheDeliveryThatAskedForIt(t *testing.T) {
	for name, back := range map[string]bool{"the store is purged": false, "the store came back": true} {
		t.Run(name, func(t *testing.T) {
			r := newRunnerFixture(t)
			if r == nil {
				return
			}
			r.hasStore()
			if back {
				// A purge that is skipped queues the store's unstarted backfills.
				r.withTheSyncRuntime()
				r.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
			} else {
				r.mutate("markStoreUninstalled", map[string]any{"storeId": r.store, "uninstalledAt": r.at.Format(time.RFC3339)})
			}
			asked := r.onTheAppSource("shop-redact")[0]
			r.deliversAs(r.conn, ConnectorName, asked, TopicShopRedact, "")
			if held := r.deliveryBodies(asked); len(held) < 3 || !strings.Contains(held[0], "shop_domain") {
				t.Fatalf("the fixture's delivery is %v", held)
			}
			r.at = r.at.Add(ShopRedactHold + time.Hour)
			if ran := r.run(r.conn); ran != 1 {
				t.Fatalf("ran %d (%v)", ran, r.job(TopicShopRedact, "")["lastError"])
			}
			if job := r.job(TopicShopRedact, ""); back != (job["status"] == "done") {
				t.Fatalf("the job is %v: %v", job["status"], job["outcome"])
			}
			left := r.deliveryBodies(asked)
			if back != (len(left) == 0) {
				t.Errorf("the delivery that asked has %d version(s) left; want it there after a first purge, and gone when the job has ended", len(left))
			}
			for i, body := range left {
				if body != "" {
					t.Errorf("version %d of the delivery that asked for the shop's erasure still holds its body: %s", i, body)
				}
			}
		})
	}
}

// removeLibraryRowsOf removes what a test filed in an owner's Library.
func (r *runnerFixture) removeLibraryRowsOf(owner string) {
	_, _ = r.db.ExecContext(context.Background(),
		`DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:library:%' AND regexp_replace(payload->>'ownerUserId', '^v1:identity:user:', '') = $1`, owner)
}

// ---- Ruling 93: the exports a data request filed ----

// withTheLibrary registers the Library on the fixture's engine, with the raw
// handle its removal deletes on, as a node's plug-in does.
func (r *runnerFixture) withTheLibrary() {
	r.t.Helper()
	lib := library.NewIntegration(r.eng)
	lib.SetMaintenanceSession(privacySessionOf(r.db))
	if err := r.eng.RegisterIntegration(lib); err != nil {
		r.t.Fatalf("register the Library: %v", err)
	}
	r.t.Cleanup(func() { r.removeLibraryRowsOf("dbtest-owner") })
}

// exported runs a data request for a customer to its end and answers the id
// of the export it filed. The index row is written as the Library's
// automation writes it when an output is created; no automation runs here.
func (r *runnerFixture) exported(customer string) string {
	r.t.Helper()
	r.delivers(r.conn, TopicDataRequest, customer)
	if ran := r.run(r.conn); ran != 1 {
		r.t.Fatalf("the data request of customer %s: ran %d", customer, ran)
	}
	job := r.job(TopicDataRequest, eraCustomerGID(customer))
	id, _ := job["exportId"].(string)
	if job["status"] != "done" || id == "" {
		r.t.Fatalf("the data request of customer %s is %v and says it filed %q", customer, job["status"], id)
	}
	owner := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), "dbtest-owner"))
	if _, err := r.eng.Execute(owner, renderCall("createArtifact", map[string]any{
		"sourceConceptRef": "v1:library:generatedOutput:" + id, "ownerUserId": "dbtest-owner", "lens": "artifact", "kind": "generated_output",
		"source": "derived", "title": "an export", "summary": "Shopify customers/data_request export for " + eraCustomerGID(customer), "format": "text",
		"mimeType": "application/json", "live": false, "partitionId": "", "agentId": "", "producedByPlanId": "", "producedByWorkerId": "", "producedByWorkerName": "",
	})); err != nil {
		r.t.Fatalf("index the export: %v", err)
	}
	return id
}

// libraryRowsOf is the Library's rows for one export: the output's own and
// the index row's, as "concept id", every version.
func (r *runnerFixture) libraryRowsOf(export string) []string {
	r.t.Helper()
	rows, err := queryRows(context.Background(), r.db,
		`SELECT concept || ' ' || id FROM "MemoryNodes"
		  WHERE (concept = 'v1:library:generatedOutput' AND id = $1) OR (concept = 'v1:library:artifact' AND payload->>'sourceConceptRef' = $1)
		  ORDER BY concept, id, "createdAt"`, "v1:library:generatedOutput:"+export)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, row := range rows {
		out = append(out, row[0])
	}
	return out
}

// A customer's erasure removes the exports her data requests filed. A data
// request files everything MemQL holds on the person in the store owner's
// Library, and the index row's summary names her; the erasure scrubbed the
// mirror and left that document where it was. It is removed by the Library's
// own removal, whole, and checked; another customer's export stays.
func TestACustomersErasureRemovesTheExportsHerDataRequestsFiled(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	r.withTheLibrary()
	r.write("customer", adaCustomer("9801", "2026-10-01T10:00:00Z"), seen)
	r.write("customer", adaCustomer("9802", "2026-10-01T10:00:00Z"), seen)
	hers, theirs := r.exported("9801"), r.exported("9802")
	if got := r.libraryRowsOf(hers); len(got) != 2 {
		t.Fatalf("the fixture's export is %v, want the output and its index row", got)
	}
	other := r.libraryRowsOf(theirs)

	r.queue(TopicRedact, eraCustomerGID("9801"), nil, r.at.Add(-time.Minute))
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the erasure's first pass: ran %d (%v)", ran, r.job(TopicRedact, eraCustomerGID("9801"))["lastError"])
	}
	if left := r.libraryRowsOf(hers); len(left) != 0 {
		t.Errorf("after her erasure the export her data request filed is still in the Library: %v", left)
	}
	if got := r.libraryRowsOf(theirs); strings.Join(got, "\n") != strings.Join(other, "\n") {
		t.Errorf("another customer's export was touched: %v, was %v", got, other)
	}
	if job := r.job(TopicRedact, eraCustomerGID("9801")); !strings.Contains(fmt.Sprint(job["outcome"]), "export") {
		t.Errorf("the outcome does not say the export was removed: %v", job["outcome"])
	}

	// An export that lands between the passes: the filing of a slow pass of
	// her data request, made after the first pass's removal. The second pass
	// removes it too. (A data request that RUNS between the passes files
	// nothing, because her erasure has been made by then:
	// TestADataRequestForACustomerWhoHasBeenErasedFilesNothing.)
	late := ComplianceJob{StoreID: r.store, Topic: TopicDataRequest, CustomerGID: eraCustomerGID("9801"), RequestID: "42429801"}
	if err := r.conn.writeExportArtifact(context.Background(), Store{ID: r.store, OwnerUserID: "dbtest-owner"}, late, []byte(`{"rows":{}}`)); err != nil {
		t.Fatalf("file the export again: %v", err)
	}
	if got := r.libraryRowsOf(hers); len(got) == 0 {
		t.Fatal("the fixture did not file the export again")
	}
	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the erasure's second pass: ran %d", ran)
	}
	if left := r.libraryRowsOf(hers); len(left) != 0 {
		t.Errorf("after the second pass an export filed between the passes is still in the Library: %v", left)
	}
	if job := r.job(TopicRedact, eraCustomerGID("9801")); job["status"] != "done" {
		t.Errorf("the erasure ends %v: %v", job["status"], job["lastError"])
	}
}

// An export that cannot be removed fails the pass, with a sentence that says
// so and nothing of the cause; the next run makes the pass again.
func TestAnExportThatCannotBeRemovedFailsThePass(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	r.withTheLibrary()
	r.write("customer", adaCustomer("9901", "2026-10-01T10:00:00Z"), seen)
	hers := r.exported("9901")
	r.queue(TopicRedact, eraCustomerGID("9901"), nil, r.at.Add(-time.Minute))

	refusing := &refusingEngine{IntegrationEngineAccess: r.narrowed(), prefix: "builtin libraryRemoveArtifact("}
	if ran := r.run(r.connectorOn(refusing, nil)); ran != 0 || refusing.refused == 0 {
		t.Fatalf("ran %d job(s) with %d removal(s) refused, want the pass failed on the export", ran, refusing.refused)
	}
	if job := r.job(TopicRedact, eraCustomerGID("9901")); job["status"] != "failed" || job["lastError"] != failExports {
		t.Errorf("the job is %v: %q, want it failed for the export", job["status"], job["lastError"])
	}
	for _, action := range r.audits() {
		if strings.HasPrefix(action, "shopify_customer_redacted") {
			t.Errorf("a pass that left an export was audited as %s", action)
		}
	}
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the next run ran %d", ran)
	}
	if left := r.libraryRowsOf(hers); len(left) != 0 {
		t.Errorf("after the pass was made again the export is still in the Library: %v", left)
	}
}

// A data request reads nothing about the person until it knows who the export
// will belong to. It used to collect every row first and refuse afterwards.
func TestADataRequestChecksTheOwnerBeforeItReadsARow(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	job := ComplianceJob{Topic: TopicDataRequest, StoreID: r.store, CustomerGID: eraCustomerGID("7901"), RequestID: "dr-7901", ReceivedAt: r.at, DueAt: r.at}
	// A connector that cannot read a row at all: were a row read first, this
	// would fail as a read.
	_, err := r.broken.runComplianceJob(context.Background(), Store{ID: r.store, Domain: r.store + ".example.com"}, true, job)
	if got := lastErrorOf(err); got != failExportOwner {
		t.Errorf("a data request for a store with no owner fails as %q (%v), want %q before any row is read", got, err, failExportOwner)
	}
}

// ---- Item 4: the statements run on the maintenance session ----

// The privacy jobs run their statements on the engine's maintenance session
// and on nothing else. The request pool ends a statement at eight seconds,
// and a statement of the scope reads every row of a concept for a store.
func TestThePrivacyStatementsRunOnTheMaintenanceSession(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	customer := eraCustomerGID("8001")
	r.write("customer", adaCustomer("8001", "2026-10-01T10:00:00Z"), seen)
	opened, closed := 0, 0
	session := func(ctx context.Context) (*sql.Conn, func(), error) {
		conn, done, err := privacySessionOf(r.db)(ctx)
		opened++
		return conn, func() { closed++; done() }, err
	}
	// No raw handle at all: the session is the only way to the table.
	conn := NewConnector(r.narrowed(), nil, NewStoreRegistry(r.eng, r.eng.ResolveSystemSecret), NewAdminClient()).WithMaintenanceSession(session)
	conn.now = func() time.Time { return r.at }
	if _, err := conn.RedactCustomer(context.Background(), r.store, customer, nil); err != nil {
		t.Fatalf("an erasure on the maintenance session alone: %v", err)
	}
	r.holdsNoWordOf("customer", customer, adaWords)
	if opened != 1 || closed != 1 {
		t.Errorf("the erasure opened %d session(s) and closed %d, want one opened and closed", opened, closed)
	}
	// One run, one session, whatever the number of jobs.
	r.hasStore()
	for _, n := range []string{"8002", "8003"} {
		r.queue(TopicRedact, eraCustomerGID(n), nil, r.at.Add(-time.Minute))
	}
	opened, closed = 0, 0
	if ran := r.run(conn); ran != 2 {
		t.Fatalf("ran %d", ran)
	}
	if opened != 1 || closed != 1 {
		t.Errorf("a run of two jobs opened %d session(s) and closed %d, want one for the run", opened, closed)
	}
	// And a connector given the raw handle alone refuses, rather than run a
	// long statement on the pool.
	onPool := NewConnector(r.narrowed(), nil, NewStoreRegistry(r.eng, r.eng.ResolveSystemSecret), NewAdminClient()).WithDatabase(func() *sql.DB { return r.db })
	if _, err := onPool.RedactCustomer(context.Background(), r.store, customer, nil); err == nil || !strings.Contains(err.Error(), "maintenance session") {
		t.Errorf("an erasure with the request pool's handle alone answered %v, want it refused for want of the maintenance session", err)
	}
}

// ---- Fix round 3 ----

// afterReadEngine runs a function once, just AFTER the first matching call has
// been answered: what another writer does in the moment after a read.
type afterReadEngine struct {
	memql.IntegrationEngineAccess
	match func(query string) bool
	fire  func()
	fired bool
}

func (e *afterReadEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	res, err := e.IntegrationEngineAccess.Execute(ctx, query)
	if !e.fired && e.match(query) {
		e.fired = true
		e.fire()
	}
	return res, err
}

// The origin is asked before EVERY purge, unless the store's row already
// records the uninstall (ruling 97). The question used to be put only to a
// store whose row says it ingests, so a store a person had paused, still
// installed and answering, was purged without being asked.
func TestAPurgeAsksTheOriginUnlessTheRowRecordsTheUninstall(t *testing.T) {
	const product = "gid://shopify/Product/3301"
	for name, tc := range map[string]struct {
		arrange func(r *runnerFixture)
		second  bool
		purged  bool
		asked   int
	}{
		"a paused store that answers is not purged": {arrange: func(r *runnerFixture) {
			r.mutate("setStoreStatus", map[string]any{"storeId": r.store, "status": StatusPaused})
			r.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
		}, asked: 1},
		"nor on the second pass": {arrange: func(r *runnerFixture) {
			r.mutate("setStoreStatus", map[string]any{"storeId": r.store, "status": StatusPaused})
			r.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
		}, second: true, asked: 1},
		"a paused store the origin refuses the app for is purged": {arrange: func(r *runnerFixture) {
			r.mutate("setStoreStatus", map[string]any{"storeId": r.store, "status": StatusPaused})
			r.admin.httpStatus("ShopifyReachable", http.StatusUnauthorized)
		}, purged: true, asked: 1},
		"a store whose row records the uninstall is purged without a question": {arrange: func(r *runnerFixture) {
			r.mutate("markStoreUninstalled", map[string]any{"storeId": r.store, "uninstalledAt": r.at.Add(-49 * time.Hour).Format(time.RFC3339)})
			r.mutate("updateStore", map[string]any{"storeId": r.store, "adminTokenRef": "EXAMPLE_ADMIN"})
			r.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
		}, purged: true, asked: 0},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRunnerFixture(t)
			if r == nil {
				return
			}
			r.hasStore()
			// A purge that is skipped queues the store's unstarted backfills.
			queued := r.withTheSyncRuntime()
			r.write("product", map[string]any{"id": product, "updatedAt": "2026-10-01T10:00:00Z", "title": "Road bicycle"}, seen)
			tc.arrange(r)
			id := r.queue(TopicShopRedact, "", nil, r.at.Add(-time.Minute))
			if tc.second {
				r.mutate("recordComplianceJob", map[string]any{"jobId": id, "status": "pending", "outcome": "first pass", "scrubbedAt": r.at.Add(-time.Hour).Format(time.RFC3339)})
			}
			if ran := r.run(r.conn); ran != 1 {
				t.Fatalf("ran %d (%v)", ran, r.job(TopicShopRedact, "")["lastError"])
			}
			if got := len(r.mirrorKeys()) == 0; got != tc.purged {
				t.Errorf("the mirror holds %v, purged: want %v", r.mirrorKeys(), tc.purged)
			}
			if got := r.admin.countOp("ShopifyReachable"); got != tc.asked {
				t.Errorf("the origin was asked %d time(s), want %d", got, tc.asked)
			}
			job := r.job(TopicShopRedact, "")
			if !tc.purged && (job["status"] != "done" || !strings.Contains(fmt.Sprint(job["outcome"]), "reachable again")) {
				t.Errorf("the job is %v: %v, want it done and skipped", job["status"], job["outcome"])
			}
			if got := len(queued.calls) > 0; got == tc.purged {
				t.Errorf("%d backfill(s) were queued; want the unstarted ones of a store that was not purged, and none for one that was", len(queued.calls))
			}
		})
	}
}

// A store on the shared app holds a grant that has to be renewed, and by the
// time its purge is due the renewal is what the origin refuses. That refusal
// was a plain error, read as "could not tell", and the job failed for ever.
// The token endpoint answers a refresh token it no longer honours -- of an app
// that was uninstalled or revoked, expired, unknown, or already replaced --
// with 401 Unauthorized and the error invalid_request: that pair is the origin
// saying the grant no longer exists. Anything else it answers, a 401 that says
// something else among them, and a renewal that does not arrive, still says
// nothing.
func TestARefusedRenewalOfASharedAppStoresGrantIsTheOriginsRefusal(t *testing.T) {
	const noGrant = `{"error":"invalid_request","error_description":"This request requires an active refresh_token"}`
	for name, tc := range map[string]struct {
		status int
		body   string
		purged bool
	}{
		"the token endpoint answers 401 invalid_request: the grant no longer exists": {http.StatusUnauthorized, noGrant, true},
		"it answers 401 for another reason":                                          {http.StatusUnauthorized, `{"error":"invalid_client"}`, false},
		"it answers 401 and does not say why":                                        {http.StatusUnauthorized, `Unauthorized`, false},
		"it answers 400 invalid_request: a request it could not read":                {http.StatusBadRequest, noGrant, false},
		"it answers 503":     {http.StatusServiceUnavailable, noGrant, false},
		"it does not answer": {0, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRunnerFixture(t)
			if r == nil {
				return
			}
			asked := 0
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				asked++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			url := token.URL
			if tc.status == 0 {
				token.Close()
			} else {
				defer token.Close()
			}
			conn := r.onTheSharedAppWithItsUninstallMissed("gid://shopify/Product/3401", url)

			ran := r.run(conn)
			job := r.job(TopicShopRedact, "")
			if tc.purged {
				if ran != 1 || len(r.mirrorKeys()) != 0 || asked != 1 {
					t.Errorf("ran %d, the origin was asked to renew %d time(s), the mirror holds %v and the job is %v: %q; want the store purged", ran, asked, r.mirrorKeys(), job["status"], job["lastError"])
				}
				return
			}
			if ran != 0 || len(r.mirrorKeys()) != 1 || job["status"] != "failed" || job["lastError"] != failReach {
				t.Errorf("ran %d, the mirror holds %v and the job is %v: %q; want the job failed and nothing purged", ran, r.mirrorKeys(), job["status"], job["lastError"])
			}
		})
	}
}

// onTheSharedAppWithItsUninstallMissed is the fixture's store connected
// through the shared app, with one mirrored product and its shop/redact due:
// its row says it ingests, because the uninstall's notice never arrived, and
// its access token expired days ago, so the first thing asked of the origin is
// the renewal of its grant, at tokenURL. It answers the connector to run.
func (r *runnerFixture) onTheSharedAppWithItsUninstallMissed(product, tokenURL string) *Connector {
	r.t.Helper()
	if _, err := r.eng.Execute(seedCtx(), renderCall("createStore", map[string]any{
		"storeId": r.store, "domain": r.store + ".myshopify.com", "name": "Example Bicycles",
		"adminTokenRef": "EXAMPLE_OFFLINE_GRANT", "apiVersion": generated.APIVersion, "ownerUserId": "dbtest-owner",
	})); err != nil {
		r.t.Fatalf("seed the store: %v", err)
	}
	r.write("product", map[string]any{"id": product, "updatedAt": "2026-10-01T10:00:00Z", "title": "Road bicycle"}, seen)
	r.queue(TopicShopRedact, "", nil, r.at.Add(-time.Minute))
	grant := offlineGrant{AccessToken: "shpat_old", RefreshToken: "shprt_old", ClientID: "id", ClientSecret: "secret", Scope: "read_orders",
		ExpiresAt: r.at.Add(-95 * time.Hour), RefreshExpiresAt: r.at.Add(80 * 24 * time.Hour)}
	registry := NewStoreRegistry(r.eng, func(context.Context, string) (string, error) { return grant.encoded(), nil })
	registry.ttl = 0
	client := NewAdminClient()
	client.endpoint = func(Store) string { return r.admin.server.URL + "/graphql" }
	conn := NewConnector(r.narrowed(), nil, registry, client).WithDatabase(func() *sql.DB { return r.db }).WithMaintenanceSession(privacySessionOf(r.db))
	conn.now = func() time.Time { return r.at }
	conn.connectTokenURL = func(string) string { return tokenURL }
	return conn
}

// A renewal that times out says nothing about the grant: the token endpoint
// took the request and did not answer in the time a renewal is given. The job
// fails, and nothing is purged. (The time is a variable, so that this does
// not sit through the fifteen seconds a node gives it.)
func TestARenewalThatTimesOutPurgesNothing(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	asked, release := 0, make(chan struct{})
	token := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		asked++
		select {
		case <-release:
		case <-req.Context().Done():
		}
	}))
	defer token.Close()
	defer close(release)
	given := tokenExchangeTimeout
	tokenExchangeTimeout = 300 * time.Millisecond
	defer func() { tokenExchangeTimeout = given }()
	conn := r.onTheSharedAppWithItsUninstallMissed("gid://shopify/Product/3451", token.URL)

	started := time.Now()
	ran := r.run(conn)
	if waited := time.Since(started); waited > 8*time.Second {
		t.Errorf("the renewal was waited for %s, want the time a renewal is given", waited.Round(time.Millisecond))
	}
	job := r.job(TopicShopRedact, "")
	if ran != 0 || asked != 1 || len(r.mirrorKeys()) != 1 || job["status"] != "failed" || job["lastError"] != failReach {
		t.Errorf("ran %d, the origin was asked to renew %d time(s), the mirror holds %v and the job is %v: %q; want the job failed and nothing purged", ran, asked, r.mirrorKeys(), job["status"], job["lastError"])
	}
}

// A reconnect that lands between the read of the store and the stamp that
// marks it redacted. The row the purge decided on recorded the uninstall, so
// nothing was asked; the purge then deleted what the reconnect had just read
// and stamped a connected store redacted. The stamp is fenced on the row that
// was read: moved meanwhile, it is not written, the store's unstarted
// backfills are queued so what the purge removed is read again, and the job
// ends done saying so.
func TestAReconnectBetweenTheReadAndTheStampLeavesTheStoreConnected(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.uninstalledWithAProduct("gid://shopify/Product/3501")
	queued := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
	hook := &afterReadEngine{IntegrationEngineAccess: queued}
	hook.match = func(q string) bool { return strings.HasPrefix(q, "storeById(") }
	hook.fire = func() {
		r.mutate("markStoreReconnected", map[string]any{"storeId": r.store, "status": StatusConfigured})
		r.mutate("updateStore", map[string]any{"storeId": r.store, "adminTokenRef": "EXAMPLE_ADMIN"})
	}
	conn := r.connectorOn(hook, nil)
	if ran := r.run(conn); ran != 1 || !hook.fired {
		t.Fatalf("ran %d, and the reconnect landed after the read: %v (%v)", ran, hook.fired, r.job(TopicShopRedact, "")["lastError"])
	}
	fresh, found, err := conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if err != nil || !found || fresh.RedactedAt != "" || fresh.UninstalledAt != "" || !fresh.Ingests() {
		t.Errorf("the store reads redactedAt %q, uninstalledAt %q, ingests %v; want it connected and not stamped redacted", fresh.RedactedAt, fresh.UninstalledAt, fresh.Ingests())
	}
	if len(queued.calls) == 0 {
		t.Error("no backfill was queued for what the purge removed from a store that had come back")
	}
	job := r.job(TopicShopRedact, "")
	if job["status"] != "done" || !strings.Contains(fmt.Sprint(job["outcome"]), "connected again") {
		t.Errorf("the job is %v: %v, want it done, saying the store was connected again", job["status"], job["outcome"])
	}
	for _, action := range r.audits() {
		if action == "shopify_shop_redacted" {
			t.Errorf("a store that came back was audited as redacted")
		}
	}
}

// A purge that a reconnect overtook owes the store the backfills that read
// again what it removed. If they cannot be queued the job fails; and when it
// is run again the store answers, so it is not purged: that run still queues
// them. It used to end "skipped: the store is reachable again", and nothing
// ever read the store again.
func TestAnOvertakenPurgeWhoseBackfillsCouldNotBeQueuedQueuesThemWhenRunAgain(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.uninstalledWithAProduct("gid://shopify/Product/3581")
	refusing := &refusingEngine{IntegrationEngineAccess: r.narrowed(), prefix: "builtin datasyncQueueBackfill("}
	hook := &afterReadEngine{IntegrationEngineAccess: refusing}
	hook.match = func(q string) bool { return strings.HasPrefix(q, "storeById(") }
	hook.fire = func() {
		r.mutate("markStoreReconnected", map[string]any{"storeId": r.store, "status": StatusConfigured})
		r.mutate("updateStore", map[string]any{"storeId": r.store, "adminTokenRef": "EXAMPLE_ADMIN"})
		r.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
	}
	if ran := r.run(r.connectorOn(hook, nil)); ran != 0 || !hook.fired || refusing.refused == 0 {
		t.Fatalf("ran %d, the reconnect landed: %v, and %d queueing(s) were refused; want the job failed on the backfills", ran, hook.fired, refusing.refused)
	}
	job := r.job(TopicShopRedact, "")
	if job["status"] != "failed" || job["lastError"] != failReadAgain {
		t.Fatalf("the job is %v: %q, want it failed saying the backfills could not be queued", job["status"], job["lastError"])
	}

	queued := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
	r.at = r.at.Add(time.Hour)
	if ran := r.run(r.connectorOn(queued, nil)); ran != 1 {
		t.Fatalf("the next run ran %d (%v)", ran, r.job(TopicShopRedact, "")["lastError"])
	}
	if len(queued.calls) == 0 {
		t.Error("run again, the job queued no backfill: nothing reads again what the purge removed from a store that had come back")
	}
	job = r.job(TopicShopRedact, "")
	if job["status"] != "done" || !strings.Contains(fmt.Sprint(job["outcome"]), "reachable again") || !strings.Contains(fmt.Sprint(job["outcome"]), "queued") {
		t.Errorf("the job is %v: %v, want it done, the store not purged and its backfills queued", job["status"], job["outcome"])
	}
	fresh, _, _ := r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if fresh.RedactedAt != "" || !fresh.Ingests() {
		t.Errorf("the store reads redactedAt %q, ingests %v; want it connected", fresh.RedactedAt, fresh.Ingests())
	}
}

// What an overtaken purge owes the store must not hang on the sentence its
// failure left: any later failure writes another over it. The purge is
// overtaken and cannot queue the backfills; the next run cannot ask the origin
// at all; the run after that finds the store answering. That run used to end
// "skipped: the store is reachable again" with nothing queued, and the
// reconnected store's mirror stayed empty with nothing saying so. A purge that
// is skipped because the store answers queues the store's unstarted backfills
// every time, and says how many.
func TestOwedBackfillsAreQueuedAfterAnInterveningFailure(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.uninstalledWithAProduct("gid://shopify/Product/3591")
	refusing := &refusingEngine{IntegrationEngineAccess: r.narrowed(), prefix: "builtin datasyncQueueBackfill("}
	hook := &afterReadEngine{IntegrationEngineAccess: refusing}
	hook.match = func(q string) bool { return strings.HasPrefix(q, "storeById(") }
	hook.fire = func() {
		r.mutate("markStoreReconnected", map[string]any{"storeId": r.store, "status": StatusConfigured})
		r.mutate("updateStore", map[string]any{"storeId": r.store, "adminTokenRef": "EXAMPLE_ADMIN"})
		r.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
	}
	if ran := r.run(r.connectorOn(hook, nil)); ran != 0 || refusing.refused == 0 {
		t.Fatalf("the overtaken purge: ran %d with %d queueing(s) refused, want it failed on the backfills", ran, refusing.refused)
	}

	// The origin cannot be asked: another failure, and another sentence.
	r.admin.httpStatus("ShopifyReachable", http.StatusServiceUnavailable)
	r.at = r.at.Add(time.Hour)
	if ran := r.run(r.connectorOn(r.narrowed(), nil)); ran != 0 {
		t.Fatalf("a run on which the origin answers 503 ran %d", ran)
	}
	if job := r.job(TopicShopRedact, ""); job["status"] != "failed" || job["lastError"] != failReach {
		t.Fatalf("the job is %v: %q, want it failed for want of the origin's answer", job["status"], job["lastError"])
	}

	// The origin answers.
	r.admin.httpStatus("ShopifyReachable", 0)
	queued := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
	r.at = r.at.Add(time.Hour)
	if ran := r.run(r.connectorOn(queued, nil)); ran != 1 {
		t.Fatalf("the run on which the store answers ran %d (%v)", ran, r.job(TopicShopRedact, "")["lastError"])
	}
	if len(queued.calls) == 0 {
		t.Error("the store answers and its purge is skipped, with no backfill queued: nothing reads again what the earlier purge removed")
	}
	job := r.job(TopicShopRedact, "")
	said := fmt.Sprintf("queued %d backfill(s)", len(queued.calls))
	if job["status"] != "done" || !strings.Contains(fmt.Sprint(job["outcome"]), "skipped: the store is reachable again") || !strings.Contains(fmt.Sprint(job["outcome"]), said) {
		t.Errorf("the job is %v: %v, want it done, the purge skipped, saying it %s", job["status"], job["outcome"], said)
	}

	// A purge skipped again, with every backfill started, queues nothing and
	// says nothing of it.
	for _, call := range queued.calls {
		concept := call[strings.Index(call, `conceptId: "`)+len(`conceptId: "`):]
		concept = concept[:strings.Index(concept, `"`)]
		r.mutate("upsertSyncState", map[string]any{
			"stateId": memqlsync.StateID(concept, ConnectorName, string(memqlsync.DirectionInbound), r.store), "conceptId": concept, "connector": ConnectorName,
			"direction": "inbound", "partitionKey": r.store, "backfillStatus": "queued",
		})
	}
	r.mutate("reopenComplianceJob", map[string]any{"jobId": ComplianceJobID(r.store, TopicShopRedact, "\x00"), "dueAt": r.at.Format(time.RFC3339)})
	again := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
	r.at = r.at.Add(time.Hour)
	if ran := r.run(r.connectorOn(again, nil)); ran != 1 {
		t.Fatalf("the skip made again ran %d (%v)", ran, r.job(TopicShopRedact, "")["lastError"])
	}
	if job := r.job(TopicShopRedact, ""); len(again.calls) != 0 || job["outcome"] != "skipped: the store is reachable again" {
		t.Errorf("with every backfill started the skip queued %d and says %q, want none queued and the sentence alone", len(again.calls), job["outcome"])
	}
}

// The store's row can move between the read and the stamp without the store
// having been connected again: the uninstall's own notice arrives late, or the
// watch writes another key of the row's health. Neither is a reconnect, and a
// purge that took one for a reconnect would leave an uninstalled store without
// its mark and without its second purge. And a reconnect can leave every
// field of the row as it was: a store whose uninstall notice was missed keeps
// its grant's name when it is connected again. So a row that moved and shows
// no connect, and records no uninstall, is asked of the origin as the purge
// was: it answers, the store is connected; it refuses, the stamp is made on
// the row as it is; it does not say, the job fails and is run again.
func TestARowThatMovedIsAReconnectOnlyWhenTheStoreIsConnected(t *testing.T) {
	health := func(r *runnerFixture) {
		r.mutate("recordStoreHealth", map[string]any{"storeId": r.store, "health": map[string]any{"verdict": "needs attention"}})
	}
	for name, tc := range map[string]struct {
		uninstalled bool
		lands       func(r *runnerFixture)
		stamped     bool
		status      string
		says        string
		asked       int
	}{
		"the uninstall's own notice lands during the purge": {lands: func(r *runnerFixture) {
			r.mutate("markStoreUninstalled", map[string]any{"storeId": r.store, "uninstalledAt": r.at.Format(time.RFC3339)})
		}, stamped: true, status: "pending", says: "first pass: purged", asked: 1},
		"another key of an uninstalled store's health is written": {uninstalled: true, lands: health,
			stamped: true, status: "pending", says: "first pass: purged", asked: 0},
		"another key of the health of a store the origin refuses is written": {lands: health,
			stamped: true, status: "pending", says: "first pass: purged", asked: 2},
		"a reconnect leaves the row's fields as they were": {lands: func(r *runnerFixture) {
			r.mutate("updateStore", map[string]any{"storeId": r.store, "adminTokenRef": "EXAMPLE_ADMIN", "scopesGranted": []string{"read_orders", "read_customers"}})
			r.admin.httpStatus("ShopifyReachable", 0)
			r.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
		}, stamped: false, status: "done", says: "connected again", asked: 2},
		"the row moved and the origin does not say": {lands: func(r *runnerFixture) {
			health(r)
			r.admin.httpStatus("ShopifyReachable", http.StatusServiceUnavailable)
		}, stamped: false, status: "failed", says: failStamp, asked: -1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRunnerFixture(t)
			if r == nil {
				return
			}
			if tc.uninstalled {
				r.uninstalledWithAProduct("gid://shopify/Product/3551")
			} else {
				r.hasStore()
				r.write("product", map[string]any{"id": "gid://shopify/Product/3551", "updatedAt": "2026-10-01T10:00:00Z", "title": "Road bicycle"}, seen)
				r.queue(TopicShopRedact, "", nil, r.at.Add(-time.Minute))
				r.admin.httpStatus("ShopifyReachable", http.StatusUnauthorized)
			}
			queued := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
			hook := &hookEngine{IntegrationEngineAccess: queued}
			hook.match = func(q string) bool { return strings.HasPrefix(q, "markStoreRedacted(") }
			hook.fire = func() { tc.lands(r) }
			conn := r.connectorOn(hook, nil)
			r.run(conn)
			if !hook.fired {
				t.Fatalf("the row was never moved: the purge did not reach its stamp (%v)", r.job(TopicShopRedact, "")["lastError"])
			}
			if left := r.mirrorKeys(); len(left) != 0 {
				t.Errorf("the purge left %v", left)
			}
			fresh, _, _ := conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
			if got := fresh.RedactedAt != ""; got != tc.stamped {
				t.Errorf("the store reads redactedAt %q, uninstalledAt %q, status %q; stamped redacted: want %v", fresh.RedactedAt, fresh.UninstalledAt, fresh.Status, tc.stamped)
			}
			job := r.job(TopicShopRedact, "")
			if job["status"] != tc.status || !strings.Contains(fmt.Sprint(job["outcome"])+fmt.Sprint(job["lastError"]), tc.says) {
				t.Errorf("the job is %v: outcome %q, lastError %q; want it %s, saying %q", job["status"], job["outcome"], job["lastError"], tc.status, tc.says)
			}
			if got := len(queued.calls) > 0; got != (tc.status == "done") {
				t.Errorf("backfills queued: %v; a store that was not connected again has none to read", queued.calls)
			}
			if got := r.admin.countOp("ShopifyReachable"); tc.asked >= 0 && got != tc.asked {
				t.Errorf("the origin was asked %d time(s), want %d", got, tc.asked)
			}
		})
	}
}

// The session of a run can be lost between two jobs: the server ends one
// that has been idle past its limit. It was opened once and held, so every
// later job of the run failed. It is checked before a job uses it and opened
// again if it is gone.
func TestASessionLostBetweenTwoJobsIsOpenedAgain(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	for i, n := range []string{"3601", "3602", "3603"} {
		r.write("customer", adaCustomer(n, "2026-10-01T10:00:00Z"), seen)
		r.queue(TopicRedact, eraCustomerGID(n), nil, r.at.Add(-time.Duration(3-i)*time.Hour))
	}
	opened, pid := 0, 0
	session := func(ctx context.Context) (*sql.Conn, func(), error) {
		conn, done, err := privacySessionOf(r.db)(ctx)
		if err == nil {
			opened++
			_ = conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid)
		}
		return conn, done, err
	}
	hook := &hookEngine{IntegrationEngineAccess: r.narrowed()}
	hook.match = func(q string) bool { return strings.HasPrefix(q, "recordComplianceJob(") }
	hook.fire = func() {
		// What the server does to a session idle past its limit.
		var ended bool
		if err := r.db.QueryRowContext(context.Background(), `SELECT pg_terminate_backend($1)`, pid).Scan(&ended); err != nil || !ended {
			t.Fatalf("could not end the session's backend %d: %v %v", pid, ended, err)
		}
	}
	conn := r.connectorOn(hook, nil).WithMaintenanceSession(session)
	if ran := r.run(conn); ran != 3 || !hook.fired {
		t.Errorf("ran %d of the three jobs after the session was ended behind the first (%v)", ran, hook.fired)
	}
	if opened != 2 {
		t.Errorf("the run opened %d session(s), want the first and the one that replaced it", opened)
	}
	for _, n := range []string{"3602", "3603"} {
		if job := r.job(TopicRedact, eraCustomerGID(n)); job["status"] != "pending" || noFirstPass(job) {
			t.Errorf("the job of customer %s is %v: %q", n, job["status"], job["lastError"])
		}
	}
}

// When a privacy job ends done, the deliveries that asked for it are DELETED
// (ruling 96); at each pass before that they are blanked. The receiver names
// a delivery by its signed body, and the dispatcher skips a row that is
// already processed: a delivery that stayed, blanked, made the same request
// arriving a second time land on it and never reach the connector.
func TestTheDeliveriesThatAskedAreDeletedWhenTheJobEndsDone(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	r.withTheLibrary()
	customer := eraCustomerGID("3701")
	r.write("customer", adaCustomer("3701", "2026-10-01T10:00:00Z"), seen)
	ids := r.onTheAppSource("redact", "data-request", "shop-redact")
	redact, request, shop := ids[0], ids[1], ids[2]

	// A data request: its delivery goes when it ends done.
	r.deliversAs(r.conn, ConnectorName, request, TopicDataRequest, "3701")
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the data request: ran %d", ran)
	}
	if left := r.deliveryBodies(request); len(left) != 0 {
		t.Errorf("after the data request ended done its delivery is still a row, of %d version(s)", len(left))
	}

	// A customer's erasure: blanked at the first pass, deleted at the second.
	r.deliversAs(r.conn, ConnectorName, redact, TopicRedact, "3701")
	r.at = r.at.Add(25 * time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the first pass: ran %d (%v)", ran, r.job(TopicRedact, customer)["lastError"])
	}
	if left := r.deliveryBodies(redact); len(left) == 0 || strings.Join(left, "") != "" {
		t.Errorf("after the first pass the delivery that asked is %v, want it there and blank: the job is not done", left)
	}
	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the second pass: ran %d", ran)
	}
	if left := r.deliveryBodies(redact); len(left) != 0 {
		t.Errorf("after the erasure ended done its delivery is still a row, of %d version(s)", len(left))
	}

	// A shop's erasure that ends because the store came back. Skipped, it
	// queues the store's unstarted backfills.
	r.withTheSyncRuntime()
	r.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
	r.deliversAs(r.conn, ConnectorName, shop, TopicShopRedact, "")
	r.at = r.at.Add(ShopRedactHold + time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the shop's erasure: ran %d", ran)
	}
	if left := r.deliveryBodies(shop); len(left) != 0 {
		t.Errorf("after the shop's erasure ended done its delivery is still a row, of %d version(s)", len(left))
	}
}

// A data request names its own request, so the same words a second time are
// the same request, and it is not answered twice. But once its job is done its
// delivery is deleted, and the same request delivered again -- the origin did
// not hear the first answer -- is then a new row that holds the customer's
// email and phone, for a job that used to be left as it was: the row stayed
// for good. The job is opened again; run again it does not file its export a
// second time, and it deletes the delivery.
func TestADataRequestDeliveredAgainAfterItWasAnsweredLeavesNoDelivery(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	r.withTheLibrary()
	customer := eraCustomerGID("3971")
	r.write("customer", adaCustomer("3971", "2026-10-01T10:00:00Z"), seen)
	request := r.onTheAppSource("data-request-again")[0]
	r.deliversAs(r.conn, ConnectorName, request, TopicDataRequest, "3971")
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the data request: ran %d", ran)
	}
	first := r.job(TopicDataRequest, customer)
	export, _ := first["exportId"].(string)
	if first["status"] != "done" || export == "" || len(r.deliveryBodies(request)) != 0 {
		t.Fatalf("the data request is %v, filed %q and left %d version(s) of its delivery", first["status"], export, len(r.deliveryBodies(request)))
	}
	filed := r.libraryRowsOf(export)

	// The same request, delivered again: the same words, so the same row id.
	r.at = r.at.Add(3 * time.Hour)
	r.deliversAs(r.conn, ConnectorName, request, TopicDataRequest, "3971")
	if held := strings.Join(r.deliveryBodies(request), ""); !strings.Contains(held, adaEmail) {
		t.Fatalf("the redelivery's row does not hold the customer's address: the fixture proves nothing (%q)", held)
	}
	if job := r.job(TopicDataRequest, customer); job["status"] != "pending" {
		t.Fatalf("a data request delivered again after it was answered left its job %v, want it opened again: nothing else deletes the delivery", job["status"])
	}
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the job opened again: ran %d (%v)", ran, r.job(TopicDataRequest, customer)["lastError"])
	}
	job := r.job(TopicDataRequest, customer)
	if job["status"] != "done" || job["exportId"] != export || !strings.Contains(fmt.Sprint(job["outcome"]), "answered before") {
		t.Errorf("the job is %v, names the export %v and says %q; want it done, the export it filed, and that it was answered before", job["status"], job["exportId"], job["outcome"])
	}
	if left := r.deliveryBodies(request); len(left) != 0 {
		t.Errorf("the redelivery of an answered data request is still a row, of %d version(s)", len(left))
	}
	if now := r.libraryRowsOf(export); strings.Join(now, "\n") != strings.Join(filed, "\n") {
		t.Errorf("the export was filed a second time: the Library held %d row version(s) of it and holds %d", len(filed), len(now))
	}
}

// The same request delivered again in the moment after its job deleted the
// first delivery and before the job was recorded done. The request is recorded
// on the job's row, so the pass's own write is refused; the job is opened
// again instead of ended, and its next run deletes the delivery.
func TestADataRequestDeliveredAgainWhileItIsAnsweredLeavesNoDelivery(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	r.withTheLibrary()
	customer := eraCustomerGID("3981")
	r.write("customer", adaCustomer("3981", "2026-10-01T10:00:00Z"), seen)
	request := r.onTheAppSource("data-request-meanwhile")[0]
	r.deliversAs(r.conn, ConnectorName, request, TopicDataRequest, "3981")
	hook := &hookEngine{IntegrationEngineAccess: r.narrowed()}
	hook.match = func(q string) bool { return strings.HasPrefix(q, "recordComplianceJob(") }
	hook.fire = func() { r.deliversAs(r.conn, ConnectorName, request, TopicDataRequest, "3981") }
	if ran := r.run(r.connectorOn(hook, nil)); ran != 1 || !hook.fired {
		t.Fatalf("ran %d, and the request was delivered again before the job was recorded: %v", ran, hook.fired)
	}
	opened := r.job(TopicDataRequest, customer)
	if opened["status"] != "pending" {
		t.Fatalf("the job is %v after the same request reached it while it ran, want it opened again: its row holds a delivery nothing else deletes", opened["status"])
	}
	// THE JOB KEEPS THE EXPORT IT FILED across the reopening. Without it the
	// run that follows found none and filed the export a second time; and if
	// that landed after the customer's erasure had made both its passes, the
	// second export stayed in the owner's Library.
	export, _ := opened["exportId"].(string)
	if export == "" || export != exportIDOf(r.store, ComplianceJob{CustomerGID: customer, RequestID: "42423981"}) {
		t.Fatalf("the job opened again names the export %v, want the one its pass filed", opened["exportId"])
	}
	filed := r.libraryRowsOf(export)
	if len(filed) != 1 {
		t.Fatalf("the pass filed %v, want the one export", filed)
	}
	r.at = r.at.Add(time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the next run ran %d (%v)", ran, r.job(TopicDataRequest, customer)["lastError"])
	}
	if job := r.job(TopicDataRequest, customer); job["status"] != "done" || job["exportId"] != export {
		t.Errorf("the job is %v and names the export %v: %v", job["status"], job["exportId"], job["lastError"])
	}
	if left := r.deliveryBodies(request); len(left) != 0 {
		t.Errorf("the delivery that arrived while the request was answered is still a row, of %d version(s)", len(left))
	}
	if now := r.libraryRowsOf(export); strings.Join(now, "\n") != strings.Join(filed, "\n") {
		t.Errorf("the run that followed the reopening filed the export again: the Library held %d row version(s) of it and holds %d", len(filed), len(now))
	}
}

// A data request whose customer has been erased files nothing, and removes
// what it finds. The erasure's own removal of her exports runs on both its
// passes, which covers an export filed before them; what it cannot cover is a
// data request that runs after them. Three ways one does: it was answered
// before her erasure and is delivered again; it runs for the first time when
// her erasure is done; and an export of it is in the Library when it runs.
func TestADataRequestForACustomerWhoHasBeenErasedFilesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		answeredBefore bool
		planted        bool
		betweenPasses  bool
	}{
		"answered before her erasure, and delivered again": {answeredBefore: true},
		"run for the first time after her erasure":         {},
		"run between the two passes of her erasure":        {betweenPasses: true},
		"an export of it is there when it runs":            {answeredBefore: true, planted: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRunnerFixture(t)
			if r == nil {
				return
			}
			store := r.hasStore()
			store.OwnerUserID = "dbtest-owner"
			r.withTheLibrary()
			const n = "3991"
			customer := eraCustomerGID(n)
			r.write("customer", adaCustomer(n, "2026-10-01T10:00:00Z"), seen)
			request := r.onTheAppSource("data-request-erased")[0]
			asked := ComplianceJob{StoreID: r.store, Topic: TopicDataRequest, CustomerGID: customer, RequestID: "4242" + n}
			export := exportIDOf(r.store, asked)
			if tc.answeredBefore {
				r.deliversAs(r.conn, ConnectorName, request, TopicDataRequest, n)
				if ran := r.run(r.conn); ran != 1 || len(r.libraryRowsOf(export)) == 0 {
					t.Fatalf("the data request: ran %d and filed %v", ran, r.libraryRowsOf(export))
				}
			}

			// Her erasure: both passes, or the first alone.
			r.queue(TopicRedact, customer, nil, r.at.Add(-time.Minute))
			if ran := r.run(r.conn); ran != 1 {
				t.Fatalf("the erasure's first pass: ran %d (%v)", ran, r.job(TopicRedact, customer)["lastError"])
			}
			if !tc.betweenPasses {
				r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
				if ran := r.run(r.conn); ran != 1 || r.job(TopicRedact, customer)["status"] != "done" {
					t.Fatalf("the erasure's second pass: ran %d, and the erasure is %v", ran, r.job(TopicRedact, customer)["status"])
				}
			}
			if left := r.libraryRowsOf(export); len(left) != 0 {
				t.Fatalf("her erasure left the export her data request filed: %v", left)
			}
			if tc.planted {
				// An export of the request is in the Library all the same, as a
				// filing that landed after the erasure's last pass left one.
				if err := r.conn.writeExportArtifact(context.Background(), store, asked, []byte(`{"rows":{}}`)); err != nil {
					t.Fatalf("plant the export: %v", err)
				}
				if len(r.libraryRowsOf(export)) == 0 {
					t.Fatal("the fixture planted no export")
				}
			}

			// The data request runs now: delivered again, or for the first
			// time. (Between the passes the clock stays inside the hour, so
			// the erasure's second pass is not due and does not run with it.)
			if !tc.betweenPasses {
				r.at = r.at.Add(time.Hour)
			}
			r.deliversAs(r.conn, ConnectorName, request, TopicDataRequest, n)
			if ran := r.run(r.conn); ran != 1 {
				t.Fatalf("the data request after her erasure: ran %d (%v)", ran, r.job(TopicDataRequest, customer)["lastError"])
			}
			job := r.job(TopicDataRequest, customer)
			if job["status"] != "done" || !strings.Contains(fmt.Sprint(job["outcome"]), "erased") {
				t.Errorf("the data request is %v: %v, want it done, saying the customer has been erased", job["status"], job["outcome"])
			}
			if left := r.libraryRowsOf(export); len(left) != 0 {
				t.Errorf("a data request for a customer who has been erased left an export in the Library: %v", left)
			}
			if left := r.deliveryBodies(request); len(left) != 0 {
				t.Errorf("its delivery is still a row, of %d version(s)", len(left))
			}
		})
	}
}

// The delivery that carried an app/uninstalled is remembered on the store and
// deleted when the store is connected again, so that the same notice arriving
// after a second uninstall is a new delivery. A shop's details do not change
// between two uninstalls, and the second notice has the first one's body.
func TestTheUninstallsDeliveryIsRememberedAndDeletedAtReconnect(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	store := r.hasStore()
	notice := r.onTheAppSource("uninstalled")[0]
	body := `{"id":548380009,"myshopify_domain":"` + store.Domain + `","name":"Example Bicycles"}`
	r.mutate("stageInboundRequest", map[string]any{
		"requestId": notice, "source": ConnectorName, "medium": "webhook", "body": body, "contentType": "application/json",
		"headersJson": `{"x-shopify-topic":"app/uninstalled"}`, "signatureVerified": true, "verifiedBy": "connector", "receivedAt": r.at.Format(time.RFC3339),
	})
	r.admin.httpStatus("ShopifyReachable", http.StatusUnauthorized)
	current, _, _ := r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if err := r.conn.applyUninstall(context.Background(), current, memqlsync.InboundRequest{RequestId: notice, Source: ConnectorName, Topic: TopicAppUninstalled, Body: []byte(body), ReceivedAt: r.at}); err != nil {
		t.Fatalf("the uninstall: %v", err)
	}
	prior, _, _ := r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if prior.UninstalledAt == "" || fmt.Sprint(uninstallDeliveriesOf(prior)) != fmt.Sprint([]string{notice}) {
		t.Fatalf("the uninstalled store reads uninstalledAt %q and remembers the deliveries %v, want %q", prior.UninstalledAt, uninstallDeliveriesOf(prior), notice)
	}
	if left := r.deliveryBodies(notice); len(left) == 0 {
		t.Fatal("the uninstall's delivery was removed while the store is still uninstalled")
	}
	// The same uninstall told a second time in other words (the shop's
	// details as they were a moment later) is another delivery, and is
	// remembered with the first, not in its place.
	other := r.onTheAppSource("uninstalled-again")[0]
	otherBody := `{"id":548380009,"myshopify_domain":"` + store.Domain + `","name":"Example Bicycles","updated_at":"2026-11-02T09:05:01Z"}`
	r.mutate("stageInboundRequest", map[string]any{
		"requestId": other, "source": ConnectorName, "medium": "webhook", "body": otherBody, "contentType": "application/json",
		"headersJson": `{"x-shopify-topic":"app/uninstalled"}`, "signatureVerified": true, "verifiedBy": "connector", "receivedAt": r.at.Format(time.RFC3339),
	})
	if err := r.conn.applyUninstall(context.Background(), prior, memqlsync.InboundRequest{RequestId: other, Source: ConnectorName, Topic: TopicAppUninstalled, Body: []byte(otherBody), ReceivedAt: r.at}); err != nil {
		t.Fatalf("the uninstall, told again: %v", err)
	}
	prior, _, _ = r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if got := uninstallDeliveriesOf(prior); fmt.Sprint(got) != fmt.Sprint([]string{notice, other}) {
		t.Fatalf("the store remembers the deliveries %v, want both that carried its uninstall", got)
	}

	// The owner connects the store again.
	if err := r.conn.markReconnected(context.Background(), operatorContext(context.Background()), r.store, prior, "dbtest-owner"); err != nil {
		t.Fatalf("the reconnect: %v", err)
	}
	for _, id := range []string{notice, other} {
		if left := r.deliveryBodies(id); len(left) != 0 {
			t.Errorf("after the reconnect the uninstall's delivery %s is still a row, of %d version(s): the same notice a second time would land on it", id, len(left))
		}
	}
	again, _, _ := r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if again.UninstalledAt != "" || len(uninstallDeliveriesOf(again)) != 0 {
		t.Errorf("the reconnected store reads uninstalledAt %q and still remembers %v", again.UninstalledAt, uninstallDeliveriesOf(again))
	}

	// Uninstalled a second time: the notice has the first one's body, so the
	// receiver gives it the first one's row id. The row is gone, so it is a
	// new delivery, waiting to be worked; and worked, it disconnects the store
	// again.
	r.at = r.at.Add(30 * 24 * time.Hour)
	r.mutate("stageInboundRequest", map[string]any{
		"requestId": notice, "source": ConnectorName, "medium": "webhook", "body": body, "contentType": "application/json",
		"headersJson": `{"x-shopify-topic":"app/uninstalled"}`, "signatureVerified": true, "verifiedBy": "connector", "receivedAt": r.at.Format(time.RFC3339),
	})
	var status string
	if err := r.db.QueryRowContext(context.Background(), `SELECT payload->>'status' FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND id = $1 ORDER BY "createdAt" DESC LIMIT 1`,
		"v1:platform:inboundRequest:"+notice).Scan(&status); err != nil || status != "received" {
		t.Fatalf("the second identical uninstall is staged as %q (%v), want a new delivery waiting to be worked: the dispatcher skips any other", status, err)
	}
	if err := r.conn.applyUninstall(context.Background(), again, memqlsync.InboundRequest{RequestId: notice, Source: ConnectorName, Topic: TopicAppUninstalled, Body: []byte(body), ReceivedAt: r.at}); err != nil {
		t.Fatalf("the second uninstall: %v", err)
	}
	if now, _, _ := r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store); now.UninstalledAt == "" {
		t.Error("the second identical uninstall did not disconnect the store again")
	}
}

// The deliveries a store's row remembers as having carried its uninstall are
// deleted at its reconnect and at its purge. What an id there names is not
// taken on trust: the row's health is written by the connector, and nothing
// on the row can show that an id in it was put there by the uninstall's
// handler. So that path deletes a delivery only if it is one of this
// connector's, for this store, and an uninstall: on the store's own source,
// or on the app-level source naming the shop. Anything else an id names is
// left: another connector's delivery, a delivery of the store's that is not
// an uninstall, another shop's uninstall.
func TestTheRememberedIdsDeleteOnlyTheStoresOwnUninstallDeliveries(t *testing.T) {
	for name, purged := range map[string]bool{"at the reconnect": false, "at the purge": true} {
		t.Run(name, func(t *testing.T) {
			r := newRunnerFixture(t)
			if r == nil {
				return
			}
			store := r.hasStore()
			own, another := storeInboundSource(r.store), "dbtest-another-connector"
			t.Cleanup(func() {
				_, _ = r.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'source' IN ($1, $2)`, own, another)
			})
			ids := r.onTheAppSource("the-uninstall", "not-an-uninstall", "another-shops-uninstall")
			tag := strings.TrimPrefix(r.store, "dbtest-era-")
			stage := func(source, id, topic, domain string) {
				t.Helper()
				r.mutate("stageInboundRequest", map[string]any{
					"requestId": id, "source": source, "medium": "webhook", "body": `{"id":548380009,"myshopify_domain":"` + domain + `","name":"Example Bicycles"}`, "contentType": "application/json",
					"headersJson": `{"x-shopify-shop-domain":"` + domain + `","x-shopify-topic":"` + topic + `"}`, "signatureVerified": true, "verifiedBy": "connector", "receivedAt": r.at.Format(time.RFC3339),
				})
				r.mutate("updateInboundRequestStatus", map[string]any{"requestId": id, "status": "processed", "attempts": 1})
			}
			// The uninstall, through its own handler: its delivery is remembered.
			body := `{"id":548380009,"myshopify_domain":"` + store.Domain + `","name":"Example Bicycles"}`
			r.mutate("stageInboundRequest", map[string]any{
				"requestId": ids[0], "source": ConnectorName, "medium": "webhook", "body": body, "contentType": "application/json",
				"headersJson": `{"x-shopify-topic":"app/uninstalled"}`, "signatureVerified": true, "verifiedBy": "connector", "receivedAt": r.at.Format(time.RFC3339),
			})
			r.admin.httpStatus("ShopifyReachable", http.StatusUnauthorized)
			current, _, _ := r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
			if err := r.conn.applyUninstall(context.Background(), current, memqlsync.InboundRequest{RequestId: ids[0], Source: ConnectorName, Topic: TopicAppUninstalled, Body: []byte(body), ReceivedAt: r.at}); err != nil {
				t.Fatalf("the uninstall: %v", err)
			}
			// What a write of the row's health that is not the connector's
			// could add to what it remembers.
			planted := map[string]string{
				"another connector's delivery":                   tag + "-another-connectors",
				"a delivery of the store's that is no uninstall": tag + "-own-order",
				"an app-level delivery that is no uninstall":     ids[1],
				"another shop's uninstall":                       ids[2],
			}
			stage(another, planted["another connector's delivery"], TopicAppUninstalled, store.Domain)
			stage(own, planted["a delivery of the store's that is no uninstall"], "orders/create", store.Domain)
			stage(ConnectorName, ids[1], "shop/update", store.Domain)
			stage(ConnectorName, ids[2], TopicAppUninstalled, "another-shop.example.com")
			remembered := []string{ids[0]}
			for _, id := range planted {
				remembered = append(remembered, id)
			}
			r.mutate("recordStoreHealth", map[string]any{"storeId": r.store, "health": map[string]any{healthUninstallDeliveries: remembered}})
			prior, _, _ := r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
			if got := uninstallDeliveriesOf(prior); len(got) != len(remembered) {
				t.Fatalf("the store's row remembers %v, want the %d ids the fixture put there", got, len(remembered))
			}

			if purged {
				r.queue(TopicShopRedact, "", nil, r.at.Add(-time.Minute))
				if ran := r.run(r.conn); ran != 1 {
					t.Fatalf("the purge: ran %d (%v)", ran, r.job(TopicShopRedact, "")["lastError"])
				}
				// The purge deletes every delivery of the store's own source
				// with its mirror, whatever it is: that one is not this path's.
				delete(planted, "a delivery of the store's that is no uninstall")
			} else if err := r.conn.markReconnected(context.Background(), operatorContext(context.Background()), r.store, prior, "dbtest-owner"); err != nil {
				t.Fatalf("the reconnect: %v", err)
			}
			if left := r.deliveryBodies(ids[0]); len(left) != 0 {
				t.Errorf("the delivery that carried the store's uninstall is still a row, of %d version(s)", len(left))
			}
			for what, id := range planted {
				if left := r.deliveryBodies(id); len(left) == 0 {
					t.Errorf("%s was deleted because the store's row named its id", what)
				}
			}
		})
	}
}

// A store's health is the connector's to write and nobody else's: it holds
// what the connector decides on. The mutation is server-only, so a cluster
// owner who calls it as a client is refused and the row is as it was; the
// connector's own write of a key goes through.
func TestAStoresHealthIsWrittenByTheConnectorAlone(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	planted := renderCall("recordStoreHealth", map[string]any{"storeId": r.store, "health": map[string]any{
		healthUninstallDeliveries: []string{"somebody-elses-delivery"}, healthOldErasureRepaired: "2026-01-01T00:00:00Z",
	}})
	if _, err := r.eng.Execute(auth.ContextWithClientOrigin(ownerCtx()), planted); err == nil || !strings.Contains(err.Error(), "server-only") {
		t.Errorf("a cluster owner calling recordStoreHealth as a client answered %v, want it refused as server-only", err)
	}
	store, _, _ := r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if len(uninstallDeliveriesOf(store)) != 0 || mapString(store.Health, healthOldErasureRepaired) != "" {
		t.Errorf("the store's health holds %v after a refused write", store.Health)
	}
	if err := r.conn.recordStoreHealthKey(context.Background(), r.store, healthOldErasureRepaired, r.at.Format(time.RFC3339), nil); err != nil {
		t.Fatalf("the connector's own write of a health key: %v", err)
	}
	store, _, _ = r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if mapString(store.Health, healthOldErasureRepaired) == "" {
		t.Error("the connector's own write of a health key did not land")
	}
}

// An uninstall notice a reinstall superseded changes nothing of the store, and
// its delivery is remembered all the same: it too is a row the same notice
// would land on after a later uninstall, and remembered it goes at the next
// reconnect or purge.
func TestASupersededUninstallsDeliveryIsRememberedToo(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	store := r.hasStore()
	notice := r.onTheAppSource("superseded")[0]
	body := `{"id":548380009,"myshopify_domain":"` + store.Domain + `","name":"Example Bicycles"}`
	r.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
	current, _, _ := r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if err := r.conn.applyUninstall(context.Background(), current, memqlsync.InboundRequest{RequestId: notice, Source: ConnectorName, Topic: TopicAppUninstalled, Body: []byte(body), ReceivedAt: r.at}); err != nil {
		t.Fatalf("the uninstall: %v", err)
	}
	now, _, _ := r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if now.UninstalledAt != "" {
		t.Fatalf("a notice the store's grant still answers for disconnected it: uninstalledAt %q", now.UninstalledAt)
	}
	if got := uninstallDeliveriesOf(now); fmt.Sprint(got) != fmt.Sprint([]string{notice}) {
		t.Errorf("the store remembers the deliveries %v of a superseded uninstall, want %q", got, notice)
	}
}

// The deliveries of privacy requests and uninstalls from before a job
// remembered them are deleted once a store: the processed ones of that shop
// whose topic is a privacy topic or the uninstall, on the store's own source
// and on the app-level one, where the shop is the one the delivery's own
// headers record. Another shop's, another topic's and one not processed stay.
func TestTheOlderPrivacyDeliveriesOfAShopAreDeletedOnce(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	shop := r.hasStore()
	own := storeInboundSource(r.store)
	t.Cleanup(func() {
		_, _ = r.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'source' = $1`, own)
	})
	ids := r.onTheAppSource("old-shop-redact", "old-redact", "old-uninstall", "theirs", "held", "old-order", "waiting", "remembered-uninstall",
		"bare-redact", "bare-uninstall", "bare-theirs")
	stage := func(source, id, topic, domain, status string) {
		t.Helper()
		r.mutate("stageInboundRequest", map[string]any{
			"requestId": id, "source": source, "medium": "webhook", "body": `{"shop_domain":"` + domain + `"}`, "contentType": "application/json",
			"headersJson": `{"x-shopify-shop-domain":"` + domain + `","x-shopify-topic":"` + topic + `"}`, "signatureVerified": true, "verifiedBy": "connector", "receivedAt": r.at.Add(-90 * 24 * time.Hour).Format(time.RFC3339),
		})
		if status != "received" {
			r.mutate("updateInboundRequestStatus", map[string]any{"requestId": id, "status": status, "attempts": 1})
		}
	}
	stage(ConnectorName, ids[0], TopicShopRedact, shop.Domain, "processed")
	stage(ConnectorName, ids[1], TopicRedact, shop.Domain, "processed")
	stage(ConnectorName, ids[2], TopicAppUninstalled, shop.Domain, "processed")
	stage(ConnectorName, ids[3], TopicShopRedact, "another-shop.example.com", "processed")
	stage(ConnectorName, ids[4], TopicRedact, shop.Domain, "failed")
	tag := strings.TrimPrefix(r.store, "dbtest-era-")
	stage(own, tag+"-own-redact", TopicRedact, shop.Domain, "processed")
	stage(own, tag+"-own-order", "orders/create", shop.Domain, "failed")
	// What something still remembers is not "older": the delivery of another
	// customer's erasure that is waiting for its hold, which goes when that
	// job ends, and the delivery of the store's uninstall, which goes when the
	// store is connected again or purged.
	stage(ConnectorName, ids[6], TopicRedact, shop.Domain, "processed")
	r.mutate("queueComplianceJob", map[string]any{
		"jobId": ComplianceJobID(r.store, TopicRedact, eraCustomerGID("3803")+"\x00"), "storeId": r.store, "topic": TopicRedact, "customerGid": eraCustomerGID("3803"), "requestId": "",
		"shopDomain": shop.Domain, "orderGids": []string{}, "dueAt": r.at.Add(9 * 24 * time.Hour).Format(time.RFC3339), "deliveryIds": []string{ids[6]},
	})
	stage(ConnectorName, ids[7], TopicAppUninstalled, shop.Domain, "processed")
	if err := r.conn.recordStoreHealthKey(context.Background(), r.store, healthUninstallDeliveries, []string{ids[7]}, nil); err != nil {
		t.Fatalf("remember the uninstall's delivery: %v", err)
	}
	// A delivery staged without the shop's domain among its headers is the
	// shop's by the field of its signed body the connector bound it by.
	bare := func(id, topic, body string) {
		t.Helper()
		r.mutate("stageInboundRequest", map[string]any{
			"requestId": id, "source": ConnectorName, "medium": "webhook", "body": body, "contentType": "application/json",
			"headersJson": `{"x-shopify-topic":"` + topic + `"}`, "signatureVerified": true, "verifiedBy": "connector", "receivedAt": r.at.Add(-90 * 24 * time.Hour).Format(time.RFC3339),
		})
		r.mutate("updateInboundRequestStatus", map[string]any{"requestId": id, "status": "processed", "attempts": 1})
	}
	bare(ids[8], TopicRedact, `{"shop_id":1, "shop_domain": "`+strings.ToUpper(shop.Domain[:1])+shop.Domain[1:]+`","customer":{"id":3899,"email":"`+adaEmail+`"}}`)
	bare(ids[9], TopicAppUninstalled, `{"id":1,"name":"Example Bicycles","myshopify_domain":"`+shop.Domain+`"}`)
	// Another shop's, whose customer's address spells this shop's field.
	bare(ids[10], TopicRedact, `{"shop_domain":"another-shop.example.com","customer":{"id":3898,"email":"\"shop_domain\":\"`+shop.Domain+`\"@example.com"}}`)

	r.write("customer", adaCustomer("3801", "2026-10-01T10:00:00Z"), seen)
	r.queue(TopicRedact, eraCustomerGID("3801"), nil, r.at.Add(-time.Minute))
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("ran %d (%v)", ran, r.job(TopicRedact, eraCustomerGID("3801"))["lastError"])
	}
	for _, gone := range []string{ids[0], ids[1], ids[2], tag + "-own-redact", ids[8], ids[9]} {
		if left := r.deliveryBodies(gone); len(left) != 0 {
			t.Errorf("the older privacy delivery %s of the shop is still a row", gone)
		}
	}
	for _, kept := range []string{ids[3], ids[4], tag + "-own-order", ids[10]} {
		if left := r.deliveryBodies(kept); len(left) == 0 {
			t.Errorf("the delivery %s was deleted: it is another shop's, not processed, or of another topic", kept)
		}
	}
	for _, kept := range []string{ids[6], ids[7]} {
		if left := r.deliveryBodies(kept); len(left) == 0 {
			t.Errorf("the delivery %s was deleted: a job that is waiting, or the store's row, still remembers it", kept)
		}
	}
	if job := r.job(TopicRedact, eraCustomerGID("3801")); !strings.Contains(fmt.Sprint(job["outcome"]), "older") {
		t.Errorf("the outcome does not say the older deliveries went: %v", job["outcome"])
	}
	// Once: a delivery of the same kind that appears afterwards is not looked for.
	stage(ConnectorName, ids[5], TopicShopRedact, shop.Domain, "processed")
	r.write("customer", adaCustomer("3802", "2026-10-01T10:00:00Z"), seen)
	r.queue(TopicRedact, eraCustomerGID("3802"), nil, r.at.Add(-time.Minute))
	r.at = r.at.Add(2 * time.Hour)
	r.run(r.conn)
	if left := r.deliveryBodies(ids[5]); len(left) == 0 {
		t.Error("a later erasure looked for older deliveries again: the store's row says that ran")
	}
}

// A job opened again starts clean: the time and the outcome of its earlier
// completion are cleared by the reopening, and not left until the first pass
// writes.
func TestAReopenedJobDoesNotKeepItsEarlierCompletion(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("3901")
	id := r.queue(TopicRedact, customer, nil, r.at.Add(-72*time.Hour))
	r.mutate("recordComplianceJob", map[string]any{"jobId": id, "status": "done", "outcome": "second pass: scrubbed 4 row version(s)", "scrubbedAt": r.at.Add(-48 * time.Hour).Format(time.RFC3339), "completedAt": r.at.Add(-47 * time.Hour).Format(time.RFC3339)})
	r.delivers(r.conn, TopicRedact, "3901")
	job := r.job(TopicRedact, customer)
	if job["status"] != "pending" || fmt.Sprint(job["outcome"]) != "" || (job["completedAt"] != nil && job["completedAt"] != "") {
		t.Errorf("the reopened job is %v with outcome %q and completedAt %v, want it waiting with neither", job["status"], job["outcome"], job["completedAt"])
	}
}

// A pass that keeps failing on the exports or the deliveries still restarts
// the store's backfills in flight: the scrub ran, and an export the origin
// took before it would write the erased fields back while the pass is made
// again and again.
func TestTheBackfillsInFlightAreRestartedRightAfterTheScrub(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	r.withTheLibrary()
	r.write("customer", adaCustomer("3951", "2026-10-01T10:00:00Z"), seen)
	r.exported("3951")
	r.queue(TopicRedact, eraCustomerGID("3951"), nil, r.at.Add(-time.Minute))
	stateID := memqlsync.StateID(generated.ConceptID("order"), ConnectorName, string(memqlsync.DirectionInbound), r.store)
	r.mutate("upsertSyncState", map[string]any{
		"stateId": stateID, "conceptId": generated.ConceptID("order"), "connector": ConnectorName, "direction": "inbound",
		"partitionKey": r.store, "backfillStatus": "running",
	})
	queued := &queuedBackfills{IntegrationEngineAccess: r.narrowed()}
	refusing := &refusingEngine{IntegrationEngineAccess: queued, prefix: "builtin libraryRemoveArtifact("}
	if ran := r.run(r.connectorOn(refusing, nil)); ran != 0 || refusing.refused == 0 {
		t.Fatalf("ran %d with %d removal(s) refused, want the pass failed on the export", ran, refusing.refused)
	}
	if len(queued.calls) != 1 || !strings.Contains(queued.calls[0], "restart: true") {
		t.Errorf("a pass that failed on its export restarted %d backfill(s) in flight, want the one: %v", len(queued.calls), queued.calls)
	}
}

// privacySessionOf is the maintenance session of a test: one connection of the
// test's own handle, held for the run and returned after it. The session a
// node opens is component/database's, which this module cannot import; what
// it is opened FOR is tested here, and the two together in component/datasync.
func privacySessionOf(db *sql.DB) func(context.Context) (*sql.Conn, func(), error) {
	return func(ctx context.Context) (*sql.Conn, func(), error) {
		conn, err := db.Conn(ctx)
		if err != nil {
			return nil, nil, err
		}
		return conn, func() { _ = conn.Close() }, nil
	}
}

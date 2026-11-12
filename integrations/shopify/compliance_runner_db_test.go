package shopify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// compliance_runner_db_test.go -- the privacy runner against a real engine
// and a real Postgres (rulings 79, 80 and 84).
//
// What is held here is the life of a job's ROW: which rows the due read
// answers, what a pass leaves on one, and what the next pass makes of that.
// A fake engine would answer whatever rows the test gave it and record
// whatever the runner wrote, so "a failed job is run again" would be a test
// of the fake. Here the job is queued through its own mutation, read through
// the runner's own query and recorded through its own write, and the erasure
// it runs is of rows the mirror wrote.

// runnerFixture is the erasure fixture with a runner: a clock of the test's
// own, a store registry that reads fresh, a fake Admin API, and a due read
// narrowed to the test's store. The database is shared and the runner runs
// every job that is due, so without the narrowing a test would run the jobs
// of every other.
type runnerFixture struct {
	*erasureFixture
	admin *fakeAdmin
	at    time.Time
	// broken is the connector without a database handle: every erasure it
	// runs fails before it reads a row.
	broken *Connector
}

type narrowedEngine struct {
	memql.IntegrationEngineAccess
	store string
}

func (e narrowedEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	res, err := e.IntegrationEngineAccess.Execute(ctx, query)
	if err != nil || res == nil || !strings.HasPrefix(query, "complianceJobsDue(") {
		return res, err
	}
	var kept []map[string]any
	for _, row := range memql.MaterializeRows(res) {
		if mapString(row, "storeId") != e.store {
			continue
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		plain := map[string]any{}
		if err := json.Unmarshal(raw, &plain); err != nil {
			return nil, err
		}
		kept = append(kept, plain)
	}
	out := resultWithRows(kept)
	if kept == nil {
		out = resultWithRows([]map[string]any{})
	}
	// The page's cursor is the engine's, so a walk still reads every page.
	out.Meta = res.GetMeta()
	return out, nil
}

func newRunnerFixture(t *testing.T) *runnerFixture {
	t.Helper()
	f := newErasureFixture(t)
	if f == nil {
		return nil
	}
	r := &runnerFixture{erasureFixture: f, admin: newFakeAdmin(t), at: time.Date(2026, 11, 2, 9, 5, 0, 0, time.UTC)}
	build := func() *Connector {
		registry := NewStoreRegistry(f.eng, func(context.Context, string) (string, error) { return "shpat_test", nil })
		registry.ttl = 0
		client := NewAdminClient()
		client.endpoint = func(Store) string { return r.admin.server.URL + "/graphql" }
		conn := NewConnector(narrowedEngine{IntegrationEngineAccess: f.eng, store: f.store}, nil, registry, client)
		conn.now = func() time.Time { return r.at }
		return conn
	}
	f.conn = build().WithDatabase(func() *sql.DB { return f.db })
	r.broken = build()
	clean := func() {
		ctx := context.Background()
		_, _ = f.db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`, f.store)
		_, _ = f.db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'partitionKey' = $1`, f.store)
		_, _ = f.db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND id LIKE $1`, "%"+f.store)
	}
	clean()
	t.Cleanup(clean)
	return r
}

// hasStore writes the store's row the way production does, through the
// engine's own mutation.
func (r *runnerFixture) hasStore() Store {
	r.t.Helper()
	domain := r.store + ".example.com"
	if _, err := r.eng.Execute(seedCtx(), renderCall("createStore", map[string]any{
		"storeId": r.store, "domain": domain, "name": "Example Bicycles",
		"adminTokenRef": "EXAMPLE_ADMIN", "apiVersion": generated.APIVersion, "ownerUserId": "dbtest-owner",
	})); err != nil {
		r.t.Fatalf("seed the store: %v", err)
	}
	return Store{ID: r.store, Domain: domain}
}

func (r *runnerFixture) mutate(name string, args map[string]any) {
	r.t.Helper()
	if _, err := r.eng.Execute(seedCtx(), renderCall(name, args)); err != nil {
		r.t.Fatalf("%s: %v", name, err)
	}
}

// queue queues one privacy job through the job's own mutation and answers its
// id.
func (r *runnerFixture) queue(topic, customer string, orders []string, dueAt time.Time) string {
	r.t.Helper()
	id := ComplianceJobID(r.store, topic, customer+"\x00")
	r.mutate("queueComplianceJob", map[string]any{
		"jobId": id, "storeId": r.store, "topic": topic, "customerGid": customer, "requestId": "",
		"shopDomain": r.store + ".example.com", "orderGids": append([]string{}, orders...), "dueAt": dueAt.UTC().Format(time.RFC3339),
	})
	return id
}

// job is a job's row as the table holds it now.
func (r *runnerFixture) job(topic, customer string) map[string]any {
	r.t.Helper()
	var text string
	err := r.db.QueryRowContext(context.Background(),
		`SELECT payload::text FROM "MemoryNodes"
		  WHERE concept = 'v1:shopify:complianceJob' AND payload->>'storeId' = $1 AND payload->>'topic' = $2 AND COALESCE(payload->>'customerGid', '') = $3
		  ORDER BY "createdAt" DESC LIMIT 1`, r.store, topic, customer).Scan(&text)
	if err != nil {
		r.t.Fatalf("read the %s job: %v", topic, err)
	}
	row := map[string]any{}
	if err := json.Unmarshal([]byte(text), &row); err != nil {
		r.t.Fatal(err)
	}
	return row
}

func (r *runnerFixture) run(conn *Connector) int {
	r.t.Helper()
	ran, err := conn.RunDueComplianceJobs(context.Background())
	if err != nil {
		r.t.Fatalf("the runner failed: %v", err)
	}
	return ran
}

// audits is the actions of the store's audit events, in the order they were
// written.
func (r *runnerFixture) audits() []string {
	r.t.Helper()
	rows, err := queryRows(context.Background(), r.db,
		`SELECT payload->>'action' FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1 ORDER BY "createdAt"`, r.store)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, row := range rows {
		out = append(out, row[0])
	}
	return out
}

func instant(t *testing.T, row map[string]any, key string) time.Time {
	t.Helper()
	raw, _ := row[key].(string)
	if raw == "" {
		return time.Time{}
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("%s = %q is not a time", key, raw)
	}
	return at.UTC()
}

// The due read answers the jobs that are waiting and whose time has come, the
// failed ones too (ruling 79), and the customers/redact jobs an earlier
// version recorded done, which carry no scrubbedAt (ruling 84). A job done
// under this version, a shop's erasure or a data request that is done, and a
// job still inside its hold are not due.
func TestTheDueReadAnswersWaitingFailedAndOldDoneErasures(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	past, future := r.at.Add(-2*time.Hour), r.at.Add(2*time.Hour)
	stamp := r.at.Add(-time.Hour).Format(time.RFC3339)
	queued := func(name, topic string, due time.Time, record map[string]any) {
		id := r.queue(topic, eraCustomerGID(name), nil, due)
		if record != nil {
			record["jobId"] = id
			r.mutate("recordComplianceJob", record)
		}
	}
	queued("1001", TopicRedact, past, nil)
	queued("1002", TopicRedact, future, nil)
	queued("1003", TopicRedact, past, map[string]any{"status": "failed", "lastError": "the customer's rows could not be scrubbed"})
	queued("1004", TopicRedact, past, map[string]any{"status": "done", "outcome": "scrubbed 3 row version(s)", "completedAt": stamp})
	queued("1005", TopicRedact, past, map[string]any{"status": "done", "outcome": "second pass", "completedAt": stamp, "scrubbedAt": stamp})
	queued("1006", TopicDataRequest, past, map[string]any{"status": "done", "outcome": "exported 2 bytes", "completedAt": stamp})
	queued("1007", TopicDataRequest, past, map[string]any{"status": "failed", "lastError": "the export could not be filed"})
	r.queue(TopicShopRedact, "", nil, past)
	r.mutate("recordComplianceJob", map[string]any{"jobId": ComplianceJobID(r.store, TopicShopRedact, "\x00"), "status": "done", "outcome": "purged 9 row version(s)", "completedAt": stamp})

	res, err := r.conn.engine.Execute(operatorContext(context.Background()), renderCall("complianceJobsDue", map[string]any{"asOf": r.at.Format(time.RFC3339)}))
	if err != nil {
		t.Fatal(err)
	}
	var due []string
	for _, row := range memql.MaterializeRows(res) {
		due = append(due, mapString(row, "topic")+" "+strings.TrimPrefix(mapString(row, "customerGid"), "gid://shopify/Customer/")+" "+mapString(row, "status"))
	}
	want := []string{
		"customers/data_request 1007 failed",
		"customers/redact 1001 pending",
		"customers/redact 1003 failed",
		"customers/redact 1004 done",
	}
	if got := sortedStrings(due); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the due read answers\n  %s\nwant the waiting one, the failed ones and the erasure done under the old rule:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Every erasure is made twice, an hour apart (ruling 80). The first pass
// leaves the job pending, with scrubbedAt and a dueAt an hour on; the second
// scrubs what was written between the two and only then is the job done. Each
// pass is audited, the second by its own name.
func TestEveryErasureIsMadeTwiceAnHourApart(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer, order := eraCustomerGID("2001"), eraOrderGID("52001")
	r.write("customer", adaCustomer("2001", "2026-10-01T10:00:00Z"), seen)
	r.write("order", bikeOrder("52001", customer, "2026-10-01T10:00:00Z"), seen)
	r.queue(TopicRedact, customer, nil, r.at.Add(-time.Minute))

	// The first pass.
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the first run ran %d jobs, want the one", ran)
	}
	first := r.at
	job := r.job(TopicRedact, customer)
	if job["status"] != "pending" {
		t.Fatalf("after the first pass the job is %v, want pending: the second pass is still to come", job["status"])
	}
	if got := instant(t, job, "scrubbedAt"); !got.Equal(first) {
		t.Errorf("scrubbedAt = %v, want when the first pass ran, %v", job["scrubbedAt"], first)
	}
	if got := instant(t, job, "dueAt"); !got.Equal(first.Add(ErasureSecondPassAfter)) || ErasureSecondPassAfter != time.Hour {
		t.Errorf("dueAt = %v, want one hour after the first pass, %v", job["dueAt"], first.Add(time.Hour))
	}
	if outcome, _ := job["outcome"].(string); !strings.Contains(outcome, "first pass") || !strings.Contains(outcome, "scrubbed") {
		t.Errorf("the outcome after the first pass is %q, want what the first pass did", outcome)
	}
	if job["lastError"] != "" || job["completedAt"] != nil {
		t.Errorf("after the first pass lastError = %v and completedAt = %v, want neither", job["lastError"], job["completedAt"])
	}
	r.holdsNoWordOf("order", order, adaWords)
	r.holdsNoWordOf("customer", customer, adaWords)
	if got := strings.Join(r.audits(), ","); got != "shopify_customer_redacted" {
		t.Errorf("after the first pass the audit trail is %q, want the one redaction", got)
	}

	// Ten minutes on the job is not due, and nothing runs.
	r.at = first.Add(10 * time.Minute)
	if ran := r.run(r.conn); ran != 0 {
		t.Errorf("a run inside the hour ran %d jobs, want none", ran)
	}

	// A write that was in flight lands after the scrub: a page of an export
	// read before it, a delivery fetched a moment before.
	late := bikeOrder("52001", customer, "2026-10-01T10:00:00Z")
	late["note"] = "Ada asked for the afternoon"
	for _, w := range mapObject(generated.Types["order"], r.store, late, "", seen) {
		if err := r.conn.writeMirror(context.Background(), w); err != nil {
			t.Fatalf("the late write: %v", err)
		}
	}
	if texts := strings.Join(r.texts("order", order), "\n"); !strings.Contains(texts, "Ada asked for the afternoon") {
		t.Fatal("the fixture's late write did not land")
	}

	// The second pass, once the hour is up.
	r.at = first.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the run after the hour ran %d jobs, want the one", ran)
	}
	job = r.job(TopicRedact, customer)
	if job["status"] != "done" {
		t.Fatalf("after the second pass the job is %v, want done", job["status"])
	}
	if got := instant(t, job, "scrubbedAt"); !got.Equal(first) {
		t.Errorf("scrubbedAt moved to %v; it is when the FIRST pass ran, %v", job["scrubbedAt"], first)
	}
	if got := instant(t, job, "completedAt"); !got.Equal(r.at) {
		t.Errorf("completedAt = %v, want when the second pass ran, %v", job["completedAt"], r.at)
	}
	if outcome, _ := job["outcome"].(string); !strings.Contains(outcome, "second pass") || strings.Contains(outcome, "scrubbed 0 row version") {
		t.Errorf("the outcome after the second pass is %q, want it to say the second pass changed the rows written in between", outcome)
	}
	r.holdsNoWordOf("order", order, adaWords)
	r.holdsNoWordOf("orderTransaction", childGID("OrderTransaction", "52001"), adaWords)
	if got := strings.Join(r.audits(), ","); got != "shopify_customer_redacted,shopify_customer_redacted_second_pass" {
		t.Errorf("the audit trail is %q, want one event for each pass, the second naming itself", got)
	}

	// Done is done.
	r.at = r.at.Add(3 * time.Hour)
	if ran := r.run(r.conn); ran != 0 {
		t.Errorf("a run after the job was done ran %d jobs", ran)
	}
}

// A failed job is run again at the next pass and ends done (ruling 79): the
// concept always said a failed job is retried, and the due read answered only
// the waiting ones. A failed FIRST pass leaves scrubbedAt empty, so its retry
// is a first pass; a failed SECOND pass is retried as a second pass.
func TestAFailedPrivacyJobIsRunAgainUntilItIsDone(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer := eraCustomerGID("2101")
	r.write("customer", adaCustomer("2101", "2026-10-01T10:00:00Z"), seen)
	r.queue(TopicRedact, customer, nil, r.at.Add(-time.Minute))

	// The first pass fails.
	if ran := r.run(r.broken); ran != 0 {
		t.Fatalf("a run whose erasure failed says it ran %d jobs", ran)
	}
	job := r.job(TopicRedact, customer)
	if job["status"] != "failed" || job["lastError"] == "" || job["scrubbedAt"] != nil {
		t.Fatalf("after a failed first pass the job is %v with lastError %q and scrubbedAt %v, want failed, saying what failed, and no first pass recorded", job["status"], job["lastError"], job["scrubbedAt"])
	}

	// The next pass runs it again, as a first pass.
	r.at = r.at.Add(time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the next run ran %d jobs, want the failed one again", ran)
	}
	first := r.at
	job = r.job(TopicRedact, customer)
	if job["status"] != "pending" || job["lastError"] != "" || !instant(t, job, "scrubbedAt").Equal(first) {
		t.Fatalf("after the retry the job is %v, lastError %q, scrubbedAt %v; want pending for its second pass, the error cleared", job["status"], job["lastError"], job["scrubbedAt"])
	}
	r.holdsNoWordOf("customer", customer, adaWords)

	// The second pass fails: the job is failed, and still knows its first
	// pass ran.
	r.at = first.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.broken); ran != 0 {
		t.Fatalf("a run whose second pass failed says it ran %d jobs", ran)
	}
	job = r.job(TopicRedact, customer)
	if job["status"] != "failed" || !instant(t, job, "scrubbedAt").Equal(first) {
		t.Fatalf("after a failed second pass the job is %v with scrubbedAt %v, want failed and the first pass still recorded", job["status"], job["scrubbedAt"])
	}
	if outcome, _ := job["outcome"].(string); !strings.Contains(outcome, "first pass") {
		t.Errorf("a failed second pass wiped what the first pass did from the outcome: %q", outcome)
	}

	// And is retried as a second pass: done at once, not pending for another.
	r.at = r.at.Add(time.Hour)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the run after a failed second pass ran %d jobs, want the one", ran)
	}
	job = r.job(TopicRedact, customer)
	if outcome, _ := job["outcome"].(string); job["status"] != "done" || !strings.Contains(outcome, "second pass") || job["lastError"] != "" {
		t.Errorf("the job is %v with outcome %q and lastError %q, want done by its second pass", job["status"], job["outcome"], job["lastError"])
	}
}

// A job whose store row is gone is not a failure (ruling 79). The erasure runs
// by the job's own store id, since the mirror's rows can outlive the row that
// configured them; no backfill is started again, and the outcome says so. A
// data request for such a store has nobody to file an export under and
// nothing configured to export, and ends done saying that.
func TestAJobWhoseStoreRowIsGoneStillErases(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	// No store row is written.
	customer, order := eraCustomerGID("2201"), eraOrderGID("52201")
	r.write("customer", adaCustomer("2201", "2026-10-01T10:00:00Z"), seen)
	r.write("order", bikeOrder("52201", customer, "2026-10-01T10:00:00Z"), seen)
	r.queue(TopicRedact, customer, nil, r.at.Add(-time.Minute))
	r.queue(TopicDataRequest, eraCustomerGID("2202"), nil, r.at.Add(-time.Minute))

	if ran := r.run(r.conn); ran != 2 {
		t.Fatalf("the run ran %d jobs, want both", ran)
	}
	erasure := r.job(TopicRedact, customer)
	outcome, _ := erasure["outcome"].(string)
	if erasure["status"] != "pending" || erasure["lastError"] != "" || !strings.Contains(outcome, "the store is no longer configured") {
		t.Errorf("the erasure of a store that is gone is %v (lastError %q) with outcome %q, want its first pass made and the outcome saying no backfill was started again", erasure["status"], erasure["lastError"], outcome)
	}
	r.holdsNoWordOf("customer", customer, adaWords)
	r.holdsNoWordOf("order", order, adaWords)

	request := r.job(TopicDataRequest, eraCustomerGID("2202"))
	if request["status"] != "done" || request["outcome"] != "the store is no longer configured; nothing to export" || request["lastError"] != "" {
		t.Errorf("the data request of a store that is gone is %v with outcome %q and lastError %q", request["status"], request["outcome"], request["lastError"])
	}

	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the second run ran %d jobs, want the erasure's second pass", ran)
	}
	if erasure = r.job(TopicRedact, customer); erasure["status"] != "done" {
		t.Errorf("the erasure of a store that is gone ends %v, want done", erasure["status"])
	}
}

// An order named only by the request, with no customer on it, is scrubbed
// with its children: the job keeps the request's orders (orderGids), and the
// runner hands them to the erasure.
func TestTheRunnerErasesTheOrdersAJobNames(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	store := r.hasStore()
	order := eraOrderGID("52301")
	r.write("order", bikeOrder("52301", "", "2026-10-01T10:00:00Z"), seen)
	body, _ := json.Marshal(map[string]any{"shop_domain": store.Domain, "customer": map[string]any{"id": 2301}, "orders_to_redact": []any{52301}})
	req := complianceDelivery(TopicRedact, nil)
	req.Body, req.ReceivedAt = body, r.at.Add(-25*time.Hour)
	if err := r.conn.enqueueComplianceJob(context.Background(), store, TopicRedact, req); err != nil {
		t.Fatalf("queue the erasure: %v", err)
	}

	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the run ran %d jobs, want the one", ran)
	}
	for concept, gid := range map[string]string{
		"order": order, "orderLineItem": childGID("LineItem", "52301"), "orderTransaction": childGID("OrderTransaction", "52301"),
		"fulfillment": childGID("Fulfillment", "52301"), "refund": childGID("Refund", "52301"),
	} {
		r.holdsNoWordOf(concept, gid, adaWords)
	}
}

// The same for a shop's erasure: purge, and purge again an hour later, which
// takes what a write in flight put back. Before the SECOND purge the store is
// checked exactly as before the first: one that is reachable and ingesting
// again is not purged a second time, because its reconnect reads everything
// again, and the job ends done saying so.
func TestAShopsErasureIsMadeTwiceUnlessTheStoreCameBack(t *testing.T) {
	product := func(n, title string) map[string]any {
		return map[string]any{"id": "gid://shopify/Product/" + n, "updatedAt": "2026-10-01T10:00:00Z", "title": title}
	}
	begin := func(t *testing.T) (*runnerFixture, time.Time) {
		r := newRunnerFixture(t)
		if r == nil {
			return nil, time.Time{}
		}
		r.hasStore()
		r.mutate("markStoreUninstalled", map[string]any{"storeId": r.store, "uninstalledAt": r.at.Add(-49 * time.Hour).Format(time.RFC3339)})
		r.write("product", product("91", "Road bicycle"), seen)
		r.queue(TopicShopRedact, "", nil, r.at.Add(-time.Minute))

		if ran := r.run(r.conn); ran != 1 {
			t.Fatalf("the first run ran %d jobs, want the purge", ran)
		}
		job := r.job(TopicShopRedact, "")
		if outcome, _ := job["outcome"].(string); job["status"] != "pending" || !instant(t, job, "scrubbedAt").Equal(r.at) ||
			!instant(t, job, "dueAt").Equal(r.at.Add(ErasureSecondPassAfter)) || !strings.Contains(outcome, "first pass") || !strings.Contains(outcome, "purged") {
			t.Fatalf("after the first purge the job is %v, scrubbedAt %v, dueAt %v, outcome %q; want pending for a second purge an hour on", job["status"], job["scrubbedAt"], job["dueAt"], job["outcome"])
		}
		if left := r.allTexts(); len(left) != 0 {
			t.Fatalf("the first purge left %d row(s) of the store", len(left))
		}
		return r, r.at
	}

	t.Run("the store is still gone", func(t *testing.T) {
		r, first := begin(t)
		if r == nil {
			return
		}
		// A page of an export in flight is applied after the purge.
		r.write("product", product("92", "Touring bicycle"), seen)
		r.at = first.Add(ErasureSecondPassAfter + time.Second)
		if ran := r.run(r.conn); ran != 1 {
			t.Fatalf("the second run ran %d jobs, want the second purge", ran)
		}
		if left := r.allTexts(); len(left) != 0 {
			t.Errorf("the second purge left what was written after the first: %v", left)
		}
		job := r.job(TopicShopRedact, "")
		if outcome, _ := job["outcome"].(string); job["status"] != "done" || !strings.Contains(outcome, "second pass") || !strings.Contains(outcome, "purged") {
			t.Errorf("the job is %v with outcome %q, want done by its second purge", job["status"], job["outcome"])
		}
		if got := strings.Join(r.audits(), ","); got != "shopify_shop_redacted,shopify_shop_redacted_second_pass" {
			t.Errorf("the audit trail is %q, want one event for each purge, the second naming itself", got)
		}
	})

	t.Run("the store came back", func(t *testing.T) {
		r, first := begin(t)
		if r == nil {
			return
		}
		// Reinstalled inside the hour: the reconnect clears what the uninstall
		// and the purge recorded and gives the store a grant again (the
		// uninstall dropped the old one), the store answers, and reads again.
		r.mutate("markStoreReconnected", map[string]any{"storeId": r.store, "status": StatusConfigured})
		r.mutate("updateStore", map[string]any{"storeId": r.store, "adminTokenRef": "EXAMPLE_ADMIN"})
		r.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
		r.write("product", product("93", "City bicycle"), seen)
		r.at = first.Add(ErasureSecondPassAfter + time.Second)
		if ran := r.run(r.conn); ran != 1 {
			t.Fatalf("the second run ran %d jobs, want the job ended", ran)
		}
		if left := r.allTexts(); len(left) != 1 {
			t.Errorf("a store that came back was purged a second time: %d row(s) left, want the one its reconnect read", len(left))
		}
		job := r.job(TopicShopRedact, "")
		if outcome, _ := job["outcome"].(string); job["status"] != "done" || !strings.Contains(outcome, "not purged a second time") || !strings.Contains(outcome, "reachable again") {
			t.Errorf("the job is %v with outcome %q, want done, saying the store is reachable again and was not purged a second time", job["status"], job["outcome"])
		}
		if got := strings.Join(r.audits(), ","); got != "shopify_shop_redacted,shopify_shop_redact_second_pass_skipped" {
			t.Errorf("the audit trail is %q, want the purge and the second pass skipped", got)
		}
	})
}

// After an erasure, a backfill of the store that is failed with a cursor that
// names an export has that export dropped from its cursor: a later "Backfill
// now" starts a new export instead of continuing one the origin took before
// the erasure. Its status and its error are kept, and no other row is
// touched.
func TestAnErasureDropsAFailedBackfillsExportFromItsCursor(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	state := func(concept string) string {
		return memqlsync.StateID(generated.ConceptID(concept), ConnectorName, string(memqlsync.DirectionInbound), r.store)
	}
	seed := func(concept, status, cursor, lastError string) {
		r.mutate("upsertSyncState", map[string]any{
			"stateId": state(concept), "conceptId": generated.ConceptID(concept), "connector": ConnectorName, "direction": "inbound",
			"partitionKey": r.store, "backfillStatus": status, "backfillCursor": cursor, "lastError": lastError,
		})
	}
	read := func(concept string) map[string]any {
		res, err := r.eng.Execute(memql.ContextWithFreshRead(operatorContext(context.Background())), renderCall("syncStateById", map[string]any{"stateId": state(concept)}))
		if err != nil {
			t.Fatal(err)
		}
		rows := memql.MaterializeRows(res)
		if len(rows) != 1 {
			t.Fatalf("the sync state of %s is %d rows", concept, len(rows))
		}
		return rows[0]
	}
	old := backfillCursor{OperationID: "gid://shopify/BulkOperation/7", OpIndex: 1, Line: 1500, Restarts: 1, Rule: originVersionRule, At: "2026-10-30T08:00:00Z"}.encode()
	seed("order", "failed", old, "backfill failed: shopify admin: HTTP 502")
	seed("customer", "failed", "", "backfill failed: shopify admin: HTTP 502")
	completed := backfillCursor{Rule: originVersionRule}.encode()
	seed("product", "complete", completed, "")

	customer := eraCustomerGID("2401")
	r.write("customer", adaCustomer("2401", "2026-10-01T10:00:00Z"), seen)
	r.queue(TopicRedact, customer, nil, r.at.Add(-time.Minute))
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the run ran %d jobs, want the one", ran)
	}

	order := read("order")
	cur := decodeBackfillCursor(mapString(order, "backfillCursor"))
	if cur.OperationID != "" || cur.Line != 0 || cur.OpIndex != 0 || cur.At != "" {
		t.Errorf("the failed backfill's cursor is %q after the erasure, want the export it named dropped from it", mapString(order, "backfillCursor"))
	}
	if cur.Rule != originVersionRule || cur.Restarts != 1 {
		t.Errorf("the cursor lost what is the backfill's own: %q, want its rule and its count of restarts kept", mapString(order, "backfillCursor"))
	}
	if mapString(order, "backfillStatus") != "failed" || mapString(order, "lastError") != "backfill failed: shopify admin: HTTP 502" {
		t.Errorf("the failed backfill is now %q with %q, want its status and its error kept", mapString(order, "backfillStatus"), mapString(order, "lastError"))
	}
	if c := read("customer"); mapString(c, "backfillCursor") != "" || mapString(c, "backfillStatus") != "failed" {
		t.Errorf("a failed backfill with no export to drop was changed: %v", c)
	}
	if p := read("product"); mapString(p, "backfillStatus") != "complete" || mapString(p, "backfillCursor") != completed {
		t.Errorf("a complete backfill was changed: %v", p)
	}
	if outcome, _ := r.job(TopicRedact, customer)["outcome"].(string); !strings.Contains(outcome, "dropped the old export of 1 failed backfill") {
		t.Errorf("the outcome is %q, want it to say a failed backfill's old export was dropped", outcome)
	}
}

// The erasures already made are made again (ruling 84). A customers/redact
// job an earlier version recorded done left an email address and a phone
// number behind, never reached the order's children, and left objects the
// mirror can no longer write. Such a job has no scrubbedAt: it is due once
// more, goes through the same two passes, and is not picked up a third time.
// A shop's erasure and a data request that are done are left alone.
func TestAnErasureDoneUnderTheOldRuleIsMadeAgain(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	r.hasStore()
	customer, order := eraCustomerGID("2501"), eraOrderGID("52501")
	r.write("customer", adaCustomer("2501", "2026-10-01T10:00:00Z"), seen)
	r.write("order", bikeOrder("52501", customer, "2026-10-01T10:00:00Z"), seen)
	r.leftByTheOldErasure(customer)
	if o, c := r.newest("order", order), r.newest("customer", customer); o["billingAddress"] != RedactionMarker || !strings.Contains(fmt.Sprint(c["defaultEmailAddress"]), adaEmail) {
		t.Fatalf("the fixture is not what the old erasure left: %v / %v", o["billingAddress"], c["defaultEmailAddress"])
	}
	later := mapObject(generated.Types["order"], r.store, map[string]any{
		"id": order, "updatedAt": "2026-11-01T10:00:00Z", "name": "#52501", "displayFinancialStatus": "REFUNDED", "totalPriceSet": euros("1299.00"),
	}, "", seen.Add(31*24*time.Hour))
	if err := r.conn.writeMirror(context.Background(), later[0]); err == nil {
		t.Fatal("the premise does not hold: the mirror can write the order the old erasure touched")
	}

	// The three jobs an earlier version recorded done, a month ago.
	longAgo := r.at.Add(-30 * 24 * time.Hour)
	done := func(topic, subject, outcome string) {
		id := r.queue(topic, subject, nil, longAgo)
		r.mutate("recordComplianceJob", map[string]any{"jobId": id, "status": "done", "outcome": outcome, "completedAt": longAgo.Add(time.Hour).Format(time.RFC3339)})
	}
	done(TopicRedact, customer, "scrubbed 2 row version(s)")
	done(TopicDataRequest, eraCustomerGID("2502"), "exported 512 bytes")
	done(TopicShopRedact, "", "skipped: the store is reachable again")

	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the run ran %d jobs, want the old erasure and no other", ran)
	}
	job := r.job(TopicRedact, customer)
	if job["status"] != "pending" || !instant(t, job, "scrubbedAt").Equal(r.at) {
		t.Fatalf("the old erasure is %v with scrubbedAt %v, want its first pass made under the new rules", job["status"], job["scrubbedAt"])
	}
	c := r.newest("customer", customer)
	if _, present := c["defaultEmailAddress"]; present {
		t.Errorf("her email address is still in place: %v", c["defaultEmailAddress"])
	}
	if _, present := r.newest("order", order)["billingAddress"]; present {
		t.Error("the order still holds the old marker in its billing address")
	}
	r.holdsNoWordOf("customer", customer, adaWords)
	r.holdsNoWordOf("order", order, adaWords)
	r.holdsNoWordOf("orderTransaction", childGID("OrderTransaction", "52501"), adaWords)
	if err := r.conn.writeMirror(context.Background(), later[0]); err != nil {
		t.Errorf("after the erasure was made again the mirror still cannot write the order: %v", err)
	}

	r.at = r.at.Add(ErasureSecondPassAfter + time.Second)
	if ran := r.run(r.conn); ran != 1 {
		t.Fatalf("the second run ran %d jobs, want the second pass", ran)
	}
	job = r.job(TopicRedact, customer)
	if job["status"] != "done" || job["scrubbedAt"] == nil {
		t.Fatalf("the job is %v with scrubbedAt %v, want done with its first pass recorded", job["status"], job["scrubbedAt"])
	}

	// Not a third time, and the other two never.
	r.at = r.at.Add(48 * time.Hour)
	if ran := r.run(r.conn); ran != 0 {
		t.Errorf("a later run ran %d jobs, want none: the erasure is made again once", ran)
	}
	if got := r.job(TopicDataRequest, eraCustomerGID("2502")); got["status"] != "done" || got["outcome"] != "exported 512 bytes" || got["scrubbedAt"] != nil {
		t.Errorf("a data request that was done was run again: %v", got)
	}
	if got := r.job(TopicShopRedact, ""); got["status"] != "done" || got["outcome"] != "skipped: the store is reachable again" || got["scrubbedAt"] != nil {
		t.Errorf("a shop's erasure that was done was run again: %v", got)
	}
}

// The runner runs every job that is due, not the first page of them. With
// failed jobs tried again at every run, a hundred standing failures would
// otherwise fill the one page it read, oldest first, and no later job would
// ever run.
func TestTheRunnerRunsEveryDueJobAndNotTheFirstPage(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	// A store that is gone: each data request ends done at once.
	const jobs = 130
	for i := 0; i < jobs; i++ {
		r.queue(TopicDataRequest, eraCustomerGID(fmt.Sprintf("3%03d", i)), nil, r.at.Add(-time.Duration(jobs-i)*time.Minute))
	}
	if ran := r.run(r.conn); ran != jobs {
		t.Fatalf("the run ran %d of %d due jobs", ran, jobs)
	}
	var left int
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM (
		   SELECT DISTINCT ON (id) payload FROM "MemoryNodes"
		    WHERE concept = 'v1:shopify:complianceJob' AND payload->>'storeId' = $1 ORDER BY id, "createdAt" DESC) latest
		  WHERE payload->>'status' <> 'done'`, r.store).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d of %d jobs are not done after one run", left, jobs)
	}
}

// The store's verdict reads the jobs that have been failing for more than a
// day: by store, failed, and due before yesterday. One that failed an hour
// ago has a pass still to come, and a done one is done.
func TestTheVerdictReadsAStoresPrivacyRequestsThatKeepFailing(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	store := r.hasStore()
	fail := func(name string, due time.Time) {
		id := r.queue(TopicRedact, eraCustomerGID(name), nil, due)
		r.mutate("recordComplianceJob", map[string]any{"jobId": id, "status": "failed", "lastError": "the customer's rows could not be scrubbed"})
	}
	// Failed and due an hour ago; done; and still waiting: none of them counts.
	fail("4001", r.at.Add(-time.Hour))
	doneID := r.queue(TopicRedact, eraCustomerGID("4002"), nil, r.at.Add(-50*time.Hour))
	r.mutate("recordComplianceJob", map[string]any{"jobId": doneID, "status": "done", "outcome": "second pass", "scrubbedAt": r.at.Format(time.RFC3339), "completedAt": r.at.Format(time.RFC3339)})
	r.queue(TopicRedact, eraCustomerGID("4003"), nil, r.at.Add(-50*time.Hour))
	got, err := r.conn.privacyRequestsFailing(context.Background(), r.store)
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 0 {
		t.Fatalf("with a job failed an hour ago, a done one and a waiting one, the read answers %+v, want none failing", got)
	}
	fresh, _, err := r.conn.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.store)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := r.conn.storeVerdict(context.Background(), fresh); err != nil || len(v.Reasons) != 0 && v.Reasons[len(v.Reasons)-1].Code == ReasonPrivacyRequestFailing {
		t.Fatalf("the store's verdict lists a failing privacy request though none has failed for a day: %v, %v", v.Reasons, err)
	}

	// Failed and due 25 hours ago, and one more due three days ago.
	fail("4004", r.at.Add(-25*time.Hour))
	fail("4005", r.at.Add(-72*time.Hour))
	got, err = r.conn.privacyRequestsFailing(context.Background(), r.store)
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 2 || got.AtLeast || !got.Since.Equal(r.at.Add(-72*time.Hour)) {
		t.Fatalf("the read answers %+v, want the two that have been failing for a day, since the older of them was due", got)
	}
	v, err := r.conn.storeVerdict(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	last := VerdictReason{}
	if len(v.Reasons) > 0 {
		last = v.Reasons[len(v.Reasons)-1]
	}
	if v.State != VerdictNeedsAttention || last.Code != ReasonPrivacyRequestFailing || last.Sentence != privacySentence || last.Count != 2 || last.Act != "" {
		t.Errorf("the store %s reads %q with %+v, want needs attention for the failing privacy requests", store.ID, v.State, v.Reasons)
	}
	// Another store's failing job is not this store's.
	other, err := r.conn.privacyRequestsFailing(context.Background(), r.store+"-other")
	if err != nil || other.Count != 0 {
		t.Errorf("another store reads %+v, %v", other, err)
	}
}

// A job's lastError says what failed and never for whom (ruling 79). The
// cause of a failure is the engine's or the database's own text, and that
// text can hold the statement it refused: a customer's gid, an address, a
// name. So the row takes one of a closed set of sentences, and the cause goes
// to the server's log.
func TestAJobsLastErrorSaysWhatFailedAndNotForWhom(t *testing.T) {
	db := openTestDB(t, "a privacy job's lastError names nobody")
	if db == nil {
		return
	}
	const customer = "gid://shopify/Customer/777000991"
	for name, tc := range map[string]struct {
		topic   string
		arrange func(h *testHarness)
		want    string
	}{
		"the export cannot be filed": {
			topic: TopicDataRequest,
			arrange: func(h *testHarness) {
				h.engine.fail["createGeneratedOutput"] = errString(`refused: createGeneratedOutput(summary: "export for ` + customer + `, Ada Example <ada@example.com>")`)
			},
			want: "the export could not be filed in the store owner's library",
		},
		"a backfill in flight cannot be started again": {
			topic: TopicRedact,
			arrange: func(h *testHarness) {
				h.engine.fail["datasyncQueueBackfill"] = errString("no connector bound for " + customer + " of ada@example.com")
			},
			want: "the erasure ran, but a backfill in flight could not be started again",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := redactionHarness(t, db, testStoreID, map[string]map[string]any{"order": {"backfillStatus": "running", "backfillCursor": "c-1"}})
			tc.arrange(h)
			h.engine.setRows("complianceJobsDue", []map[string]any{{
				"id": "shpcj-names-nobody", "storeId": testStoreID, "topic": tc.topic, "customerGid": customer,
				"status": "pending", "dueAt": h.now.Add(-time.Hour).Format(time.RFC3339),
			}})

			ran, err := h.conn.RunDueComplianceJobs(context.Background())
			if err != nil || ran != 0 {
				t.Fatalf("RunDueComplianceJobs = %d, %v; want the one job failed", ran, err)
			}
			recorded := h.engine.callsTo("recordComplianceJob")
			if len(recorded) != 1 || !strings.Contains(recorded[0], `status: "failed"`) {
				t.Fatalf("recorded %v, want the job recorded failed once", recorded)
			}
			if !strings.Contains(recorded[0], `lastError: "`+tc.want+`"`) {
				t.Errorf("the job's lastError is not the sentence for what failed (%q):\n  %s", tc.want, recorded[0])
			}
			for _, whom := range []string{"777000991", "gid://shopify/Customer", "ada@example.com", "Ada Example", "@"} {
				if strings.Contains(recorded[0], whom) {
					t.Errorf("the job's row says for whom it failed (%q):\n  %s", whom, recorded[0])
				}
			}
		})
	}
}

// The write that drops a failed backfill's export is decided on a read of the
// row, and an operator's "Backfill now" can start that row in between. The
// write is fenced on the version it read: started in between, the row keeps
// the cursor and the status its new run wrote, and the write is refused.
func TestAFailedBackfillStartedInBetweenKeepsWhatItsNewRunWrote(t *testing.T) {
	r := newRunnerFixture(t)
	if r == nil {
		return
	}
	stateID := memqlsync.StateID(generated.ConceptID("order"), ConnectorName, string(memqlsync.DirectionInbound), r.store)
	old := backfillCursor{OperationID: "gid://shopify/BulkOperation/7", Line: 1500, Rule: originVersionRule, At: "2026-10-30T08:00:00Z"}.encode()
	r.mutate("upsertSyncState", map[string]any{
		"stateId": stateID, "conceptId": generated.ConceptID("order"), "connector": ConnectorName, "direction": "inbound",
		"partitionKey": r.store, "backfillStatus": "failed", "backfillCursor": old, "lastError": "backfill failed: shopify admin: HTTP 502",
	})
	read := func() map[string]any {
		res, err := r.eng.Execute(memql.ContextWithFreshRead(operatorContext(context.Background())), renderCall("syncStateById", map[string]any{"stateId": stateID}))
		if err != nil {
			t.Fatal(err)
		}
		rows := memql.MaterializeRows(res)
		if len(rows) != 1 {
			t.Fatalf("the sync state is %d rows", len(rows))
		}
		return rows[0]
	}
	asRead := read()

	// "Backfill now", between the erasure's read and its write.
	begun := backfillCursor{OperationID: "gid://shopify/BulkOperation/8", Rule: originVersionRule, At: "2026-11-02T09:05:00Z"}.encode()
	r.mutate("patchSyncState", map[string]any{"stateId": stateID, "backfillStatus": "running", "backfillCursor": begun})

	did, err := r.conn.dropFailedExport(context.Background(), stateID, asRead)
	if err == nil || did {
		t.Fatalf("the write decided on a row another writer has since changed was made (%v, %v), want it refused", did, err)
	}
	if now := read(); mapString(now, "backfillCursor") != begun || mapString(now, "backfillStatus") != "running" {
		t.Errorf("the row is %q at %q, want what its new run wrote", mapString(now, "backfillStatus"), mapString(now, "backfillCursor"))
	}

	// Decided on the row as it is, the write is made.
	r.mutate("patchSyncState", map[string]any{"stateId": stateID, "backfillStatus": "failed"})
	if did, err := r.conn.dropFailedExport(context.Background(), stateID, read()); err != nil || !did {
		t.Fatalf("the write decided on the row as it is: %v, %v", did, err)
	}
	if now := read(); decodeBackfillCursor(mapString(now, "backfillCursor")).OperationID != "" || mapString(now, "backfillStatus") != "failed" {
		t.Errorf("the row is %q at %q, want its export dropped and its status kept", mapString(now, "backfillStatus"), mapString(now, "backfillCursor"))
	}
}

package shopify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// compliance_test.go -- the three privacy topics (#4395).

func complianceDelivery(topic string, body map[string]any) memqlsync.InboundRequest {
	raw, _ := json.Marshal(body)
	return memqlsync.InboundRequest{
		RequestId: "inb-c", Source: "shopify-" + testStoreID, Topic: topic, Body: raw,
		Headers:    map[string]string{"x-shopify-topic": topic, "x-shopify-shop-domain": "acme-widgets.myshopify.com"},
		ReceivedAt: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
}

func TestAComplianceDeliveryIsQueuedNotRunInline(t *testing.T) {
	h := newHarness(t)
	req := complianceDelivery(TopicRedact, map[string]any{
		"shop_domain":      "acme-widgets.myshopify.com",
		"customer":         map[string]any{"id": 991},
		"orders_to_redact": []any{},
	})
	writes, err := h.conn.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 0 {
		t.Fatalf("a compliance topic produced %d mirror writes", len(writes))
	}
	queued := h.engine.callsTo("queueComplianceJob")
	if len(queued) != 1 {
		t.Fatalf("queued %d jobs", len(queued))
	}
	// Its own queue, not the outbox: the outbox's drain hands every entry
	// to Propagate, and a compliance job handed to Propagate would try to
	// write a customer's export into Shopify.
	if len(h.engine.callsTo("appendOutboxEntry")) != 0 {
		t.Error("a privacy request was queued on the outbound push queue")
	}
	if !strings.Contains(queued[0], `topic: "customers/redact"`) {
		t.Errorf("the job did not record its topic:\n%s", queued[0])
	}
	// Receipt is audited even though the work has not run: the obligation is
	// to be able to show WHAT was done and when, and a request nobody
	// recorded is indistinguishable from one that never arrived.
	if len(h.engine.callsTo("createAuditEvent")) != 1 {
		t.Error("the request was not audited on receipt")
	}
}

func TestTheHoldsAreTheRegulatedOnes(t *testing.T) {
	store := Store{ID: testStoreID, Domain: "acme-widgets.myshopify.com"}
	received := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	shop, err := parseComplianceJob(TopicShopRedact, store,
		complianceDelivery(TopicShopRedact, map[string]any{"shop_domain": store.Domain}), received)
	if err != nil {
		t.Fatal(err)
	}
	if got := shop.DueAt.Sub(shop.ReceivedAt); got != ShopRedactHold {
		t.Errorf("shop/redact hold = %v, want 48h -- an uninstall an operator reverses must not cost the mirror", got)
	}

	redact, err := parseComplianceJob(TopicRedact, store,
		complianceDelivery(TopicRedact, map[string]any{"shop_domain": store.Domain, "customer": map[string]any{"id": 1}, "orders_to_redact": []any{}}), received)
	if err != nil {
		t.Fatal(err)
	}
	if !redact.DueAt.After(redact.ReceivedAt) {
		t.Error("customers/redact runs inline, overriding any grace period the merchant configured")
	}
	if redact.CustomerGID != "gid://shopify/Customer/1" {
		t.Errorf("customer GID = %q", redact.CustomerGID)
	}
}

func TestTheExportIsRefusedWhenTheStoreHasNoOwner(t *testing.T) {
	h := newHarness(t)
	store := h.store(t)
	store.OwnerUserID = ""
	err := h.conn.writeExportArtifact(context.Background(), store, ComplianceJob{}, []byte("{}"))
	if err == nil || !strings.Contains(err.Error(), "ownerUserId") {
		t.Fatalf("err = %v, want a refusal naming the field -- an export is a document about a real person and the Library is owner-tier", err)
	}
}

func TestTheExportIsFiledUnderTheStoreOwner(t *testing.T) {
	h := newHarness(t)
	job := ComplianceJob{RequestID: "dr-1", CustomerGID: "gid://shopify/Customer/991", ReceivedAt: h.now}
	if err := h.conn.writeExportArtifact(context.Background(), h.store(t), job, []byte(`{"rows":{}}`)); err != nil {
		t.Fatal(err)
	}
	calls := h.engine.callsTo("createGeneratedOutput")
	if len(calls) != 1 {
		t.Fatalf("wrote %d artifacts", len(calls))
	}
	if !strings.Contains(calls[0], "dr-1") {
		t.Errorf("the Shopify request id is not recorded with the export:\n%s", calls[0])
	}
}

// With no maintenance session there is nothing to run a statement on, and
// each of the three says so rather than run on another pool. (That a scrubbed
// string is marked, and what else is kept and lost, is the policy's:
// erasure_fields_test.go, its golden, and erasure_db_test.go against the
// table.)
func TestRedactionPurgeAndExportRefuseWithoutASession(t *testing.T) {
	h := newHarness(t)
	for what, run := range map[string]func() error{
		"the redaction": func() error {
			_, err := h.conn.RedactCustomer(context.Background(), testStoreID, "gid://shopify/Customer/1", nil)
			return err
		},
		"the purge": func() error {
			_, err := h.conn.PurgeStore(context.Background(), h.store(t))
			return err
		},
		// The export reads the rows the erasure would scrub, from the same table.
		"the export": func() error {
			_, err := h.conn.ExportCustomerData(context.Background(), h.store(t), "gid://shopify/Customer/1", nil)
			return err
		},
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "maintenance session") {
			t.Errorf("%s with no session answered %v, want it refused for want of the maintenance session", what, err)
		}
	}
}

// An uninstall an operator reverses inside the 48-hour hold must not cost the
// mirror.
func TestShopRedactSkipsThePurgeWhenTheStoreIsBackx(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})
	job := ComplianceJob{Topic: TopicShopRedact, StoreID: testStoreID, DueAt: h.now.Add(-time.Hour), ReceivedAt: h.now.Add(-49 * time.Hour)}

	if _, err := h.conn.runComplianceJob(context.Background(), h.store(t), true, job); err != nil {
		t.Fatal(err)
	}
	audits := h.engine.callsTo("createAuditEvent")
	if len(audits) != 1 || !strings.Contains(audits[0], "shopify_shop_redact_skipped") {
		t.Fatalf("audits = %v, want a recorded skip", audits)
	}
}

func TestAComplianceJobRunsOnlyWhenItsHoldHasElapsed(t *testing.T) {
	h := newHarness(t)
	job := ComplianceJob{
		Topic: TopicRedact, StoreID: testStoreID, CustomerGID: "gid://shopify/Customer/1",
		ReceivedAt: h.now, DueAt: h.now.Add(time.Hour),
	}
	_ = job
	// complianceJobsDue filters on dueAt server-side, so a job still
	// inside its hold is simply not returned. The read here is empty,
	// which is what "the hold has not elapsed" looks like to the runner.
	h.engine.setRows("complianceJobsDue", nil)

	ran, err := h.conn.RunDueComplianceJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ran != 0 {
		t.Fatalf("ran %d jobs before the hold elapsed", ran)
	}
}

// One captured, still-valid privacy delivery replayed under each other
// compliance topic queues nothing. The topic is an unsigned header and the
// three payloads share one signing secret per app, so the operation is bound
// to the payload shape only its own topic carries: without that, a
// customers/data_request body replayed as customers/redact or shop/redact
// is a real redaction (memql#5707 review).
func TestAReplayedPrivacyBodyUnderAnotherTopicQueuesNothing(t *testing.T) {
	const shop = "acme-widgets.myshopify.com"
	bodies := map[string]map[string]any{
		TopicDataRequest: {"shop_domain": shop, "customer": map[string]any{"id": 991}, "data_request": map[string]any{"id": 42}, "orders_requested": []any{}},
		TopicRedact:      {"shop_domain": shop, "customer": map[string]any{"id": 991}, "orders_to_redact": []any{}},
		TopicShopRedact:  {"shop_domain": shop, "shop_id": 954889},
	}
	for signedAs, body := range bodies {
		for _, replayedAs := range []string{TopicDataRequest, TopicRedact, TopicShopRedact} {
			t.Run(signedAs+" replayed as "+replayedAs, func(t *testing.T) {
				h := newHarness(t)
				_, err := h.conn.Apply(context.Background(), complianceDelivery(replayedAs, body))
				queued := len(h.engine.callsTo("queueComplianceJob"))
				if signedAs == replayedAs {
					if err != nil || queued != 1 {
						t.Fatalf("the genuine delivery: queued=%d err=%v", queued, err)
					}
					return
				}
				if err == nil || queued != 0 {
					t.Fatalf("a replay under another topic queued %d jobs (err=%v); the signed body must decide the operation", queued, err)
				}
			})
		}
	}
}

// customers/redact is bound to its shape by what it CARRIES (a customer) and
// what it does NOT (a data_request), not by orders_to_redact (memql#5707
// residual). Shopify documents that key as always present, but it is the one
// part of the shape that separates nothing: the customer already separates it
// from shop/redact and the absent data_request from customers/data_request. So
// a body that omits it is still the redaction it says it is and is queued --
// refusing it would turn a legal request into a failed row -- while the replay
// matrix holds with the key gone: the same body under either other topic
// queues nothing.
func TestARedactBodyWithoutOrdersToRedactIsStillARedaction(t *testing.T) {
	const shop = "acme-widgets.myshopify.com"
	body := map[string]any{"shop_domain": shop, "customer": map[string]any{"id": 991}}
	for _, topic := range []string{TopicRedact, TopicDataRequest, TopicShopRedact} {
		t.Run("delivered as "+topic, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.conn.Apply(context.Background(), complianceDelivery(topic, body))
			queued := h.engine.callsTo("queueComplianceJob")
			if topic == TopicRedact {
				if err != nil || len(queued) != 1 || !strings.Contains(queued[0], `customerGid: "gid://shopify/Customer/991"`) {
					t.Fatalf("a customers/redact with no orders_to_redact: err=%v, jobs=%v; want it queued for the customer", err, queued)
				}
				return
			}
			if err == nil || len(queued) != 0 {
				t.Fatalf("the redact body replayed as %s queued %d jobs (err=%v)", topic, len(queued), err)
			}
		})
	}
}

// The orders a request names are kept on its job (ruling 78): an erasure
// scrubs them whether or not they name the customer, and a data request
// exports them. The request's own list of orders used to be read, for the
// data request only, and then dropped.
func TestTheOrdersARequestNamesAreKeptOnItsJob(t *testing.T) {
	const shop = "acme-widgets.myshopify.com"
	for topic, body := range map[string]map[string]any{
		// Ids are numbers in Shopify's body, and one beyond what a float holds
		// exactly has to arrive with every digit.
		TopicRedact:      {"shop_domain": shop, "customer": map[string]any{"id": 991}, "orders_to_redact": []any{299938, 9007199254740993}},
		TopicDataRequest: {"shop_domain": shop, "customer": map[string]any{"id": 991}, "data_request": map[string]any{"id": 42}, "orders_requested": []any{299938, 9007199254740993}},
	} {
		t.Run(topic, func(t *testing.T) {
			h := newHarness(t)
			if _, err := h.conn.Apply(context.Background(), complianceDelivery(topic, body)); err != nil {
				t.Fatal(err)
			}
			queued := h.engine.callsTo("queueComplianceJob")
			if len(queued) != 1 {
				t.Fatalf("queued %d jobs", len(queued))
			}
			if want := `orderGids: ["gid://shopify/Order/299938", "gid://shopify/Order/9007199254740993"]`; !strings.Contains(queued[0], want) {
				t.Errorf("the job does not keep the orders its request names:\n  %s\nwant it to hold\n  %s", queued[0], want)
			}
		})
	}
	// A shop's erasure names no order, and its job says so with an empty list.
	h := newHarness(t)
	if _, err := h.conn.Apply(context.Background(), complianceDelivery(TopicShopRedact, map[string]any{"shop_domain": shop, "shop_id": 954889})); err != nil {
		t.Fatal(err)
	}
	if queued := h.engine.callsTo("queueComplianceJob"); len(queued) != 1 || !strings.Contains(queued[0], "orderGids: []") {
		t.Errorf("a shop's erasure queued %v, want one job that names no order", queued)
	}
}

// The runner reads the orders back from the job's row.
func TestTheRunnerReadsTheOrdersAJobNames(t *testing.T) {
	job := complianceJobOf(map[string]any{
		"id": "shpcj1", "storeId": testStoreID, "topic": TopicRedact, "customerGid": "gid://shopify/Customer/991",
		"orderGids": []any{"gid://shopify/Order/299938", "gid://shopify/Order/280263"}, "dueAt": "2026-08-24T12:00:00Z",
	})
	if strings.Join(job.OrderGIDs, ",") != "gid://shopify/Order/299938,gid://shopify/Order/280263" {
		t.Errorf("the job read from its row names the orders %v", job.OrderGIDs)
	}
	if job.ID != "shpcj1" || job.Topic != TopicRedact || job.CustomerGID != "gid://shopify/Customer/991" || job.DueAt.IsZero() {
		t.Errorf("the job read from its row is %+v", job)
	}
}

// What a failed run leaves on its job's row is a sentence of the runner's own,
// whatever the failure was: a failure the runner did not name leaves the one
// sentence that says nothing, and never the failure's text.
func TestAFailedRunLeavesASentenceAndNeverItsCause(t *testing.T) {
	cause := errString(`refused: insert(payload={"customerGid":"gid://shopify/Customer/991","email":"ada@example.com","name":"Ada Example"})`)
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"a failure the runner named":     {failedJob(failScrub, cause), "the customer's rows could not be scrubbed"},
		"the same, wrapped on its way":   {fmt.Errorf("shopify: the pass: %w", failedJob(failPurge, cause)), "the store's mirror could not be deleted"},
		"a failure nobody named":         {cause, "the job could not be run"},
		"a failure with no cause at all": {failedJob(failNoCustomer, nil), "the request names no customer"},
	} {
		t.Run(name, func(t *testing.T) {
			got := lastErrorOf(tc.err)
			if got != tc.want {
				t.Errorf("lastError would be %q, want %q", got, tc.want)
			}
			for _, whom := range []string{"991", "ada@example.com", "Ada", "gid://"} {
				if strings.Contains(got, whom) {
					t.Errorf("lastError would say for whom (%q): %q", whom, got)
				}
			}
		})
	}
	// The cause is not lost: it is the error the runner logs.
	if err := failedJob(failScrub, cause); !strings.Contains(err.Error(), "refused: insert") {
		t.Errorf("the failure's own text lost its cause: %v", err)
	}
}

// A request names a customer and its orders by ids of one form, and one that
// does not is refused at the door: the staged delivery's row says why, and no
// job is queued that no run could complete. Shopify sends numbers; a gid is
// accepted too.
func TestAPrivacyRequestWithAnIdThatIsNotAnIdIsRefused(t *testing.T) {
	store := Store{ID: testStoreID, Domain: "acme-widgets.myshopify.com"}
	at := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	redact := func(customer, orders any) (ComplianceJob, error) {
		body := map[string]any{"shop_domain": store.Domain, "customer": map[string]any{"id": customer}}
		if orders != nil {
			body["orders_to_redact"] = orders
		}
		return parseComplianceJob(TopicRedact, store, complianceDelivery(TopicRedact, body), at)
	}
	for name, id := range map[string]any{
		"a number":           991,
		"a number as a word": "991",
		"a gid":              "gid://shopify/Customer/991",
	} {
		job, err := redact(id, []any{299938, "gid://shopify/Order/280263"})
		if err != nil || job.CustomerGID != "gid://shopify/Customer/991" || strings.Join(job.OrderGIDs, ",") != "gid://shopify/Order/299938,gid://shopify/Order/280263" {
			t.Errorf("%s: parsed %q with orders %v, %v", name, job.CustomerGID, job.OrderGIDs, err)
		}
	}
	for name, id := range map[string]any{
		"nothing":                     "",
		"a word":                      "ada",
		"a pattern":                   "99%",
		"a number and more":           "991 OR 1=1",
		"another type's gid":          "gid://shopify/Order/991",
		"a gid with a path":           "gid://shopify/Customer/991/extra",
		"a quote":                     `99"1`,
		"a gid of a customer by name": "gid://shopify/Customer/ada",
	} {
		if job, err := redact(id, []any{}); err == nil {
			t.Errorf("a customer named by %s (%v) was accepted as %q", name, id, job.CustomerGID)
		}
	}
	for name, order := range map[string]any{
		"a word":             "first",
		"a pattern":          "2999_8",
		"another type's gid": "gid://shopify/Customer/299938",
		"an empty word":      "",
	} {
		if job, err := redact(991, []any{299938, order}); err == nil {
			t.Errorf("an order named by %s (%v) was accepted as %v", name, order, job.OrderGIDs)
		}
	}
	// A data request's orders are held to the same form.
	body := map[string]any{"shop_domain": store.Domain, "customer": map[string]any{"id": 991}, "data_request": map[string]any{"id": 42}, "orders_requested": []any{"x"}}
	if _, err := parseComplianceJob(TopicDataRequest, store, complianceDelivery(TopicDataRequest, body), at); err == nil {
		t.Error("a data request naming an order by a word was accepted")
	}
}

// likeEscape makes a value match itself, and only itself, in a LIKE pattern.
// A gid reaches a statement only in its one form (the scope refuses any
// other), so this is the second line; it is held all the same, because the
// first line is somebody else's to keep.
func TestAValueInALikePatternMatchesOnlyItself(t *testing.T) {
	for value, want := range map[string]string{
		"gid://shopify/Customer/123": "gid://shopify/Customer/123",
		"1_3":                        `1\_3`,
		"1%3":                        `1\%3`,
		`1\3`:                        `1\\3`,
		`%_\`:                        `\%\_\\`,
		"[redacted]":                 "[redacted]",
	} {
		if got := likeEscape(value); got != want {
			t.Errorf("likeEscape(%q) = %q, want %q", value, got, want)
		}
	}
}

// The scope refuses a customer or an order whose id is not in its one form,
// whoever calls it: its statements name rows by these ids and match them as
// text.
func TestTheScopeRefusesAnIdThatIsNotAnId(t *testing.T) {
	for _, customer := range []string{"", "gid://shopify/Customer/", "gid://shopify/Customer/1%", `gid://shopify/Customer/1"2`, "gid://shopify/Order/1", "1"} {
		if _, err := erasureScopeOf(context.Background(), nil, testStoreID, customer, nil); err == nil {
			t.Errorf("the scope took %q for a customer", customer)
		}
	}
	for _, order := range []string{"", "gid://shopify/Order/x", "gid://shopify/Customer/1", "gid://shopify/Order/1_"} {
		if _, err := erasureScopeOf(context.Background(), nil, testStoreID, "gid://shopify/Customer/1", []string{order}); err == nil {
			t.Errorf("the scope took %q for an order", order)
		}
	}
	if _, err := erasureScopeOf(context.Background(), nil, "", "gid://shopify/Customer/1", nil); err == nil {
		t.Error("the scope ran with no store id")
	}
}

// The plug-in hands the connector the engine's maintenance session. Nothing
// else does, and without it every privacy job of a node fails for want of a
// session: the jobs do not fall back to the request pool. The factory is
// reached only through the engine's registry, so its source is read, as
// app/maintenance_session_wiring_test.go reads the app's half.
func TestThePlugInHandsTheConnectorTheMaintenanceSession(t *testing.T) {
	src, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "connector.WithMaintenanceSession(pctx.OpenMaintenanceSession)") {
		t.Error("plugin.go no longer hands the connector PluginContext.OpenMaintenanceSession: every privacy job would fail for want of a session to run its statements on")
	}
}

// The class of an error is a word about its kind, for the log, and never its
// text: the text can quote a statement, an address, a whole export.
func TestTheClassOfAnErrorIsNeverItsText(t *testing.T) {
	const secret = "ada@example.com"
	plain := errString("refused: insert for " + secret)
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"nothing":                         {nil, "none"},
		"a failure with no cause":         {failedJob(failNoCustomer, nil), "none"},
		"a deadline":                      {failedJob(failScrub, fmt.Errorf("scrub of %s: %w", secret, context.DeadlineExceeded)), "deadline exceeded"},
		"a cancellation":                  {fmt.Errorf("for %s: %w", secret, context.Canceled), "canceled"},
		"a row another writer changed":    {failedJob(failUnknown, fmt.Errorf("job of %s: %w", secret, memql.ErrRowVersionChanged)), "row version changed"},
		"the origin's refusal":            {failedJob(failReach, &AdminError{StatusCode: 503, Body: secret}), "origin HTTP 503"},
		"the database's refusal":          {failedJob(failScrub, fmt.Errorf("update for %s: %w", secret, sqlStateError{code: "57014", text: secret})), "database SQLSTATE 57014"},
		"an error of no known kind":       {failedJob(failExportWrite, fmt.Errorf("file the export of %s: %w", secret, plain)), "shopify.errString"},
		"the first of several, of a kind": {failedJob(failRestart, errors.Join(fmt.Errorf("order of %s: %w", secret, plain), plain)), "shopify.errString"},
	} {
		got := errorClass(tc.err)
		if got != tc.want {
			t.Errorf("%s: class %q, want %q", name, got, tc.want)
		}
		if strings.Contains(got, secret) || strings.Contains(got, "refused") {
			t.Errorf("%s: the class carries the error's text: %q", name, got)
		}
	}
}

// sqlStateError is an error as the database driver answers one: its SQLSTATE
// under the field 'C', and a message that can quote the statement.
type sqlStateError struct{ code, text string }

func (e sqlStateError) Error() string { return "ERROR: " + e.text + " (SQLSTATE=" + e.code + ")" }
func (e sqlStateError) Field(k byte) string {
	if k == 'C' {
		return e.code
	}
	return e.text
}

// A count the Library's removal answers is read as a whole number or not at
// all: an answer that is missing, or is not a number, must never read as
// "nothing remains".
func TestACountThatIsNotThereIsNotZero(t *testing.T) {
	for name, tc := range map[string]struct {
		answer any
		want   int64
	}{
		"an integer":        {int64(9), 9},
		"a whole float":     {float64(0), 0},
		"a JSON number":     {json.Number("12"), 12},
		"nothing":           {nil, -1},
		"a word":            {"0", -1},
		"a fraction":        {0.5, -1},
		"a negative number": {float64(-1), -1},
		"a number past what a float carries exactly": {1e30, -1},
		"a number that is not one":                   {json.Number("x"), -1},
	} {
		if got := wholeNumber(tc.answer); got != tc.want {
			t.Errorf("%s: read as %d, want %d", name, got, tc.want)
		}
	}
}

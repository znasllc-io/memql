package datasync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	languageparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
	"google.golang.org/protobuf/types/known/structpb"
)

// shopify_redaction_db_test.go -- a customer redaction while a backfill is
// mid-stream, through a real engine, a real Postgres, the real connector, the
// real runner and a fake Shopify transport (ruling 73).
//
// The scrub of customers/redact is one point-in-time update. A backfill whose
// export the origin started before it would, streamed after it, write the
// erased fields back. So the job starts the store's backfills in flight again:
// the old export's remaining lines are never applied, and the export read next
// is one the origin took after the erasure.
//
// Composed here for the reason shopify_partitions_db_test.go is: the runner
// depends on the connector, and the integrations module cannot import back
// into the runtime.

const redactionConcept = "v1:shopify:customer"

// exportingShopify is a Shopify whose every bulk run is a new operation with a
// file of its own: export n is served from /exports/<n>.jsonl on the download
// host, and holds whatever the test says the origin held when it was started.
type exportingShopify struct {
	fake *fakeShopify // the fixture's, for its token table

	mu        stdsync.Mutex
	files     map[int]string // export n -> its JSONL
	next      string         // what the next export started will hold
	started   int
	downloads map[int]int
}

func (s *exportingShopify) RoundTrip(r *http.Request) (*http.Response, error) {
	answer := func(status int, body string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	if r.URL.Host == downloadHost {
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/exports/"), ".jsonl"))
		s.mu.Lock()
		file, ok := s.files[n]
		s.downloads[n]++
		s.mu.Unlock()
		if !ok {
			return answer(http.StatusForbidden, "")
		}
		return answer(http.StatusOK, file)
	}
	if r.URL.Host != "acme-widgets.myshopify.com" {
		return nil, fmt.Errorf("unexpected external request: %s", r.URL.Host)
	}
	s.fake.mu.Lock()
	_, known := s.fake.tokens[r.Header.Get("X-Shopify-Access-Token")]
	s.fake.mu.Unlock()
	if !known {
		return nil, fmt.Errorf("a request carried no token of this test's stores")
	}
	var body struct {
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	switch body.OperationName {
	case "ShopifyBulkRun":
		s.mu.Lock()
		s.started++
		n := s.started
		s.files[n] = s.next
		s.mu.Unlock()
		return answer(http.StatusOK, fmt.Sprintf(`{"data":{"bulkOperationRunQuery":{"bulkOperation":{"id":"gid://shopify/BulkOperation/%d","status":"CREATED"},"userErrors":[]}}}`, n))
	case "ShopifyBulkStatus":
		id, _ := body.Variables["id"].(string)
		n := strings.TrimPrefix(id, "gid://shopify/BulkOperation/")
		return answer(http.StatusOK, fmt.Sprintf(`{"data":{"node":{"id":%q,"status":"COMPLETED","url":"https://%s/exports/%s.jsonl"}}}`, id, downloadHost, n))
	}
	return nil, fmt.Errorf("unexpected Shopify operation: %s", body.OperationName)
}

func (s *exportingShopify) counts() (started int, downloads map[int]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	downloads = map[int]int{}
	for n, c := range s.downloads {
		downloads[n] = c
	}
	return s.started, downloads
}

// redactionEngine is the engine the connector under test reaches. It is the
// real one, except that the sync runtime's queue builtin is answered by this
// test's runtime -- an engine built in a test binds no integration -- through
// the builtin's own handler, so its gate is asked; and that the due privacy
// jobs are narrowed to this test's store, because the database is shared.
type redactionEngine struct {
	memql.IntegrationEngineAccess
	runtime *Integration
	store   string
}

func (e redactionEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(query, "builtin datasyncQueueBackfill(") {
		_, err := e.runtime.handleQueueBackfill(ctx, renderedArgs(query), 0)
		return &memql.ExecuteResult{}, err
	}
	res, err := e.IntegrationEngineAccess.Execute(ctx, query)
	if err != nil || !strings.HasPrefix(query, "complianceJobsDue(") {
		return res, err
	}
	var nodes []*memqlv1.MemoryNode
	for _, row := range memql.MaterializeRows(res) {
		if stringField(row, "storeId") != e.store {
			continue
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		payload := map[string]any{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		id, _ := payload["id"].(string)
		delete(payload, "id")
		pb, err := structpb.NewStruct(payload)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, &memqlv1.MemoryNode{Id: id, Payload: pb})
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}, nil
}

func TestARedactionWhileABackfillIsMidStreamIsNotUndoneByTheOldExport(t *testing.T) {
	f := newPartitionsFixture(t)
	if f == nil {
		return
	}
	store := f.storeIDs[0]
	f.connect(t, store)

	raw := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:customer' AND payload->>'storeId' = $1`,
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:complianceJob' AND payload->>'storeId' = $1`,
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`,
		} {
			_, _ = raw.ExecContext(context.Background(), q, store)
		}
		_ = raw.Close()
	})

	// The origin: two customers before the erasure. The first is in the
	// export's first batch; the second is its line 501, past it.
	const erased, later = "gid://shopify/Customer/8800001", "gid://shopify/Customer/8800002"
	line := func(fields map[string]any) string {
		b, _ := json.Marshal(fields)
		return string(b) + "\n"
	}
	before := line(map[string]any{"id": erased, "firstName": "First", "lastName": "Buyer", "displayName": "First Buyer", "updatedAt": "2026-11-01T10:00:00Z"}) +
		strings.Repeat("~\n", 499) + // filler the stream counts and maps to nothing
		line(map[string]any{"id": later, "firstName": "Second", "lastName": "Buyer", "displayName": "Second Buyer", "updatedAt": "2026-11-01T10:00:00Z"})
	origin := &exportingShopify{fake: f.shopify, files: map[int]string{}, downloads: map[int]int{}, next: before}
	http.DefaultTransport = origin
	t.Cleanup(func() { http.DefaultTransport = f.shopify })

	// The connector as production builds it, over the engine above, with the
	// session the privacy jobs run their statements on: the engine's
	// maintenance session, opened as a node opens it.
	logger := slog.New(slog.DiscardHandler)
	runtime := &Integration{runner: f.runner, logger: logger}
	conn := shopify.NewConnector(redactionEngine{IntegrationEngineAccess: f.eng, runtime: runtime, store: store}, logger, f.stores, shopify.NewAdminClient()).
		WithDatabase(func() *sql.DB { return raw }).
		WithMaintenanceSession(func(ctx context.Context) (*sql.Conn, func(), error) {
			return database.OpenMaintenanceSessionDSN(ctx, dbtest.DSN())
		})
	f.runner.lookup = func(name string) (memqlsync.Connector, bool) { return conn, name == conn.Name() }
	f.runner.bound = func() []string { return []string{shopify.ConnectorName} }
	clock := &passClock{at: time.Now()}
	f.runner.now, f.runner.pause = clock.now, clock.pause

	ctx := context.Background()
	state := func() SyncState {
		t.Helper()
		st, err := f.store.SyncStateFor(OperatorContext(ctx), redactionConcept, shopify.ConnectorName, "inbound", store)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	// versions is every stored version of one customer's row, oldest first.
	versions := func(gid string) []map[string]any {
		t.Helper()
		rows, err := raw.QueryContext(ctx, `SELECT payload::text FROM "MemoryNodes" WHERE concept = 'v1:shopify:customer' AND payload->>'storeId' = $1 AND payload->>'gid' = $2 ORDER BY "createdAt"`, store, gid)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{}
			if err := json.Unmarshal([]byte(text), &payload); err != nil {
				t.Fatal(err)
			}
			out = append(out, payload)
		}
		return out
	}

	// 1. The backfill starts its export and reads the first batch of it: two
	// pages, and the file has not ended.
	f.runner.maxPages = 2
	if _, err := f.runner.StartBackfill(ctx, shopify.ConnectorName, redactionConcept, store); err != nil {
		t.Fatalf("StartBackfill: %v", err)
	}
	mid := state()
	if mid.BackfillStatus != "running" || !strings.Contains(mid.BackfillCursor, `"line":500`) {
		t.Fatalf("the backfill is %s at %q, want running mid-stream at line 500", mid.BackfillStatus, mid.BackfillCursor)
	}
	if got := versions(erased); len(got) != 1 || got[0]["firstName"] != "First" || got[0]["displayName"] != "First Buyer" {
		t.Fatalf("before the redaction the customer's row is %v, want the one version the export wrote", got)
	}

	// 2. The customer is erased at the origin, and the redaction's job comes
	// due here. From now on an export holds the erased state and nothing of
	// the second customer.
	after := line(map[string]any{"id": erased, "displayName": "erased at the origin", "updatedAt": "2026-11-11T09:00:00Z"})
	origin.mu.Lock()
	origin.next = after
	origin.mu.Unlock()
	job := shopify.ComplianceJobID(store, shopify.TopicRedact, erased+"\x00")
	if _, err := f.eng.Execute(auth.ContextWithInternalOrigin(f.owner), fmt.Sprintf(
		`mutation queueComplianceJob(jobId: %s, storeId: %s, topic: %s, customerGid: %s, shopDomain: "acme-widgets.myshopify.com", dueAt: "2026-11-01T00:00:00Z")`,
		languageparser.QuoteString(job), languageparser.QuoteString(store), languageparser.QuoteString(shopify.TopicRedact), languageparser.QuoteString(erased))); err != nil {
		t.Fatalf("queue the redaction: %v", err)
	}
	ran, err := conn.RunDueComplianceJobs(ctx)
	if err != nil || ran != 1 {
		t.Fatalf("RunDueComplianceJobs = %d, %v; want this test's one job run", ran, err)
	}
	for i, v := range versions(erased) {
		for _, field := range []string{"firstName", "lastName", "displayName"} {
			if v[field] != shopify.RedactionMarker {
				t.Fatalf("after the redaction, version %d keeps %s = %v", i, field, v[field])
			}
		}
	}
	restarted := state()
	if restarted.BackfillStatus != "queued" || restarted.BackfillCursor != "" {
		t.Errorf("after the redaction the backfill is %s at %q, want queued again from nothing", restarted.BackfillStatus, restarted.BackfillCursor)
	}

	// 3. The continuing pass carries the backfill on.
	f.runner.maxPages = 500
	res, err := f.runner.ContinueBackfills(ctx)
	if err != nil {
		t.Fatalf("ContinueBackfills: %v", err)
	}
	if res != (ContinueResult{Started: 1, Completed: 1}) {
		t.Errorf("the pass after the redaction = %+v, want the backfill started again and completed", res)
	}
	if st := state(); st.BackfillStatus != "complete" {
		t.Errorf("the backfill is %s, want complete", st.BackfillStatus)
	}

	// The old export was read once, for its first batch, and never after the
	// erasure: its remaining lines were not applied.
	started, downloads := origin.counts()
	if started != 2 || downloads[1] != 1 || downloads[2] != 1 {
		t.Errorf("exports started %d, read %v; want a second export started and read, and the first never read again", started, downloads)
	}
	if got := versions(later); len(got) != 0 {
		t.Errorf("a line of the export taken before the erasure was applied after it: %v", got)
	}
	// And the mirror carries the erased state: no version of the customer's
	// row holds what the old export said, and the newest is the new export's.
	rows := versions(erased)
	if len(rows) < 2 {
		t.Fatalf("the customer's row has %d version(s), want the scrubbed one and the new export's", len(rows))
	}
	for i, v := range rows {
		for _, field := range []string{"firstName", "lastName", "displayName"} {
			if s, _ := v[field].(string); strings.Contains(s, "First") || strings.Contains(s, "Buyer") {
				t.Errorf("version %d of the erased customer holds %s = %q", i, field, s)
			}
		}
	}
	if newest := rows[len(rows)-1]; newest["updatedAt"] != "2026-11-11T09:00:00Z" || newest["displayName"] != "erased at the origin" {
		t.Errorf("the newest version = updatedAt %v, displayName %v; want what the export taken after the erasure holds", newest["updatedAt"], newest["displayName"])
	}
}

package datasync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	languageparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
	"google.golang.org/protobuf/types/known/structpb"
)

// shopify_partitions_db_test.go -- each Shopify store is a partition, through
// a real engine, the real connector and a fake Shopify transport.
//
// Composed here for the reason shopify_propagation_db_test.go is: the runner
// depends on the connector, and the integrations module cannot import back
// into the runtime. Both stores share the harness domain and are told apart by
// their Admin token, which is all the fake transport needs; the store id is
// what makes each a partition.

const partitionsConcept = "v1:shopify:product"

// scopedStoreEngine lets the connector see only this test's stores. The
// database is shared with other packages, and every store it holds would
// otherwise be a partition this test sweeps. The registry renders the read
// bare, as renderCall does, with no `query` keyword.
type scopedStoreEngine struct {
	memql.IntegrationEngineAccess
	stores map[string]bool
}

func (e scopedStoreEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	res, err := e.IntegrationEngineAccess.Execute(ctx, query)
	if err != nil || !strings.HasPrefix(query, "storesForConnector(") {
		return res, err
	}
	var nodes []*memqlv1.MemoryNode
	for _, row := range memql.MaterializeRows(res) {
		id, _ := row["id"].(string)
		if !e.stores[memql.BareShortId(id)] {
			continue
		}
		// Through JSON, so a time the engine hands back as time.Time
		// becomes the string a struct can carry.
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		payload := map[string]any{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		delete(payload, "id")
		pb, err := structpb.NewStruct(payload)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, &memqlv1.MemoryNode{Id: id, Payload: pb})
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}, Meta: res.Meta}, nil
}

// fakeShopify answers the bulk export and the product list, recording each
// request under the store whose token made it.
type fakeShopify struct {
	mu       stdsync.Mutex
	tokens   map[string]string // admin token -> store id
	requests map[string][]fakeShopifyRequest
}

type fakeShopifyRequest struct {
	Operation string
	Variables map[string]any
}

func (f *fakeShopify) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "acme-widgets.myshopify.com" {
		return nil, fmt.Errorf("unexpected external request: %s", r.URL.Host)
	}
	store, ok := f.tokens[r.Header.Get("X-Shopify-Access-Token")]
	if !ok {
		return nil, fmt.Errorf("a request carried no token of this test's stores")
	}
	var body struct {
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.requests[store] = append(f.requests[store], fakeShopifyRequest{Operation: body.OperationName, Variables: body.Variables})
	f.mu.Unlock()
	var data string
	switch body.OperationName {
	case "ShopifyBulkRun":
		data = `{"bulkOperationRunQuery":{"bulkOperation":{"id":"gid://shopify/BulkOperation/` + store + `","status":"CREATED"},"userErrors":[]}}`
	case "ShopifyBulkStatus":
		// Completed with no file: the export matched nothing, which is a
		// finished domain.
		data = `{"node":{"id":"gid://shopify/BulkOperation/` + store + `","status":"COMPLETED","url":null}}`
	case "ShopifyListProduct":
		data = `{"products":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}`
	default:
		return nil, fmt.Errorf("unexpected Shopify operation: %s", body.OperationName)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"data":` + data + `}`)), Request: r}, nil
}

func (f *fakeShopify) ops(store, op string) []fakeShopifyRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeShopifyRequest
	for _, r := range f.requests[store] {
		if r.Operation == op {
			out = append(out, r)
		}
	}
	return out
}

// partitionsFixture is a real engine, the real Shopify connector over this
// test's stores, and a runner bound to it.
type partitionsFixture struct {
	eng      *memql.MemQLEngine
	owner    context.Context
	scope    map[string]bool
	stores   *shopify.StoreRegistry
	shopify  *fakeShopify
	runner   *Runner
	store    *Store
	storeIDs []string
	conn     *shopify.Connector
}

func newPartitionsFixture(t *testing.T) *partitionsFixture {
	t.Helper()
	eng, db := healthEngine(t)
	if eng == nil {
		return nil
	}
	run := fmt.Sprintf("dbtest-partition-%d", time.Now().UnixNano())
	f := &partitionsFixture{
		eng:     eng,
		scope:   map[string]bool{},
		shopify: &fakeShopify{tokens: map[string]string{}, requests: map[string][]fakeShopifyRequest{}},
	}
	f.storeIDs = []string{run + "-a", run + "-b"}
	foreign := run + "-foreign"
	t.Cleanup(func() {
		for _, id := range append([]string{foreign}, f.storeIDs...) {
			_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND id = $1`, "v1:shopify:store:"+id)
			_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'partitionKey' = $1`, id)
		}
	})
	claims := map[string]any{"sub": "dbtest-owner", "role": "owner"}
	owner := auth.ContextWithClaims(context.Background(), claims)
	owner = auth.ContextWithToken(owner, auth.BuildTokenInfo(claims))
	f.owner = auth.ContextWithAccess(owner, &auth.AccessContext{UserId: "dbtest-owner", Role: auth.RoleOwner})
	// A store that is not this test's, as the shared database may hold: the
	// scoped registry must keep it from being a partition.
	if _, err := eng.Execute(f.owner, fmt.Sprintf(`mutation createStore(storeId: %s, domain: "acme-widgets.myshopify.com", adminTokenRef: "foreign-admin", apiVersion: "2026-07")`,
		languageparser.QuoteString(foreign))); err != nil {
		t.Fatalf("write the foreign store: %v", err)
	}

	previous := http.DefaultTransport
	http.DefaultTransport = f.shopify
	t.Cleanup(func() { http.DefaultTransport = previous })

	f.stores = shopify.NewStoreRegistry(scopedStoreEngine{IntegrationEngineAccess: eng, stores: f.scope}, func(_ context.Context, ref string) (string, error) {
		return "token-" + ref, nil
	})
	logger := slog.New(slog.DiscardHandler)
	connector := shopify.NewConnector(eng, logger, f.stores, shopify.NewAdminClient())
	f.conn = connector
	f.store = NewStore(engineSeam{eng})
	f.runner = NewRunner(f.store, NewApplier(f.store, NewEngineMirrorWriter(engineSeam{eng})), logger)
	f.runner.lookup = func(name string) (memqlsync.Connector, bool) {
		return connector, name == connector.Name()
	}
	return f
}

// connect writes one of this test's stores and makes it visible.
func (f *partitionsFixture) connect(t *testing.T, id string) {
	t.Helper()
	ref := id + "-admin"
	if _, err := f.eng.Execute(f.owner, fmt.Sprintf(`mutation createStore(storeId: %s, domain: "acme-widgets.myshopify.com", adminTokenRef: %s, apiVersion: "2026-07")`,
		languageparser.QuoteString(id), languageparser.QuoteString(ref))); err != nil {
		t.Fatalf("write test store %s: %v", id, err)
	}
	f.shopify.mu.Lock()
	f.shopify.tokens["token-"+ref] = id
	f.shopify.mu.Unlock()
	f.scope[id] = true
	f.stores.Invalidate()
}

// assertPartitions holds the connector to exactly this test's stores, so a
// store filter that does nothing cannot pass.
func (f *partitionsFixture) assertPartitions(t *testing.T, want ...string) {
	t.Helper()
	got, err := f.conn.Partitions(context.Background())
	if err != nil {
		t.Fatalf("Partitions: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Partitions = %v, want exactly this test's stores %v", got, want)
	}
}

func (f *partitionsFixture) state(t *testing.T, partition string) SyncState {
	t.Helper()
	st, err := f.store.SyncStateFor(OperatorContext(context.Background()), partitionsConcept, shopify.ConnectorName, "inbound", partition)
	if err != nil {
		t.Fatalf("SyncStateFor(%s): %v", partition, err)
	}
	return st
}

// backfillUntilDone drives the fan-out until every partition is complete: the
// export is asynchronous, so the first call only starts it.
func (f *partitionsFixture) backfillUntilDone(t *testing.T) {
	t.Helper()
	for i := 0; i < 5; i++ {
		res, err := f.runner.StartBackfill(context.Background(), shopify.ConnectorName, partitionsConcept, "")
		if err != nil {
			t.Fatalf("StartBackfill: %v", err)
		}
		if res.Done {
			return
		}
	}
	t.Fatal("the backfill did not finish")
}

func TestTheSecondStoreBackfillsAfterTheFirstIsComplete(t *testing.T) {
	f := newPartitionsFixture(t)
	if f == nil {
		return
	}
	a, b := f.storeIDs[0], f.storeIDs[1]

	f.connect(t, a)
	f.assertPartitions(t, a)
	f.backfillUntilDone(t)
	if st := f.state(t, a); st.BackfillStatus != "complete" {
		t.Fatalf("store A's backfill = %q, want complete", st.BackfillStatus)
	}

	f.connect(t, b)
	f.assertPartitions(t, a, b)
	if st := f.state(t, b); st.Stored {
		t.Fatalf("store B has a row before it was ever backfilled: %+v", st)
	}
	res, err := f.runner.StartBackfill(context.Background(), shopify.ConnectorName, partitionsConcept, b)
	if err != nil {
		t.Fatalf("StartBackfill(B): %v", err)
	}
	if res.Pages == 0 || !res.Done {
		t.Fatalf("store B's backfill = %+v, want its own export run to the end", res)
	}
	if runs := f.shopify.ops(b, "ShopifyBulkRun"); len(runs) != 1 {
		t.Fatalf("store B started %d exports, want its own one -- it inherited store A's finished backfill", len(runs))
	}
	stB := f.state(t, b)
	if stB.BackfillStatus != "complete" {
		t.Fatalf("store B's backfill = %q, want complete", stB.BackfillStatus)
	}
	if stB.Partition != b || f.state(t, a).Partition != a {
		t.Fatalf("the two stores' rows are not their own: B=%q", stB.Partition)
	}
}

func TestAPausedStoreIsSweptFromWhereItStopped(t *testing.T) {
	f := newPartitionsFixture(t)
	if f == nil {
		return
	}
	a, b := f.storeIDs[0], f.storeIDs[1]
	f.connect(t, a)
	f.connect(t, b)
	f.assertPartitions(t, a, b)

	clock := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	f.runner.now = func() time.Time { return clock }
	sweep := func() {
		t.Helper()
		if _, err := f.runner.Reconcile(context.Background(), shopify.ConnectorName, partitionsConcept, ""); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	sweep()
	aStopped := f.state(t, a).LastReconcileAt
	if aStopped.IsZero() {
		t.Fatal("store A's first sweep left no watermark")
	}

	if err := f.runner.SetPaused(context.Background(), shopify.ConnectorName, partitionsConcept, a, true); err != nil {
		t.Fatalf("pause A: %v", err)
	}
	for i := 0; i < 2; i++ {
		clock = clock.Add(time.Hour)
		sweep()
	}
	if got := f.state(t, a).LastReconcileAt; !got.Equal(aStopped) {
		t.Fatalf("a sweep of B moved paused A's watermark from %s to %s", aStopped, got)
	}
	bLast := f.state(t, b).LastReconcileAt
	if !bLast.After(aStopped) {
		t.Fatalf("store B's watermark %s did not move past A's %s", bLast, aStopped)
	}
	if n := len(f.shopify.ops(a, "ShopifyListProduct")); n != 1 {
		t.Fatalf("paused store A was listed %d times, want only its first sweep", n)
	}

	if err := f.runner.SetPaused(context.Background(), shopify.ConnectorName, partitionsConcept, a, false); err != nil {
		t.Fatalf("resume A: %v", err)
	}
	clock = clock.Add(time.Hour)
	sweep()

	lists := f.shopify.ops(a, "ShopifyListProduct")
	if len(lists) != 2 {
		t.Fatalf("store A was listed %d times, want twice", len(lists))
	}
	filter, _ := lists[1].Variables["query"].(string)
	want := "updated_at:>'" + aStopped.UTC().Format(time.RFC3339) + "'"
	if filter != want {
		t.Fatalf("resumed store A swept with %q, want its own last sweep %q (store B's was %s)", filter, want, bLast.UTC().Format(time.RFC3339))
	}
}

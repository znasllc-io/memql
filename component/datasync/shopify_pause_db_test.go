package datasync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	languageparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// shopify_pause_db_test.go -- a store paused while a continuing pass runs,
// through a real engine, the real connector and a fake Shopify transport.
//
// Composed here for the reason shopify_partitions_db_test.go is: the runner
// depends on the connector, and the integrations module cannot import back
// into the runtime.

// steeredShopify stands in front of the fixture's fake Shopify: it tells the
// test of every Admin request before it is answered, and answers the status of
// a store's export as still running for as long as the test says so.
type steeredShopify struct {
	inner http.RoundTripper
	fake  *fakeShopify

	mu        stdsync.Mutex
	preparing map[string]bool
	before    func(store, operation string)
}

func (s *steeredShopify) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body struct {
		OperationName string `json:"operationName"`
	}
	_ = json.Unmarshal(raw, &body)
	s.fake.mu.Lock()
	store := s.fake.tokens[r.Header.Get("X-Shopify-Access-Token")]
	s.fake.mu.Unlock()
	s.mu.Lock()
	before, preparing := s.before, s.preparing[store]
	s.mu.Unlock()
	if before != nil {
		before(store, body.OperationName)
	}
	if body.OperationName == "ShopifyBulkStatus" && preparing {
		answer := `{"data":{"node":{"id":"gid://shopify/BulkOperation/` + store + `","status":"RUNNING","url":null}}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(answer)), Request: r}, nil
	}
	return s.inner.RoundTrip(r)
}

func (s *steeredShopify) setPreparing(store string, preparing bool) {
	s.mu.Lock()
	s.preparing[store] = preparing
	s.mu.Unlock()
}

// A pause that lands while a pass runs: the store was listed as a partition,
// and by the time its step is asked it no longer ingests. Its backfill keeps
// its status and its cursor, and the pass after the resume carries it on from
// there -- the same export, not a new one.
func TestAStorePausedWhileAPassRunsKeepsItsBackfillAndIsContinuedAfterItResumes(t *testing.T) {
	f := newPartitionsFixture(t)
	if f == nil {
		return
	}
	a, b := f.storeIDs[0], f.storeIDs[1]
	f.connect(t, a)
	f.connect(t, b)
	f.assertPartitions(t, a, b)

	steer := &steeredShopify{inner: f.shopify, fake: f.shopify, preparing: map[string]bool{a: true, b: true}}
	http.DefaultTransport = steer
	t.Cleanup(func() { http.DefaultTransport = f.shopify })

	f.runner.bound = func() []string { return []string{shopify.ConnectorName} }
	clock := &passClock{at: time.Now()}
	f.runner.now, f.runner.pause = clock.now, clock.pause
	setStatus := func(store, status string) {
		t.Helper()
		if _, err := f.eng.Execute(f.owner, fmt.Sprintf(`mutation setStoreStatus(storeId: %s, status: %s)`,
			languageparser.QuoteString(store), languageparser.QuoteString(status))); err != nil {
			t.Fatalf("set store %s to %s: %v", store, status, err)
		}
		f.stores.Invalidate()
	}

	ctx := context.Background()
	if err := f.runner.QueueBackfill(ctx, shopify.ConnectorName, partitionsConcept, ""); err != nil {
		t.Fatalf("QueueBackfill: %v", err)
	}

	// Store b is paused the second time store a's export is polled: after the
	// pass listed both stores and started both exports, and before store b's
	// own step of that round.
	polls := 0
	steer.mu.Lock()
	steer.before = func(store, operation string) {
		if store != a || operation != "ShopifyBulkStatus" {
			return
		}
		if polls++; polls == 2 {
			setStatus(b, "paused")
		}
	}
	steer.mu.Unlock()

	res, err := f.runner.ContinueBackfills(ctx)
	if err != nil {
		t.Fatalf("the pass a pause landed in failed: %v", err)
	}
	if res != (ContinueResult{Started: 2}) {
		t.Errorf("the pass = %+v, want both exports started and neither done", res)
	}
	paused := f.state(t, b)
	if paused.BackfillStatus != "running" || !strings.Contains(paused.BackfillCursor, "gid://shopify/BulkOperation/"+b) || paused.LastError != "" {
		t.Fatalf("the paused store's row = %s at %q, lastError %q; want it left running at its export's cursor with no error",
			paused.BackfillStatus, paused.BackfillCursor, paused.LastError)
	}
	if n := len(f.shopify.ops(b, "ShopifyBulkRun")); n != 1 {
		t.Fatalf("store b started %d exports, want its one", n)
	}

	// Resumed, and both exports ready: the next pass reads each to its end.
	setStatus(b, "live")
	steer.setPreparing(a, false)
	steer.setPreparing(b, false)
	steer.mu.Lock()
	steer.before = nil
	steer.mu.Unlock()
	res, err = f.runner.ContinueBackfills(ctx)
	if err != nil {
		t.Fatalf("the pass after the resume failed: %v", err)
	}
	if res != (ContinueResult{Completed: 2}) {
		t.Errorf("the pass after the resume = %+v, want both backfills completed", res)
	}
	if st := f.state(t, b); st.BackfillStatus != "complete" || st.LastError != "" {
		t.Errorf("the resumed store's row = %s, lastError %q; want complete", st.BackfillStatus, st.LastError)
	}
	if n := len(f.shopify.ops(b, "ShopifyBulkRun")); n != 1 {
		t.Errorf("store b started %d exports, want still its one: the resume continues it", n)
	}
}

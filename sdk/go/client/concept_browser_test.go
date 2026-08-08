package client

import (
	"context"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

// Reuses the mockStream helper from dispatcher_test.go (same package).

// TestBrowseConceptPage_RoundTrip proves the keyset-paginated concept-browse
// wrapper (1) declares sort + paginate so the query opts OUT of the engine's
// implicit 50-row unmarked-list backstop and into a keyset window, (2) stamps
// the inbound continuation cursor on ExecuteQueryMsg.cursor, and (3) returns
// the engine's nextCursor + the page rows (full nested node shape). This is
// the contract the cockpit Concepts tab relies on to page past 50 rows
// (memql#2008).
func TestBrowseConceptPage_RoundTrip(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	qc := NewQueryClient(d)

	type out struct {
		page *PageResult
		err  error
	}
	done := make(chan out, 1)
	go func() {
		p, err := qc.BrowseConceptPage(context.Background(), "v1:cognition:utterance", "cursor-from-page-1", 200)
		done <- out{page: p, err: err}
	}()

	sent := <-stream.sendCh
	q := sent.GetExecuteQuery()
	if q == nil {
		t.Fatalf("expected ExecuteQueryMsg payload, got %+v", sent.GetPayload())
	}
	wantQuery := `sort(paginate(concept==v1:cognition:utterance, 200), "createdAt", "asc")`
	if q.GetQuery() != wantQuery {
		t.Errorf("query: want %q, got %q (must declare sort+paginate to escape the 50-row backstop)", wantQuery, q.GetQuery())
	}
	if q.GetCursor() != "cursor-from-page-1" {
		t.Errorf("cursor: want %q, got %q (inbound cursor must ride the request)", "cursor-from-page-1", q.GetCursor())
	}

	// Full nested MemoryNode shape (bundle path) -- payload + intrinsics
	// survive, which the cockpit's generic inspector needs.
	payload, _ := structpb.NewStruct(map[string]any{"text": "hello"})
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_QueryResult{
			QueryResult: &memqlv1.QueryResultChunk{
				Result: &memqlv1.Result{
					Bundle: &memqlv1.GraphBundle{
						Nodes: []*memqlv1.MemoryNode{{
							Id:      "v1:cognition:utterance:u-a",
							Concept: "v1:cognition:utterance",
							Payload: payload,
						}},
					},
					Meta: &memqlv1.ResultMeta{Cursor: "cursor-to-page-2", HasMore: true},
				},
			},
		},
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("BrowseConceptPage: %v", got.err)
		}
		if got.page.NextCursor != "cursor-to-page-2" {
			t.Errorf("NextCursor: want cursor-to-page-2, got %q", got.page.NextCursor)
		}
		if !got.page.HasMore {
			t.Errorf("HasMore: want true")
		}
		if len(got.page.Rows) != 1 {
			t.Fatalf("rows: want 1, got %d", len(got.page.Rows))
		}
		if RowString(got.page.Rows[0], "id") != "v1:cognition:utterance:u-a" {
			t.Errorf("row id intrinsic not carried: %+v", got.page.Rows[0])
		}
		if p, ok := got.page.Rows[0]["payload"].(map[string]any); !ok || p["text"] != "hello" {
			t.Errorf("nested payload dropped (cockpit inspector needs it): %+v", got.page.Rows[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for BrowseConceptPage to return")
	}
}

// TestBrowseConceptPage_DefaultPageSize proves a non-positive pageSize falls
// back to DefaultConceptBrowsePageSize in the emitted paginate() literal.
func TestBrowseConceptPage_DefaultPageSize(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	qc := NewQueryClient(d)

	go func() { _, _ = qc.BrowseConceptPage(context.Background(), "v1:cluster:node", "", 0) }()

	sent := <-stream.sendCh
	q := sent.GetExecuteQuery()
	wantQuery := `sort(paginate(concept==v1:cluster:node, 200), "createdAt", "asc")`
	if q.GetQuery() != wantQuery {
		t.Errorf("query: want %q, got %q (pageSize<=0 must use DefaultConceptBrowsePageSize=%d)", wantQuery, q.GetQuery(), DefaultConceptBrowsePageSize)
	}
	if q.GetCursor() != "" {
		t.Errorf("cursor: want empty on first page, got %q", q.GetCursor())
	}
	// Drain so the dispatcher waiter completes cleanly.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_QueryResult{
			QueryResult: &memqlv1.QueryResultChunk{Result: &memqlv1.Result{}},
		},
	}
}

// TestBrowseConcept_RequiresConceptId guards the empty-id error on both forms.
func TestBrowseConcept_RequiresConceptId(t *testing.T) {
	qc := NewQueryClient(nil)
	if _, err := qc.BrowseConcept(context.Background(), ""); err == nil {
		t.Error("BrowseConcept(\"\"): want error")
	}
	if _, err := qc.BrowseConceptPage(context.Background(), "", "", 0); err == nil {
		t.Error("BrowseConceptPage(\"\"): want error")
	}
}

// TestGetRowByConceptAndId_UsesCanonicalAndConnective pins the filter this
// SDK emits for a single-row lookup.
//
// The two clauses used to be joined with the legacy `;`-AND separator, which
// memql#971 retired in favour of the one Go boolean grammar -- and which the
// TS SDK's getRowByConceptAndId had already moved to. Nothing downstream
// noticed, because both spellings still parse; that is exactly why it needs a
// test rather than a comment. Two SDKs spelling the same two-clause filter
// differently is a difference every reader has to stop and confirm is not
// meaningful.
func TestGetRowByConceptAndId_UsesCanonicalAndConnective(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	qc := NewQueryClient(d)

	go func() { _, _ = qc.GetRowByConceptAndId(context.Background(), "v1:agents:agent", "a1") }()

	sent := <-stream.sendCh
	q := sent.GetExecuteQuery()
	if q == nil {
		t.Fatalf("expected ExecuteQueryMsg payload, got %+v", sent.GetPayload())
	}
	wantQuery := "concept==v1:agents:agent && id==a1"
	if q.GetQuery() != wantQuery {
		t.Errorf("query: want %q, got %q (the retired `;`-AND form must not come back)", wantQuery, q.GetQuery())
	}

	// Drain so the dispatcher waiter completes cleanly.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_QueryResult{
			QueryResult: &memqlv1.QueryResultChunk{Result: &memqlv1.Result{}},
		},
	}
}

// TestGetRowByConceptAndId_RequiresBothIds guards the empty-argument errors.
func TestGetRowByConceptAndId_RequiresBothIds(t *testing.T) {
	qc := NewQueryClient(nil)
	if _, err := qc.GetRowByConceptAndId(context.Background(), "", "a1"); err == nil {
		t.Error("GetRowByConceptAndId(\"\", \"a1\"): want error")
	}
	if _, err := qc.GetRowByConceptAndId(context.Background(), "v1:agents:agent", ""); err == nil {
		t.Error("GetRowByConceptAndId(concept, \"\"): want error")
	}
}

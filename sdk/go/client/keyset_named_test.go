package client

import (
	"context"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
)

// Reuses the mockStream helper from dispatcher_test.go (same package).

// TestQueryAllPlansPage_RoundTrip proves the keyset-paginated wrapper
// (1) sends the named query string with the inbound continuation cursor
// stamped on ExecuteQueryMsg.cursor, and (2) returns the engine's
// nextCursor + the page rows. This is the contract the cockpit Planner
// "load more" relies on; the plain generated QueryAllPlans can't carry a
// cursor either way.
func TestQueryAllPlansPage_RoundTrip(t *testing.T) {
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
		p, err := qc.QueryAllPlansPage(context.Background(), QueryAllPlansArgs{}, "cursor-from-page-1")
		done <- out{page: p, err: err}
	}()

	sent := <-stream.sendCh
	q := sent.GetExecuteQuery()
	if q == nil {
		t.Fatalf("expected ExecuteQueryMsg payload, got %+v", sent.GetPayload())
	}
	if q.GetQuery() != "queryAllPlans({})" {
		t.Errorf("query: want %q, got %q (must stay on the named-primitive surface)", "queryAllPlans({})", q.GetQuery())
	}
	if q.GetCursor() != "cursor-from-page-1" {
		t.Errorf("cursor: want %q, got %q (inbound cursor must ride the request)", "cursor-from-page-1", q.GetCursor())
	}

	row, _ := structpb.NewStruct(map[string]any{
		"id":     "v1:planner:plan:plan-a",
		"status": "succeeded",
	})
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_QueryResult{
			QueryResult: &memqlv1.QueryResultChunk{
				Result: &memqlv1.Result{
					Data: []*structpb.Value{structpb.NewStructValue(row)},
					Meta: &memqlv1.ResultMeta{Cursor: "cursor-to-page-2", HasMore: true},
				},
			},
		},
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("QueryAllPlansPage: %v", got.err)
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
		if RowString(got.page.Rows[0], "status") != "succeeded" {
			t.Errorf("row status not carried: %+v", got.page.Rows[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for QueryAllPlansPage to return")
	}
}

// TestQueryAllPlansPage_LastPageNoCursor proves an exhausted set comes
// back with an empty NextCursor so the "load more" affordance hides.
func TestQueryAllPlansPage_LastPageNoCursor(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	qc := NewQueryClient(d)

	done := make(chan *PageResult, 1)
	go func() {
		p, _ := qc.QueryAllPlansPage(context.Background(), QueryAllPlansArgs{}, "")
		done <- p
	}()

	sent := <-stream.sendCh
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_QueryResult{
			QueryResult: &memqlv1.QueryResultChunk{
				Result: &memqlv1.Result{
					Data: []*structpb.Value{},
					Meta: &memqlv1.ResultMeta{Cursor: "", HasMore: false},
				},
			},
		},
	}

	select {
	case p := <-done:
		if p.NextCursor != "" {
			t.Errorf("NextCursor: want empty on exhausted set, got %q", p.NextCursor)
		}
		if p.HasMore {
			t.Errorf("HasMore: want false on exhausted set")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
}

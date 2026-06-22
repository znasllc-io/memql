package pat

import (
	"context"
	"fmt"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// pagedFakeEngine returns a fixed sequence of keyset pages, one per
// Execute call, stamping a non-empty Meta.Cursor on every page except
// the last. It satisfies identity.EngineExecutor. The inbound cursor
// rides the context opaquely (cursorFromContext is unexported), so the
// fake advances by call count -- which is exactly the drain contract:
// keep calling until the engine stops handing back a cursor.
type pagedFakeEngine struct {
	pages [][]string // per-page list of PAT ids
	calls int
}

func (f *pagedFakeEngine) Execute(_ context.Context, _ string) (*memqlengine.ExecuteResult, error) {
	idx := f.calls
	f.calls++
	if idx >= len(f.pages) {
		return &memqlengine.ExecuteResult{Meta: &memqlengine.ResultMeta{}}, nil
	}
	nodes := make([]*memqlv1.MemoryNode, 0, len(f.pages[idx]))
	for _, id := range f.pages[idx] {
		nodes = append(nodes, &memqlv1.MemoryNode{
			Id: id,
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":     structpb.NewStringValue(id),
				"active": structpb.NewBoolValue(true),
			}},
		})
	}
	next := ""
	if idx < len(f.pages)-1 {
		next = fmt.Sprintf("cursor-page-%d", idx+1)
	}
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{Nodes: nodes},
		Meta:   &memqlengine.ResultMeta{Cursor: next},
	}, nil
}

// TestListForUserDrainsAllKeysetPages proves ListForUser walks the
// keyset cursor across every page rather than stopping at the first 50
// (queryPatIdentitiesForUser is `paginate 50`). A user with PATs spread
// across three pages must surface all of them -- the admin token roll-up
// and the revoke ownership check both depend on the COMPLETE set.
func TestListForUserDrainsAllKeysetPages(t *testing.T) {
	eng := &pagedFakeEngine{pages: [][]string{
		{"v1:identity:identity:pat-a", "v1:identity:identity:pat-b"},
		{"v1:identity:identity:pat-c", "v1:identity:identity:pat-d"},
		{"v1:identity:identity:pat-e"},
	}}
	s := &Store{Engine: eng}

	rows, err := s.ListForUser(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("ListForUser returned %d rows, want 5 (all pages drained)", len(rows))
	}
	if eng.calls != 3 {
		t.Fatalf("engine called %d times, want 3 (one per page until cursor exhausted)", eng.calls)
	}
}

// TestListForUserPageReturnsSinglePageAndCursor proves the bounded
// /me/tokens path fetches exactly one page and surfaces the continuation
// cursor for "load more" rather than draining the whole set.
func TestListForUserPageReturnsSinglePageAndCursor(t *testing.T) {
	eng := &pagedFakeEngine{pages: [][]string{
		{"v1:identity:identity:pat-a", "v1:identity:identity:pat-b"},
		{"v1:identity:identity:pat-c"},
	}}
	s := &Store{Engine: eng}

	rows, next, err := s.ListForUserPage(context.Background(), "user-1", "")
	if err != nil {
		t.Fatalf("ListForUserPage: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("first page returned %d rows, want 2", len(rows))
	}
	if next == "" {
		t.Fatalf("expected a non-empty next cursor when more pages remain")
	}
	if eng.calls != 1 {
		t.Fatalf("engine called %d times, want 1 (single bounded page)", eng.calls)
	}
}

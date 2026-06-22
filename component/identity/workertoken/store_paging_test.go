package workertoken

import (
	"context"
	"fmt"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// pagedFakeEngine returns one keyset page per Execute call, stamping a
// non-empty Meta.Cursor on every page except the last. Satisfies
// identity.EngineExecutor.
type pagedFakeEngine struct {
	pages [][]string // per-page list of worker-token ids
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
				"userId": structpb.NewStringValue("user-1"),
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
// keyset cursor across every page (queryWorkerTokensForUser is
// `paginate 50`). The revoke ownership check fans out over this list, so
// a worker token on page 2+ must still be found-as-owned rather than
// silently rejected.
func TestListForUserDrainsAllKeysetPages(t *testing.T) {
	eng := &pagedFakeEngine{pages: [][]string{
		{"v1:identity:identity:wt-a", "v1:identity:identity:wt-b"},
		{"v1:identity:identity:wt-c", "v1:identity:identity:wt-d"},
		{"v1:identity:identity:wt-e"},
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
	want := "v1:identity:identity:wt-e"
	found := false
	for _, r := range rows {
		if r.ID == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("last-page token %q missing -- ownership check would falsely reject it", want)
	}
}

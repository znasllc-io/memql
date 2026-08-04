package workertoken

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// originRecordingEngine records the CallOrigin of every context it is
// handed, so a test can assert what the STORE stamped rather than what the
// test itself stamped. Satisfies identity.EngineExecutor.
type originRecordingEngine struct {
	sawInternal []bool
}

func (f *originRecordingEngine) Execute(ctx context.Context, _ string) (*memqlengine.ExecuteResult, error) {
	f.sawInternal = append(f.sawInternal, auth.OriginFromContext(ctx).IsInternal())
	return &memqlengine.ExecuteResult{
		Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
			Id: "v1:identity:identity:wt-a",
			Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
				"id":     structpb.NewStringValue("v1:identity:identity:wt-a"),
				"userId": structpb.NewStringValue("user-1"),
				"active": structpb.NewBoolValue(true),
			}},
		}}},
		Meta: &memqlengine.ResultMeta{},
	}, nil
}

// TestListForUserStampsInternalOriginItself pins the PRODUCTION half of
// memql#3063. workerTokensForUser is @serverOnly, so the engine refuses it
// unless the call says it is server-initiated -- and the only place that gets
// said is inside ListForUser.
//
// The DB-gated neighbour of this test (component/memql's
// TestWorkerTokensForUserIsServerOnlyAndInternalOriginPasses) asserts the
// ENGINE side: a client-origin call is refused, an internal-origin one is
// allowed. It cannot pin this half, because it stamps internal origin at its
// own call site and never routes through ListForUser -- delete the stamp in
// store.go and that test stays green while worker-token revoke breaks at
// runtime. Measured, not assumed. This test is the half that fails.
//
// So: hand ListForUser a plain client-origin context -- the shape the revoke
// handler in component/grpc/worker_token_handlers.go actually calls it with --
// and assert the engine saw internal.
func TestListForUserStampsInternalOriginItself(t *testing.T) {
	eng := &originRecordingEngine{}
	s := &Store{Engine: eng}

	clientCtx := auth.ContextWithClientOrigin(context.Background())
	if _, err := s.ListForUser(clientCtx, "user-1"); err != nil {
		t.Fatalf("ListForUser: %v", err)
	}

	if len(eng.sawInternal) == 0 {
		t.Fatal("ListForUser never reached the engine, so this test asserts nothing")
	}
	for i, internal := range eng.sawInternal {
		if !internal {
			t.Fatalf("ListForUser passed a NON-internal context to the engine on page %d. "+
				"workerTokensForUser is @serverOnly (memql#3063), so the real engine refuses "+
				"this read and the per-user revoke ownership check in "+
				"component/grpc/worker_token_handlers.go fails closed at runtime -- every "+
				"revoke rejected as not-owned. The stamp in ListForUser is the only thing "+
				"that makes the @serverOnly annotation survivable; it is gone or it moved.", i)
		}
	}

	// The stamp must not have widened the CALLER's context. Internal origin
	// opens every @serverOnly construct for as long as the context lives,
	// which is the escalation memql#2989 refused, and it is the claim this
	// package's line in the root call_origin_conformance_test.go allowlist
	// makes in as many words.
	if auth.OriginFromContext(clientCtx).IsInternal() {
		t.Error("ListForUser stamped internal origin onto the CALLER's context. That hands the " +
			"rest of the request every @serverOnly construct in the tree (memql#2989); the " +
			"stamp belongs on a context scoped to this one query.")
	}
}

package pat

import (
	"context"
	"strings"
	"sync"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// A PAT-authenticated request writes ONE row, and it goes on the activity log
// (memql#4328).
//
// This is the highest-volume writer of the four: one row per REQUEST, not per
// session and not per rotation. A CI job polling every few seconds put a row in
// the operator's Audit Trail every few seconds, which is what made the trail
// unreadable alongside the rotation storm.
//
// The REJECTIONS stay on the audit log. `pat_auth_rejected` means a revoked
// credential or a suspended user was used -- that is a decision and a signal,
// not a mechanic, and it is exactly what an operator scans the trail for.

type patAuditRecorder struct {
	mu     sync.Mutex
	events []identity.AuditEvent
}

func (r *patAuditRecorder) Log(_ context.Context, ev identity.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

// patFakeEngine answers the PAT row lookup and the lastUsedAt bump.
type patFakeEngine struct {
	keyHash string
	active  bool
}

func (f *patFakeEngine) Execute(_ context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if !strings.Contains(q, "patIdentityByKeyHash(") {
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	node := &memqlv1.MemoryNode{
		Id: "v1:identity:identity:pat-1",
		Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
			"id":     structpb.NewStringValue("v1:identity:identity:pat-1"),
			"userId": structpb.NewStringValue("v1:identity:user:ci-1"),
			"active": structpb.NewBoolValue(f.active),
			"label":  structpb.NewStringValue("ci-runner"),
		}},
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{node}}}, nil
}

type stubUserLookup struct{ active bool }

func (s stubUserLookup) UserById(_ context.Context, id string) (*UserSummary, error) {
	return &UserSummary{
		ID:           id,
		PrimaryEmail: "ci@example.com",
		Role:         "writer",
		Active:       s.active,
	}, nil
}

func TestPATAcceptanceWritesActivityAndRejectionWritesAudit(t *testing.T) {
	token, hash, err := Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	accepted := &patAuditRecorder{}
	v := &Verifier{
		Store: &Store{Engine: &patFakeEngine{keyHash: hash, active: true}},
		Users: stubUserLookup{active: true},
		Audit: accepted,
	}
	if _, err := v.VerifyToken(context.Background(), token); err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if len(accepted.events) != 1 {
		t.Fatalf("an accepted PAT wrote %d row(s), want 1: %+v", len(accepted.events), accepted.events)
	}
	ev := accepted.events[0]
	if ev.Action != "pat_auth_accepted" {
		t.Fatalf("action = %q, want pat_auth_accepted", ev.Action)
	}
	if ev.Stream != identity.StreamActivity {
		t.Errorf("pat_auth_accepted landed on the %q stream, want the activity stream -- this is "+
			"one row per REQUEST, and on the audit log it is what makes the operator's Trail "+
			"unreadable (memql#4328)", ev.Stream)
	}
	if ev.ActorEmail == "" || ev.ActorRole == "" {
		t.Errorf("actor = (%q, %q); the row must name who authenticated", ev.ActorEmail, ev.ActorRole)
	}
	if ev.ActorIdentity != "v1:identity:identity:pat-1" {
		t.Errorf("actorIdentity = %q, want the PAT row -- it is what distinguishes one CI "+
			"credential's traffic from another's", ev.ActorIdentity)
	}

	// A REVOKED PAT is a decision, and it stays where an operator looks.
	rejected := &patAuditRecorder{}
	v = &Verifier{
		Store: &Store{Engine: &patFakeEngine{keyHash: hash, active: false}},
		Users: stubUserLookup{active: true},
		Audit: rejected,
	}
	if _, err := v.VerifyToken(context.Background(), token); err == nil {
		t.Fatal("a revoked PAT verified")
	}
	if len(rejected.events) != 1 || rejected.events[0].Action != "pat_auth_rejected" {
		t.Fatalf("rejection wrote %+v, want one pat_auth_rejected", rejected.events)
	}
	if rejected.events[0].Stream != identity.StreamAudit {
		t.Errorf("pat_auth_rejected landed on the %q stream; a revoked credential being used is "+
			"a security signal and belongs on the audit log", rejected.events[0].Stream)
	}
}

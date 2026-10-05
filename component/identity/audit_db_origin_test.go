package identity

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// auditOriginEngine records the statement and the origin the sink wrote under.
type auditOriginEngine struct {
	queries []string
	origins []auth.CallOrigin
	actors  []string
}

func (e *auditOriginEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	e.queries = append(e.queries, query)
	e.origins = append(e.origins, auth.OriginFromContext(ctx))
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		actor = ac.UserId
	}
	e.actors = append(e.actors, actor)
	return &memqlengine.ExecuteResult{}, nil
}

// TestEngineAuditSinkStampsInternalOriginOnItsOneWrite pins the sink's half
// of memql#5624. v1:identity:auditEvent's create admits a cluster owner or
// server code, and this sink records decisions about every caller -- an
// admin's console act, a signed-in person's passkey change, a refusal --
// under that caller's own context. Unstamped, every one of those writes is
// refused and SlogAuditLogger swallows the refusal at WARN, so the trail
// simply stops recording non-owners. The caller's actor is kept: the stamp
// says who is writing, not who acted.
func TestEngineAuditSinkStampsInternalOriginOnItsOneWrite(t *testing.T) {
	e := &auditOriginEngine{}
	sink := &EngineAuditSink{Engine: e}
	ctx := auth.ContextWithUserActor(context.Background(), "person-1")

	if err := sink.WriteAuditEvent(ctx, AuditEvent{
		Category: AuditCategoryIdentity, Action: "passkey_renamed",
		ActorUserId: "person-1", Outcome: AuditOutcomeSuccess,
	}); err != nil {
		t.Fatalf("WriteAuditEvent: %v", err)
	}
	if len(e.queries) != 1 || !strings.HasPrefix(e.queries[0], "mutation createAuditEvent(") {
		t.Fatalf("the sink wrote %q, want exactly one createAuditEvent", e.queries)
	}
	if !e.origins[0].IsInternal() {
		t.Fatalf("the audit write ran with origin %v. auditEvent's create admits a cluster owner or server "+
			"code (memql#5624), so a non-owner's event is refused and the trail loses it", e.origins[0])
	}
	if e.actors[0] != "person-1" {
		t.Fatalf("the audit write ran as %q, want the caller it records", e.actors[0])
	}
}

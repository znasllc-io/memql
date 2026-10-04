package groups

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// auditOriginEngine records the origin each createAuditEvent arrived under.
type auditOriginEngine struct{ audits []auth.CallOrigin }

func (e *auditOriginEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "mutation createAuditEvent(") {
		e.audits = append(e.audits, auth.OriginFromContext(ctx))
	}
	return nil, nil
}

// TestGroupAuditWritesAreStamped: an admin managing groups is not a cluster
// owner, and auditEvent's create admits only a cluster owner or server code
// (memql#5624). Unstamped, the group decision lands and its trail entry does
// not.
func TestGroupAuditWritesAreStamped(t *testing.T) {
	e := &auditOriginEngine{}
	i := &Integration{store: &Store{engine: e, now: time.Now}, now: time.Now}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "admin-1", Role: auth.RoleAdmin})

	i.log(ctx, caller{userID: "admin-1", role: auth.RoleAdmin}, "group_created", "group-1", map[string]any{"name": "Design"})
	if len(e.audits) != 1 {
		t.Fatalf("want one createAuditEvent write, got %d", len(e.audits))
	}
	if !e.audits[0].IsInternal() {
		t.Fatalf("the group audit write ran with origin %v; an admin's group change would leave no trail", e.audits[0])
	}
}

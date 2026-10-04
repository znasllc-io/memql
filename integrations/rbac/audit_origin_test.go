package rbac

import (
	"context"
	"strings"
	"testing"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// auditOriginEngine records the origin each createAuditEvent arrived under.
type auditOriginEngine struct{ audits []componentAuth.CallOrigin }

func (e *auditOriginEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "mutation createAuditEvent(") {
		e.audits = append(e.audits, componentAuth.OriginFromContext(ctx))
	}
	return &memql.ExecuteResult{}, nil
}

// TestRoleAndGrantAuditWritesAreStamped is memql#5624's example case: role
// and grant authoring run under an admin, who is not a cluster owner, and
// auditEvent's create now admits only a cluster owner or server code. Both
// audit writers discard their error, so an unstamped write would leave the
// role or grant changed and the trail silent.
func TestRoleAndGrantAuditWritesAreStamped(t *testing.T) {
	e := &auditOriginEngine{}
	i := &Integration{engine: e}
	access := &componentAuth.AccessContext{UserId: "admin-1", Role: componentAuth.RoleAdmin}
	ctx := componentAuth.ContextWithAccess(context.Background(), access)

	i.auditRole(ctx, access, "role_created", "auditor", nil)
	i.auditGrant(ctx, access, "grant_set", "grant-1", grantSpec{
		kind: "user", subject: "person-1", verb: "read", resource: "app:fleet", effect: "allow",
	})
	if len(e.audits) != 2 {
		t.Fatalf("want two createAuditEvent writes, got %d", len(e.audits))
	}
	for n, origin := range e.audits {
		if !origin.IsInternal() {
			t.Fatalf("audit write %d ran with origin %v; an admin's role or grant change would leave no trail", n, origin)
		}
	}
}

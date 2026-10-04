package sitepublish

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// auditOriginEngine records the origin each createAuditEvent arrived under.
type auditOriginEngine struct{ audits []auth.CallOrigin }

func (e *auditOriginEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "mutation createAuditEvent(") {
		e.audits = append(e.audits, auth.OriginFromContext(ctx))
	}
	return &memql.ExecuteResult{}, nil
}

// TestPublishAuditWriteIsStamped: publishing a site is something its owner
// does, and auditEvent's create admits only a cluster owner or server code
// (memql#5624). Unstamped, the publish lands and its trail entry does not.
func TestPublishAuditWriteIsStamped(t *testing.T) {
	e := &auditOriginEngine{}
	i := &SitePublishIntegration{engine: e, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "person-1", Role: auth.RoleWriter})

	i.audit(ctx, "site-1", "artifact-1", publishResult{SiteId: "site-1"}, nil)
	if len(e.audits) != 1 {
		t.Fatalf("want one createAuditEvent write, got %d", len(e.audits))
	}
	if !e.audits[0].IsInternal() {
		t.Fatalf("the publish audit write ran with origin %v; a person's publish would leave no trail", e.audits[0])
	}
}

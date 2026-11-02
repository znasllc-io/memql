package packages

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// TestDeployAuditWriteIsStamped: whoever deploys a package need not be a
// cluster owner, and auditEvent's create admits only a cluster owner or
// server code (memql#5624). The write goes through the store's one stamping
// site, which TestTheStampNeverEscapesItsCall counts.
func TestDeployAuditWriteIsStamped(t *testing.T) {
	e := &originEngine{}
	a := &engineAuditor{engine: e, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "dev-1", Role: auth.RoleDeveloper})

	a.Deploy(ctx, DeployAuditEvent{PackageId: "p", DeploymentId: "d", Status: StatusSucceeded})
	got, ok := e.origins["createAuditEvent"]
	if !ok {
		t.Fatalf("the deploy audit wrote no createAuditEvent; saw %v", e.origins)
	}
	if !got.IsInternal() {
		t.Fatalf("the deploy audit write ran with origin %v; a developer's deploy would leave no trail", got)
	}
}

// A developer may roll back and may not write the trail, so this has to be stamped
// for the reason the deploy's is: the record of how a rollback ended would otherwise
// be refused for most of the people who can start one.
func TestRollbackAuditWriteIsStamped(t *testing.T) {
	e := &originEngine{}
	a := &engineAuditor{engine: e, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "dev-1", Role: auth.RoleDeveloper})

	a.Rollback(ctx, RollbackAuditEvent{PackageId: "p", DeploymentId: "d", Outcome: "failure", FailureReason: "the api server said no"})
	got, ok := e.origins["createAuditEvent"]
	if !ok {
		t.Fatalf("the rollback audit wrote no createAuditEvent; saw %v", e.origins)
	}
	if !got.IsInternal() {
		t.Fatalf("the rollback audit write ran with origin %v; a developer's rollback would leave no trail", got)
	}
}

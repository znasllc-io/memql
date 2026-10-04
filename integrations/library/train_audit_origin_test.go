package library

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// trainAuditEngine records the origin of the train audit write. auditTrain
// reaches nothing on the engine but Execute, so the rest of the interface is
// left unimplemented on purpose: a call to any of it would be a new
// dependency this test should hear about.
type trainAuditEngine struct {
	memql.IntegrationEngineAccess
	audits []auth.CallOrigin
}

func (e *trainAuditEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "mutation createAuditEvent(") {
		e.audits = append(e.audits, auth.OriginFromContext(ctx))
	}
	return &memql.ExecuteResult{}, nil
}

// TestTrainAuditWriteIsStamped: training a file is something its owner does,
// and auditEvent's create admits only a cluster owner or server code
// (memql#5624). Unstamped, the training lands and its trail entry does not.
func TestTrainAuditWriteIsStamped(t *testing.T) {
	e := &trainAuditEngine{}
	i := &Integration{engine: e}
	access := &auth.AccessContext{UserId: "person-1", Role: auth.RoleWriter}
	ctx := auth.ContextWithAccess(context.Background(), access)

	i.auditTrain(ctx, access, "person-1", "file-1", "artifact-1", "domain-1", 42)
	if len(e.audits) != 1 {
		t.Fatalf("want one createAuditEvent write, got %d", len(e.audits))
	}
	if !e.audits[0].IsInternal() {
		t.Fatalf("the train audit write ran with origin %v; a person's training would leave no trail", e.audits[0])
	}
}

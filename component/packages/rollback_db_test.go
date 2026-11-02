package packages

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// rollbackFixture is a real package with a real succeeded deployment to return to,
// and the real auditor. Only storage, rollout and the site write are offline fakes.
type rollbackFixture struct {
	*confirmationFixture
	held heldRepoint
	req  RollbackRequest
}

func newRollbackFixture(t *testing.T, source string) *rollbackFixture {
	t.Helper()
	tree := spaOnlyPackage()
	if source == "with DSL" {
		tree = validPackage()
	}
	f := newConfirmationFixtureSource(t, tree)
	out, err := Deploy(f.ctx, f.deps[0], DeployRequest{
		PackageId:    f.pkg,
		DeploymentId: f.run,
		Confirmed:    true,
		Actor:        actorFromContext(f.ctx),
		Placements:   map[string]Placement{"storefront": {Skip: true}},
	})
	if err != nil || out == nil || out.Status != StatusSucceeded {
		t.Fatalf("the deployment to return to: %+v %v", out, err)
	}
	awaitAnalysis(t, f.audit)
	held := heldRepoint{livePublisher: livePublisher{f.h.publisher}, entered: make(chan context.Context, 4), release: make(chan struct{})}
	f.deps[0].Publisher = held
	f.deps[0].Auditor = &engineAuditor{engine: f.deps[0].Store.engine, logger: discardLogger()}
	return &rollbackFixture{
		confirmationFixture: f,
		held:                held,
		// Bare, as the OS sends it.
		req: RollbackRequest{PackageId: memql.BareShortId(f.pkg), DeploymentId: memql.BareShortId(f.run), Actor: actorFromContext(f.ctx)},
	}
}

// leaveMidRollback holds the rollback at its first site, leaves, and lets it go on.
func (f *rollbackFixture) leaveMidRollback(t *testing.T) {
	t.Helper()
	requestCtx, leave := context.WithCancel(f.ctx)
	defer leave()
	returned := make(chan error, 1)
	go func() {
		_, err := Rollback(requestCtx, f.deps[0], f.req)
		returned <- err
	}()
	f.held.await(t)
	leave()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the caller that left is told so: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call kept its caller after it left")
	}
	close(f.held.release)
}

// awaitRollbackEvent reads the rollback's ending back through the query a cluster
// owner's Audit section uses.
func (f *rollbackFixture) awaitRollbackEvent(t *testing.T) map[string]any {
	t.Helper()
	eng := f.deps[0].Store.engine.(*memql.MemQLEngine)
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, row := range mustExecute(t, eng, memql.ContextWithFreshRead(f.ctx), "query auditEventsByTarget(targetId: "+langparser.QuoteString(f.req.PackageId)+")") {
			if rowString(row, "action") == AuditActionRollback {
				return row
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the rollback left no event on the trail")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An error is free text from a remote server, and the record is a statement the
// engine has to parse: this one must arrive whole, through the real createAuditEvent.
// The caller has gone, so nothing else carries it.
func TestARollbackThatFailsAfterItsCallerLeftLeavesTheRealErrorOnTheTrail(t *testing.T) {
	const hostile = "the store said \"no\" to <packages/acme>\nback\\slash \x01 control"
	f := newRollbackFixture(t, "without DSL")
	f.held.err = errors.New(hostile)
	f.deps[0].Publisher = f.held
	f.leaveMidRollback(t)

	row := f.awaitRollbackEvent(t)
	if rowString(row, "outcome") != "failure" {
		t.Fatalf("the trail must say it failed: %+v", row)
	}
	if got := rowString(row, "failureReason"); !strings.HasSuffix(got, hostile) {
		t.Fatalf("the trail must carry the real error whole: %q", got)
	}
	if rowString(row, "actorUserId") != f.owner {
		t.Fatalf("the trail must name who asked, after they left: %+v", row)
	}
	detail, _ := row["detail"].(map[string]any)
	if rowString(detail, "deploymentId") != f.req.DeploymentId {
		t.Fatalf("the trail must name the deployment it was returning to: %+v", detail)
	}
	if _, sites := detail["sites"]; sites {
		t.Fatalf("no site was put back, so the trail must name none: %+v", detail)
	}
}

// What a rollback writes through the engine after its caller left has to pass on the
// authority it kept: who answers for the DSL goes on the package before the pointer
// moves, and the list of domains after the roll.
func TestARollbackThatOutlivesItsCallerWritesThePackageUnderItsAuthority(t *testing.T) {
	f := newRollbackFixture(t, "with DSL")
	// The cluster has moved on since, so the pointer has to move back.
	f.h.stager.active["acme"] = newerPrefix
	recorded, err := f.deps[0].Store.deploymentById(memql.ContextWithFreshRead(f.ctx), f.run)
	if err != nil || rowString(recorded, "dslVersion") == "" {
		t.Fatalf("the deployment to return to must carry DSL: %+v %v", recorded, err)
	}
	f.leaveMidRollback(t)

	row := f.awaitRollbackEvent(t)
	if rowString(row, "outcome") != "success" {
		t.Fatalf("the rollback must reach its end: %+v", row)
	}
	detail, _ := row["detail"].(map[string]any)
	if moved, _ := detail["pointerMoved"].(bool); !moved {
		t.Fatalf("the trail must say the pointer moved: %+v", detail)
	}
	if rolled, _ := detail["rolled"].(bool); !rolled {
		t.Fatalf("the trail must say the roll was asked for: %+v", detail)
	}
	if got := f.h.stager.active["acme"]; got != rowString(recorded, "dslVersion") {
		t.Fatalf("the pointer must be back on %q, got %q", rowString(recorded, "dslVersion"), got)
	}
	if f.h.roller.rolls < 1 {
		t.Fatal("the roll was not requested")
	}
	pkg, err := f.deps[0].Store.packageById(memql.ContextWithFreshRead(f.ctx), f.pkg)
	if err != nil || pkg == nil {
		t.Fatalf("package: %v", err)
	}
	if got := rowString(pkg, "dslDeployedBy"); got != f.owner {
		t.Fatalf("whoever put the DSL back answers for it, got %q", got)
	}
	if got := rowStrings(pkg, "dslDomains"); len(got) != 1 || got[0] != "acme" {
		t.Fatalf("the list of domains must come back with the DSL, got %v", got)
	}
}

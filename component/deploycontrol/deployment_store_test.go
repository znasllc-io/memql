package deploycontrol

import (
	"context"
	"strings"
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
)

// fakeEngine records the queries Execute is asked to run so tests can
// assert which deployment mutations a write RPC emitted. Satisfies
// identity.EngineExecutor.
type fakeEngine struct {
	queries []string
}

func (f *fakeEngine) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	f.queries = append(f.queries, query)
	return &memqlengine.ExecuteResult{}, nil
}

func newTestServiceWithEngine(t *testing.T, exec Executor, audit identity.AuditLogger, eng identity.EngineExecutor) *Service {
	t.Helper()
	svc, err := NewService(Options{
		Logger:   quietLogger(),
		Audit:    audit,
		RepoRoot: t.TempDir(),
		Executor: exec,
		Engine:   eng,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func countContaining(queries []string, substrs ...string) int {
	n := 0
	for _, q := range queries {
		all := true
		for _, s := range substrs {
			if !strings.Contains(q, s) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

// A successful DeployStaging records a deployment (in_progress) then
// transitions it to succeeded (#1872).
func TestDeployStagingPersistsDeploymentRecordOnSuccess(t *testing.T) {
	exec := &fakeExecutor{promoteOut: "SUCCESS: promoted"}
	eng := &fakeEngine{}
	svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

	res, err := svc.DeployStaging(ctxWithRole(auth.RoleOwner), &memqlv1.DeployStagingRequest{Version: "0.9.9"})
	if err != nil {
		t.Fatalf("DeployStaging err = %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("ok = false, message = %q", res.GetMessage())
	}

	if got := countContaining(eng.queries, "mutationCreateDeployment(", `status: "in_progress"`, `environment: "staging"`); got != 1 {
		t.Errorf("create-deployment(in_progress, staging) calls = %d, want 1; queries = %v", got, eng.queries)
	}
	if got := countContaining(eng.queries, "mutationUpdateDeploymentStatus(", `status: "succeeded"`); got != 1 {
		t.Errorf("transition(succeeded) calls = %d, want 1; queries = %v", got, eng.queries)
	}
}

// A failed promote still records the deployment and transitions it to
// failed (#1872).
func TestDeployStagingTransitionsFailedOnPromoteError(t *testing.T) {
	exec := &fakeExecutor{promoteErr: context.DeadlineExceeded}
	eng := &fakeEngine{}
	svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

	res, _ := svc.DeployStaging(ctxWithRole(auth.RoleAdmin), &memqlv1.DeployStagingRequest{Version: "0.9.9"})
	if res.GetOk() {
		t.Fatalf("expected ok = false on promote error")
	}
	if got := countContaining(eng.queries, "mutationCreateDeployment("); got != 1 {
		t.Errorf("create-deployment calls = %d, want 1; queries = %v", got, eng.queries)
	}
	if got := countContaining(eng.queries, "mutationUpdateDeploymentStatus(", `status: "failed"`); got != 1 {
		t.Errorf("transition(failed) calls = %d, want 1; queries = %v", got, eng.queries)
	}
}

// A denied (non-owner/admin) caller writes NO deployment record: the
// authorize gate short-circuits before the persistence path.
func TestDeniedDeployWritesNoDeploymentRecord(t *testing.T) {
	exec := &fakeExecutor{}
	eng := &fakeEngine{}
	svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

	_, _ = svc.DeployStaging(ctxWithRole(auth.RoleWriter), &memqlv1.DeployStagingRequest{Version: "0.9.9"})
	if len(eng.queries) != 0 {
		t.Errorf("expected no deployment mutations for denied caller, got %v", eng.queries)
	}
}

// With no engine wired the deploy path runs unchanged (no panic, no
// persistence) -- the engineless / unit-test posture.
func TestDeployStagingNoEngineIsNoop(t *testing.T) {
	exec := &fakeExecutor{promoteOut: "SUCCESS"}
	svc := newTestService(t, exec, &fakeAudit{})
	res, err := svc.DeployStaging(ctxWithRole(auth.RoleOwner), &memqlv1.DeployStagingRequest{Version: "0.9.9"})
	if err != nil || !res.GetOk() {
		t.Fatalf("DeployStaging (no engine) err = %v ok = %v", err, res.GetOk())
	}
}

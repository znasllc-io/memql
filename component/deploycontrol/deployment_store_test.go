package deploycontrol

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// fakeEngine records the queries Execute is asked to run so tests can
// assert which deployment mutations a write RPC emitted. Satisfies
// identity.EngineExecutor. Read (query*) calls return queryNodes as a
// GraphBundle; mutation* calls return mutationErr (nil by default) --
// supporting both the #1872 deploy-path tests (which only inspect
// queries) and the #1877 cut-version tests (which need query results +
// write-failure injection).
type fakeEngine struct {
	queries []string

	queryNodes  []*memqlv1.MemoryNode // returned for query* Execute calls
	queryErr    error                 // injected error for query* calls
	mutationErr error                 // injected error for mutation* calls
}

func (f *fakeEngine) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	f.queries = append(f.queries, query)
	// Post-C6 (memql#2036) mutation construct names are bare (no `mutation`
	// prefix); deploycontrol's writes are createDeployment / updateDeploymentStatus.
	if strings.HasPrefix(query, "createDeployment(") || strings.HasPrefix(query, "updateDeploymentStatus(") {
		if f.mutationErr != nil {
			return nil, f.mutationErr
		}
		return &memqlengine.ExecuteResult{}, nil
	}
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: f.queryNodes}}, nil
}

func newTestServiceWithEngine(t *testing.T, exec Executor, audit identity.AuditLogger, eng identity.EngineExecutor) *Service {
	t.Helper()
	svc, err := NewService(Options{
		Logger:   quietLogger(),
		Audit:    audit,
		RepoRoot: repoRootWithOverlay(t),
		Executor: exec,
		Engine:   eng,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	stubRepairWatch(svc)
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

// A denied (non-owner/admin) caller writes NO deployment record: the
// authorize gate short-circuits before the persistence path.
func TestDeniedWriteRecordsNoDeployment(t *testing.T) {
	eng := &fakeEngine{}
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, eng)

	_, _ = svc.CutVersion(ctxWithRole(auth.RoleReader), &memqlv1.CutVersionRequest{Bump: "patch"})
	if len(eng.queries) != 0 {
		t.Errorf("expected no deployment mutations for denied caller, got %v", eng.queries)
	}
}

// A cut records the pending deployment WITHOUT an environment: epic
// memql#3943 removed the field, so nothing in a write may name one.
func TestCutVersionRecordsNoEnvironment(t *testing.T) {
	eng := &fakeEngine{}
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, eng)

	res, err := svc.CutVersion(ctxWithRole(auth.RoleOwner), &memqlv1.CutVersionRequest{Version: "1.0.0"})
	if err != nil {
		t.Fatalf("CutVersion: %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("ok = false: %q", res.GetMessage())
	}
	if got := countContaining(eng.queries, "createDeployment(", `status: "pending"`); got != 1 {
		t.Errorf("create-deployment(pending) calls = %d, want 1; queries = %v", got, eng.queries)
	}
	for _, q := range eng.queries {
		if strings.Contains(q, "environment") {
			t.Errorf("a write named an environment: %q", q)
		}
	}
}

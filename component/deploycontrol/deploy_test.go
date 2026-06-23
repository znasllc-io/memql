package deploycontrol

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
)

// fullDeploymentNode builds a v1:cluster:deployment query-result node
// carrying every field loadDeployment reads.
func fullDeploymentNode(fields map[string]any) *memqlv1.MemoryNode {
	st, _ := structpb.NewStruct(fields)
	return &memqlv1.MemoryNode{Payload: st}
}

// --- selectDriver ---------------------------------------------------------

func TestSelectDriver(t *testing.T) {
	exec := &fakeExecutor{}
	cases := map[string]string{
		"azure": "azure",
		"":      "azure", // empty defaults to azure
	}
	for provider, want := range cases {
		d, err := selectDriver(exec, provider)
		if err != nil {
			t.Errorf("selectDriver(%q) err = %v", provider, err)
			continue
		}
		if d.provider() != want {
			t.Errorf("selectDriver(%q) -> %q, want %q", provider, d.provider(), want)
		}
	}
	if _, err := selectDriver(exec, "gcp"); err == nil {
		t.Error("selectDriver(gcp) expected error for unknown provider")
	}
	// The retired docker-local provider is now a hard error: local
	// clusters are operated via `make up` (k3d + ArgoCD), not the console.
	if _, err := selectDriver(exec, "docker-local"); err == nil {
		t.Error("selectDriver(docker-local) expected error for retired provider")
	}
}

func TestConsoleEnvFor(t *testing.T) {
	cases := map[string]string{
		"production":  "prod",
		"staging":     "staging",
		"development": "",
		"":            "",
	}
	for env, want := range cases {
		if got := consoleEnvFor(env); got != want {
			t.Errorf("consoleEnvFor(%q) = %q, want %q", env, got, want)
		}
	}
}

// --- Deploy: driver selection by provider ---------------------------------

// An azure-provider record deploys via promote.sh (the Argo path) and
// transitions in_progress -> succeeded.
func TestDeployAzureProviderUsesPromote(t *testing.T) {
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		fullDeploymentNode(map[string]any{
			"deploymentId": "d1", "status": "pending", "version": "1.2.3",
			"imageDigest": "sha256:abc", "provider": "azure", "environment": "staging",
		}),
	}}
	exec := &fakeExecutor{promoteOut: "SUCCESS: promoted"}
	svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

	res, err := svc.Deploy(ctxWithRole(auth.RoleOwner), &memqlv1.DeployRequest{DeploymentId: "d1"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("ok = false: %q", res.GetMessage())
	}
	// azure driver -> promote.sh (version, staging).
	if len(exec.promoteCalls) != 1 || exec.promoteCalls[0] != [2]string{"1.2.3", "staging"} {
		t.Errorf("promote calls = %v, want [[1.2.3 staging]]", exec.promoteCalls)
	}
	// in_progress then succeeded transitions.
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "in_progress"`); got != 1 {
		t.Errorf("in_progress transition = %d, want 1; queries = %v", got, eng.queries)
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "succeeded"`); got != 1 {
		t.Errorf("succeeded transition = %d, want 1; queries = %v", got, eng.queries)
	}
}

// A docker-local record can no longer deploy: the provider is retired
// and selectDriver rejects it, so the record transitions to failed and
// no driver runs (local clusters use `make up`, not the deploy console).
func TestDeployDockerLocalProviderRejected(t *testing.T) {
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		fullDeploymentNode(map[string]any{
			"deploymentId": "d2", "status": "pending", "version": "9.9.9",
			"provider": "docker-local", "environment": "development",
		}),
	}}
	exec := &fakeExecutor{}
	svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

	res, _ := svc.Deploy(ctxWithRole(auth.RoleAdmin), &memqlv1.DeployRequest{DeploymentId: "d2"})
	if res.GetOk() {
		t.Error("ok = true, want false for retired docker-local provider")
	}
	if len(exec.promoteCalls) != 0 {
		t.Errorf("retired docker-local deploy must NOT call promote.sh, got %v", exec.promoteCalls)
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "failed"`); got != 1 {
		t.Errorf("failed transition = %d, want 1; queries = %v", got, eng.queries)
	}
}

// A driver failure transitions the record to failed and surfaces the reason.
func TestDeployTransitionsFailedOnDriverError(t *testing.T) {
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		fullDeploymentNode(map[string]any{
			"deploymentId": "d3", "status": "pending", "version": "1.0.0",
			"provider": "azure", "environment": "production",
		}),
	}}
	exec := &fakeExecutor{promoteErr: errors.New("promote.sh exit 1")}
	audit := &fakeAudit{}
	svc := newTestServiceWithEngine(t, exec, audit, eng)

	res, err := svc.Deploy(ctxWithRole(auth.RoleOwner), &memqlv1.DeployRequest{DeploymentId: "d3"})
	if err != nil {
		t.Fatalf("Deploy returned status err = %v, want ActionResult ok=false", err)
	}
	if res.GetOk() {
		t.Error("ok = true, want false on driver error")
	}
	if !strings.Contains(res.GetMessage(), "promote.sh exit 1") {
		t.Errorf("message = %q, want driver failure reason", res.GetMessage())
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "failed"`); got != 1 {
		t.Errorf("failed transition = %d, want 1; queries = %v", got, eng.queries)
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeFailure {
		t.Fatalf("want one failure audit event, got %+v", audit.events)
	}
}

// An unknown provider marks the record failed and never runs a driver.
func TestDeployUnknownProviderFails(t *testing.T) {
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		fullDeploymentNode(map[string]any{
			"deploymentId": "d4", "status": "pending", "version": "1.0.0",
			"provider": "gcp", "environment": "production",
		}),
	}}
	exec := &fakeExecutor{}
	svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

	res, _ := svc.Deploy(ctxWithRole(auth.RoleOwner), &memqlv1.DeployRequest{DeploymentId: "d4"})
	if res.GetOk() {
		t.Error("ok = true, want false on unknown provider")
	}
	if len(exec.promoteCalls) != 0 {
		t.Error("no driver should run for an unknown provider")
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "failed"`); got != 1 {
		t.Errorf("failed transition = %d, want 1; queries = %v", got, eng.queries)
	}
}

// A not-found deployment id surfaces a clear error, no driver run.
func TestDeployNotFound(t *testing.T) {
	eng := &fakeEngine{} // empty query result
	exec := &fakeExecutor{}
	svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

	res, _ := svc.Deploy(ctxWithRole(auth.RoleOwner), &memqlv1.DeployRequest{DeploymentId: "missing"})
	if res.GetOk() {
		t.Error("ok = true, want false for missing deployment")
	}
	if !strings.Contains(res.GetMessage(), "not found") {
		t.Errorf("message = %q, want not-found notice", res.GetMessage())
	}
	if len(exec.promoteCalls) != 0 {
		t.Error("no driver should run for a missing deployment")
	}
}

func TestDeployRequiresDeploymentId(t *testing.T) {
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, &fakeEngine{})
	_, err := svc.Deploy(ctxWithRole(auth.RoleOwner), &memqlv1.DeployRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestDeployDeniesNonAdmin(t *testing.T) {
	eng := &fakeEngine{}
	audit := &fakeAudit{}
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, audit, eng)
	_, err := svc.Deploy(ctxWithRole(auth.RoleWriter), &memqlv1.DeployRequest{DeploymentId: "d1"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
	}
	if len(eng.queries) != 0 {
		t.Errorf("denied caller must not touch the engine, got %v", eng.queries)
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeBlocked {
		t.Errorf("want one blocked audit event, got %+v", audit.events)
	}
}

// --- RollbackDeployment ---------------------------------------------------

// Rollback of a succeeded azure deployment creates a NEW record pointing
// at the historical digest, redeploys via the same driver, and lands in
// rolled_back.
func TestRollbackDeploymentRedeploysHistoricalDigest(t *testing.T) {
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		fullDeploymentNode(map[string]any{
			"deploymentId": "good1", "status": "succeeded", "version": "1.4.0",
			"imageDigest": "sha256:good", "provider": "azure", "environment": "production",
			"region": "eastus", "clusterId": "v1:cluster:cluster:c1",
		}),
	}}
	exec := &fakeExecutor{promoteOut: "SUCCESS: re-promoted"}
	audit := &fakeAudit{}
	svc := newTestServiceWithEngine(t, exec, audit, eng)

	res, err := svc.RollbackDeployment(ctxWithRole(auth.RoleOwner), &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "good1"})
	if err != nil {
		t.Fatalf("RollbackDeployment: %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("ok = false: %q", res.GetMessage())
	}
	// Redeployed the historical version via the azure driver.
	if len(exec.promoteCalls) != 1 || exec.promoteCalls[0] != [2]string{"1.4.0", "prod"} {
		t.Errorf("promote calls = %v, want [[1.4.0 prod]]", exec.promoteCalls)
	}
	// A NEW record was created carrying the historical digest +
	// previousDeploymentId provenance.
	var createCall string
	for _, q := range eng.queries {
		if strings.HasPrefix(q, "createDeployment(") {
			createCall = q
		}
	}
	if createCall == "" {
		t.Fatalf("rollback must create a new deployment record; queries = %v", eng.queries)
	}
	if !strings.Contains(createCall, `imageDigest: "sha256:good"`) {
		t.Errorf("new record must carry historical digest: %s", createCall)
	}
	if !strings.Contains(createCall, `previousDeploymentId: "good1"`) {
		t.Errorf("new record must point previousDeploymentId at the target: %s", createCall)
	}
	if !strings.Contains(createCall, `status: "in_progress"`) {
		t.Errorf("new rollback record should start in_progress: %s", createCall)
	}
	// Terminal transition is rolled_back, NOT a color flip / git revert.
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "rolled_back"`); got != 1 {
		t.Errorf("rolled_back transition = %d, want 1; queries = %v", got, eng.queries)
	}
	if res.GetDetails()["newDeploymentId"] == "" {
		t.Error("details.newDeploymentId empty")
	}
}

// Rolling back to a non-succeeded deployment is rejected (no driver, no
// new record).
func TestRollbackDeploymentRejectsNonSucceededTarget(t *testing.T) {
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		fullDeploymentNode(map[string]any{
			"deploymentId": "bad1", "status": "failed", "version": "1.4.0",
			"provider": "azure", "environment": "production",
		}),
	}}
	exec := &fakeExecutor{}
	svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

	res, _ := svc.RollbackDeployment(ctxWithRole(auth.RoleOwner), &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "bad1"})
	if res.GetOk() {
		t.Error("ok = true, want false rolling back to a failed deployment")
	}
	if !strings.Contains(res.GetMessage(), "must be succeeded") {
		t.Errorf("message = %q, want succeeded-target requirement", res.GetMessage())
	}
	if len(exec.promoteCalls) != 0 {
		t.Error("no driver should run for an invalid rollback target")
	}
	if countContaining(eng.queries, "createDeployment(") != 0 {
		t.Error("rejected rollback must not create a new record")
	}
}

// A driver failure during rollback transitions the NEW record to failed.
func TestRollbackDeploymentTransitionsFailedOnDriverError(t *testing.T) {
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		fullDeploymentNode(map[string]any{
			"deploymentId": "good2", "status": "succeeded", "version": "2.0.0",
			"imageDigest": "sha256:two", "provider": "azure", "environment": "staging",
		}),
	}}
	exec := &fakeExecutor{promoteErr: errors.New("promote boom")}
	svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

	res, _ := svc.RollbackDeployment(ctxWithRole(auth.RoleOwner), &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "good2"})
	if res.GetOk() {
		t.Error("ok = true, want false on rollback driver error")
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "failed"`); got != 1 {
		t.Errorf("failed transition = %d, want 1; queries = %v", got, eng.queries)
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "rolled_back"`); got != 0 {
		t.Errorf("must NOT mark rolled_back on driver failure; queries = %v", eng.queries)
	}
}

func TestRollbackDeploymentRequiresToId(t *testing.T) {
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, &fakeAudit{}, &fakeEngine{})
	_, err := svc.RollbackDeployment(ctxWithRole(auth.RoleOwner), &memqlv1.RollbackDeploymentRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestRollbackDeploymentDeniesNonAdmin(t *testing.T) {
	audit := &fakeAudit{}
	svc := newTestServiceWithEngine(t, &fakeExecutor{}, audit, &fakeEngine{})
	_, err := svc.RollbackDeployment(ctxWithRole(auth.RoleReader), &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "good1"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeBlocked {
		t.Errorf("want one blocked audit event, got %+v", audit.events)
	}
}

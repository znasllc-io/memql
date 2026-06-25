package deploycontrol

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
)

// Deploy / RollbackDeployment are automation-driven kick-offs (#2115 step 6
// retired the synchronous Go apply): the RPC validates the record and
// transitions it to in_progress (Deploy) / creates the rollback record at
// in_progress (RollbackDeployment), then returns an async ack. The deploy
// pack automations (examples/deploypack) own promote + the terminal
// transition. So the RPC NEVER calls promote.sh and NEVER writes a terminal
// status -- the only synchronous write is the in_progress kick-off edge.

// fullDeploymentNode builds a v1:cluster:deployment query-result node
// carrying every field loadDeployment reads.
func fullDeploymentNode(fields map[string]any) *memqlv1.MemoryNode {
	st, _ := structpb.NewStruct(fields)
	return &memqlv1.MemoryNode{Payload: st}
}

// --- provider allow-list + env mapping ------------------------------------

func TestValidateDeploymentProvider(t *testing.T) {
	// azure + empty (legacy default) are accepted.
	for _, ok := range []string{"azure", ""} {
		if err := validateDeploymentProvider(ok); err != nil {
			t.Errorf("validateDeploymentProvider(%q) err = %v, want nil", ok, err)
		}
	}
	// An unknown provider is rejected.
	if err := validateDeploymentProvider("gcp"); err == nil {
		t.Error("validateDeploymentProvider(gcp) expected error for unknown provider")
	}
	// The retired docker-local provider is a hard error: local clusters are
	// operated via `make up` (k3d + ArgoCD), not the console.
	if err := validateDeploymentProvider("docker-local"); err == nil {
		t.Error("validateDeploymentProvider(docker-local) expected error for retired provider")
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
		if got := ConsoleEnvFor(env); got != want {
			t.Errorf("ConsoleEnvFor(%q) = %q, want %q", env, got, want)
		}
	}
}

// --- Deploy: automation-driven kick-off -----------------------------------

// An azure-provider record kicks off the lifecycle: the RPC transitions the
// record to in_progress and returns an async ack. It does NOT run promote.sh
// and does NOT write a terminal transition -- the deploy pack's
// driveDeploymentInProgress automation owns those (#2115).
func TestDeployKicksOffWithoutApply(t *testing.T) {
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		fullDeploymentNode(map[string]any{
			"deploymentId": "d1", "status": "pending", "version": "1.2.3",
			"imageDigest": "sha256:abc", "provider": "azure", "environment": "staging",
		}),
	}}
	exec := &fakeExecutor{promoteOut: "SHOULD NOT RUN"}
	svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

	res, err := svc.Deploy(ctxWithRole(auth.RoleOwner), &memqlv1.DeployRequest{DeploymentId: "d1"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("ok = false: %q", res.GetMessage())
	}
	// Async ack contract: accepted + kicked off, status in_progress.
	if res.GetDetails()["async"] != "true" || res.GetDetails()["status"] != "in_progress" {
		t.Errorf("details = %v, want async=true status=in_progress", res.GetDetails())
	}
	// The RPC must NOT have driven promote.sh.
	if len(exec.promoteCalls) != 0 {
		t.Errorf("automation-driven Deploy must NOT call promote.sh, got %v", exec.promoteCalls)
	}
	// Exactly one in_progress transition; NO terminal transition.
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "in_progress"`); got != 1 {
		t.Errorf("in_progress transition = %d, want 1; queries = %v", got, eng.queries)
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "succeeded"`); got != 0 {
		t.Errorf("automation-driven Deploy must NOT write succeeded, got %d; queries = %v", got, eng.queries)
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "failed"`); got != 0 {
		t.Errorf("automation-driven Deploy must NOT write failed, got %d; queries = %v", got, eng.queries)
	}
}

// A docker-local record can no longer deploy: the provider is retired and
// validateDeploymentProvider rejects it, so the record transitions to failed
// and no kick-off happens (local clusters use `make up`, not the console).
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
	// A rejected provider never reaches the in_progress kick-off.
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "in_progress"`); got != 0 {
		t.Errorf("rejected provider must NOT kick off in_progress, got %d; queries = %v", got, eng.queries)
	}
}

// An unknown provider marks the record failed and never kicks off.
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
		t.Error("no promote should run for an unknown provider")
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "failed"`); got != 1 {
		t.Errorf("failed transition = %d, want 1; queries = %v", got, eng.queries)
	}
}

// A not-found deployment id surfaces a clear error, no kick-off.
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
		t.Error("no promote should run for a missing deployment")
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

// --- RollbackDeployment: automation-driven kick-off -----------------------

// Rolling back a succeeded azure deployment creates a NEW record at
// in_progress carrying the historical digest + previousDeploymentId
// provenance, returns an async ack, and does NOT run promote.sh or write the
// terminal -- the deploy pack automation re-pins the digest and lands the
// record in rolled_back (#2168).
func TestRollbackDeploymentKicksOffWithoutApply(t *testing.T) {
	eng := &fakeEngine{queryNodes: []*memqlv1.MemoryNode{
		fullDeploymentNode(map[string]any{
			"deploymentId": "good1", "status": "succeeded", "version": "1.4.0",
			"imageDigest": "sha256:good", "provider": "azure", "environment": "production",
			"region": "eastus", "clusterId": "v1:cluster:cluster:c1",
		}),
	}}
	exec := &fakeExecutor{promoteOut: "SHOULD NOT RUN"}
	svc := newTestServiceWithEngine(t, exec, &fakeAudit{}, eng)

	res, err := svc.RollbackDeployment(ctxWithRole(auth.RoleOwner), &memqlv1.RollbackDeploymentRequest{ToDeploymentId: "good1"})
	if err != nil {
		t.Fatalf("RollbackDeployment: %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("ok = false: %q", res.GetMessage())
	}
	// Async ack contract: accepted + kicked off, with a new record id.
	if res.GetDetails()["async"] != "true" || res.GetDetails()["status"] != "in_progress" {
		t.Errorf("details = %v, want async=true status=in_progress", res.GetDetails())
	}
	if res.GetDetails()["newDeploymentId"] == "" {
		t.Error("details.newDeploymentId empty")
	}
	// A NEW record was created at in_progress carrying the historical digest +
	// previousDeploymentId provenance (the kick-off CDC edge).
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
	// The RPC must NOT have driven promote.sh or any terminal transition.
	if len(exec.promoteCalls) != 0 {
		t.Errorf("automation-driven rollback must NOT call promote.sh, got %v", exec.promoteCalls)
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "rolled_back"`); got != 0 {
		t.Errorf("automation-driven rollback must NOT write rolled_back, got %d; queries = %v", got, eng.queries)
	}
	if got := countContaining(eng.queries, "updateDeploymentStatus(", `status: "failed"`); got != 0 {
		t.Errorf("automation-driven rollback must NOT write failed, got %d; queries = %v", got, eng.queries)
	}
}

// Rolling back to a non-succeeded deployment is rejected (no kick-off, no
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
		t.Error("no promote should run for an invalid rollback target")
	}
	if countContaining(eng.queries, "createDeployment(") != 0 {
		t.Error("rejected rollback must not create a new record")
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

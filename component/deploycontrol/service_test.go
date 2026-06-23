package deploycontrol

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
)

// fakeExecutor records calls + returns canned output. Read paths
// return the configured JSON fixtures; action paths record the call.
type fakeExecutor struct {
	promoteCalls  [][2]string // (version, env)
	rollbackCalls [][2]string // (env, sha)
	rolloutCalls  [][3]string // (env, rollout, action)

	promoteOut string
	promoteErr error

	argoJSON     []byte
	rolloutsJSON []byte
	analysisJSON []byte
	kubectlErr   error
}

func (f *fakeExecutor) RunPromote(_ context.Context, version, env string) (string, error) {
	f.promoteCalls = append(f.promoteCalls, [2]string{version, env})
	return f.promoteOut, f.promoteErr
}

func (f *fakeExecutor) RunRollback(_ context.Context, env, sha string) (string, error) {
	f.rollbackCalls = append(f.rollbackCalls, [2]string{env, sha})
	return "reverted " + sha, nil
}

func (f *fakeExecutor) RunRolloutAction(_ context.Context, env, rollout, action string) (string, error) {
	f.rolloutCalls = append(f.rolloutCalls, [3]string{env, rollout, action})
	return action + " " + rollout, nil
}

func (f *fakeExecutor) KubectlJSON(_ context.Context, args ...string) ([]byte, error) {
	if f.kubectlErr != nil {
		return nil, f.kubectlErr
	}
	// Discriminate on the resource arg.
	for _, a := range args {
		switch a {
		case "app":
			return f.argoJSON, nil
		case "rollout":
			return f.rolloutsJSON, nil
		case "analysisrun":
			return f.analysisJSON, nil
		}
	}
	return []byte("{}"), nil
}

func (f *fakeExecutor) Git(_ context.Context, _ ...string) (string, error) { return "", nil }

// fakeAudit records emitted events.
type fakeAudit struct {
	events []identity.AuditEvent
}

func (f *fakeAudit) Log(_ context.Context, ev identity.AuditEvent) {
	f.events = append(f.events, ev)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ctxWithRole(role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId:       "v1:identity:user.u1",
		PrimaryEmail: "actor@example.com",
		Role:         role,
		IdentityId:   "v1:identity:identity.i1",
	})
}

func newTestService(t *testing.T, exec Executor, audit identity.AuditLogger) *Service {
	t.Helper()
	svc, err := NewService(Options{
		Logger:   quietLogger(),
		Audit:    audit,
		RepoRoot: t.TempDir(),
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// --- (a) owner + admin admitted -------------------------------------------

func TestDeployStagingAdmittedForOwnerAndAdmin(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
		exec := &fakeExecutor{promoteOut: "SUCCESS: promoted"}
		audit := &fakeAudit{}
		svc := newTestService(t, exec, audit)

		res, err := svc.DeployStaging(ctxWithRole(role), &memqlv1.DeployStagingRequest{Version: "0.9.9"})
		if err != nil {
			t.Fatalf("role %s: DeployStaging err = %v", role, err)
		}
		if !res.GetOk() {
			t.Errorf("role %s: ok = false, message = %q", role, res.GetMessage())
		}
		if len(exec.promoteCalls) != 1 || exec.promoteCalls[0] != [2]string{"0.9.9", "staging"} {
			t.Errorf("role %s: promote calls = %v", role, exec.promoteCalls)
		}
	}
}

// --- (b) writer + reader denied + blocked audit event ----------------------

func TestWriteRPCsDenyNonAdminWithBlockedAudit(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader} {
		exec := &fakeExecutor{}
		audit := &fakeAudit{}
		svc := newTestService(t, exec, audit)

		_, err := svc.Promote(ctxWithRole(role), &memqlv1.PromoteRequest{Version: "0.9.9"})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("role %s: code = %v, want PermissionDenied", role, status.Code(err))
		}
		if len(exec.promoteCalls) != 0 {
			t.Errorf("role %s: promote should NOT run on denial, got %v", role, exec.promoteCalls)
		}
		if len(audit.events) != 1 {
			t.Fatalf("role %s: want exactly 1 audit event, got %d", role, len(audit.events))
		}
		ev := audit.events[0]
		if ev.Outcome != identity.AuditOutcomeBlocked {
			t.Errorf("role %s: outcome = %q, want blocked", role, ev.Outcome)
		}
		if ev.Action != "deployment_console_promote" {
			t.Errorf("role %s: action = %q", role, ev.Action)
		}
		if ev.Category != identity.AuditCategoryAdmin {
			t.Errorf("role %s: category = %q, want admin", role, ev.Category)
		}
	}
}

func TestUnauthenticatedDenied(t *testing.T) {
	exec := &fakeExecutor{}
	audit := &fakeAudit{}
	svc := newTestService(t, exec, audit)

	_, err := svc.Rollback(context.Background(), &memqlv1.RollbackRequest{Env: "prod", CommitSha: "abc"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeBlocked {
		t.Errorf("want one blocked audit event, got %+v", audit.events)
	}
}

// --- (c) successful write emits exactly one success event ------------------

func TestSuccessfulWriteEmitsOneSuccessEventWithId(t *testing.T) {
	exec := &fakeExecutor{promoteOut: "SUCCESS: promoted to prod"}
	audit := &fakeAudit{}
	svc := newTestService(t, exec, audit)

	res, err := svc.Promote(ctxWithRole(auth.RoleOwner), &memqlv1.PromoteRequest{Version: "1.0.0"})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if !res.GetOk() {
		t.Fatalf("ok = false: %q", res.GetMessage())
	}
	if len(audit.events) != 1 {
		t.Fatalf("want exactly 1 audit event, got %d", len(audit.events))
	}
	ev := audit.events[0]
	if ev.Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("outcome = %q, want success", ev.Outcome)
	}
	if ev.ActorRole != string(auth.RoleOwner) {
		t.Errorf("actor role = %q", ev.ActorRole)
	}
	if ev.ActorEmail != "actor@example.com" {
		t.Errorf("actor email = %q", ev.ActorEmail)
	}
	if res.GetAuditEventId() == "" {
		t.Error("ActionResult.AuditEventId is empty")
	}
	if res.GetAuditEventId() != ev.CorrelationId {
		t.Errorf("ActionResult id %q != audit event correlation id %q", res.GetAuditEventId(), ev.CorrelationId)
	}
	if res.GetDetails()["version"] != "1.0.0" || res.GetDetails()["env"] != "prod" {
		t.Errorf("details = %v", res.GetDetails())
	}
}

func TestFailedWriteEmitsFailureEvent(t *testing.T) {
	exec := &fakeExecutor{promoteErr: errors.New("promote.sh exit 1")}
	audit := &fakeAudit{}
	svc := newTestService(t, exec, audit)

	res, err := svc.DeployStaging(ctxWithRole(auth.RoleAdmin), &memqlv1.DeployStagingRequest{Version: "0.9.9"})
	if err != nil {
		t.Fatalf("DeployStaging returned status err = %v (expected ActionResult with ok=false)", err)
	}
	if res.GetOk() {
		t.Error("ok = true, want false on script failure")
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeFailure {
		t.Fatalf("want one failure audit event, got %+v", audit.events)
	}
}

func TestRolloutActionValidation(t *testing.T) {
	exec := &fakeExecutor{}
	svc := newTestService(t, exec, &fakeAudit{})

	_, err := svc.RolloutAction(ctxWithRole(auth.RoleOwner), &memqlv1.RolloutActionRequest{
		Env: "staging", Rollout: "bff", Action: "bogus",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument for bad action", status.Code(err))
	}

	res, err := svc.RolloutAction(ctxWithRole(auth.RoleOwner), &memqlv1.RolloutActionRequest{
		Env: "staging", Rollout: "bff", Action: "promote",
	})
	if err != nil {
		t.Fatalf("RolloutAction: %v", err)
	}
	if !res.GetOk() {
		t.Error("ok = false")
	}
	if len(exec.rolloutCalls) != 1 || exec.rolloutCalls[0] != [3]string{"staging", "bff", "promote"} {
		t.Errorf("rollout calls = %v", exec.rolloutCalls)
	}
}

// --- (d) read RPC returns mapped status, writes NO audit event -------------

func TestGetDeploymentStatusReadsAndDoesNotAudit(t *testing.T) {
	tmp := t.TempDir()
	// Write a prod overlay + matching lockfile into the temp repo root.
	overlayDir := filepath.Join(tmp, "deploy", "k8s", "overlays", "prod")
	if err := os.MkdirAll(overlayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"), []byte(prodOverlayFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	releasesDir := filepath.Join(tmp, "releases")
	if err := os.MkdirAll(releasesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releasesDir, "0.9.9.yaml"), []byte(lockfileFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := &fakeExecutor{
		argoJSON:     []byte(`{"status":{"sync":{"status":"OutOfSync","revision":"r1"},"health":{"status":"Degraded"},"operationState":{"phase":"Error","finishedAt":"2026-06-02T00:00:00Z"}}}`),
		rolloutsJSON: []byte(`{"items":[{"metadata":{"name":"bff"},"spec":{"strategy":{"blueGreen":{}}},"status":{"phase":"Paused"}}]}`),
		analysisJSON: []byte(`{"items":[{"metadata":{"name":"g1","creationTimestamp":"2026-06-02T00:00:00Z"},"status":{"phase":"Failed","completedAt":"2026-06-02T00:05:00Z","metricResults":[{"name":"authenticated-query","phase":"Failed","message":"Unauthenticated"}]}}]}`),
	}
	audit := &fakeAudit{}
	svc, err := NewService(Options{Logger: quietLogger(), Audit: audit, RepoRoot: tmp, Executor: exec})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// An owner/admin can read; the read path is admin-gated (#728)
	// but a successful read is NOT audited.
	out, err := svc.GetDeploymentStatus(ctxWithRole(auth.RoleOwner), &memqlv1.GetDeploymentStatusRequest{Env: "prod"})
	if err != nil {
		t.Fatalf("GetDeploymentStatus: %v", err)
	}
	if out.GetVersion() != "0.9.9" {
		t.Errorf("Version = %q, want 0.9.9", out.GetVersion())
	}
	if out.GetEngineVersion() != "0.9.9" {
		t.Errorf("EngineVersion = %q", out.GetEngineVersion())
	}
	if len(out.GetComponents()) != 2 {
		t.Fatalf("got %d components, want 2", len(out.GetComponents()))
	}
	// Repo enriched from the lockfile.
	if out.GetComponents()[0].GetRepo() != "znasllc-io/memql" {
		t.Errorf("component[0] repo = %q", out.GetComponents()[0].GetRepo())
	}
	if !out.GetArgocd().GetOutOfSync() {
		t.Error("argocd should be OutOfSync")
	}
	if out.GetArgocd().GetHealthStatus() != "Degraded" {
		t.Errorf("argocd health = %q", out.GetArgocd().GetHealthStatus())
	}
	if len(out.GetRollouts()) != 1 || out.GetRollouts()[0].GetKind() != "bluegreen" {
		t.Errorf("rollouts = %+v", out.GetRollouts())
	}
	if out.GetGateResult().GetResult() != "fail" {
		t.Errorf("gate result = %q, want fail", out.GetGateResult().GetResult())
	}

	// Read path must NOT audit.
	if len(audit.events) != 0 {
		t.Errorf("read path emitted %d audit events, want 0", len(audit.events))
	}
}

// --- (e) read RPC denies non-admin with a blocked audit event -------------

func TestGetDeploymentStatusDeniesNonAdminWithBlockedAudit(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader} {
		exec := &fakeExecutor{}
		audit := &fakeAudit{}
		svc := newTestService(t, exec, audit)

		_, err := svc.GetDeploymentStatus(ctxWithRole(role), &memqlv1.GetDeploymentStatusRequest{Env: "staging"})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("role %s: code = %v, want PermissionDenied", role, status.Code(err))
		}
		if len(audit.events) != 1 {
			t.Fatalf("role %s: want exactly 1 audit event, got %d", role, len(audit.events))
		}
		ev := audit.events[0]
		if ev.Outcome != identity.AuditOutcomeBlocked {
			t.Errorf("role %s: outcome = %q, want blocked", role, ev.Outcome)
		}
		if ev.Action != "deployment_console_get_status" {
			t.Errorf("role %s: action = %q, want deployment_console_get_status", role, ev.Action)
		}
		if ev.Category != identity.AuditCategoryAdmin {
			t.Errorf("role %s: category = %q, want admin", role, ev.Category)
		}
	}
}

func TestGetDeploymentStatusUnauthenticatedDenied(t *testing.T) {
	exec := &fakeExecutor{}
	audit := &fakeAudit{}
	svc := newTestService(t, exec, audit)

	_, err := svc.GetDeploymentStatus(context.Background(), &memqlv1.GetDeploymentStatusRequest{Env: "staging"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeBlocked {
		t.Errorf("want one blocked audit event, got %+v", audit.events)
	}
}

func TestGetDeploymentStatusInvalidEnv(t *testing.T) {
	svc := newTestService(t, &fakeExecutor{}, &fakeAudit{})
	_, err := svc.GetDeploymentStatus(ctxWithRole(auth.RoleOwner), &memqlv1.GetDeploymentStatusRequest{Env: "dev"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

// --- Epic 2 (E2.6 / #2099): azure-path regression after the pack ----------

// TestEpic2AzurePathRegression is the Epic 2 acceptance guard that the deploy
// PACK (examples/deploypack) and the E2.5 effect-seam consolidation
// (promoteRelease) left the live azure deploy path byte-for-byte intact: the
// two legacy console promote actions still shell out to promote.sh with the
// correct (version, env) via the SAME Executor.RunPromote, persist the
// deployment record, and emit one audit event under the right verb. If the
// promoteRelease consolidation ever drifts a call, env, or audit verb, this
// fails -- the pack must stay ADDITIVE, never a behavioral change to the
// authoritative Go path.
func TestEpic2AzurePathRegression(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		version string
		run     func(svc *Service) (*memqlv1.ActionResult, error)
	}{
		{
			name:    "DeployStaging",
			env:     "staging",
			version: "0.9.9",
			run: func(svc *Service) (*memqlv1.ActionResult, error) {
				return svc.DeployStaging(ctxWithRole(auth.RoleOwner), &memqlv1.DeployStagingRequest{Version: "0.9.9"})
			},
		},
		{
			name:    "Promote",
			env:     "prod",
			version: "0.9.9",
			run: func(svc *Service) (*memqlv1.ActionResult, error) {
				return svc.Promote(ctxWithRole(auth.RoleOwner), &memqlv1.PromoteRequest{Version: "0.9.9"})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeExecutor{promoteOut: "SUCCESS: promoted"}
			audit := &fakeAudit{}
			svc := newTestService(t, exec, audit)

			res, err := tc.run(svc)
			if err != nil {
				t.Fatalf("%s: err = %v", tc.name, err)
			}
			if !res.GetOk() {
				t.Fatalf("%s: ok = false: %q", tc.name, res.GetMessage())
			}
			// promote.sh invoked exactly once with the right (version, env).
			if len(exec.promoteCalls) != 1 || exec.promoteCalls[0] != [2]string{tc.version, tc.env} {
				t.Fatalf("%s: promote calls = %v, want [[%s %s]]", tc.name, exec.promoteCalls, tc.version, tc.env)
			}
			// exactly one audit event, success outcome.
			if len(audit.events) != 1 {
				t.Fatalf("%s: audit events = %d, want 1", tc.name, len(audit.events))
			}
			if audit.events[0].Outcome != identity.AuditOutcomeSuccess {
				t.Errorf("%s: audit outcome = %q, want success", tc.name, audit.events[0].Outcome)
			}
		})
	}
}

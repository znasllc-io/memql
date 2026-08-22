package deploycontrol

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	rollbackCalls []string    // sha
	rollbackErr   error       // when set, RunRollback returns it
	rolloutCalls  [][2]string // (rollout, action)
	repairCalls   []string    // the marker (repair record id) per RunRepair
	repairErr     error       // when set, RunRepair returns it

	argoJSON     []byte
	rolloutsJSON []byte
	analysisJSON []byte
	kubectlErr   error
	// argoJSONSeq, when set, is what successive Application reads return, in
	// order, the last entry repeating -- so a watcher test can script the
	// controller's progress (previous operation -> ours Running -> ours
	// Succeeded) without a clock. Takes precedence over argoJSON.
	argoJSONSeq [][]byte
	argoReads   int

	// kubectlCalls records the full argv of every read, so a test can assert
	// WHICH namespace / Application was addressed. A wrong address does not
	// error -- `kubectl -n <nonexistent> get rollout` is an empty list and exit
	// 0 -- so nothing but the argv distinguishes "nothing is rolling out" from
	// "asked the wrong namespace".
	kubectlCalls [][]string
}

func (f *fakeExecutor) RunRollback(_ context.Context, sha string) (string, error) {
	f.rollbackCalls = append(f.rollbackCalls, sha)
	if f.rollbackErr != nil {
		return "", f.rollbackErr
	}
	return "reverted " + sha, nil
}

func (f *fakeExecutor) RunRolloutAction(_ context.Context, rollout, action string) (string, error) {
	f.rolloutCalls = append(f.rolloutCalls, [2]string{rollout, action})
	return action + " " + rollout, nil
}

func (f *fakeExecutor) RunRepair(_ context.Context, marker string) (string, error) {
	f.repairCalls = append(f.repairCalls, marker)
	if f.repairErr != nil {
		return "", f.repairErr
	}
	return "application/memql annotated\napplication.argoproj.io/memql patched", nil
}

func (f *fakeExecutor) KubectlJSON(_ context.Context, args ...string) ([]byte, error) {
	f.kubectlCalls = append(f.kubectlCalls, append([]string(nil), args...))
	if f.kubectlErr != nil {
		return nil, f.kubectlErr
	}
	// Discriminate on the resource arg.
	for _, a := range args {
		switch a {
		case "app":
			if len(f.argoJSONSeq) > 0 {
				idx := f.argoReads
				if idx >= len(f.argoJSONSeq) {
					idx = len(f.argoJSONSeq) - 1
				}
				f.argoReads++
				return f.argoJSONSeq[idx], nil
			}
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
		RepoRoot: repoRootWithOverlay(t),
		Executor: exec,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	stubRepairWatch(svc)
	return svc
}

// repoRootWithOverlay is a repo root that HAS a deploy checkout -- which is
// what every deploy-capable node has, and what these tests are about.
//
// It matters that this is explicit (memql#4265). The harness used to hand out
// a bare t.TempDir(), and the overlay reader swallowed the resulting ENOENT
// and returned "" -- so a whole suite ran against a state no real deploy node
// is ever in, and the swallow that made it possible was the defect. The
// overlay written here promotes nothing, which is the genuine "no version cut
// yet" case those tests mean.
func repoRootWithOverlay(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, overlayDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("overlay dir: %v", err)
	}
	const empty = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(empty), 0o644); err != nil {
		t.Fatalf("overlay: %v", err)
	}
	return root
}

// stubRepairWatch replaces the repair watcher goroutine (memql#4209) with a
// recorder that releases the one-repair-at-a-time guard immediately, and
// returns the ids it was handed. Every test helper installs it so an
// admitted Repair in an unrelated suite never leaves a goroutine polling a
// fake executor across test boundaries; the watcher itself is exercised
// synchronously in repair_test.go.
func stubRepairWatch(svc *Service) *[]string {
	var started []string
	svc.startRepairWatch = func(_ context.Context, id string) {
		started = append(started, id)
		svc.releaseRepair()
	}
	return &started
}

// --- (a) owner + admin admitted -------------------------------------------

func TestWriteRPCAdmittedForOwnerAndAdmin(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleAdmin} {
		exec := &fakeExecutor{}
		audit := &fakeAudit{}
		svc := newTestService(t, exec, audit)

		res, err := svc.Rollback(ctxWithRole(role), &memqlv1.RollbackRequest{CommitSha: "abc1234"})
		if err != nil {
			t.Fatalf("role %s: Rollback err = %v", role, err)
		}
		if !res.GetOk() {
			t.Errorf("role %s: ok = false, message = %q", role, res.GetMessage())
		}
		if len(exec.rollbackCalls) != 1 || exec.rollbackCalls[0] != "abc1234" {
			t.Errorf("role %s: rollback calls = %v", role, exec.rollbackCalls)
		}
	}
}

// --- (b) writer + reader denied + blocked audit event ----------------------

func TestWriteRPCsDenyNonAdminWithBlockedAudit(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleWriter, auth.RoleReader} {
		exec := &fakeExecutor{}
		audit := &fakeAudit{}
		svc := newTestService(t, exec, audit)

		_, err := svc.RolloutAction(ctxWithRole(role), &memqlv1.RolloutActionRequest{Rollout: "bff", Action: "promote"})
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
		if ev.Action != "deployment_console_rollout_action" {
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

	_, err := svc.Rollback(context.Background(), &memqlv1.RollbackRequest{CommitSha: "abc"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeBlocked {
		t.Errorf("want one blocked audit event, got %+v", audit.events)
	}
}

// --- (c) successful write emits exactly one success event ------------------

func TestSuccessfulWriteEmitsOneSuccessEventWithId(t *testing.T) {
	exec := &fakeExecutor{}
	audit := &fakeAudit{}
	svc := newTestService(t, exec, audit)

	res, err := svc.Rollback(ctxWithRole(auth.RoleOwner), &memqlv1.RollbackRequest{CommitSha: "abc1234"})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
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
	if res.GetDetails()["commitSha"] != "abc1234" {
		t.Errorf("details = %v", res.GetDetails())
	}
}

func TestFailedWriteEmitsFailureEvent(t *testing.T) {
	exec := &fakeExecutor{rollbackErr: errors.New("git revert exit 1")}
	audit := &fakeAudit{}
	svc := newTestService(t, exec, audit)

	res, err := svc.Rollback(ctxWithRole(auth.RoleAdmin), &memqlv1.RollbackRequest{CommitSha: "abc1234"})
	if err != nil {
		t.Fatalf("Rollback returned status err = %v (expected ActionResult with ok=false)", err)
	}
	if res.GetOk() {
		t.Error("ok = true, want false on effect failure")
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeFailure {
		t.Fatalf("want one failure audit event, got %+v", audit.events)
	}
}

func TestRolloutActionValidation(t *testing.T) {
	exec := &fakeExecutor{}
	svc := newTestService(t, exec, &fakeAudit{})

	_, err := svc.RolloutAction(ctxWithRole(auth.RoleOwner), &memqlv1.RolloutActionRequest{
		Rollout: "bff", Action: "bogus",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument for bad action", status.Code(err))
	}

	res, err := svc.RolloutAction(ctxWithRole(auth.RoleOwner), &memqlv1.RolloutActionRequest{
		Rollout: "bff", Action: "promote",
	})
	if err != nil {
		t.Fatalf("RolloutAction: %v", err)
	}
	if !res.GetOk() {
		t.Error("ok = false")
	}
	if len(exec.rolloutCalls) != 1 || exec.rolloutCalls[0] != [2]string{"bff", "promote"} {
		t.Errorf("rollout calls = %v", exec.rolloutCalls)
	}
}

// --- (d) read RPC returns mapped status, writes NO audit event -------------

func TestGetDeploymentStatusReadsAndDoesNotAudit(t *testing.T) {
	tmp := t.TempDir()
	// Write the cloud overlay + matching lockfile into the temp repo root.
	overlayDir := filepath.Join(tmp, "deploy", "k8s", "overlays", "cloud")
	if err := os.MkdirAll(overlayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"), []byte(overlayFixture), 0o644); err != nil {
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
	out, err := svc.GetDeploymentStatus(ctxWithRole(auth.RoleOwner), &memqlv1.GetDeploymentStatusRequest{})
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

		_, err := svc.GetDeploymentStatus(ctxWithRole(role), &memqlv1.GetDeploymentStatusRequest{})
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

	_, err := svc.GetDeploymentStatus(context.Background(), &memqlv1.GetDeploymentStatusRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
	if len(audit.events) != 1 || audit.events[0].Outcome != identity.AuditOutcomeBlocked {
		t.Errorf("want one blocked audit event, got %+v", audit.events)
	}
}

// A node with NO deploy checkout answers with a typed precondition naming
// which situation it is in -- not an Internal error carrying a file path
// (memql#4265).
//
// This is the state every in-cluster node is actually in today: the console
// was designed around an on-disk checkout named by MEMQL_DEPLOY_REPO_ROOT, no
// manifest sets it, and no image ships deploy/. The surface has to be honest
// about that rather than rendering an ENOENT at an operator and then claiming
// "nothing is pinned", which reads as "nothing is deployed" and is false.
func TestGetDeploymentStatusWithoutACheckoutIsAPrecondition(t *testing.T) {
	svc, err := NewService(Options{
		Logger:   quietLogger(),
		Audit:    &fakeAudit{},
		RepoRoot: t.TempDir(), // deliberately bare: no deploy/ tree
		Executor: &fakeExecutor{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	stubRepairWatch(svc)

	_, err = svc.GetDeploymentStatus(ctxWithRole(auth.RoleOwner), &memqlv1.GetDeploymentStatusRequest{})
	if err == nil {
		t.Fatal("expected a refusal from a node with no deploy checkout")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (got %v)", st.Code(), err)
	}
	if !strings.HasPrefix(st.Message(), ReasonNoOverlayCheckout+":") {
		t.Errorf("message must lead with the machine-readable reason so a client can\n"+
			"branch without parsing prose; got:\n%s", st.Message())
	}
	if !strings.Contains(st.Message(), "MEMQL_DEPLOY_REPO_ROOT") {
		t.Errorf("the message must name what is unset, so an operator knows what to do:\n%s", st.Message())
	}
}

// On a LOCAL cluster the same absence means something different, and says so.
// driver.go has refused docker-local deploys since the console was written;
// this is the read side finally able to explain it, now that the local overlay
// stamps the provider (memql#4265).
func TestGetDeploymentStatusOnALocalClusterSaysSo(t *testing.T) {
	t.Setenv("MEMQL_DEPLOY_PROVIDER", "docker-local")
	svc, err := NewService(Options{
		Logger:   quietLogger(),
		Audit:    &fakeAudit{},
		RepoRoot: t.TempDir(),
		Executor: &fakeExecutor{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	stubRepairWatch(svc)

	_, err = svc.GetDeploymentStatus(ctxWithRole(auth.RoleOwner), &memqlv1.GetDeploymentStatusRequest{})
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", st.Code())
	}
	if !strings.HasPrefix(st.Message(), ReasonLocalCluster+":") {
		t.Errorf("a local cluster must be told it is not a deploy target, not that a\n"+
			"checkout is missing; got:\n%s", st.Message())
	}
	if !strings.Contains(st.Message(), "make up") {
		t.Errorf("the message must name how a local cluster IS operated:\n%s", st.Message())
	}
}

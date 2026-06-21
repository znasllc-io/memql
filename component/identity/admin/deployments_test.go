package admin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
)

// newRenderableAdminServer builds an AdminServer with just enough
// wiring (a real web Server for the shared Layout) to render an
// admin page in a test.
func newRenderableAdminServer(t *testing.T) *AdminServer {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	web, err := identityweb.NewServer(identity.Config{}, logger, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return &AdminServer{
		Logger:    logger,
		WebServer: web,
	}
}

// fakeDeployAudit records audit events emitted by the deploy-control
// service. The admin read path must NOT trigger any on a successful
// read (success reads are un-audited).
type fakeDeployAudit struct {
	events []identity.AuditEvent
}

func (f *fakeDeployAudit) Log(_ context.Context, ev identity.AuditEvent) {
	f.events = append(f.events, ev)
}

// fakeDeployExecutor returns canned read fixtures; the action paths
// are never reached on the read view.
type fakeDeployExecutor struct{}

func (fakeDeployExecutor) RunPromote(context.Context, string, string) (string, error) {
	return "", nil
}
func (fakeDeployExecutor) RunRollback(context.Context, string, string) (string, error) {
	return "", nil
}
func (fakeDeployExecutor) RunRolloutAction(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (fakeDeployExecutor) RunDockerComposeDeploy(context.Context, string) (string, error) {
	return "", nil
}
func (fakeDeployExecutor) KubectlJSON(context.Context, ...string) ([]byte, error) {
	// Read paths are best-effort; returning an error leaves the
	// Argo/rollout/gate sections empty without failing the read.
	return nil, io.EOF
}
func (fakeDeployExecutor) Git(context.Context, ...string) (string, error) { return "", nil }

// adminClaimsContext mirrors what requireAdmin stamps: the validated
// *identity.AccessTokenClaims under claimsCtxKey{}. Used so the test
// exercises the same context shape the live handler sees.
func adminClaimsContext(role string) context.Context {
	claims := &identity.AccessTokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: "v1:identity:user.admin1"},
		Email:            "admin@example.com",
		Role:             role,
	}
	return context.WithValue(context.Background(), claimsCtxKey{}, claims)
}

// TestDeployActorContextAdmitsGatedRead proves the in-process admin
// context built by deployActorContext satisfies the owner/admin gate
// on the deploy-control read RPC (#728): an admin context reads
// successfully, and the successful read is NOT audited.
func TestDeployActorContextAdmitsGatedRead(t *testing.T) {
	repo := t.TempDir()
	writeDeployFixtures(t, repo)

	audit := &fakeDeployAudit{}
	svc, err := deploycontrol.NewService(deploycontrol.Options{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit:    audit,
		RepoRoot: repo,
		Executor: fakeDeployExecutor{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	reader := deployReaderFunc(func(ctx context.Context, env string) (*memqlv1.DeploymentStatus, error) {
		return svc.GetDeploymentStatus(ctx, &memqlv1.GetDeploymentStatusRequest{Env: env})
	})

	// Without the access context (only the requireAdmin-stamped
	// claims), the gated RPC must reject -- proves the AccessContext
	// stamp is load-bearing.
	rawCtx := adminClaimsContext("admin")
	if _, err := reader.DeploymentStatus(rawCtx, "prod"); err == nil {
		t.Fatal("read succeeded WITHOUT an access context; the gate should have denied it")
	}

	// With deployActorContext applied, the same admin claims now
	// resolve to an owner/admin actor and the read is admitted. The
	// successful read must NOT emit any new audit event.
	beforeSuccess := len(audit.events)
	gatedCtx := deployActorContext(rawCtx)
	out, err := reader.DeploymentStatus(gatedCtx, "prod")
	if err != nil {
		t.Fatalf("admin read denied unexpectedly: %v", err)
	}
	if out.GetVersion() != "0.9.9" {
		t.Errorf("version = %q, want 0.9.9", out.GetVersion())
	}
	if len(audit.events) != beforeSuccess {
		t.Errorf("successful read emitted %d new audit events, want 0", len(audit.events)-beforeSuccess)
	}

	// A reader-role admin context is still denied (defence in depth:
	// requireAdmin already blocks non-admins, but the RPC gate must
	// hold independently).
	if _, err := reader.DeploymentStatus(deployActorContext(adminClaimsContext("reader")), "prod"); err == nil {
		t.Fatal("reader role admitted by the gated read RPC; want denial")
	}
}

// TestHandleDeploymentsGetRendersWithAdminContext drives the full
// handler: an admin-stamped request renders the deployment view
// without error and asks the reader for the requested env.
func TestHandleDeploymentsGetRendersWithAdminContext(t *testing.T) {
	var gotEnv string
	var gotResolved bool
	reader := deployReaderFunc(func(ctx context.Context, env string) (*memqlv1.DeploymentStatus, error) {
		gotEnv = env
		_, gotResolved = auth.AccessFromContext(ctx)
		return &memqlv1.DeploymentStatus{Env: env, Version: "1.2.3"}, nil
	})

	s := newRenderableAdminServer(t)
	s.SetDeployControlReader(reader)

	req := httptest.NewRequest(http.MethodGet, "/admin/deployments?env=prod", nil)
	req = req.WithContext(adminClaimsContext("admin"))
	rec := httptest.NewRecorder()

	s.handleDeploymentsGet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotEnv != "prod" {
		t.Errorf("reader called with env = %q, want prod", gotEnv)
	}
	if !gotResolved {
		t.Error("handler did not stamp an auth.AccessContext before calling the reader")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Deployments") || !strings.Contains(body, "1.2.3") {
		t.Errorf("rendered body missing expected content:\n%s", body)
	}
}

// fakeDeployActions records calls to the write port so tests can
// assert which action ran (and that a rejected confirm never invokes
// the service). Each method returns a canned ActionResult.
type fakeDeployActions struct {
	deployStagingCalls []string
	promoteCalls       []string
	rollbackCalls      [][2]string
	rolloutCalls       [][3]string

	result *memqlv1.ActionResult
	err    error
}

func (f *fakeDeployActions) DeployStaging(_ context.Context, version string) (*memqlv1.ActionResult, error) {
	f.deployStagingCalls = append(f.deployStagingCalls, version)
	return f.result, f.err
}

func (f *fakeDeployActions) Promote(_ context.Context, version string) (*memqlv1.ActionResult, error) {
	f.promoteCalls = append(f.promoteCalls, version)
	return f.result, f.err
}

func (f *fakeDeployActions) Rollback(_ context.Context, env, commitSha string) (*memqlv1.ActionResult, error) {
	f.rollbackCalls = append(f.rollbackCalls, [2]string{env, commitSha})
	return f.result, f.err
}

func (f *fakeDeployActions) RolloutAction(_ context.Context, env, rollout, action string) (*memqlv1.ActionResult, error) {
	f.rolloutCalls = append(f.rolloutCalls, [3]string{env, rollout, action})
	return f.result, f.err
}

// postForm builds an admin-stamped POST request carrying the given
// form values.
func postForm(path string, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req.WithContext(adminClaimsContext("admin"))
}

// TestHandleDeployStagingPostInvokesAction proves a deploy-staging POST
// with an admin context invokes the action adapter and redirects with a
// SUCCESS flash carrying the audit-event id.
func TestHandleDeployStagingPostInvokesAction(t *testing.T) {
	actions := &fakeDeployActions{result: &memqlv1.ActionResult{Ok: true, AuditEventId: "evt-123"}}
	s := newRenderableAdminServer(t)
	s.SetDeployControlActions(actions)

	rec := httptest.NewRecorder()
	s.handleDeployStagingPost(rec, postForm("/admin/deployments/deploy-staging", url.Values{
		"version": {"0.9.10"},
	}))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if len(actions.deployStagingCalls) != 1 || actions.deployStagingCalls[0] != "0.9.10" {
		t.Fatalf("deploy-staging calls = %v, want [0.9.10]", actions.deployStagingCalls)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "flash_kind=success") || !strings.Contains(loc, "evt-123") {
		t.Errorf("redirect = %q, want success flash carrying audit id", loc)
	}
}

// TestHandleDeployPromotePostWrongConfirm proves a promote with a
// mismatched confirm token is rejected with an ERROR flash and never
// reaches the action adapter.
func TestHandleDeployPromotePostWrongConfirm(t *testing.T) {
	actions := &fakeDeployActions{result: &memqlv1.ActionResult{Ok: true, AuditEventId: "evt-x"}}
	s := newRenderableAdminServer(t)
	s.SetDeployControlActions(actions)

	rec := httptest.NewRecorder()
	s.handleDeployPromotePost(rec, postForm("/admin/deployments/promote", url.Values{
		"version": {"0.9.10"},
		"confirm": {"0.9.11"}, // wrong
	}))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if len(actions.promoteCalls) != 0 {
		t.Fatalf("promote invoked despite wrong confirm: %v", actions.promoteCalls)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "flash_kind=error") {
		t.Errorf("redirect = %q, want error flash", loc)
	}
}

// TestHandleDeployPromotePostCorrectConfirm proves a promote with a
// matching confirm token invokes the action adapter and surfaces the
// audit id.
func TestHandleDeployPromotePostCorrectConfirm(t *testing.T) {
	actions := &fakeDeployActions{result: &memqlv1.ActionResult{Ok: true, AuditEventId: "evt-456"}}
	s := newRenderableAdminServer(t)
	s.SetDeployControlActions(actions)

	rec := httptest.NewRecorder()
	s.handleDeployPromotePost(rec, postForm("/admin/deployments/promote", url.Values{
		"version": {"0.9.10"},
		"confirm": {"0.9.10"},
	}))

	if len(actions.promoteCalls) != 1 || actions.promoteCalls[0] != "0.9.10" {
		t.Fatalf("promote calls = %v, want [0.9.10]", actions.promoteCalls)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "flash_kind=success") || !strings.Contains(loc, "evt-456") {
		t.Errorf("redirect = %q, want success flash carrying audit id", loc)
	}
}

// TestHandleDeployRollbackPostConfirm covers both the rejected and the
// accepted rollback confirm paths.
func TestHandleDeployRollbackPostConfirm(t *testing.T) {
	t.Run("wrong confirm rejected", func(t *testing.T) {
		actions := &fakeDeployActions{result: &memqlv1.ActionResult{Ok: true, AuditEventId: "evt"}}
		s := newRenderableAdminServer(t)
		s.SetDeployControlActions(actions)

		rec := httptest.NewRecorder()
		s.handleDeployRollbackPost(rec, postForm("/admin/deployments/rollback", url.Values{
			"env":       {"prod"},
			"commitSha": {"a1b2c3d"},
			"confirm":   {"nope"},
		}))
		if len(actions.rollbackCalls) != 0 {
			t.Fatalf("rollback invoked despite wrong confirm: %v", actions.rollbackCalls)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "flash_kind=error") {
			t.Errorf("redirect = %q, want error flash", loc)
		}
	})

	t.Run("literal rollback confirm invokes", func(t *testing.T) {
		actions := &fakeDeployActions{result: &memqlv1.ActionResult{Ok: true, AuditEventId: "evt-789"}}
		s := newRenderableAdminServer(t)
		s.SetDeployControlActions(actions)

		rec := httptest.NewRecorder()
		s.handleDeployRollbackPost(rec, postForm("/admin/deployments/rollback", url.Values{
			"env":       {"prod"},
			"commitSha": {"a1b2c3d"},
			"confirm":   {"rollback"},
		}))
		if len(actions.rollbackCalls) != 1 || actions.rollbackCalls[0] != [2]string{"prod", "a1b2c3d"} {
			t.Fatalf("rollback calls = %v, want [[prod a1b2c3d]]", actions.rollbackCalls)
		}
		loc := rec.Header().Get("Location")
		if !strings.Contains(loc, "flash_kind=success") || !strings.Contains(loc, "evt-789") {
			t.Errorf("redirect = %q, want success flash carrying audit id", loc)
		}
	})
}

// TestHandleDeployRolloutPost covers promote (no confirm) + abort
// (confirm required) paths.
func TestHandleDeployRolloutPost(t *testing.T) {
	t.Run("promote needs no confirm", func(t *testing.T) {
		actions := &fakeDeployActions{result: &memqlv1.ActionResult{Ok: true, AuditEventId: "evt-p"}}
		s := newRenderableAdminServer(t)
		s.SetDeployControlActions(actions)

		rec := httptest.NewRecorder()
		s.handleDeployRolloutPost(rec, postForm("/admin/deployments/rollout", url.Values{
			"env":     {"staging"},
			"rollout": {"memql-bff"},
			"action":  {"promote"},
		}))
		if len(actions.rolloutCalls) != 1 || actions.rolloutCalls[0] != [3]string{"staging", "memql-bff", "promote"} {
			t.Fatalf("rollout calls = %v, want [[staging memql-bff promote]]", actions.rolloutCalls)
		}
	})

	t.Run("abort without confirm rejected", func(t *testing.T) {
		actions := &fakeDeployActions{result: &memqlv1.ActionResult{Ok: true, AuditEventId: "evt"}}
		s := newRenderableAdminServer(t)
		s.SetDeployControlActions(actions)

		rec := httptest.NewRecorder()
		s.handleDeployRolloutPost(rec, postForm("/admin/deployments/rollout", url.Values{
			"env":     {"staging"},
			"rollout": {"memql-bff"},
			"action":  {"abort"},
		}))
		if len(actions.rolloutCalls) != 0 {
			t.Fatalf("abort invoked despite missing confirm: %v", actions.rolloutCalls)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "flash_kind=error") {
			t.Errorf("redirect = %q, want error flash", loc)
		}
	})

	t.Run("abort with confirm invokes", func(t *testing.T) {
		actions := &fakeDeployActions{result: &memqlv1.ActionResult{Ok: true, AuditEventId: "evt-a"}}
		s := newRenderableAdminServer(t)
		s.SetDeployControlActions(actions)

		rec := httptest.NewRecorder()
		s.handleDeployRolloutPost(rec, postForm("/admin/deployments/rollout", url.Values{
			"env":     {"staging"},
			"rollout": {"memql-bff"},
			"action":  {"abort"},
			"confirm": {"abort"},
		}))
		if len(actions.rolloutCalls) != 1 || actions.rolloutCalls[0] != [3]string{"staging", "memql-bff", "abort"} {
			t.Fatalf("rollout calls = %v, want [[staging memql-bff abort]]", actions.rolloutCalls)
		}
	})
}

// deployReaderFunc adapts a func to the DeployControlReader port.
type deployReaderFunc func(ctx context.Context, env string) (*memqlv1.DeploymentStatus, error)

func (f deployReaderFunc) DeploymentStatus(ctx context.Context, env string) (*memqlv1.DeploymentStatus, error) {
	return f(ctx, env)
}

func writeDeployFixtures(t *testing.T, repo string) {
	t.Helper()
	overlayDir := filepath.Join(repo, "deploy", "k8s", "overlays", "prod")
	if err := os.MkdirAll(overlayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	overlay := "# Promoted from releases/0.9.9.yaml -- DIGEST COPY (#702).\n" +
		"apiVersion: kustomize.config.k8s.io/v1beta1\n" +
		"kind: Kustomization\n" +
		"images:\n" +
		"  - name: znasllc-io/memql\n" +
		"    digest: sha256:abc\n"
	if err := os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"), []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
}

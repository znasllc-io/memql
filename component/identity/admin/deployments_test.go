package admin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

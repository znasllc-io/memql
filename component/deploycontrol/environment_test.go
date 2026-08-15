package deploycontrol

// environment_test.go -- the deploy console addresses the ENVIRONMENT it was
// asked about (memql#3769, epic memql#3748).
//
// WHY THIS NEEDS A TEST AT ALL. Every assertion here would have passed
// vacuously before the split, when `memql` was the one namespace and the one
// Application. What makes it worth pinning now is that the failure it catches
// is SILENT: `kubectl -n <a namespace that does not exist> get rollout` returns
// `{"items":[]}` and exit 0. A status read aimed at the wrong namespace does
// not error -- it reports a healthy, idle environment. So the argv is the only
// evidence, and it is what these assert.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestEveryEnvironmentHasOneSurface holds the table itself: the closed set of
// deploy targets, what each is called in the cluster, and which one is promoted
// from which. Stated here as well as in environment.go because these values are
// wire-visible -- an operator's `kubectl -n memql-prod` and this map have to
// agree, and a typo in either is not a compile error.
func TestEveryEnvironmentHasOneSurface(t *testing.T) {
	want := map[string]EnvironmentSurface{
		"staging": {
			ConsoleEnv: "staging", Namespace: "memql-staging", ArgoApp: "memql-staging",
			OverlayDir: filepath.Join("deploy", "k8s", "overlays", "staging"), PromoteFrom: "",
		},
		"prod": {
			ConsoleEnv: "prod", Namespace: "memql-prod", ArgoApp: "memql-prod",
			OverlayDir: filepath.Join("deploy", "k8s", "overlays", "prod"), PromoteFrom: "staging",
		},
	}

	for env, expect := range want {
		got, ok := SurfaceFor(env)
		if !ok {
			t.Fatalf("SurfaceFor(%q) reports no surface", env)
		}
		if got != expect {
			t.Errorf("SurfaceFor(%q) =\n  %+v\nwant\n  %+v", env, got, expect)
		}
		if _, err := os.Stat(filepath.Join(repoRootFromTest(t), got.OverlayDir, "kustomization.yaml")); err != nil {
			t.Errorf("%s's overlay dir does not exist in this checkout: %v", env, err)
		}
	}
	if len(environmentSurfaces) != len(want) {
		t.Errorf("the deploy target set grew to %d without this test deciding what the new one promotes from",
			len(environmentSurfaces))
	}
}

// TestDevelopmentIsNotADeployTarget -- the refusal the design keeps explicitly
// ("`deploycontrol` still refuses local. Unchanged", virtual-environments
// design §5). Two halves, because they are two mechanisms:
//
//   - no local/development SURFACE exists, so nothing can be addressed;
//   - the retired `docker-local` PROVIDER is refused with the message that
//     points at `make up`, which is the message an operator actually sees.
//
// Verified rather than assumed: this issue moved every address in the package
// and the cheapest way to have broken the refusal would have been to add a
// well-meaning `local` row to the surface table.
func TestDevelopmentIsNotADeployTarget(t *testing.T) {
	for _, name := range []string{"local", "development", "dev", "", "nowhere"} {
		if s, ok := SurfaceFor(name); ok {
			t.Errorf("SurfaceFor(%q) resolved to %+v; development is not a deploy target and local clusters are operated via `make up`", name, s)
		}
	}

	err := validateDeploymentProvider("docker-local")
	if err == nil {
		t.Fatal("validateDeploymentProvider(docker-local) admitted a retired provider")
	}
	// The EXISTING message, verbatim in its load-bearing parts: an operator
	// reading it must be told where to go instead.
	for _, want := range []string{"no longer supported", "make up", "k3d", "ArgoCD", "not the deploy console"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message lost %q:\n  %s", want, err.Error())
		}
	}

	// ConsoleEnvFor is UNCHANGED by this issue, and development still maps to
	// the empty console env -- which is what keeps a development record from
	// reaching a surface lookup in the first place.
	for _, env := range []string{"development", "local", "", "whatever"} {
		if got := ConsoleEnvFor(env); got != "" {
			t.Errorf("ConsoleEnvFor(%q) = %q, want \"\"", env, got)
		}
	}
	if got := ConsoleEnvFor("production"); got != "prod" {
		t.Errorf("ConsoleEnvFor(production) = %q, want prod", got)
	}
	if got := ConsoleEnvFor("staging"); got != "staging" {
		t.Errorf("ConsoleEnvFor(staging) = %q, want staging", got)
	}
}

// TestStatusReadsAddressTheEnvironmentsOwnSurface is the repointing itself: the
// three cluster reads behind GetDeploymentStatus name the environment's
// namespace and its Application, not the constant `memql` they named while that
// was the product pack's surface.
func TestStatusReadsAddressTheEnvironmentsOwnSurface(t *testing.T) {
	for _, tc := range []struct{ env, namespace, app string }{
		{"prod", "memql-prod", "memql-prod"},
		{"staging", "memql-staging", "memql-staging"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			tmp := t.TempDir()
			overlayDir := filepath.Join(tmp, "deploy", "k8s", "overlays", tc.env)
			if err := os.MkdirAll(overlayDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"), []byte(prodOverlayFixture), 0o644); err != nil {
				t.Fatal(err)
			}

			exec := &fakeExecutor{
				argoJSON:     []byte(`{"status":{"sync":{"status":"Synced","revision":"r1"},"health":{"status":"Healthy"}}}`),
				rolloutsJSON: []byte(`{"items":[]}`),
				analysisJSON: []byte(`{"items":[]}`),
			}
			svc, err := NewService(Options{Logger: quietLogger(), Audit: &fakeAudit{}, RepoRoot: tmp, Executor: exec})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if _, err := svc.GetDeploymentStatus(ctxWithRole(auth.RoleOwner), &memqlv1.GetDeploymentStatusRequest{Env: tc.env}); err != nil {
				t.Fatalf("GetDeploymentStatus: %v", err)
			}

			if len(exec.kubectlCalls) != 3 {
				t.Fatalf("want 3 cluster reads (argo app, rollouts, analysisruns), got %d: %v", len(exec.kubectlCalls), exec.kubectlCalls)
			}
			assertArgv(t, exec.kubectlCalls[0], []string{"-n", "argocd", "get", "app", tc.app, "-o", "json"})
			assertArgv(t, exec.kubectlCalls[1], []string{"-n", tc.namespace, "get", "rollout", "-o", "json"})
			assertArgv(t, exec.kubectlCalls[2], []string{"-n", tc.namespace, "get", "analysisrun", "-o", "json"})

			for _, call := range exec.kubectlCalls {
				for i, arg := range call {
					// The bare `memql` is the product pack's namespace and the
					// name of an Application that no longer exists. It is not a
					// near-miss for either environment's address -- it is a
					// different, live thing.
					if arg == "memql" && i > 0 && call[i-1] != "argocd" {
						t.Errorf("a read still addresses the bare `memql` surface: %v", call)
					}
				}
			}
		})
	}
}

func assertArgv(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("kubectl argv =\n  %v\nwant\n  %v", got, want)
	}
}

// repoRootFromTest walks up from the package directory to the repository root.
// component/deploycontrol is its own Go module, so the test's working directory
// is the package dir and the root is two levels above it.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

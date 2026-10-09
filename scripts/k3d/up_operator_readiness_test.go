package k3d

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArgoReadinessRequiresReconciliationComponents(t *testing.T) {
	for _, resource := range []string{"deployment/argocd-redis", "deployment/argocd-repo-server", "statefulset/argocd-application-controller", "deployment/argocd-server"} {
		t.Run(resource, func(t *testing.T) {
			out, code, calls := kubectlCalls(t, "FAKE_ROLLOUT_FAILURE="+resource)
			if code == 0 || !strings.Contains(out, resource) || !strings.Contains(out, "ImagePullBackOff") {
				t.Fatalf("unready reconciler declared ready: %d\n%s", code, out)
			}
			if !anyCall(calls, "get pods -n argocd") {
				t.Fatal("missing pod diagnostics")
			}
		})
	}
}
func TestMissingOperatorReportsItsOwnApplication(t *testing.T) {
	out, code := runUpFunc(t, "MEMQL_K3D_OPERATOR_TIMEOUT=0\n_wait_for_operator cert-manager cert-manager cert-manager\n", "FAKE_OPERATOR_ABSENT=1", "FAKE_APP_CONDITIONS=ComparisonError: repository fetch failed", "FAKE_APP_SYNC=Unknown")
	if code != 5 || !strings.Contains(out, "Application cert-manager") || !strings.Contains(out, "repository fetch failed") {
		t.Fatalf("opaque operator failure: %d\n%s", code, out)
	}
}

func TestOperatorWithoutStatusIncludesBoundedReconcilerDiagnostics(t *testing.T) {
	log := filepath.Join(t.TempDir(), "kubectl-calls")
	out, code := runUpFunc(t, "MEMQL_K3D_OPERATOR_TIMEOUT=0\n_wait_for_operator cert-manager cert-manager cert-manager\n",
		"FAKE_KUBECTL_LOG="+log, "FAKE_OPERATOR_ABSENT=1", "FAKE_APP_CONDITIONS=", "FAKE_APP_SYNC=",
		"FAKE_CONTROLLER_LOG=controller: unable to load cluster state",
		"FAKE_REPO_LOG=repo-server: connection refused", "FAKE_LOG_EXIT=1")
	if code != 5 || !strings.Contains(out, "unable to load cluster state") || !strings.Contains(out, "repo-server: connection refused") || !strings.Contains(out, "timed out") {
		t.Fatalf("diagnostics hid the failure or stopped before reporting it: %d\n%s", code, out)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if strings.Contains(call, "get deployment cert-manager") {
			continue
		}
		if !strings.Contains(call, "--request-timeout=10s") {
			t.Errorf("failure diagnostics may wait indefinitely: %s", call)
		}
	}
}

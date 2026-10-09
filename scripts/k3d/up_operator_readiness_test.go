package k3d

import (
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

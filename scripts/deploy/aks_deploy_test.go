// Tests for the AKS deploy orchestrator (scripts/deploy/aks-deploy.sh)
// and its rollback companion (scripts/deploy/aks-rollback.sh), focused on
// the smoke health-GATE + rollback contract added in znasllc-io/memql#554
// (epic #549). Function-based bash per the Skills+Scripts architecture;
// these run from `go test ./...` so CI catches regressions without a live
// cluster. Same package as deploy_setup_test.go -- names are aks-prefixed
// to avoid collisions. Cases that need `kubectl` on PATH skip when it is
// absent (--help/-n do not).
package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func aksScript(t *testing.T, name string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s not found at %s: %v", name, p, err)
	}
	return p
}

func runAks(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", append([]string{aksScript(t, name)}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func aksSyntax(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", "-n", aksScript(t, name)).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n %s failed: %v\n%s", name, err, out)
	}
}

func requireKubectl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not on PATH; skipping cluster-touching dry-run case")
	}
}

func TestAksDeploySyntax(t *testing.T)     { aksSyntax(t, "aks-deploy.sh") }
func TestAksRollbackSyntax(t *testing.T)   { aksSyntax(t, "aks-rollback.sh") }
func TestAksAutoscalerSyntax(t *testing.T) { aksSyntax(t, "aks-autoscaler.sh") }

// The deploy --help must advertise the gate knobs added in #554, plus the
// pre-deploy headroom guard added in #614.
func TestAksDeployHelpHasGateFlags(t *testing.T) {
	out, err := runAks(t, "aks-deploy.sh", "--help")
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{"Usage:", "--no-gate", "--no-smoke", "--skip-migrate", "--gate-headroom", "--skip-headroom"} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy --help missing %q:\n%s", want, out)
		}
	}
}

// A dry-run deploy plans the pre-deploy headroom check with the correct surge
// math (8 Deployments x 200m = 1600m) and never touches Azure or the cluster.
func TestAksDeployDryRunPlansHeadroomCheck(t *testing.T) {
	requireKubectl(t)
	out, err := runAks(t, "aks-deploy.sh", "--dry-run", "--skip-build")
	if err != nil {
		t.Fatalf("--dry-run exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{"headroom guard", "8 pods", "1600m"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run plan missing %q:\n%s", want, out)
		}
	}
}

// --skip-headroom suppresses the surge check.
func TestAksDeploySkipHeadroom(t *testing.T) {
	requireKubectl(t)
	out, err := runAks(t, "aks-deploy.sh", "--dry-run", "--skip-build", "--skip-headroom")
	if err != nil {
		t.Fatalf("--dry-run exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "skipping the surge headroom check") {
		t.Errorf("--skip-headroom should suppress the check:\n%s", out)
	}
}

// The autoscaler IaC --help advertises the codified #614 / §9 sizing and the
// owner-gated live command.
func TestAksAutoscalerHelp(t *testing.T) {
	out, err := runAks(t, "aks-autoscaler.sh", "--help")
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{"Usage:", "--dry-run", "--show", "--enable-cluster-autoscaler", "--min-count 2", "--max-count 5", "nodepool1"} {
		if !strings.Contains(out, want) {
			t.Errorf("autoscaler --help missing %q:\n%s", want, out)
		}
	}
}

// A dry-run autoscaler converge plans the exact §9 enable command (min 2 /
// max 5 on nodepool1) and mutates nothing -- no az calls.
func TestAksAutoscalerDryRunPlansEnable(t *testing.T) {
	out, err := runAks(t, "aks-autoscaler.sh", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run exited non-zero: %v\n%s", err, out)
	}
	want := "az aks nodepool update -g rg-memql-staging --cluster-name aks-memql-staging -n nodepool1 --enable-cluster-autoscaler --min-count 2 --max-count 5"
	if !strings.Contains(out, want) {
		t.Errorf("autoscaler dry-run missing the §9 enable command %q:\n%s", want, out)
	}
}

// An env with no codified defaults requires explicit --resource-group/--cluster.
func TestAksAutoscalerUnknownEnvRejected(t *testing.T) {
	out, err := runAks(t, "aks-autoscaler.sh", "--env=prod", "--dry-run")
	if err == nil {
		t.Fatalf("expected non-zero exit for an env with no codified defaults:\n%s", out)
	}
	if !strings.Contains(out, "no codified defaults") {
		t.Errorf("expected 'no codified defaults' error:\n%s", out)
	}
}

// min > max is rejected.
func TestAksAutoscalerBadRangeRejected(t *testing.T) {
	out, err := runAks(t, "aks-autoscaler.sh", "--min=5", "--max=2", "--dry-run")
	if err == nil {
		t.Fatalf("expected non-zero exit for min>max:\n%s", out)
	}
	if !strings.Contains(out, "must be <=") {
		t.Errorf("expected min<=max validation error:\n%s", out)
	}
}

func TestAksRollbackHelp(t *testing.T) {
	out, err := runAks(t, "aks-rollback.sh", "--help")
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{"Usage:", "--to-revision=", "--only=", "rollout history"} {
		if !strings.Contains(out, want) {
			t.Errorf("rollback --help missing %q:\n%s", want, out)
		}
	}
}

// A dry-run rollback plans `kubectl rollout undo` for every node and mutates
// nothing.
func TestAksRollbackDryRunPlansUndo(t *testing.T) {
	requireKubectl(t)
	out, err := runAks(t, "aks-rollback.sh", "--dry-run")
	if err != nil {
		t.Fatalf("--dry-run exited non-zero: %v\n%s", err, out)
	}
	for _, node := range []string{"identity", "bff", "cognition", "voice", "agent", "planner", "workbench", "copresent"} {
		want := "rollout undo deployment/" + node
		if !strings.Contains(out, want) {
			t.Errorf("dry-run plan missing %q:\n%s", want, out)
		}
	}
}

// --only restricts the target set; --to-revision is threaded through.
func TestAksRollbackOnlyAndRevision(t *testing.T) {
	requireKubectl(t)
	out, err := runAks(t, "aks-rollback.sh", "--only=bff,cognition", "--to-revision=7", "--dry-run")
	if err != nil {
		t.Fatalf("exited non-zero: %v\n%s", err, out)
	}
	if !strings.Contains(out, "rollout undo deployment/bff -n memql --to-revision=7") {
		t.Errorf("expected bff undo at revision 7:\n%s", out)
	}
	if strings.Contains(out, "rollout undo deployment/voice") {
		t.Errorf("--only=bff,cognition should NOT roll back voice:\n%s", out)
	}
}

// An unknown --only node is rejected with a clear error (non-zero exit).
func TestAksRollbackInvalidNodeRejected(t *testing.T) {
	requireKubectl(t)
	out, err := runAks(t, "aks-rollback.sh", "--only=bogus", "--dry-run")
	if err == nil {
		t.Fatalf("expected non-zero exit for an unknown node, got success:\n%s", out)
	}
	if !strings.Contains(out, "unknown node") {
		t.Errorf("expected 'unknown node' error:\n%s", out)
	}
}

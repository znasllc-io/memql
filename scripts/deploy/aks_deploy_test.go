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

func TestAksDeploySyntax(t *testing.T)   { aksSyntax(t, "aks-deploy.sh") }
func TestAksRollbackSyntax(t *testing.T) { aksSyntax(t, "aks-rollback.sh") }

// The deploy --help must advertise the gate knobs added in #554.
func TestAksDeployHelpHasGateFlags(t *testing.T) {
	out, err := runAks(t, "aks-deploy.sh", "--help")
	if err != nil {
		t.Fatalf("--help exited non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{"Usage:", "--no-gate", "--no-smoke"} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy --help missing %q:\n%s", want, out)
		}
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

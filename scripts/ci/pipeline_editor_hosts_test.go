package ci

import (
	"os/exec"
	"testing"
)

func TestNativeEditorHostRunnerRetainsFailuresAndCoversHostMatrix(t *testing.T) {
	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", "scripts/ci", "-p", "pipeline_editor_hosts_test.py")
	cmd.Dir = RepoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native editor host runner: %v\n%s", err, out)
	}
}

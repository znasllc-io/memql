package ci

import (
	"os/exec"
	"testing"
)

func TestVSCodeDownloadTransportRecovery(t *testing.T) {
	cmd := exec.Command("node", "--test", "scripts/vscode/download.test.cjs")
	cmd.Dir = RepoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("VS Code download recovery: %v\n%s", err, out)
	}
}

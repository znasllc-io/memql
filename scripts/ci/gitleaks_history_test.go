package ci

import (
	"os/exec"
	"testing"
)

func TestGitleaksHistoryRequiresCompleteCoverage(t *testing.T) {
	cmd := exec.Command("python3", "gitleaks_history_test.py")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bounded history scan: %v\n%s", err, out)
	}
}

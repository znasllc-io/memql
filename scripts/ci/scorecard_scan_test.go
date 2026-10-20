package ci

import (
	"os/exec"
	"testing"
)

func TestNativeScorecardProtocol(t *testing.T) {
	cmd := exec.Command("python3", "scorecard_scan_test.py")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native Scorecard protocol: %v\n%s", err, output)
	}
}

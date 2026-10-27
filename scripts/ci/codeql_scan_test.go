package ci

import (
	"os/exec"
	"testing"

	"github.com/znasllc-io/memql/scripts/ci/pyyaml"
)

func TestCodeQLStandaloneScannerRejectsIncompleteEvidence(t *testing.T) {
	pyyaml.Require(t)
	cmd := exec.Command("python3", "codeql_scan_test.py")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("standalone CodeQL contract: %v\n%s", err, out)
	}
}

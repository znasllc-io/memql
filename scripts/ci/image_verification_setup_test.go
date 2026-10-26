package ci

import (
	"os/exec"
	"testing"
)

// The setup and qualification wrappers are ordinary native CI inputs. This
// exercises their offline boundary controls without installing a builder or
// requiring the optional Linux service to be available on the CI worker.
func TestImageVerificationSetupAndQualificationControls(t *testing.T) {
	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", "scripts/image-verification", "-p", "test_*.py", "-v")
	cmd.Dir = RepoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("image verification setup controls: %v\n%s", err, out)
	}
}

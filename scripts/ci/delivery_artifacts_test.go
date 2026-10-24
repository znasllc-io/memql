package ci

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDeliveryArtifactFixtureProtocol(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required for the artifact qualification fixture")
	}
	cmd := exec.Command(python, "-B", filepath.Join(RepoRoot(), "scripts/ci/delivery_artifacts_test.py"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture protocol: %v\n%s", err, output)
	}
}

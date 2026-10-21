package ci

import (
	"os/exec"
	"slices"
	"testing"
)

func TestNativeInstallerFixtureOwnershipAndFailureCleanup(t *testing.T) {
	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", "scripts/install/ci", "-p", "*_test.py", "-v")
	cmd.Dir = RepoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("installer fixture boundary: %v\n%s", err, out)
	}
}

func TestNativeInstallerLifecycleIsWeeklyAndKeepsEveryLeg(t *testing.T) {
	for _, stage := range nativePipeline(t).Stages {
		if stage.Name != "installer-lifecycle" {
			continue
		}
		if stage.Schedule != "weekly" || !slices.Equal(stage.On, []string{"schedule", "manual"}) {
			t.Fatalf("installer lifecycle must be weekly/manual, never PR or merge-group execution: %+v", stage)
		}
		want := []string{"round-trip", "repair", "upgrade", "from-source", "pipelines"}
		if len(stage.Steps) != len(want) {
			t.Fatal("installer lifecycle lost a distinct assertion leg")
		}
		for i, step := range stage.Steps {
			if step.Name != want[i] || step.Placement != "fleet" || step.Execution != "native" || step.Platform != "linux/amd64" || !step.Needs["docker"] {
				t.Fatalf("installer leg must use its opted-in Linux Docker machine: %+v", step)
			}
			if step.Run != "python3 scripts/install/ci/fleet-fixture.py --leg="+want[i] || !slices.Equal(step.Artifacts, []string{".memql-install-evidence"}) {
				t.Fatalf("installer leg must retain the bounded fixture's own evidence: %+v", step)
			}
		}
		return
	}
	t.Fatal("native installer lifecycle is absent")
}

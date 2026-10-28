package install_test

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// Native Go test lanes must exercise the shipped shell entry points, not only Go
// installer helpers. These suites use temporary homes, fixture binaries and
// mocked services; they never enroll a machine or install a real release.
// Missing declared Bash/Python prerequisites are failures, not silent skips.
func TestInstallerScripts(t *testing.T) {
	for _, suite := range []struct {
		name string
		tool string
		file string
	}{
		{"release-discovery", "python3", "release_discovery_test.py"},
		{"presentation", "python3", "ui_test.py"},
		{"library", "bash", "lib_test.sh"},
		{"menubar", "bash", "menubar_test.sh"},
	} {
		t.Run(suite.name, func(t *testing.T) {
			tool, err := exec.LookPath(suite.tool)
			if err != nil {
				t.Fatalf("installer checks require %s: %v", suite.tool, err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			output, err := exec.CommandContext(ctx, tool, suite.file).CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed (%v):\n%s", suite.file, err, output)
			}
		})
	}
}

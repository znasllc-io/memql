// Package makehelp holds the drift guard for `make help`.
//
// `make help` is auto-generated from the '## ' doc comments above each Makefile
// target (see scripts/make/help.sh). That makes the menu complete only insofar
// as every target actually carries a doc comment. This test closes the loop:
// it fails if any target is undocumented, turning "every command shows up in
// `make help`" into a structural guarantee rather than author discipline.
package makehelp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMakeHelpCompleteness(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "make", "help.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("help.sh not found at %s: %v", script, err)
	}
	out, err := exec.Command("bash", script, "--check").CombinedOutput()
	if err != nil {
		t.Fatalf("undocumented Makefile target(s) detected -- add a '## <summary>' line "+
			"directly above each, or it silently vanishes from `make help`:\n%s", out)
	}
}

// repoRoot walks up from the test's working directory (the package dir) to the
// module root, located by go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod (repo root) from test working directory")
		}
		dir = parent
	}
}

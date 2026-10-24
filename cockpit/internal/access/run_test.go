package access

import (
	"strings"
	"testing"
)

func runCmd(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e strings.Builder
	code = run(args, &o, &e, nil)
	return code, o.String(), e.String()
}

// An asked-for help text is a SUCCESS, and it belongs on STDOUT. Exiting
// non-zero breaks `memql access --help && ...`; writing to stderr makes
// `memql access --help > usage.txt` produce an empty file, which no sibling
// verb does.
func TestHelpExitsZeroOnStdout(t *testing.T) {
	code, out, errOut := runCmd(t, "--help")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "Usage: memql access") {
		t.Errorf("help text not on stdout:\nstdout=%q\nstderr=%q", out, errOut)
	}
	if errOut != "" {
		t.Errorf("asked-for help wrote to stderr: %q", errOut)
	}
}

// Finding 2: Go's flag package stops at the first positional, so a flag AFTER
// the cluster name was silently dropped -- `memql access prod --json | jq`
// printed a table and exited 0.
func TestFlagsAreHonouredAfterTheClusterName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// --json after the name must not be swallowed. With no clusters the command
	// fails before rendering, so the observable proof is that --help placed
	// after a positional is still seen.
	code, out, _ := runCmd(t, "somecluster", "--help")
	if code != 0 {
		t.Errorf("`access <name> --help` exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "Usage: memql access") {
		t.Errorf("help after a positional was swallowed:\n%s", out)
	}
}

// A typo'd flag is a USAGE error (2), distinct from an unregistered cluster
// (1), so a wrapper can tell them apart.
func TestUnknownFlagAfterAPositionalStillExitsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	code, _, errOut := runCmd(t, "acme", "--bogus")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "--bogus") {
		t.Errorf("stderr does not name the bad flag: %q", errOut)
	}
}

// A second positional is a mistake worth naming rather than ignoring.
func TestASecondPositionalIsRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	code, _, errOut := runCmd(t, "acme", "beta")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "too many positional arguments") {
		t.Errorf("stderr does not explain: %q", errOut)
	}
}

// A flag that does not exist is a usage error, which is a different thing.
func TestUnknownFlagExitsTwo(t *testing.T) {
	code, _, _ := runCmd(t, "--not-a-flag")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

// With no clusters registered the command fails with the repair, and prints
// nothing at all on stdout -- a script piping this into jq must not get half
// a document.
func TestNoClustersFailsWithTheRepairAndPrintsNothingOnStdout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	code, out, errOut := runCmd(t)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut, "memql cluster add") {
		t.Errorf("stderr does not name the repair:\n%s", errOut)
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
}

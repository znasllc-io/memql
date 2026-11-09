// tag_submodules_bool_flags_test.go -- tag-submodules.sh's booleans take a
// value.
//
// `make tag-submodules` forwarded DRY_RUN and NO_PUSH as bare flags whenever
// the variable was non-empty, and the script accepted only the bare form, so
// `make tag-submodules DRY_RUN=0` was a dry run and `NO_PUSH=0` did not push.
// The recipe forwards the value now and the script reads `--flag=<bool>` with
// scripts/lib/bool_flag.sh.
//
// Hermetic: each case runs in a throwaway git repository holding copies of
// the script and its reader, with the user's git config shut out. Tags are
// only ever created there, and nothing is pushed: the repository has no
// remote.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tagFixture builds the throwaway repository and returns its root.
func tagFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{"scripts/release/tag-submodules.sh", "scripts/lib/bool_flag.sh"} {
		src, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, src, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-q", "-m", "fixture"}} {
		if out, err := tagGit(root, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

func tagEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com")
}

func tagGit(root string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = tagEnv()
	return cmd
}

// tagRun runs the fixture's copy of the script for the wire line at HEAD and
// returns its output, exit code and the tags the repository holds afterwards.
func tagRun(t *testing.T, args ...string) (out string, code int, tags string) {
	t.Helper()
	root := tagFixture(t)
	cmd := exec.Command("bash", append([]string{
		filepath.Join(root, "scripts/release/tag-submodules.sh"),
		"--version=1.2.3", "--commit=HEAD", "--line=wire"}, args...)...)
	cmd.Dir = root
	cmd.Env = tagEnv()
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v\n%s", err, b)
	}
	listed, lerr := tagGit(root, "tag", "-l").Output()
	if lerr != nil {
		t.Fatalf("git tag -l: %v", lerr)
	}
	return string(b), code, strings.TrimSpace(string(listed))
}

func TestTagSubmodulesBooleanFlagsTakeAValue(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		tagged bool
		plan   string
	}{
		{name: "--dry-run=0 tags", args: []string{"--dry-run=0", "--no-push"}, tagged: true, plan: "push    : false"},
		{name: "--dry-run=false tags", args: []string{"--dry-run=false", "--no-push=true"}, tagged: true, plan: "push    : false"},
		{name: "--dry-run=1 tags nothing", args: []string{"--dry-run=1"}, plan: "would tag"},
		{name: "--no-push=false pushes", args: []string{"--dry-run", "--no-push=false"}, plan: "push    : true"},
		{name: "--no-push=yes does not", args: []string{"--dry-run", "--no-push=yes"}, plan: "push    : false"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out, code, tags := tagRun(t, tc.args...)
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if !strings.Contains(out, tc.plan) {
				t.Errorf("output does not say %q\n%s", tc.plan, out)
			}
			if got := tags != ""; got != tc.tagged {
				t.Errorf("tags created = %v, want %v: %q\n%s", got, tc.tagged, tags, out)
			}
		})
	}
}

// A value that is neither is a bad argument (2), refused before any tag.
func TestTagSubmodulesRefusesAMistypedBoolean(t *testing.T) {
	for _, arg := range []string{"--dry-run=ture", "--no-push=maybe"} {
		arg := arg
		t.Run(arg, func(t *testing.T) {
			out, code, tags := tagRun(t, arg)
			if code != 2 {
				t.Errorf("exit %d, want 2 (bad argument)\n%s", code, out)
			}
			if !strings.Contains(out, "takes a boolean") {
				t.Errorf("the refusal does not say the flag takes a boolean\n%s", out)
			}
			if tags != "" {
				t.Errorf("refused, yet tagged %q", tags)
			}
		})
	}
}

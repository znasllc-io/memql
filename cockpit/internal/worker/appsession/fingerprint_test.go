//go:build linux || darwin

package appsession

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/harness"
)

// fingerprint_test.go holds each part of the fingerprint to what it
// claims: a listing of names and kinds, variables as digests, tools as
// they report themselves and never a shim that opens a window.

func listing(t *testing.T, root, config, backup string) workspaceListing {
	t.Helper()
	l, err := listWorkspace(root, config, backup, maxListingEntries)
	if err != nil {
		t.Fatalf("listWorkspace: %v", err)
	}
	return l
}

func TestListWorkspaceIsNamesAndKindsNotContents(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	// The same tree, built in a different order.
	writeFile(t, a, "src/main.go", "package main")
	writeFile(t, a, "README.md", "hi")
	writeFile(t, b, "README.md", "hi")
	writeFile(t, b, "src/main.go", "package main")
	if listing(t, a, "", "").digest != listing(t, b, "", "").digest {
		t.Fatal("the same tree built in another order listed differently")
	}
	// Contents are not the listing's business: the action that read a file
	// digests what it saw.
	writeFile(t, b, "README.md", "a different body entirely")
	if listing(t, a, "", "").digest != listing(t, b, "", "").digest {
		t.Error("editing a file changed the listing digest")
	}
	writeFile(t, b, "NEW.md", "")
	if listing(t, a, "", "").digest == listing(t, b, "", "").digest {
		t.Error("adding a file did not change the listing digest")
	}
	if l := listing(t, a, "", ""); l.entries != 3 || l.truncated || !strings.HasPrefix(l.digest, harness.DigestPrefix) {
		t.Errorf("listing = %+v, want 3 entries (src/, src/main.go, README.md)", l)
	}
}

func TestListWorkspaceLeavesTheSessionOut(t *testing.T) {
	pristine, session := t.TempDir(), t.TempDir()
	writeFile(t, pristine, ".mcp.json", "the person's own")
	writeFile(t, pristine, "app.go", "package app")

	// The same workspace as a Claude Code session sees it: the person's
	// .mcp.json moved aside, the cockpit's written in its place, and the
	// session directory beside them.
	writeFile(t, session, ".mcp.json"+backupSuffix, "the person's own")
	writeFile(t, session, ".mcp.json", "the cockpit's, with the bearer")
	writeFile(t, session, "app.go", "package app")
	writeFile(t, session, sessionScaffoldDir+"/transcript.log", "[text] hi")
	writeFile(t, session, sessionScaffoldDir+"/codex/config.toml", "bearer")

	got := listing(t, session, filepath.Join(session, ".mcp.json"), filepath.Join(session, ".mcp.json"+backupSuffix))
	if want := listing(t, pristine, "", ""); got.digest != want.digest || got.entries != want.entries {
		t.Errorf("the session's workspace lists as %+v, want the workspace as the person left it: %+v", got, want)
	}
}

func TestListWorkspaceListsDependencyDirectoriesWithoutEnteringThem(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "node_modules/left-pad/index.js", "module.exports")
	writeFile(t, root, ".git/HEAD", "ref: refs/heads/main")
	writeFile(t, root, "index.js", "require('left-pad')")
	if l := listing(t, root, "", ""); l.entries != 3 {
		t.Errorf("entries = %d, want node_modules/, .git/ and index.js -- the directories named, never entered", l.entries)
	}
	bare := t.TempDir()
	writeFile(t, bare, "index.js", "require('left-pad')")
	if listing(t, root, "", "").digest == listing(t, bare, "", "").digest {
		t.Error("whether node_modules is there is a fact a command depends on, and the listing lost it")
	}
}

func TestListWorkspaceSaysWhenItStopped(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		writeFile(t, root, name, "")
	}
	l, err := listWorkspace(root, "", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !l.truncated || l.entries != 3 {
		t.Errorf("listing = %+v, want 3 entries and truncated", l)
	}
	if _, err := listWorkspace(filepath.Join(root, "missing"), "", "", 3); err == nil {
		t.Error("a workspace that is not there listed without an error")
	}
}

func TestFingerprintVariablesAreDigestsNotValues(t *testing.T) {
	env := []string{"PATH=/home/someone/bin:/usr/bin", "LANG=", "TZ=UTC", "TZ=Europe/Madrid"}
	got := fingerprintVariables([]string{"PATH", "LANG", "TZ", "SHELL"}, env)
	want := []harness.Variable{
		{Name: "PATH", Set: true, Digest: harness.Digest([]byte("/home/someone/bin:/usr/bin"))},
		{Name: "LANG", Set: true, Digest: harness.Digest(nil)},
		{Name: "TZ", Set: true, Digest: harness.Digest([]byte("Europe/Madrid"))},
		{Name: "SHELL", Set: false},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("variable %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), "someone") || strings.Contains(string(body), "Madrid") {
		t.Errorf("a variable's value reached the fingerprint: %s", body)
	}
}

// fakeTools is a toolchain whose binaries are files in a temp directory
// and whose probes are counted.
type fakeTools struct {
	dir      string
	mu       sync.Mutex
	runs     map[string]int
	versions map[string]string
}

func newFakeTools(t *testing.T, versions map[string]string) (*fakeTools, *toolchain) {
	t.Helper()
	f := &fakeTools{dir: t.TempDir(), runs: map[string]int{}, versions: versions}
	for name := range versions {
		writeFile(t, f.dir, name, "#!/bin/sh\n")
	}
	tc := &toolchain{
		lookPath: func(name string) (string, error) {
			path := filepath.Join(f.dir, name)
			if _, err := os.Stat(path); err != nil {
				return "", err
			}
			return path, nil
		},
		run: func(_ context.Context, bin string, _ []string) (string, error) {
			name := filepath.Base(bin)
			f.mu.Lock()
			f.runs[name]++
			f.mu.Unlock()
			v, ok := f.versions[name]
			if !ok || v == "" {
				return "", errors.New("no version")
			}
			return "\n" + v + "\nsecond line\n", nil
		},
		goos:     "linux",
		devTools: func() bool { return false },
		now:      time.Now,
		cache:    map[string]toolEntry{},
	}
	return f, tc
}

func (f *fakeTools) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[name]
}

func TestToolchainReportsEachToolAsItReportsItself(t *testing.T) {
	_, tc := newFakeTools(t, map[string]string{
		"go": "go version go1.26.6 linux/amd64", "git": "git version 2.43.0", "npm": "",
	})
	got := tc.versions(context.Background())
	want := []harness.ToolVersion{
		{Name: "git", Version: "git version 2.43.0"},
		{Name: "go", Version: "go version go1.26.6 linux/amd64"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("versions = %+v, want %+v: the first line, in name order, and a tool that did not answer left out", got, want)
	}
}

func TestToolchainAsksEachBinaryOnce(t *testing.T) {
	f, tc := newFakeTools(t, map[string]string{"git": "git version 2.43.0", "npm": ""})
	tc.versions(context.Background())
	tc.versions(context.Background())
	if n := f.count("git"); n != 1 {
		t.Errorf("git was asked %d times across two sessions, want once", n)
	}
	if n := f.count("npm"); n != 1 {
		t.Errorf("npm, which does not answer, was asked %d times; its silence is an answer until it changes", n)
	}
	// Replacing the binary is an upgrade, and the next session asks again.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(f.dir, "git"), later, later); err != nil {
		t.Fatal(err)
	}
	tc.versions(context.Background())
	if n := f.count("git"); n != 2 {
		t.Errorf("git was asked %d times after it changed, want 2", n)
	}
}

func TestToolchainNeverRunsAMacShimWithoutTheDeveloperTools(t *testing.T) {
	var runs atomic.Int32
	tc := &toolchain{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(context.Context, string, []string) (string, error) {
			runs.Add(1)
			return "git version 2.39.5 (Apple Git-154)", nil
		},
		goos:     "darwin",
		devTools: func() bool { return false },
		now:      time.Now,
		cache:    map[string]toolEntry{},
	}
	if got := tc.versions(context.Background()); len(got) != 0 || runs.Load() != 0 {
		t.Fatalf("with no developer tools, %d stubs ran and %+v was reported; each would have raised an install dialog", runs.Load(), got)
	}
	tc.devTools = func() bool { return true }
	if got := tc.versions(context.Background()); len(got) == 0 || runs.Load() == 0 {
		t.Error("with the developer tools installed, /usr/bin/git is git and must be asked")
	}
}

// TestRunToolVersionDoesNotWaitOnAChildHoldingItsOutput: a version
// command that leaves a child behind -- a shim starting a daemon, a
// version manager warming a cache -- leaves that child holding stdout
// open. The probe returns once its own process is done, with what it
// printed, instead of waiting on the pipe.
func TestRunToolVersionDoesNotWaitOnAChildHoldingItsOutput(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "lingers")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30 &\nprintf '%s' \"$!\" > \"$0.pid\"\necho 'lingers 1.2.3'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runToolVersion(context.Background(), bin, []string{"--version"})
	if err != nil || strings.TrimSpace(out) != "lingers 1.2.3" {
		t.Errorf("= %q, %v, want the version it printed", out, err)
	}
	data, err := os.ReadFile(bin + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid child pid %q: %v", data, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	// Inspect whether the child was stopped, independent of scheduler load.
	// Zombies have exited and only await reaping by their new parent.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		if err != nil {
			if exited, ok := err.(*exec.ExitError); ok && exited.ExitCode() == 1 {
				return
			}
			t.Fatalf("inspect child process: %v", err)
		}
		if strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("version probe left child %d running", pid)
}

package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
)

// servicepath_test.go pins the PATH the worker's service runs with. A
// LaunchAgent starts with launchd's four system directories and nothing
// else, so a `claude` from Claude Code's own installer (~/.local/bin) or
// from Homebrew was never found: the machine reported no apps at all, and
// the session exec -- which starts the bare name -- could not have started
// one either. The registration row of the machine this was found on said
// apps=[] for 1492 versions, until PATH was added to its plist by hand.

// TestServicePath_AppendsTheAppDirectoriesAfterTheSystemOnes. The order is
// the safety property: the worker runs launchctl, plutil and friends by
// bare name, and a directory the user can write to must never shadow them.
func TestServicePath_AppendsTheAppDirectoriesAfterTheSystemOnes(t *testing.T) {
	got := servicePath(launchdDefaultPath, "/Users/someone")
	want := "/usr/bin:/bin:/usr/sbin:/sbin:/Users/someone/.local/bin:/Users/someone/.claude/local:/opt/homebrew/bin:/usr/local/bin"
	if got != want {
		t.Errorf("servicePath = %q\nwant           %q", got, want)
	}
}

// TestServicePath_NeverMovesOrRepeatsAnEntry. A worker started from a
// terminal already has the owner's own PATH; nothing in it moves, and a
// directory it already names is not named twice.
func TestServicePath_NeverMovesOrRepeatsAnEntry(t *testing.T) {
	current := "/opt/homebrew/bin:/Users/someone/.local/bin:/usr/bin:/bin"
	got := servicePath(current, "/Users/someone")
	want := current + ":/Users/someone/.claude/local:/usr/local/bin"
	if got != want {
		t.Errorf("servicePath = %q\nwant           %q", got, want)
	}
	if again := servicePath(got, "/Users/someone"); again != got {
		t.Errorf("a second pass changed PATH again: %q -> %q", got, again)
	}
}

// TestServicePath_EmptyStartsFromTheSystemFloor. An empty PATH is not "no
// directories"; it is a process nobody gave one, and the app directories
// alone would be exactly the shadowing the order above rules out.
func TestServicePath_EmptyStartsFromTheSystemFloor(t *testing.T) {
	got := servicePath("", "/Users/someone")
	if !strings.HasPrefix(got, launchdDefaultPath+":") {
		t.Errorf("servicePath(\"\") = %q, want it to start with %q", got, launchdDefaultPath)
	}
}

// TestServicePath_NoHomeAddsOnlyTheFixedDirectories. A home-relative
// directory computed from an empty home would be ".local/bin" -- relative
// to wherever the worker happens to be running.
func TestServicePath_NoHomeAddsOnlyTheFixedDirectories(t *testing.T) {
	got := servicePath(launchdDefaultPath, "")
	if want := launchdDefaultPath + ":/opt/homebrew/bin:/usr/local/bin"; got != want {
		t.Errorf("servicePath = %q, want %q", got, want)
	}
}

// TestEnsureServicePath_TheDetectorAndTheSessionFindTheSameClaude is the
// bug as it happened: Claude Code's native installer, a service PATH of the
// system directories only. Before the fix the detector found nothing; after
// it, the inventory reports the app AND the spec a session starts names the
// very binary the inventory found -- one lookup, not two that could differ.
func TestEnsureServicePath_TheDetectorAndTheSessionFindTheSameClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the worker service runs on macOS and Linux")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(bin, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\necho '2.1.283 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	ctx := context.Background()

	if got := (&apps.Detector{Home: home}).Detect(ctx, nil); len(got) != 0 {
		t.Fatalf("precondition: with the system PATH alone the detector must find nothing, got %+v", got)
	}

	ensureServicePath(quietLogger())

	d := &apps.Detector{Home: home}
	got := d.Detect(ctx, []string{apps.IDClaudeCode})
	if len(got) != 1 || got[0].Id != apps.IDClaudeCode || got[0].Version != "2.1.283 (Claude Code)" || !got[0].Allowed {
		t.Fatalf("Detect = %+v, want claude-code 2.1.283, allowed", got)
	}
	spec, ok := d.ResolveSpec(ctx, apps.IDClaudeCode)
	if !ok {
		t.Fatal("ResolveSpec: claude-code is on the service PATH now, so a session must be able to start it")
	}
	if spec.Binary != claude {
		t.Errorf("the session would start %q, want the binary the inventory found, %q", spec.Binary, claude)
	}
	if found, err := exec.LookPath("claude"); err != nil || found != claude {
		t.Errorf("exec.LookPath(claude) = %q, %v; want %q -- the harness's own lookup must agree", found, err, claude)
	}
}

// TestLaunchAgentPlist_CarriesTheServicePath. The Go writer (the pair flow)
// writes the same PATH ensureServicePath arrives at, so a worker started
// from the plist is already there and the two cannot disagree about order.
func TestLaunchAgentPlist_CarriesTheServicePath(t *testing.T) {
	home := "/Users/some & one"
	plist := launchAgentPlist("com.znasllc.memql-worker", "/Applications/MemQL.app/Contents/MacOS/MemQL", "", home+"/.memql/state", home)
	want := "<key>PATH</key>\n        <string>" +
		"/usr/bin:/bin:/usr/sbin:/sbin:/Users/some &amp; one/.local/bin:/Users/some &amp; one/.claude/local:/opt/homebrew/bin:/usr/local/bin" +
		"</string>"
	if !strings.Contains(plist, want) {
		t.Errorf("plist has no escaped service PATH; want\n%s\nin\n%s", want, plist)
	}
	if !strings.Contains(plist, "<key>HOME</key>\n        <string>/Users/some &amp; one</string>") {
		t.Errorf("plist lost HOME:\n%s", plist)
	}
}

// TestShellPlistWritersCarryTheServicePath. Two shell scripts write the
// same LaunchAgent -- install-mac.sh and the root install.sh's legacy-label
// migration -- and neither can import the Go above. Each spells the PATH
// out; this pins every spelling to it, so a directory added here and
// forgotten there fails a test instead of a machine. MemQL.app writes no
// plist: its agent is registered from the bundle (memql-cockpit#545), and
// the worker extends its own PATH.
func TestShellPlistWritersCarryTheServicePath(t *testing.T) {
	want := servicePath(launchdDefaultPath, "${HOME}")
	for _, rel := range []string{
		filepath.Join("..", "..", "scripts", "install", "install-mac.sh"),
		filepath.Join("..", "..", "install.sh"),
	} {
		body, err := os.ReadFile(rel)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s does not write the service PATH %q", rel, want)
		}
	}
}

package work

// The footprint decides where a procedure replays (epic memql#5408, task
// memql#5410; design record D4).

import (
	"errors"
	"testing"
)

// A file inside the workspace the recording ran in travels with the
// procedure: the workbench gives every replay its own workspace, so a write
// there is the same write -- whether the app spelled it relative or absolute.
func TestAWorkspaceRelativeWriteIsPortable(t *testing.T) {
	for _, p := range []string{"/w/run1/out.txt", "out.txt", "./out/../out.txt", "/w/run1"} {
		fp := ActionFootprint("fs_write", "/w/run1", []string{p})
		if fp.Machine {
			t.Errorf("%q inside the workspace was called machine-local", p)
		}
		if !fp.Files {
			t.Errorf("%q: a write writes a filesystem, and the side-effect gate must see it", p)
		}
		if got := ReplayTargetFor(fp); got != TargetWorkbench {
			t.Errorf("%q: target = %q, want the workbench", p, got)
		}
	}
}

// A file outside the workspace is the person's own -- their notes, their
// dotfiles -- and exists only on the machine the recording ran on. A workbench
// replay would read a file that is not there or write one nobody will ever see.
func TestAnAbsolutePathOutsideTheWorkspaceIsMachineLocal(t *testing.T) {
	for _, tool := range []string{"fs_write", "fs_read", "exec"} {
		fp := ActionFootprint(tool, "/w/run1", []string{"out.txt", "/Users/x/notes.md"})
		if !fp.Machine {
			t.Errorf("%s of /Users/x/notes.md was called portable", tool)
		}
		if got := ReplayTargetFor(fp); got != TargetMachine {
			t.Errorf("%s: target = %q, want the machine", tool, got)
		}
	}
}

// A workspace is a directory, not a string prefix: /w/run10 is not inside
// /w/run1, however the two strings begin.
func TestASiblingDirectorySharingThePrefixIsOutsideTheWorkspace(t *testing.T) {
	for _, p := range []string{"/w/run10/out.txt", "/w/run1-old/out.txt"} {
		if fp := ActionFootprint("fs_write", "/w/run1", []string{p}); !fp.Machine {
			t.Errorf("%q shares a prefix with the workspace and was called inside it", p)
		}
	}
}

// Relative is not the same as inside. "../" climbs out of the workspace, and
// "~" is the home directory of whoever runs it -- both name the machine.
func TestAPathThatLeavesTheWorkspaceIsMachineLocal(t *testing.T) {
	for _, p := range []string{"../secrets.txt", "sub/../../secrets.txt", "~/notes.md", "~"} {
		if fp := ActionFootprint("fs_read", "/w/run1", []string{p}); !fp.Machine {
			t.Errorf("%q leaves the workspace and was called portable", p)
		}
	}
}

// With no workspace recorded there is nothing an absolute path can be shown to
// be inside, so it stays on the machine it came from; a relative one is still
// relative to wherever the procedure runs. The filesystem root is no workspace
// either: every path is "inside" it, and /etc/hosts replayed into a workbench
// directory is not the file the recording wrote.
func TestAnAbsolutePathWithNoWorkspaceIsMachineLocal(t *testing.T) {
	for _, cwd := range []string{"", "/", "relative/dir"} {
		for _, p := range []string{"/etc/hosts", "/"} {
			if fp := ActionFootprint("fs_write", cwd, []string{p}); !fp.Machine {
				t.Errorf("cwd %q: the absolute path %q, with no workspace to be inside of, was called portable", cwd, p)
			}
		}
	}
	if fp := ActionFootprint("fs_write", "", []string{"out.txt"}); fp.Machine {
		t.Error("a relative path with no workspace recorded was called machine-local")
	}
}

// D4: the network is portable. A fetch leaves the cluster -- which the
// side-effect gate must know -- but it does so identically from the workbench.
func TestAFetchIsExternalAndPortable(t *testing.T) {
	fp := ActionFootprint("fetch", "/w/run1", nil)
	if !fp.External || fp.Machine {
		t.Fatalf("a fetch is external and portable; got %+v", fp)
	}
	if got := ReplayTargetFor(fp); got != TargetWorkbench {
		t.Fatalf("target = %q, want the workbench", got)
	}
}

// Whether a completed step is reported to the app as a side effect -- "do not
// repeat this" -- reads IsSideEffect of its footprint. A read changes nothing
// and must not be reported as one; a command may change anything and must be.
func TestOnlyAnActionThatCanChangeSomethingIsASideEffect(t *testing.T) {
	for _, tc := range []struct {
		tool string
		want bool
	}{
		{"fs_read", false},
		{"fs_write", true},
		{"exec", true},
		{"fetch", true},
		{"app_answer", false},
	} {
		if got := ActionFootprint(tc.tool, "/w/run1", []string{"in.txt"}).IsSideEffect(); got != tc.want {
			t.Errorf("%s: IsSideEffect = %v, want %v", tc.tool, got, tc.want)
		}
	}
}

// A MemQL tool called back over MCP runs in the cluster, which is portable
// (D4). What it writes is decided by the tool, which the dispatcher knows and
// this function does not.
func TestAnMCPCallIsPortable(t *testing.T) {
	if fp := ActionFootprint("mcp", "/w/run1", nil); fp.Machine {
		t.Fatalf("an MCP call back into MemQL was called machine-local: %+v", fp)
	}
}

// A tool this package cannot name is one the workbench cannot be shown to
// run. Keeping it on the machine the recording ran on is the one choice that
// reproduces the recording.
func TestAToolThisPackageDoesNotKnowIsMachineLocal(t *testing.T) {
	if fp := ActionFootprint("screenshot", "/w/run1", nil); !fp.Machine {
		t.Fatalf("an unknown tool was called portable: %+v", fp)
	}
}

// The #5411 acceptance test (D4 failure mode): a machine-local footprint sent
// to the workbench is refused BEFORE the first step, because a replay that
// fails at step three has already run steps one and two somewhere they mean
// nothing.
func TestAMachineLocalFootprintSentToTheWorkbenchIsRefusedBeforeTheFirstStep(t *testing.T) {
	err := CheckTarget(Footprint{Machine: true}, TargetWorkbench)
	if !errors.Is(err, ErrMachineLocalOnWorkbench) {
		t.Fatalf("CheckTarget = %v, want ErrMachineLocalOnWorkbench", err)
	}
	for _, tc := range []struct {
		name   string
		fp     Footprint
		target ReplayTarget
	}{
		{"machine-local on its machine", Footprint{Machine: true, Files: true}, TargetMachine},
		{"portable on the workbench", Footprint{Files: true, External: true}, TargetWorkbench},
		{"portable on a machine", Footprint{Files: true}, TargetMachine},
	} {
		if err := CheckTarget(tc.fp, tc.target); err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
	}
	if err := CheckTarget(Footprint{}, ReplayTarget("elsewhere")); err == nil {
		t.Error("a target nobody declared was accepted; a replay must not start somewhere unnamed")
	}
}

func TestReplayTargetForAPortableFootprintIsTheWorkbench(t *testing.T) {
	for _, tc := range []struct {
		name string
		fp   Footprint
		want ReplayTarget
	}{
		{"nothing at all", Footprint{}, TargetWorkbench},
		{"files, network, rows and money", Footprint{Concepts: []string{"v1:x:y"}, Files: true, External: true, Spend: true}, TargetWorkbench},
		{"the machine", Footprint{Machine: true}, TargetMachine},
	} {
		if got := ReplayTargetFor(tc.fp); got != tc.want {
			t.Errorf("%s: ReplayTargetFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

package work

// target.go -- the footprint decides where a procedure replays (epic
// memql#5408, task memql#5410; design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// D4).
//
// A procedure whose footprint is portable -- workspace files, MemQL tools, the
// network -- replays in the sandboxed workbench in the cluster. One whose
// footprint names a machine's own files replays on that machine through the
// worker. D4 chose this over always-the-machine (the laptop has to be on) and
// always-the-workbench (a procedure needing the machine's files could never
// replay).
//
// THE WORKSPACE IS A DIRECTORY, NOT A PREFIX, and relative is not the same as
// inside. /w/run10 is not in /w/run1; "../x" and "~/x" leave it. The paths are
// the cockpit's, which ships for darwin and linux only, so they are
// slash-separated and path (not filepath) reads them the same on every engine.
//
// The target is checked BEFORE the first step (CheckTarget). A machine-local
// procedure that failed at step three on the workbench has already run steps
// one and two somewhere they mean nothing.

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// ReplayTarget is where a procedure's steps run.
type ReplayTarget string

const (
	// TargetWorkbench is a per-run sandboxed workspace in the cluster.
	TargetWorkbench ReplayTarget = "workbench"
	// TargetMachine is the owner's own machine, through the worker.
	TargetMachine ReplayTarget = "machine"
)

// ErrMachineLocalOnWorkbench refuses a machine-local procedure sent to the
// workbench.
var ErrMachineLocalOnWorkbench = errors.New("work: a machine-local procedure cannot replay on the workbench")

// ActionFootprint is what one recorded action may touch. tool is the action's
// step type (kind.go's StepType constants); cwd is the workspace the recording
// ran in -- the session's working directory from its fingerprint, not a
// directory a command moved to; paths are every file path the action named, in
// its arguments or its contents.
//
// Machine is set when any path leaves the workspace. Files is set when the
// action may write a filesystem: a write, and a command, which may write
// anything. A read writes nothing, so it is no side effect, and the guidance a
// divergence hands the app does not tell it a read cannot be repeated. A fetch
// is External. An MCP call back into MemQL runs in the cluster, and what it
// writes is the named tool's business, which the dispatcher knows and this
// function does not. A tool this package cannot name stays on the machine the
// recording ran on, the one place it is known to work.
func ActionFootprint(tool, cwd string, paths []string) Footprint {
	var fp Footprint
	switch tool {
	case StepTypeExec, StepTypeFSWrite:
		fp.Files = true
	case StepTypeFetch:
		fp.External = true
	case StepTypeFSRead, StepTypeMCP, StepTypeAppAnswer:
	default:
		fp.Machine = true
	}
	for _, p := range paths {
		if leavesWorkspace(cwd, p) {
			fp.Machine = true
			break
		}
	}
	return fp
}

// leavesWorkspace reports whether p names something outside the workspace
// rooted at cwd. A relative path is inside unless it climbs out; an absolute
// one is inside only when it is the workspace or below it. With no absolute
// workspace to be inside of -- none recorded, or the filesystem root, which
// every path is "inside" -- an absolute path is the machine's.
func leavesWorkspace(cwd, p string) bool {
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return false
	case strings.HasPrefix(p, "~"):
		// The home directory of whoever runs it: the machine, by definition.
		return true
	case !path.IsAbs(p):
		rel := path.Clean(p)
		return rel == ".." || strings.HasPrefix(rel, "../")
	}
	root := strings.TrimSpace(cwd)
	if !path.IsAbs(root) {
		return true
	}
	root = path.Clean(root)
	if root == "/" {
		return true
	}
	abs := path.Clean(p)
	return abs != root && !strings.HasPrefix(abs, root+"/")
}

// ReplayTargetFor is D4: the machine for a machine-local footprint, the
// workbench for everything else.
func ReplayTargetFor(fp Footprint) ReplayTarget {
	if fp.Machine {
		return TargetMachine
	}
	return TargetWorkbench
}

// CheckTarget refuses a replay whose target cannot run its footprint, before
// the first step. A portable procedure may run on a machine -- the machine has
// everything the workbench has -- but a machine-local one never runs on the
// workbench, and a target nobody declared never runs at all.
func CheckTarget(fp Footprint, target ReplayTarget) error {
	switch target {
	case TargetWorkbench:
		if fp.Machine {
			return ErrMachineLocalOnWorkbench
		}
		return nil
	case TargetMachine:
		return nil
	}
	return fmt.Errorf("work: %q is not a replay target", target)
}

//go:build linux || darwin

package tools

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

// applyShellSysProcAttr sets the SysProcAttr on the child exec
// command. Always sets Setpgid=true so context-cancellation kills
// the whole process tree (defeats grandchild orphaning). When
// limits.RunAsUser is set, additionally configures setuid via
// Credential -- requires the worker process to be running as root,
// silently a no-op otherwise (the syscall will fail and bubble up
// when the child runs).
func applyShellSysProcAttr(cmd *exec.Cmd, limits ShellLimits) error {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if limits.RunAsUser != "" {
		uid, gid, err := lookupUIDGID(limits.RunAsUser)
		if err != nil {
			return fmt.Errorf("run_as_user %q: %w", limits.RunAsUser, err)
		}
		// Bound-check before narrowing to uint32: a uid/gid outside the
		// unsigned 32-bit range would wrap silently
		// (CodeQL go/incorrect-integer-conversion).
		if uid < 0 || uid > math.MaxUint32 || gid < 0 || gid > math.MaxUint32 {
			return fmt.Errorf("run_as_user %q: uid/gid %d/%d out of uint32 range", limits.RunAsUser, uid, gid)
		}
		attr.Credential = &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		}
	}
	cmd.SysProcAttr = attr
	return nil
}

// prepareShellLimits applies resource limits in a child shell before it execs
// the requested command. No Setrlimit runs in the long-lived worker. Numeric
// limits and the command are positional arguments, never interpolated code.
// A limit the host refuses stops the wrapper before any command can run.
func prepareShellLimits(cmd *exec.Cmd, limits ShellLimits) error {
	memoryKiB, err := shellMemoryLimitKiB(limits.MaxMemoryMB)
	if err != nil {
		return err
	}
	script := strings.Join([]string{
		`if [ "$1" -gt 0 ]; then ulimit -t "$1" || exit 125; fi`,
		`if [ "$2" -gt 0 ]; then ulimit -n "$2" || exit 125; fi`,
		`if [ "$3" -gt 0 ]; then ulimit -v "$3" || exit 125; fi`,
		`exec /bin/sh -c "$4"`,
	}, "\n")
	command := cmd.Args[len(cmd.Args)-1]
	cmd.Args = []string{cmd.Path, "-c", script, "memql-exec", strconv.Itoa(max(0, limits.MaxCPUSeconds)),
		strconv.Itoa(max(0, limits.MaxOpenFiles)), strconv.Itoa(memoryKiB), command}
	return nil
}

// ownProcessGroup starts cmd as the leader of a process group of its own,
// so the group can be signalled whole (pipeline_step, memql#5494). Nothing
// from the shell policy applies: a CI step is not an exec call.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup signals the process group cmd leads: SIGTERM, or
// SIGKILL when kill is set. A group already gone is not an error.
func signalProcessGroup(cmd *exec.Cmd, kill bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// exitStatusOf reports how a process ended the way a shell -- and the
// cluster's Job -- reports it: its exit code, or 128 plus the number of the
// signal that ended it.
func exitStatusOf(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return state.ExitCode()
}

func lookupUIDGID(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid: %w", err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid: %w", err)
	}
	return uid, gid, nil
}

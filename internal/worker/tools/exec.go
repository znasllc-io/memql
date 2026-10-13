package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// runExec implements workerHost.exec: run a shell command, capture
// stdout/stderr, return exit code + previews. Uses a process group
// so context cancellation kills the whole tree.
func runExec(ctx context.Context, args map[string]any, policy *Policy) (*memqlv1.Success, *memqlv1.Failure) {
	return runExecWithDirectory(ctx, args, policy, nil)
}

// A scoped command retains the exact reviewed bytes and inherits a directory
// descriptor opened without following links. The legacy path keeps its defaults.
func runExecWithDirectory(ctx context.Context, args map[string]any, policy *Policy, directory *os.File) (*memqlv1.Success, *memqlv1.Failure) {
	cmdLine := argString(args, "cmd")
	if directory == nil {
		cmdLine = strings.TrimSpace(cmdLine)
	}
	if strings.TrimSpace(cmdLine) == "" {
		return nil, failure("bad_request", "exec: cmd required")
	}
	if err := policy.CheckShell(cmdLine); err != nil {
		return nil, failure("denied_by_policy", err.Error())
	}

	cwd := strings.TrimSpace(argString(args, "cwd"))
	if cwd == "" {
		cwd = policy.WorkspaceRoot()
	}
	timeoutSec := argInt(args, "timeoutSec", 60)
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	if timeoutSec > 600 {
		timeoutSec = 600
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "/bin/sh", "-c", cmdLine)
	limits := policy.ShellLimits()
	if err := applyShellSysProcAttr(cmd, limits); err != nil {
		return nil, failure("denied_by_policy", err.Error())
	}
	if err := prepareShellLimits(cmd, limits); err != nil {
		return nil, failure("denied_by_policy", err.Error())
	}
	if directory != nil {
		// Enter the held directory in a short-lived child, never by reopening
		// its pathname or changing the long-lived worker's current directory.
		self, err := os.Executable()
		if err != nil {
			return nil, failure("exec_failed", "Cannot locate the scoped command launcher.")
		}
		cmd.Args = append([]string{self, scopedCommandChildFlag}, cmd.Args...)
		cmd.Path = self
		cmd.ExtraFiles = []*os.File{directory}
		cwd = ""
	}
	cmd.Cancel = func() error { signalProcessGroup(cmd, true); return nil }
	cmd.WaitDelay = time.Second
	if cwd != "" {
		cmd.Dir = cwd
	}
	if env := argMap(args, "env"); env != nil {
		envv := make([]string, 0, len(env))
		for k, v := range env {
			if s, ok := v.(string); ok {
				envv = append(envv, fmt.Sprintf("%s=%s", k, s))
			}
		}
		cmd.Env = envv
	}
	if stdin := argString(args, "stdin"); stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if execCtx.Err() != nil {
		return nil, failure("timeout", "exec timed out or cancelled")
	}
	exitCode := 0
	signalName := ""
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ProcessState.ExitCode()
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				signalName = ws.Signal().String()
			}
		} else {
			return nil, failure("exec_failed", fmt.Sprintf("exec could not complete: %v", err))
		}
	}

	preview := strings.TrimSpace(stdout.String())
	if preview == "" {
		preview = strings.TrimSpace(stderr.String())
	}

	out := map[string]any{
		"stdout":   stdout.String(),
		"stderr":   stderr.String(),
		"exitCode": exitCode,
	}
	if signalName != "" {
		out["signal"] = signalName
	}
	bytesOut := stdout.Len() + stderr.Len()
	return successJSON(out, exitCode, 0, bytesOut, preview), nil
}

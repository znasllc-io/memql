package deploycontrol

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
)

// Executor is the side-effect boundary for the deploy-control service.
// All cluster reads, git operations, and action-script invocations go
// through it so the Service logic is testable with a fake.
//
// Read paths use KubectlJSON and never mutate. Action paths go through
// the sanctioned scripts (scripts/release/promote.sh), `git revert`,
// and `kubectl argo rollouts promote|abort` -- NEVER ad-hoc
// `kubectl set image`.
type Executor interface {
	// RunPromote invokes scripts/release/promote.sh --version=<version>
	// --env=<env>. Used by both DeployStaging (env=staging) and Promote
	// (env=prod). Returns the script's combined output.
	RunPromote(ctx context.Context, version, env string) (output string, err error)

	// RunRollback reverts the overlay commit identified by sha via
	// `git revert --no-edit <sha>`.
	RunRollback(ctx context.Context, env, sha string) (output string, err error)

	// RunRolloutAction runs `kubectl argo rollouts <action> <rollout>
	// -n memql` for action in {promote, abort}.
	RunRolloutAction(ctx context.Context, env, rollout, action string) (output string, err error)

	// KubectlJSON runs `kubectl <args...>` and returns stdout. Used for
	// the read paths (argo app / rollouts / analysisruns -o json).
	KubectlJSON(ctx context.Context, args ...string) ([]byte, error)

	// Git runs `git <args...>` rooted at the repo and returns stdout.
	Git(ctx context.Context, args ...string) (output string, err error)
}

// execExecutor is the real Executor: it shells out via os/exec. Repo
// root anchors the action-script + git invocations.
type execExecutor struct {
	repoRoot string
}

// newExecExecutor builds the real Executor anchored at repoRoot.
func newExecExecutor(repoRoot string) *execExecutor {
	return &execExecutor{repoRoot: repoRoot}
}

// NewExecutor returns the real os/exec-backed Executor anchored at
// repoRoot -- the SAME side-effect boundary the deploy-control Service
// uses (promote.sh / git / kubectl argo rollouts), exported so the
// deploy PACK (examples/deploypack, Epic 2 / #2095) can drive the same
// effects through the IntegrationProvider boundary without duplicating
// the shell-out wiring. An empty repoRoot anchors at the process
// working directory, matching NewService's RepoRoot default.
func NewExecutor(repoRoot string) Executor {
	return newExecExecutor(repoRoot)
}

func (e *execExecutor) RunPromote(ctx context.Context, version, env string) (string, error) {
	script := filepath.Join(e.repoRoot, "scripts", "release", "promote.sh")
	return e.run(ctx, e.repoRoot, script,
		"--version="+version,
		"--env="+env,
	)
}

func (e *execExecutor) RunRollback(ctx context.Context, env, sha string) (string, error) {
	return e.run(ctx, e.repoRoot, "git", "revert", "--no-edit", sha)
}

func (e *execExecutor) RunRolloutAction(ctx context.Context, env, rollout, action string) (string, error) {
	return e.run(ctx, e.repoRoot, "kubectl", "argo", "rollouts", action, rollout, "-n", "memql")
}

func (e *execExecutor) KubectlJSON(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Dir = e.repoRoot
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kubectl %v: %w: %s", args, err, errBuf.String())
	}
	return out.Bytes(), nil
}

func (e *execExecutor) Git(ctx context.Context, args ...string) (string, error) {
	return e.run(ctx, e.repoRoot, "git", args...)
}

// run executes a command in dir, returning combined stdout+stderr.
func (e *execExecutor) run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

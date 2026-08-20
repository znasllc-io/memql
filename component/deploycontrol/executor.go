package deploycontrol

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Executor is THE single side-effect boundary for MemQL deployments
// (Epic 2 / #2098). Every cluster read, git operation, and action-script
// invocation goes through it, so the Service logic is testable with a
// fake AND -- crucially -- so the deploy PACK (examples/deploypack) drives
// the EXACT same effects through the IntegrationProvider layer instead of
// re-implementing the shell-outs. The pack acquires this boundary via the
// exported NewExecutor(repoRoot); its capabilities (commit / argoSync /
// observeReconciledState) call the same methods the gRPC Service does. Keeping the effects here -- one place -- is what makes the
// DSL automation layer ADDITIVE rather than a parallel, drifting copy.
//
// Read paths use KubectlJSON and never mutate. Action paths go through
// `git revert` and `kubectl argo rollouts promote|abort` -- NEVER ad-hoc
// `kubectl set image`.
//
// Every cluster address here is THIS installation's (service.go's
// clusterNamespace). It was a per-environment lookup for the length of epic
// memql#3748; memql#3943 removed the concept, so there is one address again.
//
// Capability-script contract (#2221): the deploy/ops scripts a DSL action
// resolves to are CAPABILITY SCRIPTS -- non-interactive, structured params in,
// a single JSON result envelope on stdout, logs on stderr, honest exit codes
// (docs/internal/design/capability-script-contract.md). Those are run by
// component/automations/steps, which parses the envelope with this package's
// ParseCapabilityResult (capability_result.go) rather than scraping prose. No
// effect on THIS interface invokes one; git and kubectl are not capability
// scripts and their output is read directly.
//
// Scope note: the effect SEAM is consolidated here (the deploy pack reuses
// this Executor). The synchronous Go deploy LIFECYCLE that deploy.go's
// Deploy/RollbackDeployment once ran (select driver -> apply -> terminal
// transition) was RETIRED in #2115 step 6: those actions now only validate +
// kick off the lifecycle (transition to in_progress), and the deploy pack
// automations (examples/deploypack, anchored on the identity node) own the
// terminal transition through THIS Executor's effects. The legacy console
// actions (Rollback / RolloutAction) still call these effects synchronously.
type Executor interface {
	// RunRollback reverts the overlay commit identified by sha via
	// `git revert --no-edit <sha>`. Because a digest change is one commit
	// touching one overlay, reverting it re-pins exactly the prior digests --
	// which is what makes rollback a git operation rather than a second
	// forward action.
	RunRollback(ctx context.Context, sha string) (output string, err error)

	// RunRolloutAction runs `kubectl argo rollouts <action> <rollout>
	// -n <the installation's namespace>` for action in {promote, abort}.
	//
	// `promote`/`abort` here is the ARGO ROLLOUT verb -- advance or cancel an
	// in-flight progressive rollout. It is unrelated to the environment-to-
	// environment promote epic memql#3943 removed, and to the DSL authoring
	// catalog promote; only the name is shared.
	RunRolloutAction(ctx context.Context, rollout, action string) (output string, err error)

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
// uses (git / kubectl argo rollouts), exported so the
// deploy PACK (examples/deploypack, Epic 2 / #2095) can drive the same
// effects through the IntegrationProvider boundary without duplicating
// the shell-out wiring. An empty repoRoot anchors at the process
// working directory, matching NewService's RepoRoot default.
func NewExecutor(repoRoot string) Executor {
	return newExecExecutor(repoRoot)
}

func (e *execExecutor) RunRollback(ctx context.Context, sha string) (string, error) {
	return e.run(ctx, e.repoRoot, "git", "revert", "--no-edit", sha)
}

func (e *execExecutor) RunRolloutAction(ctx context.Context, rollout, action string) (string, error) {
	return e.run(ctx, e.repoRoot, "kubectl", "argo", "rollouts", action, rollout, "-n", clusterNamespace)
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

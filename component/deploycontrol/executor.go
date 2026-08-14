package deploycontrol

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
)

// Executor is THE single side-effect boundary for memQL deployments
// (Epic 2 / #2098). Every cluster read, git operation, and action-script
// invocation goes through it, so the Service logic is testable with a
// fake AND -- crucially -- so the deploy PACK (examples/deploypack) drives
// the EXACT same effects through the IntegrationProvider layer instead of
// re-implementing the shell-outs. The pack acquires this boundary via the
// exported NewExecutor(repoRoot); its capabilities (runPromote / commit /
// argoSync / observeReconciledState) call the same methods the gRPC
// Service does. Keeping the effects here -- one place -- is what makes the
// DSL automation layer ADDITIVE rather than a parallel, drifting copy.
//
// Read paths use KubectlJSON and never mutate. Action paths go through
// the sanctioned scripts (scripts/deploy/promote-overlay.sh), `git revert`,
// and `kubectl argo rollouts promote|abort` -- NEVER ad-hoc
// `kubectl set image`.
//
// Every cluster address here is resolved per-ENVIRONMENT through
// EnvironmentSurface (environment.go). It used to be the constant `memql`,
// which was the product pack's single namespace + Application and stopped being
// any environment's address when memql#3766 split the cloud in two.
//
// Capability-script contract (#2221): the deploy/ops scripts an action
// resolves to are CAPABILITY SCRIPTS -- non-interactive, structured params in,
// a single JSON result envelope on stdout, logs on stderr, honest exit codes
// (docs/internal/design/capability-script-contract.md). Parse such a script's
// stdout with ParseCapabilityResult (capability_result.go) rather than scraping
// prose, and trust the exit code. The k3d engine-local scripts (scripts/k3d/*)
// are the reference implementation; promote.sh + the Azure/ArgoCD scripts
// converge onto the contract via the Makefile/ArgoCD refactor track.
//
// Scope note: the effect SEAM is consolidated here (the gRPC Service's
// legacy promote actions share promoteRelease, and the deploy pack reuses
// this Executor + ConsoleEnvFor). The synchronous Go deploy LIFECYCLE that
// deploy.go's Deploy/RollbackDeployment once ran (select driver -> apply ->
// terminal transition) was RETIRED in #2115 step 6: those actions now only
// validate + kick off the lifecycle (transition to in_progress), and the
// deploy pack automations (examples/deploypack, anchored on the identity
// node) own promote + the terminal transition through THIS Executor's
// effects. The legacy console actions (DeployStaging / Promote / Rollback /
// RolloutAction) still call these effects synchronously.
type Executor interface {
	// RunPromote pins <env>'s overlay to the image digests the environment it
	// is promoted FROM is running, and lands that edit as ONE COMMIT. For the
	// only environment that has a source, that reads: pin production to the
	// digests staging is running (EnvironmentSurface.PromoteFrom).
	//
	// `version` names the release being promoted. It is provenance rather than
	// input -- the digests come off the source overlay, not off a version
	// lookup -- and it travels into the commit message, which is where a
	// one-tree promote records what happened.
	//
	// An env with no promote source (staging, whose digests are cut on the
	// build server and pinned into its overlay directly) is refused, naming
	// that. Returns the script's combined output.
	RunPromote(ctx context.Context, version, env string) (output string, err error)

	// RunRollback reverts the overlay commit identified by sha via
	// `git revert --no-edit <sha>`. Because a promote is one commit touching
	// one overlay, reverting it re-pins exactly the prior digests -- which is
	// what makes rollback a git operation instead of a second promote.
	RunRollback(ctx context.Context, env, sha string) (output string, err error)

	// RunRolloutAction runs `kubectl argo rollouts <action> <rollout>
	// -n <the env's namespace>` for action in {promote, abort}.
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

// RunPromote is the engine promotion: pin the target environment's overlay to
// the digests its source environment is running, then commit that one edit.
//
// # Why this is two effects and not one script
//
// The pin is a capability script (scripts/deploy/promote-overlay.sh) because
// rewriting YAML is mechanical work with a structured result. The commit is
// NOT, and deliberately does not live inside it: `git revert` of that same
// commit is the rollback (RunRollback), so what the commit CONTAINS is a
// contract between the two, and it belongs at the seam that owns both. It also
// keeps the script's contract clean -- one capability, one job, no decision
// about whether a change is worth recording.
//
// # Nothing is pushed
//
// A promote lands a commit and stops. `main` refuses direct pushes (a
// repository ruleset, not a convention), so a promote reaches ArgoCD the way
// every other change does: as a reviewed pull request. That review is also
// where the diff answers questions a script must not decide -- notably whether
// an image staging carries and production has deliberately pinned closed
// (memql-mcp) should now be open in production.
func (e *execExecutor) RunPromote(ctx context.Context, version, env string) (string, error) {
	target, ok := SurfaceFor(env)
	if !ok {
		return "", fmt.Errorf("deploycontrol: promote: no cluster surface for env %q (want staging|prod)", env)
	}
	source, ok := SurfaceFor(target.PromoteFrom)
	if !ok {
		return "", fmt.Errorf(
			"deploycontrol: promote: %q is promoted from nothing -- its image digests are cut on the build "+
				"server and pinned into %s directly, so there is no source overlay to copy them from. "+
				"Only production is promoted (from staging)", env, target.OverlayDir)
	}

	script := filepath.Join(e.repoRoot, "scripts", "deploy", "promote-overlay.sh")
	stdout, stderr, err := e.runCapture(ctx, e.repoRoot, script,
		"--from="+source.OverlayDir,
		"--to="+target.OverlayDir,
		"--version="+version,
		"--dryRun=false",
	)
	combined := stderr + stdout
	if err != nil {
		return combined, err
	}
	// Parse the capability envelope rather than scrape prose (#2221). A script
	// that exits 0 while reporting ok=false is a contract violation, but Err()
	// costs nothing and turns it into a message instead of a silent success.
	res, perr := ParseCapabilityResult([]byte(stdout))
	if perr != nil {
		return combined, perr
	}
	if rerr := res.Err(); rerr != nil {
		return combined, rerr
	}
	// No empty commit. `changed=false` means the target already carries exactly
	// the source's digests, which is the normal answer to promoting the same
	// version twice -- an idempotent no-op, not a failure.
	if !res.Changed {
		return combined, nil
	}

	overlayFile := filepath.Join(target.OverlayDir, "kustomization.yaml")
	commitOut, cerr := e.Git(ctx, "commit",
		"-m", promoteCommitMessage(version, source.ConsoleEnv, target.ConsoleEnv),
		"--", overlayFile)
	return combined + commitOut, cerr
}

// promoteCommitMessage is the provenance of a one-tree promote.
//
// It lives in the commit rather than in a header comment inside the overlay,
// which is what the retired scripts/release/promote.sh wrote ("# Promoted from
// releases/<version>.yaml ..."). With both environments in ONE repository the
// commit already carries the who, the when, and the exact diff; a comment line
// restating a subset of that is a second copy to keep honest, and it collided
// with an overlay header that now explains the environment split at length.
func promoteCommitMessage(version, from, to string) string {
	return fmt.Sprintf(
		"chore(deploy): promote %s from %s to %s\n\n"+
			"Pins the %s overlay's image digests to the ones %s is running. Digest copy,\n"+
			"no rebuild: the images are the same product-agnostic engine images %s has\n"+
			"already exercised.\n\n"+
			"Trained constructs do NOT travel with this. A promoted construct is a\n"+
			"v1:authoring:construct row and the two environments are separated by their\n"+
			"Postgres schema search path, so training %s is a separate, explicit act\n"+
			"against %s.",
		version, from, to, to, from, from, to, to)
}

func (e *execExecutor) RunRollback(ctx context.Context, env, sha string) (string, error) {
	return e.run(ctx, e.repoRoot, "git", "revert", "--no-edit", sha)
}

func (e *execExecutor) RunRolloutAction(ctx context.Context, env, rollout, action string) (string, error) {
	surface, ok := SurfaceFor(env)
	if !ok {
		return "", fmt.Errorf("deploycontrol: rollout action: no cluster surface for env %q (want staging|prod)", env)
	}
	// `-n <namespace>`, not the constant `memql` this passed until memql#3769.
	// The `env` argument was already on the signature and was simply unused --
	// so a rollout promote/abort aimed at production was addressed to a
	// namespace that no longer exists in the cloud estate.
	return e.run(ctx, e.repoRoot, "kubectl", "argo", "rollouts", action, rollout, "-n", surface.Namespace)
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

// runCapture is run() with the two streams kept apart, which a capability
// script requires: the contract puts human logs on stderr and exactly ONE JSON
// envelope on stdout (#2221), and merging them hands ParseCapabilityResult a
// buffer where a stray log line can land after the envelope.
func (e *execExecutor) runCapture(ctx context.Context, dir, name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

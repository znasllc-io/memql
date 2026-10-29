package pipelinesteps

import (
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// attempt_class.go -- what an attempt that did not succeed proves about whether
// its command could have run (spec section 4 of
// docs/superpowers/specs/2026-10-28-pipeline-fleet-first-retries-failover-design.md,
// decision D3).
//
// WHY THE EXECUTOR DECIDES. The driver retries a failed step, and what it may
// safely do next turns on one question it cannot answer from outside: could
// the command have run? Only the executor knows whether a dispatch reached a
// machine and what the machine or the Job answered. So it classifies every
// attempt that did not succeed, once, here, and the driver reads the class and
// nothing else. Four answers:
//
//   - nothing ran: the command never started, so another attempt cannot run it
//     twice. Any step may be retried.
//   - retry requested: the command ran and exited pl.ExitRetryRequested (75), the
//     command's own word that another run is safe and useful. Any step may be
//     retried, because the command said so.
//   - outcome unknown: the command may have run and nothing says how it ended.
//     Only a step that declares repeating it harmless is retried (D5).
//   - final: nothing about this ending is worth another try.
//
// WHY THE DEFAULT IS FINAL, AND WHY THE TABLE IS A LIST OF PROOFS. A code is
// retryable only when something PROVES the command did not run (or ran and
// asked to be repeated). Everything else, including a code this file has never
// seen, is final: a missed row costs a step that fails as it did before
// attempts existed, and a wrong row could run a release step twice. Each case
// below says what proves it. A code not in the spec's table stays final until
// the table says otherwise (the cluster's pipeline_artifact_unavailable is one:
// the spec lists it for a machine only).
//
// WHERE THE CLASS COMES FROM. It is set here (the cluster's reading, from the
// result alone) and in attempt_class_fleet.go (a machine's reading, which also
// needs the dispatcher's own word), by Executor.Execute, never taken from a
// runner's reply: a reply from a replica of another version must not be able to
// make an ordinary failure retryable.

// clusterAttemptClass is the class of an attempt read from its result alone:
// the whole answer for a Job, and the part of a machine's answer that does not
// depend on what the dispatcher said (fleetAttemptClass starts from it). It
// answers "" for a step that succeeded, which has no class.
//
// It is also what Execute applies to a result no surface classified: an
// executor-made refusal, a ceiling reached before a step started, a fleet
// router that does not classify.
func clusterAttemptClass(res pl.StepResult) pl.AttemptClass {
	switch res.Status {
	case pl.OutcomeSucceeded:
		return ""
	case pl.OutcomeCancelled:
		// FINAL. The run was cancelled or the drive that ran the step ended;
		// the driver's own cancel settles the step and nothing retries it.
		return pl.AttemptFinal
	}
	if res.Failure == nil {
		// The command ran and its exit code is the whole answer. 75 is the
		// command saying "transient, retry me" (EX_TEMPFAIL): it needs no
		// `repeatable` in the manifest, because the command itself is vouching
		// that another run is safe. Any other exit is the command failing, and a
		// failing command is not retried. A typed failure beside an exit code
		// (the step also timed out) is that failure's, handled below.
		if res.Status == pl.OutcomeFailed && res.ExitCode == pl.ExitRetryRequested {
			return pl.AttemptRetryRequested
		}
		return pl.AttemptFinal
	}
	switch res.Failure.Code {

	// NOTHING RAN -- the cluster could not start the command.

	case pl.CodeImagePullFailed:
		// The kubelet could not pull the image; the container was never created.
		return pl.AttemptNothingRan
	case pl.CodeServiceFailed:
		// A declared service stopped BEFORE the step started (the classifier reads
		// it only then: the kubelet stops every service once the step ends, so a
		// stopped service afterwards is not a failure). The command never started.
		return pl.AttemptNothingRan
	case pl.CodeJobUnschedulable:
		// No node could take the pod within the schedule timeout; nothing was
		// created that could run.
		return pl.AttemptNothingRan
	case pl.CodeRunnerUnavailable:
		// No workbench replica could be reached, or the one reached has no
		// pipeline runner: the step was never handed to a runner. (The fleet's
		// "no dispatcher on this node" says the same of a machine.)
		return pl.AttemptNothingRan
	case pl.CodeCloneFailed:
		// The checkout failed before the command (the Job's clone retries its
		// fetch itself first; a machine's fetch is retried by the driver). Also
		// what a clone token that could not be minted reads as, on a machine.
		return pl.AttemptNothingRan
	case pl.CodeNoMachineForNeed:
		// Only ever made when the dispatcher PROVED no machine started the
		// command (Fleet.classify's reading of Result.RefusedBeforeStart, and the
		// bounded waits that give up). A fleet router that does not classify its
		// own refusal still reads right.
		return pl.AttemptNothingRan

	// OUTCOME UNKNOWN -- the command may have run, and nothing says how it ended.

	case pl.CodeNodeLost:
		// The node under the Job went away, or the cluster stopped the pod (a
		// drain, a preemption, an eviction): the command may have been running,
		// possibly to completion, when it was killed. On a machine the same code
		// is "the connection ended after the dispatch left".
		return pl.AttemptOutcomeUnknown
	case pl.CodeExecutionUncertain:
		// The executor's own word that a previous driver may have started the
		// command and no durable result was recorded (a recovery of an attempt
		// that has no receipt). It is not in the spec's table, which describes
		// what a surface answers; both surfaces read it as unknown so a repeatable
		// step continues and any other step stops, exactly as the spec's lifecycle
		// section says of a takeover.
		return pl.AttemptOutcomeUnknown

	// FINAL -- everything else, spelled out so the list reads as the spec's
	// does; the default is the same answer.

	case pl.CodeStepTimeout, pl.CodeRunCeiling:
		// A timeout is final: a hang and a stall look alike from outside, and a
		// script that KNOWS it stalled exits 75 instead. The run's ceiling is the
		// run's, and a retry would meet it again.
		return pl.AttemptFinal
	case pl.CodeStepDiskExceeded, pl.CodeJobRejected, pl.CodeIsolationUnenforced, pl.CodeArtifactTooLarge, pl.CodeExecutorError:
		// The step's own doing (disk), a Job the cluster refuses to run (it can
		// never run), a cluster that cannot isolate steps (an operator's to fix),
		// artifacts that will be too large again, an error of the engine's own.
		return pl.AttemptFinal
	}
	// A refusal (the executor would not start the step), a secret failure, and
	// any code nobody classified: final (D3).
	return pl.AttemptFinal
}

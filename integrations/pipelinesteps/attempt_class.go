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
// make an ordinary failure retryable. The two places a runner's outcome enters
// the agent drop any class it carries (fromARunner, executor_cluster.go).
//
// WHAT A CODE CANNOT SAY. Two cluster codes look like "the command never
// started" and are not always. They are read here from more than the code:
//
//   - pipeline_runner_unavailable depends on WHO MADE IT. The agent makes it
//     when it cannot hand a step to anything (noRunnerReached, below), before any
//     runner took the step: nothing ran. A runner makes it where the step's Job
//     exists or may exist: unknown.
//   - pipeline_image_pull_failed depends on WHETHER THE STEP'S OWN CONTAINER
//     STARTED, which the result records in StartedAt.
//
// A class decided from a code that does not prove it is how a non-repeatable step
// runs twice, so each is spelled out at its case.

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
		// NOTHING RAN only when the result proves the STEP'S OWN container never
		// started; otherwise UNKNOWN. The classifier (Classify, podFailure)
		// reports a pull failure of ANY container of the pod, a service's sidecar
		// included, and does so even while the step is running: the kubelet pulls
		// a sidecar's image again each time it restarts it (imagePullPolicy
		// Always), and Classify asks whether the step is running only AFTER it
		// has checked every container's pull state. So a registry that fails for a
		// restarted sidecar reports this code about a command that is running.
		//
		// The exit status cannot tell the two apart (-1 in both: no exit was
		// observed). StartedAt can: the runner takes it from the step container's
		// own state (stepTimes) and it stays empty until that container starts.
		// A pull failure with an exit status could not have come from Classify at
		// all, so it is not trusted either. Missing the proof costs a retry that
		// only a repeatable step would have been given; having it wrong could run
		// a command twice.
		if res.StartedAt == "" && res.ExitCode < 0 {
			return pl.AttemptNothingRan
		}
		return pl.AttemptOutcomeUnknown
	case pl.CodeServiceFailed:
		// A declared service stopped BEFORE the step started: the classifier reads
		// it only while the step container is not running (podFailure's gate, and
		// a terminated step is read before anything else). Unlike a pull failure,
		// then, it cannot be reported about a running command. The command never
		// started.
		return pl.AttemptNothingRan
	case pl.CodeJobUnschedulable:
		// No node could take the pod within the schedule timeout (PodScheduled is
		// false): nothing was bound to a node, so nothing ran.
		return pl.AttemptNothingRan
	case pl.CodeCloneFailed:
		// The clone init container exited non-zero. The step container starts only
		// after every init container has completed, so it never started. (The Job's
		// clone retries its fetch itself first; a machine's fetch is retried by the
		// driver.) Also what a clone token that could not be minted reads as, on a
		// machine.
		return pl.AttemptNothingRan
	case pl.CodeNoMachineForNeed:
		// Only ever made when the dispatcher PROVED no machine started the
		// command (Fleet.classify's reading of Result.RefusedBeforeStart, and the
		// bounded waits that give up). A fleet router that does not classify its
		// own refusal still reads right.
		return pl.AttemptNothingRan

	// OUTCOME UNKNOWN -- the command may have run, and nothing says how it ended.

	case pl.CodeRunnerUnavailable:
		// UNKNOWN here: a result that reaches this function with this code was
		// reported by a runner, or arrived in a way the agent cannot vouch for. A
		// runner answers it where the step's Job exists or may exist:
		//
		//   - a new Run starts without having seen the Job, so a failed read of it
		//     answers this code (a re-forward after a lost replica or a stale
		//     heartbeat, and a recovery while Job N may be running);
		//   - a recovery whose queued record cannot be read (the attempt may
		//     already have executed);
		//   - adopting a Job that exists: the claim of it fails (ten transient
		//     errors, or one that is not), or its log archive cannot be opened.
		//
		// The Job does not wait for the runner's claim (it is suspended only under
		// Kueue), and a runner that reports leaves it in place (the agent's
		// acknowledgement, or a cancel, deletes it), so a command can be running
		// while the runner says it cannot take the step. The next attempt would
		// then make a new Job (its name includes the attempt) and run the command
		// twice. Other runner sites answer it before any Job exists, but the code
		// does not say which; a distinct code would let them read as nothing ran.
		//
		// The agent's own refusal, made before any runner took the step, is nothing
		// ran, and is classified where it is made (noRunnerReached).
		return pl.AttemptOutcomeUnknown
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

// noRunnerReached is the failure the AGENT makes when it could not hand a step
// to anything that could run it, before any workbench runner or machine took
// the step: nothing ran. It is the one kind of pipeline_runner_unavailable that
// proves it, which is why the class is decided HERE, where the agent makes the
// result, and not read back from the code (clusterAttemptClass says why the code
// alone cannot): the same code from a runner means its Job may exist.
//
// The agent makes it in exactly these places, each before the step reached a
// runner or a machine:
//
//   - executor_cluster.go, runCluster: this node has no route to a workbench
//     replica at all;
//   - executor_cluster.go, noPeerTooLong: no replica was reachable for the whole
//     patience, and none ever took or answered the step (the gate is !s.reached);
//   - executor_cluster.go, answer: a replica said it has no pipeline runner, and
//     no replica had taken the step before it (a replica that answers so AFTER
//     another took the step and was lost is not this: the first may have made the
//     Job);
//   - executor.go, runFleet and runPool: this node has no fleet dispatcher;
//   - fleet.go, runStep: the same, and a dispatcher that could not take the step,
//     which is an error before anything is sent.
//
// NEVER ON A RECOVERY. A recovery (StepRequest.RecoverOnly) re-sends an attempt a
// previous driver journaled and recorded no receipt for, so that attempt's Job may
// exist and be running under a runner this node has never heard from, or its
// command may have been dispatched to a machine. "No replica took the step" is this
// executor's own memory, which starts empty, so it proves nothing about it. A
// recovery therefore treats the step as already reached (clusterStep.reached) and,
// with no route at all, answers pipeline_execution_uncertain as the runner itself
// does for a recovery that cannot find the Job; the fleet reads a recovery before
// it asks whether it has a dispatcher. None of those sentences says "Nothing ran.".
//
// A result that arrives from a runner is stripped of any class on the way in, so
// a class on a result here can only have been set by the agent's own code.
func noRunnerReached(message string) pl.StepResult {
	res := failedResult(pl.CodeRunnerUnavailable, message)
	res.AttemptClass = pl.AttemptNothingRan
	return res
}

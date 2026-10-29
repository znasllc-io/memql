//go:build agent

package pipelinesteps

import (
	pl "github.com/znasllc-io/memql/component/pipelines"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// attempt_class_fleet.go -- what a MACHINE's answer proves about whether the
// command could have run (spec section 4; the cluster's rows and the reasoning
// for the whole table are in attempt_class.go).
//
// A machine's answer needs two inputs the cluster's does not:
//
//   - the dispatcher's own verdict (worker.Result.RefusedBeforeStart): that
//     NOTHING started on any machine, whatever code it travelled under. A code
//     list cannot keep up with the codes a sibling replica refuses with, so the
//     verdict decides, never the code (Fleet.classify reads it the same way);
//   - the machine's RAW code, which Fleet.classify throws away when it turns
//     most of them into pipeline_executor_error. "The machine's checkout failed"
//     and "the machine could not run the command" must stay different here even
//     though they are one code in the result.
//
// Agent-tagged: the dispatcher's Result is.

// The raw codes a machine answers with. They are the cockpit's
// (cockpit/internal/worker/tools/pipeline_step.go) and the dispatcher's
// (integrations/agent/worker); neither exports them, and the existing fleet
// code spells the capacity ones the same way (fleet_capacity.go).
const (
	machineCacheUnavailable    = "pipeline_cache_unavailable"
	machineRuntimeUnavailable  = "pipeline_runtime_unavailable"
	machineCapacityBusy        = "pipeline_capacity_busy"
	machineCapacityFenced      = "pipeline_capacity_fenced"
	machineCapacityUnavailable = "pipeline_capacity_unavailable"
	machineRecoveryRequired    = "pipeline_recovery_required"
	machinePipelinesNotAllowed = "pipelines_not_allowed"
	machineFeatureDisabled     = "worker_feature_disabled"
	machineFeatureUnavailable  = "worker_feature_unavailable"
	machineDeniedByPolicy      = "denied_by_policy"
	machineWorkerDisconnected  = "worker_disconnected"
)

// machineRefusal reads a machine's own code for the one thing a code can
// prove: that the command never started there, and whether that machine should
// not be tried again for this step. Anything else is false, false.
func machineRefusal(code string) (nothingRan, notRetriedHere bool) {
	switch code {

	// NOTHING RAN, and the same machine may be tried again after the driver's
	// delay: each is checked BEFORE the command, and each is the kind of thing a
	// delay can fix.

	case pl.CodeCloneFailed:
		// The machine's own `git fetch` (it runs once; the cluster's Job retries
		// its own three times) failed or the clone token was refused, before the
		// command. A flaky network or a rate limit is exactly what a same-machine
		// retry is for.
		return true, false
	case machineCacheUnavailable:
		// The machine could not prepare the step's cache directory, which it does
		// before the checkout.
		return true, false
	case machineRuntimeUnavailable:
		// Docker, or the declared runtime, was not usable. Checked before the
		// checkout and before the container is created.
		return true, false
	case machineCapacityBusy, machineCapacityFenced, machineCapacityUnavailable:
		// The machine's one-command-per-OS-user reservation, its disk or memory
		// headroom, or an operator's fence refused. The capacity wait normally
		// absorbs these (dispatchWhenCapacityAvailable); one that reaches the
		// result is still a refusal before the command ("a capacity refusal that
		// reached the result", spec section 4).
		return true, false
	case machineDeniedByPolicy:
		// The machine's own pipelines policy refused the repository or the limits,
		// at the top of the step, before anything else. The dispatcher lists it as
		// proof of no start (pipelineRefusedBeforeStart). The spec does not say to
		// stop retrying it, so it is not flagged; the driver's own bounds do.
		return true, false
	case machineFeatureUnavailable:
		// The settings read that decides whether Host commands are allowed could
		// not be made by the replica holding the machine's stream. Transient, so
		// the same machine may be tried again (ruling 5). The refusal precedes the
		// send.
		return true, false

	// NOTHING RAN, and that machine is not tried again: what refused will still
	// be true when a retry's delay is over, so a retry here only spends it.
	//
	// pipeline_recovery_required is deliberately NOT here. It proves nothing
	// about whether the command ran, so it is an unknown outcome, decided in
	// fleetAttemptClass before the dispatcher's flag is read. Do not add it to
	// this list: that is the mistake the spec's table made.

	case machineFeatureDisabled:
		// The owner switched Host commands off in Fleet settings, and the machine's
		// stream session refused before sending anything (Task 2). An owner's
		// switch does not flip back in a retry delay.
		return true, true
	case machinePipelinesNotAllowed:
		// The machine's live registration no longer advertises pipelines=allowed
		// at the contract the engine needs, so the dispatcher refused before
		// sending. Its policy or its Cockpit changed; waiting does not fix that.
		return true, true
	}
	return false, false
}

// fleetAttemptClass is the class of an attempt that went to the fleet, and
// whether the machine it names should not be tried again for this step
// (pl.StepResult.NotRetriedHere). r is the dispatcher's last answer, res what
// Fleet made of it (and of everything after it: the artifacts can fail a
// command that exited 0), and dispatched says whether r is the source of res at
// all. It is false for a result the fleet made without a dispatch answering
// (a refusal of the request, a clone token that could not be minted, a wait
// that gave up, a stop), and then res alone decides.
func fleetAttemptClass(r worker.Result, res pl.StepResult, dispatched bool) (pl.AttemptClass, bool) {
	if res.Status == pl.OutcomeSucceeded {
		return "", false
	}

	// FINAL, whatever the machine said. Fleet.classify reads the context before
	// the dispatcher's answer, so a step cancelled or run out of time while a
	// stream dropped is a cancel or a ceiling and not "the connection ended".
	// Reading the raw code first would turn it into an unknown outcome and a
	// retry of a step the run has already stopped.
	if res.Status == pl.OutcomeCancelled {
		return pl.AttemptFinal, false
	}
	if res.Failure != nil {
		switch res.Failure.Code {
		case pl.CodeStepTimeout, pl.CodeRunCeiling:
			return pl.AttemptFinal, false
		case pl.CodeFleetDisabled:
			// The owner's off switch ("nothing runs on my machines"). classify reads
			// its code before the dispatcher's flags, so it is the owner's decision
			// however the verdict travelled -- including flagged as refused before
			// start, which would otherwise read as "no machine could take it" and be
			// tried again on the next one. Retrying a switch is never right.
			return pl.AttemptFinal, false
		}
	}

	if !dispatched {
		// The fleet made this itself, and no dispatcher answered. For most codes
		// what the result says on its face is the whole proof: pipeline_clone_failed
		// (the clone token could not be minted: nothing was sent),
		// pipeline_no_machine_for_need (a wait gave up), pipeline_execution_uncertain
		// (a recovery), or a refusal.
		//
		// pipeline_runner_unavailable is NOT one of them. Its face proves nothing
		// (the same code from a runner means a Job may exist), so the fleet's own
		// (no dispatcher; a dispatcher that errored before it sent anything) is
		// classified where it is made, nothing ran, by noRunnerReached, and arrives
		// here already classified: RunStep keeps a class that is set. A bare one
		// that got here is not the agent's, and clusterAttemptClass reads it as an
		// unknown outcome. A recovery never reaches that site: it is read first.
		return clusterAttemptClass(res), false
	}

	switch {
	case r.RefusedByGate:
		// FINAL. THIS engine refused before routing (its own gate or a read it
		// could not make): a decision about the request, or an engine fault,
		// reported as pipeline_executor_error. It names no machine to blame or
		// retry, and the owner's off switch (kill_switch_engaged) is among them.
		return pl.AttemptFinal, false
	case r.ErrorCode == machineRecoveryRequired:
		// OUTCOME UNKNOWN, and that machine is not tried again. The Cockpit
		// answers this code BEFORE the command (it could not reconcile a dirty
		// reservation, or record the ownership of the checkout or of the command
		// it was about to start) and AFTER it (it could not record the cleanup of
		// a command that has finished, native or container:
		// cockpit/internal/worker/tools pipeline_step.go and pipeline_container.go
		// use the one code for both). The code alone cannot say which, and the
		// worse reading is the one that must hold: read as "nothing ran" it could
		// start a command that already ran a second time on another machine, and
		// a non-repeatable step (a release, a publish) would have run twice. As an
		// unknown outcome a repeatable step still fails over to another machine
		// and any other step fails as it does without retries.
		//
		// NOT RETRIED HERE: the machine refuses new work until its reservation is
		// reconciled, so another attempt on it only spends the retry's delay.
		//
		// Decided before the dispatcher's verdict because no verdict can improve
		// it. The dispatcher never flags this code as refused before start, and
		// pins that (TestOnlyConfirmedContractRefusalsAllowAnotherMachine in
		// integrations/agent/worker); if a change ever did, the Cockpit's two uses
		// of the code would still make "nothing ran" unprovable.
		return pl.AttemptOutcomeUnknown, true
	case r.RefusedBeforeStart:
		// NOTHING RAN, by the dispatcher's own verdict: no candidate, or every
		// candidate refused before anything was sent to it, a sibling's refusals
		// under codes of their own included. Whether the machine it names is
		// worth another try follows that machine's own code.
		_, notRetried := machineRefusal(r.ErrorCode)
		return pl.AttemptNothingRan, notRetried
	case r.OK:
		// The command ran to an exit. The exit decides (75 asks to be retried),
		// except for one thing the exit cannot see: it exited 0 and its artifacts
		// did not arrive intact. The command's effects happened, and nothing says
		// whether a copy is harmless, so the outcome is unknown rather than the
		// command failing.
		if res.Failure != nil && res.Failure.Code == pl.CodeArtifactUnavailable && res.ExitCode == 0 {
			return pl.AttemptOutcomeUnknown, false
		}
		return clusterAttemptClass(res), false
	}

	// The machine answered with a failure, without the dispatcher's verdict
	// that nothing started.
	if nothingRan, notRetried := machineRefusal(r.ErrorCode); nothingRan {
		return pl.AttemptNothingRan, notRetried
	}
	if r.ErrorCode == machineWorkerDisconnected {
		// OUTCOME UNKNOWN: the connection ended after the dispatch left, or the
		// reply was lost. Not flagged as refused before start (that case is
		// above), so the command may be running, or may have finished.
		return pl.AttemptOutcomeUnknown, false
	}
	// Anything else a machine says (its own timeout, a command that could not
	// run, a container it could not create, a cleanup it could not confirm, a
	// code nobody classified) is read from the result: final unless it is one
	// the shared rules know.
	return clusterAttemptClass(res), false
}

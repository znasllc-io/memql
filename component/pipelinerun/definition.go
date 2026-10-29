package pipelinerun

import (
	"strings"

	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/buildinfo"
)

// fingerprintScope says what a step fingerprint binds besides the step itself.
type fingerprintScope int

const (
	// recoveryScope is the execution definition: everything a replica that takes
	// a run over must find unchanged before it carries the run on (definitionOf).
	// It binds the installed compute placement policy for every command step.
	recoveryScope fingerprintScope = iota
	// evidenceScope is the identity of a verification receipt (verificationOf).
	// It binds what the command computes and nothing about where or how hard it
	// was run, so it leaves the compute placement policy out.
	evidenceScope
)

// definitionOf binds the command, image, placement, services, dependency graph,
// the installed compute placement policy (for a command step) and immutable run
// inputs. Only a digest is journaled; literal command text and resolved secret
// values never enter the call metadata. Selection decisions are restored from
// the original rows before comparing this definition.
func (dr *runDriver) definitionOf(step pipelines.Step) string {
	return dr.definitionForRun(step, dr.run, recoveryScope)
}

// Only immutable run inputs may be normalized. The live driver owns atomic
// journal state and locks and must never be copied to compute a fingerprint.
func (dr *runDriver) definitionForRun(step pipelines.Step, run Run, scope fingerprintScope) string {
	revision := buildinfo.Commit()
	if dr.d.EngineRevision != nil {
		revision = dr.d.EngineRevision()
	}
	if revision == "" || strings.HasSuffix(revision, "-dirty") {
		// Unknown or mutable source cannot prove that another binary implements
		// the same execution contract. Fresh work may run; recovery may not.
		return ""
	}
	domain := ""
	if dr.d.Domain != nil {
		domain = dr.d.Domain()
	}
	step.Skip = nil // the journal owns the original selection, not a new compare
	call := map[string]any{
		"engineRevision": revision, "step": step,
		"workflow":   dr.workflowIdentity(),
		"repository": dr.p.Repository, "pipelineId": bareID(dr.p.ID),
		"ownerUserId": dr.p.OwnerUserID, "compute": dr.p.Compute,
		"sha": run.SHA, "version": run.Version, "inboundRequestId": run.InboundRequestID,
		"mode": run.Mode, "event": run.Event, "headBranch": run.HeadBranch, "domain": domain,
	}
	if scope == recoveryScope && step.Kind == pipelines.StepCommand {
		// The installed policy decides how many times, how soon and where this
		// step is attempted again (its retry limits) and, for a pool step, where
		// its first attempt goes. A replica that took the run over under other
		// numbers would be changing a retry that is already under way, so a
		// changed answer must change this fingerprint: recovery then refuses the
		// run (diverged) instead of applying limits the first replica never
		// agreed to. Retries apply to every command step, so every one is bound;
		// binding pool steps alone would leave a cluster or fleet step free to be
		// retried under new limits.
		//
		// The policy's SOURCE is immutable for the run (the snapshot), yet an
		// authored expression can still use a clock, so the validated answer is
		// bound as well as the source. A step the driver delivers itself is not
		// bound: no executor runs it and no limit applies to it.
		call["computePlacement"] = dr.computePlacement
	}
	value, err := workjournal.DefinitionFingerprint("pipeline-execution-v1", []workjournal.StepDecl{{
		Key: step.Key, Kind: workjournal.KindDeterministic, StepType: "exec",
		Call: call,
	}})
	if err != nil {
		return "" // typed definition data should always encode; never bless a failure
	}
	return value
}

// Verification has a deliberately smaller environment: it cannot read an
// event, release version, base or run identity. Its proof still binds the
// command's executable definition, source SHA, selected packages and owner, and
// nothing about where or how hard it was run: evidence is what the command
// computes from its source, so editing the installation's retry limits, or a
// pool step's Fleet preference, must not make a passed verification unreusable.
// The compute placement policy therefore stays out of this fingerprint
// (evidenceScope) even though recovery binds it (recoveryScope).
func (dr *runDriver) verificationOf(step pipelines.Step) string {
	if !step.Verification || step.Kind != pipelines.StepCommand {
		return ""
	}
	run := dr.run
	run.Event, run.Mode, run.Version, run.InboundRequestID, run.HeadBranch = "", "", "", "", ""
	return dr.definitionForRun(step, run, evidenceScope)
}

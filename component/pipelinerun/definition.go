package pipelinerun

import (
	"strings"

	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/buildinfo"
)

// definitionOf binds the command, image, placement, services, dependency graph
// and immutable run inputs. Only a digest is journaled; literal command text
// and resolved secret values never enter the call metadata. Selection decisions
// are restored from the original rows before comparing this definition.
func (dr *runDriver) definitionOf(step pipelines.Step) string {
	return dr.definitionForRun(step, dr.run)
}

// Only immutable run inputs may be normalized. The live driver owns atomic
// journal state and locks and must never be copied to compute a fingerprint.
func (dr *runDriver) definitionForRun(step pipelines.Step, run Run) string {
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
	value, err := workjournal.DefinitionFingerprint("pipeline-execution-v1", []workjournal.StepDecl{{
		Key: step.Key, Kind: workjournal.KindDeterministic, StepType: "exec",
		Call: map[string]any{
			"engineRevision": revision, "step": step,
			"workflow":   dr.workflowIdentity(),
			"repository": dr.p.Repository, "pipelineId": bareID(dr.p.ID),
			"ownerUserId": dr.p.OwnerUserID, "compute": dr.p.Compute,
			"sha": run.SHA, "version": run.Version, "inboundRequestId": run.InboundRequestID,
			"mode": run.Mode, "event": run.Event, "domain": domain,
		},
	}})
	if err != nil {
		return "" // typed definition data should always encode; never bless a failure
	}
	return value
}

// Verification has a deliberately smaller environment: it cannot read an
// event, release version, base or run identity. Its proof still binds the
// complete executable definition, source SHA, selected packages and owner.
func (dr *runDriver) verificationOf(step pipelines.Step) string {
	if !step.Verification || step.Kind != pipelines.StepCommand {
		return ""
	}
	run := dr.run
	run.Event, run.Mode, run.Version, run.InboundRequestID = "", "", "", ""
	return dr.definitionForRun(step, run)
}

// treeFingerprintOf is verificationOf with two changes, and it is what a release
// compares to reuse a receipt across commits (evidence.go,
// docs/superpowers/specs/2026-10-29-release-same-tree-reuse-design.md, D2):
//
//   - The tree hash takes the commit's place. A merge commit and the pull request
//     head it landed differ as commits and not as source. A verification step
//     cannot read MEMQL_SHA (pipelines.StepRequest.Environment), so the commit is
//     not an input to its command.
//   - DependsOn goes. It lists the previous stage's steps, which differ between a
//     pull request plan and a release plan on one tree, and it orders the plan
//     without reaching the command. Left in, no step after an event-gated stage
//     would ever match.
//
// A step that declares checkoutHistory has inputs beyond the tree, so it gets no
// fingerprint and is never reused across commits. No tree, no fingerprint: an
// unread hash must not compare equal to another unread hash.
func (dr *runDriver) treeFingerprintOf(step pipelines.Step) string {
	if !pipelines.ValidTreeHash(dr.run.TreeHash) || !step.Verification || step.Kind != pipelines.StepCommand || step.CheckoutHistory != "" {
		return ""
	}
	run := dr.run
	run.Event, run.Mode, run.Version, run.InboundRequestID = "", "", "", ""
	run.SHA = dr.run.TreeHash
	step.DependsOn = nil
	return dr.definitionForRun(step, run)
}

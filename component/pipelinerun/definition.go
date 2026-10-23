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
			"sha": dr.run.SHA, "version": dr.run.Version, "inboundRequestId": dr.run.InboundRequestID,
			"mode": dr.run.Mode, "event": dr.run.Event, "domain": domain,
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
	copy := *dr
	copy.run = dr.run
	copy.run.Event, copy.run.Mode, copy.run.Version, copy.run.InboundRequestID = "", "", "", ""
	return copy.definitionOf(step)
}

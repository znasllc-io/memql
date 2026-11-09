package argocd

import (
	"context"
	"errors"
)

// PlanRollback reads the same Application and derives a reversal of one exact
// owned, terminal operation. It grants no authority and performs no write. The
// installation journal must admit and persist this new intent before Apply.
// An uncertain/pending operation cannot be replaced by a guessed rollback.
//
// The terminal outcome is read from status.operationState, or, after a later
// sync replaced it there, from the caller's evidence that the operation
// Succeeded (WithRecordedSuccess), under observe's conditions.
//
// The read is the name-filtered list ReadPlanning makes, so the reversal is a
// history-backed (format 2) intent bound to a global watch boundary. Argo's
// status writes advance an Application's generation on CRDs without a status
// subresource; a reversal bound to the exact generation it was planned at
// refused its patch after one such write, on every retry. Applying this intent
// instead needs a ContinuityProof (Admit, BoundaryProof, ApplyWithContinuity)
// that accepts status-only generation advances and still refuses any spec,
// marker, operation or identity change since this read. When that history is
// no longer available, a caller whose journal proves nothing of the reversal
// was written plans it again here rather than strand it.
func (c *Client) PlanRollback(ctx context.Context, prior Intent, request string) (Intent, error) {
	before, after, digest, err := prior.validate()
	if err != nil {
		return Intent{}, err
	}
	snapshot, err := c.ReadPlanning(ctx, prior.Target)
	if err != nil {
		return Intent{}, err
	}
	facts, err := observe(snapshot, prior, after, digest, successEvidenceIn(ctx)[digest])
	if err != nil {
		return Intent{}, err
	}
	if err := idle(snapshot.object); err != nil {
		return Intent{}, err
	}
	if facts.OperationPhase != "Succeeded" && facts.OperationPhase != "Failed" && facts.OperationPhase != "Error" {
		return Intent{}, errors.New("ArgoCD rollback requires the owned operation's terminal outcome")
	}
	rollback, err := PlanRevision(snapshot, request, stringAt(object(before, "source"), "targetRevision"), prior.Prune)
	if err != nil {
		return Intent{}, err
	}
	if err := ValidateRollback(prior, rollback); err != nil {
		return Intent{}, err
	}
	return rollback, nil
}

// ValidateRollback checks the complete persisted parent/reversal relation. It
// does not establish fresh controller state or installation approval. In
// particular a valid digest for an unrelated intent is not a rollback proof.
func ValidateRollback(prior, rollback Intent) error {
	before, after, digest, err := prior.validate()
	if err != nil {
		return err
	}
	rollbackBefore, rollbackAfter, _, err := rollback.validate()
	if err != nil {
		return err
	}
	if prior.Target != rollback.Target || rollback.RequestID == prior.RequestID || rollback.BeforeMarker != digest ||
		rollback.BeforeGeneration < prior.BeforeGeneration || rollback.Prune != prior.Prune ||
		!commitSHA.MatchString(stringAt(object(before, "source"), "targetRevision")) ||
		rollback.Revision == prior.Revision || !equalJSON(rollbackBefore, after) || !equalJSON(rollbackAfter, before) {
		return errors.New("ArgoCD rollback does not reverse the exact recorded installation intent")
	}
	return nil
}

// PlanUnappliedRollback fences a recorded-but-undispatched forward intent by
// syncing its exact original baseline. Unlike a local no-effect assumption,
// this CAS prevents an uncertain delayed forward request from landing later.
func PlanUnappliedRollback(snapshot Snapshot, prior Intent, request string) (Intent, error) {
	expected, err := PlanRevision(snapshot, prior.RequestID, prior.Revision, prior.Prune)
	if err != nil {
		return Intent{}, err
	}
	before, _, _, err := prior.validate()
	if err != nil {
		return Intent{}, err
	}
	// A retained history-backed forward intent and this fresh read may use
	// different intent formats. Compare their actual baseline, not their
	// encoding. Classifying the baseline as untouched deliberately keeps the
	// strict generation comparison with the forward intent: a changed baseline
	// cannot be adopted without separately admitted reversal evidence, so a
	// status-driven generation advance since the forward intent was planned
	// classifies it as touched and the caller needs PlanRollback instead.
	// Given a ReadPlanning snapshot, the fence itself is history-backed for
	// its own patch (PlanRevision), like every other reversal.
	if expected.Target != prior.Target || expected.BeforeGeneration != prior.BeforeGeneration ||
		expected.BeforeMarker != prior.BeforeMarker || !equalJSON(object(snapshot.object, "spec"), before) {
		return Intent{}, ErrChanged
	}
	reversal, err := PlanRevision(snapshot, request, stringAt(object(before, "source"), "targetRevision"), prior.Prune)
	if err != nil {
		return Intent{}, err
	}
	return reversal, ValidateUnappliedRollback(prior, reversal)
}
func ValidateUnappliedRollback(prior, reversal Intent) error {
	before, _, _, err := prior.validate()
	if err != nil {
		return err
	}
	reverseBefore, reverseAfter, _, err := reversal.validate()
	if err != nil {
		return err
	}
	if prior.Target != reversal.Target || prior.RequestID == reversal.RequestID || prior.BeforeGeneration != reversal.BeforeGeneration || prior.BeforeMarker != reversal.BeforeMarker || prior.Prune != reversal.Prune || !commitSHA.MatchString(reversal.Revision) || !equalJSON(before, reverseBefore) || !equalJSON(before, reverseAfter) {
		return errors.New("unapplied rollback must fence the exact original Application baseline")
	}
	return nil
}

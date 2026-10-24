package argocd

import (
	"context"
	"errors"
)

// PlanRollback reads the same Application and derives a reversal of one exact
// owned, terminal operation. It grants no authority and performs no write. The
// installation journal must admit and persist this new intent before Apply.
// An uncertain/pending operation cannot be replaced by a guessed rollback.
func (c *Client) PlanRollback(ctx context.Context, prior Intent, request string) (Intent, error) {
	before, after, digest, err := prior.validate()
	if err != nil {
		return Intent{}, err
	}
	snapshot, err := c.Read(ctx, prior.Target)
	if err != nil {
		return Intent{}, err
	}
	facts, err := observe(snapshot, prior, after, digest)
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
	a, err := expected.Digest()
	if err != nil {
		return Intent{}, err
	}
	b, err := prior.Digest()
	if err != nil || a != b {
		return Intent{}, ErrChanged
	}
	before, _, _, err := prior.validate()
	if err != nil {
		return Intent{}, err
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

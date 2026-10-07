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

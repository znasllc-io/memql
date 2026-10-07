package installation

import (
	"context"
	"errors"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type preparationRetirementFiles interface {
	pipelinesteps.LibraryArtifactScopeRetirement
	pipelinesteps.LibraryArtifactLifecycle
}

func retirementArtifactScope(spec sourceCaptureSpec) pipelinesteps.RunFileReceiptScope {
	return pipelinesteps.RunFileReceiptScope{OwnerUserID: spec.OwnerUserID, WorkRunID: spec.WorkRunID, StepKey: spec.StepKey, Attempt: spec.Attempt}
}

// Each method is one bounded effect chosen by the cleanup recipe. Database
// transitions bind the observed result to the original scope before a later
// operation may advance. No method chooses another operation or loops pages.
func (j *preparationJournal) stopRetiringProducer(ctx context.Context, installation, key, workflow, cleanupWorkflow string, executor sourceCaptureExecutor) (preparationRecord, error) {
	r, err := j.retirementRecord(ctx, installation, key, workflow, cleanupWorkflow)
	if err != nil {
		return preparationRecord{}, err
	}
	if r.Retirement.ProducerStopped {
		return r, nil
	}
	if executor == nil {
		return preparationRecord{}, errors.New("source cleanup executor unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = executor.Cancel(ctx, r.Scope.Captures["candidate"].RunID); err != nil {
		return preparationRecord{}, errors.New("source producer cleanup remains unconfirmed")
	}
	return j.withRetirement(ctx, installation, key, workflow, cleanupWorkflow, func(r *preparationRecord) error {
		r.Retirement.ProducerStopped = true
		return nil
	})
}

func (j *preparationJournal) fenceRetiringCapture(ctx context.Context, installation, key, workflow, cleanupWorkflow, role string, files preparationRetirementFiles) (preparationRecord, error) {
	r, err := j.retirementRecord(ctx, installation, key, workflow, cleanupWorkflow)
	if err != nil {
		return preparationRecord{}, err
	}
	c, found := r.Retirement.Captures[role]
	if !found || !r.Retirement.ProducerStopped || files == nil {
		return preparationRecord{}, errors.New("artifact fence requires a stopped scoped producer")
	}
	if c.Fenced {
		return r, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = files.FenceRunFileScope(ctx, retirementArtifactScope(r.Scope.Captures[role])); err != nil {
		return preparationRecord{}, errors.New("artifact producer fence remains unconfirmed")
	}
	return j.withRetirement(ctx, installation, key, workflow, cleanupWorkflow, func(r *preparationRecord) error {
		c := r.Retirement.Captures[role]
		c.Fenced = true
		r.Retirement.Captures[role] = c
		return nil
	})
}

func (j *preparationJournal) releaseRetiringCapturePins(ctx context.Context, installation, key, workflow, cleanupWorkflow, role string, files preparationRetirementFiles) (preparationRecord, error) {
	r, err := j.retirementRecord(ctx, installation, key, workflow, cleanupWorkflow)
	if err != nil {
		return preparationRecord{}, err
	}
	c, found := r.Retirement.Captures[role]
	if !found || !c.Fenced || files == nil {
		return preparationRecord{}, errors.New("source reference release requires its producer fence")
	}
	if c.PinsReleased {
		return r, nil
	}
	// Verification cannot pin before recordReceipt commits. Retirement rejects
	// all late receipt/verification callbacks, so an absent receipt means no
	// consumer could have been admitted. A present receipt closes that exact
	// consumer permanently, including any verifier still in flight.
	if receipt := r.Captures[role].Receipt; receipt != nil {
		capture, err := newSourceCapture(r.Scope.Captures[role])
		if err != nil {
			return preparationRecord{}, err
		}
		ref, err := capture.reference(*receipt)
		if err != nil {
			return preparationRecord{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err = files.ReleaseRunFileReference(ctx, ref); err != nil {
			return preparationRecord{}, errors.New("source reference release remains unconfirmed")
		}
	}
	return j.withRetirement(ctx, installation, key, workflow, cleanupWorkflow, func(r *preparationRecord) error {
		c := r.Retirement.Captures[role]
		c.PinsReleased = true
		r.Retirement.Captures[role] = c
		return nil
	})
}

func (j *preparationJournal) nextRetiringArtifacts(ctx context.Context, installation, key, workflow, cleanupWorkflow, role string, files preparationRetirementFiles) (preparationRecord, error) {
	r, err := j.retirementRecord(ctx, installation, key, workflow, cleanupWorkflow)
	if err != nil {
		return preparationRecord{}, err
	}
	previous, found := r.Retirement.Captures[role]
	if !found || !previous.PinsReleased || files == nil {
		return preparationRecord{}, errors.New("artifact inventory requires a released scoped consumer")
	}
	if previous.Complete {
		return r, nil
	}
	cursor := previous.Cursor
	for _, artifact := range previous.Page {
		if artifact.ReceiptDigest == "" {
			// Recover a lost page-admission response on a fresh host. Never
			// advance past an unconfirmed intent or require in-memory progress.
			return r, nil
		}
		cursor = artifact.IntentID
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ids, err := files.ReadFencedRunFileIntents(ctx, retirementArtifactScope(r.Scope.Captures[role]), cursor)
	if err != nil {
		return preparationRecord{}, errors.New("artifact inventory remains unconfirmed")
	}
	if len(ids) > 128 || !pl.ValidArtifactIntentIDs(ids) {
		return preparationRecord{}, errors.New("artifact inventory exceeds its contract")
	}
	page := make([]preparationArtifactRetirement, 0, len(ids))
	last := cursor
	for _, id := range ids {
		if id <= last {
			return preparationRecord{}, errors.New("artifact inventory changed its cursor order")
		}
		page = append(page, preparationArtifactRetirement{IntentID: id})
		last = id
	}
	return j.withRetirement(ctx, installation, key, workflow, cleanupWorkflow, func(r *preparationRecord) error {
		if !sameJSON(r.Retirement.Captures[role], previous) {
			return errChanged
		}
		c := previous
		c.Cursor, c.Page, c.Complete = cursor, page, len(page) == 0
		r.Retirement.Captures[role] = c
		return nil
	})
}

func (j *preparationJournal) retireCaptureArtifact(ctx context.Context, installation, key, workflow, cleanupWorkflow, role, intentID string, files preparationRetirementFiles) (preparationRecord, error) {
	r, err := j.retirementRecord(ctx, installation, key, workflow, cleanupWorkflow)
	if err != nil {
		return preparationRecord{}, err
	}
	c, found := r.Retirement.Captures[role]
	if !found || !c.PinsReleased || files == nil {
		return preparationRecord{}, errors.New("artifact retirement requires its scoped consumer release")
	}
	index := -1
	for n, artifact := range c.Page {
		if artifact.IntentID == intentID {
			index = n
		}
	}
	if index < 0 {
		return preparationRecord{}, errors.New("artifact is outside the recorded retirement page")
	}
	if c.Page[index].ReceiptDigest != "" {
		return r, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	receipt, err := files.RetireRunFile(ctx, retirementArtifactScope(r.Scope.Captures[role]), intentID)
	if err != nil {
		return preparationRecord{}, errors.New("artifact provider retirement remains unconfirmed")
	}
	if receipt.IntentID != intentID || receipt.FileID == "" || receipt.TombstoneETag == "" {
		return preparationRecord{}, errors.New("artifact retirement returned a disagreeing receipt")
	}
	digest := preparationReceiptDigest(receipt)
	return j.withRetirement(ctx, installation, key, workflow, cleanupWorkflow, func(r *preparationRecord) error {
		current := r.Retirement.Captures[role]
		if current.Cursor != c.Cursor || len(current.Page) != len(c.Page) || current.Page[index].IntentID != intentID {
			return errChanged
		}
		if old := current.Page[index].ReceiptDigest; old != "" && old != digest {
			return errors.New("artifact retirement receipt changed")
		}
		current.Page[index].ReceiptDigest = digest
		r.Retirement.Captures[role] = current
		return nil
	})
}

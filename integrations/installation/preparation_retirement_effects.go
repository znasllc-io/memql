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
	pipelinesteps.LibraryArtifactPresentationRetirement
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
	if err = executor.Cancel(ctx, preparationSourceRun(r.Scope.InstallationID, r.Scope.RequestID, r.Scope.RequestedBy)); err != nil {
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
	spec, err := preparationCaptureSpec(r, role)
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
	if err = files.FenceRunFileScope(ctx, retirementArtifactScope(spec)); err != nil {
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
	spec, err := preparationCaptureSpec(r, role)
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
		capture, err := newSourceCapture(spec)
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
	spec, err := preparationCaptureSpec(r, role)
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
		if !artifact.settled() {
			// Recover a lost page-admission response on a fresh host. Never
			// advance past an unconfirmed retirement or Library removal, or
			// require in-memory progress: an intent behind the cursor is never
			// read again, so a retired file left in the Library there would
			// stay as an entry pointing at a zero-byte tombstone. A kept file
			// is settled: its bytes stay with its entries.
			return r, nil
		}
		cursor = artifact.IntentID
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ids, err := files.ReadFencedRunFileIntents(ctx, retirementArtifactScope(spec), cursor)
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
	spec, err := preparationCaptureSpec(r, role)
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
	// A recorded answer is final for this cleanup: a kept file is not asked
	// about again, even if its owner later stops keeping it.
	if c.Page[index].ReceiptDigest != "" || c.Page[index].Retained {
		return r, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	receipt, err := files.RetireRunFile(ctx, retirementArtifactScope(spec), intentID)
	if errors.Is(err, pipelinesteps.ErrArtifactRetained) {
		// The store refused before any storage effect because the Library keeps
		// the file (or a durable consumer pins it; a capture's own consumer was
		// released before this page). That is an answer, not an uncertain
		// effect, as it is for the pipeline sweep, which skips the file but asks
		// again on its next visit; this cleanup does not ask again. Record it so
		// the page, and completion, can go on. Every other error, including a
		// lost reply to this refusal, stays unconfirmed and keeps the page.
		return j.withRetirement(ctx, installation, key, workflow, cleanupWorkflow, func(r *preparationRecord) error {
			current := r.Retirement.Captures[role]
			if current.Cursor != c.Cursor || len(current.Page) != len(c.Page) || current.Page[index].IntentID != intentID {
				return errChanged
			}
			// Another pass may have retired the file after its owner stopped
			// keeping it, and recorded that first: the receipt stands.
			if current.Page[index].ReceiptDigest == "" {
				current.Page[index].Retained = true
			}
			r.Retirement.Captures[role] = current
			return nil
		})
	}
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
		// A racing pass may have recorded the file as kept before its owner
		// stopped keeping it. While the page still holds the entry, the receipt
		// outranks that answer: the bytes are gone, so the entry must still
		// leave the Library. Once a kept answer let the page move on, this
		// write is refused (errChanged) instead: the retired file's Library
		// entries are left to the pipeline sweep, and a retirement that did not
		// finish stays retiring with nothing to finish it. Known limit; closing
		// it needs cleanup passes serialized per preparation.
		current.Page[index].ReceiptDigest, current.Page[index].Retained = digest, false
		r.Retirement.Captures[role] = current
		return nil
	})
}

// purgeRetiredCaptureArtifact removes one retired artifact's Library entries,
// the last of fence, retire, then purge. Its authority is this journal's own
// retirement receipt for the intent, never the store's tombstone alone: a
// file another path retired is not proof that this cleanup reached it, and a
// file the Library keeps has no receipt, so its entries are never removed. The
// Library store rechecks the permanent storage fence and deletes only the
// unchanged generated rows; a row already gone is an idempotent success, so a
// fresh host repeats a removal whose reply was lost rather than skipping it.
//
// Until this step existed (found on production 2026-11-08), cancelling a
// preparation retired the bytes and left their Library rows, and the pipeline
// artifact sweep, which can resolve no window for this producer, claimed it on
// every rotation and failed. The sweep now removes only the Library entries of
// files a preparation's cleanup already retired, which reaches preparations
// cancelled before this step (claimArtifactRetentionScan, component/pipelinerun).
func (j *preparationJournal) purgeRetiredCaptureArtifact(ctx context.Context, installation, key, workflow, cleanupWorkflow, role, intentID string, files preparationRetirementFiles) (preparationRecord, error) {
	r, err := j.retirementRecord(ctx, installation, key, workflow, cleanupWorkflow)
	if err != nil {
		return preparationRecord{}, err
	}
	spec, err := preparationCaptureSpec(r, role)
	if err != nil {
		return preparationRecord{}, err
	}
	c, found := r.Retirement.Captures[role]
	if !found || !c.PinsReleased || files == nil {
		return preparationRecord{}, errors.New("artifact Library cleanup requires its scoped consumer release")
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
	if c.Page[index].ReceiptDigest == "" {
		return preparationRecord{}, errors.New("artifact Library cleanup requires its recorded retirement receipt")
	}
	if c.Page[index].Purged {
		return r, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := files.PurgeRetiredRunFile(ctx, retirementArtifactScope(spec), intentID); err != nil {
		return preparationRecord{}, errors.New("artifact Library cleanup remains unconfirmed")
	}
	return j.withRetirement(ctx, installation, key, workflow, cleanupWorkflow, func(r *preparationRecord) error {
		current := r.Retirement.Captures[role]
		if current.Cursor != c.Cursor || len(current.Page) != len(c.Page) || current.Page[index].IntentID != intentID ||
			current.Page[index].ReceiptDigest != c.Page[index].ReceiptDigest {
			return errChanged
		}
		current.Page[index].Purged = true
		r.Retirement.Captures[role] = current
		return nil
	})
}

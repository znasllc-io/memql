package installation

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

func (j *preparationJournal) captureRecord(ctx context.Context, installation, key, workflow, role string) (preparationRecord, sourceCapture, error) {
	r, err := j.get(ctx, installation, key)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	actor, err := preparationActor(ctx)
	if err != nil || actor != r.Scope.RequestedBy || workflow != r.Scope.WorkflowDigest || r.State != "preparing" {
		return preparationRecord{}, sourceCapture{}, errors.New("preparation effect authority or workflow changed")
	}
	spec, found := r.Scope.Captures[role]
	if !found {
		return preparationRecord{}, sourceCapture{}, errors.New("source role is outside this preparation")
	}
	capture, err := newSourceCapture(spec)
	return r, capture, err
}

// Each method performs one bounded operation selected by DSL. Dispatch intent
// and result persistence are mandatory mechanics beneath that operation, not
// workflow choices. A changed host never dispatches an already-started scope.
func (j *preparationJournal) capture(ctx context.Context, installation, key, workflow, role string, executor sourceCaptureExecutor, pullCredential string) (preparationRecord, error) {
	r, capture, err := j.captureRecord(ctx, installation, key, workflow, role)
	if err != nil {
		return preparationRecord{}, err
	}
	if r.Captures[role].Receipt != nil {
		return r, nil
	}
	if executor == nil || (capture.request.Step.ImagePullSecret == "") != (pullCredential == "") {
		return preparationRecord{}, errors.New("source capture executor or credential is unavailable")
	}
	committed, recoverOnly, err := j.beginCapture(ctx, installation, key, workflow, role)
	if err != nil {
		return preparationRecord{}, err
	}
	if committed.Captures[role].Receipt != nil {
		return committed, nil
	}
	receipt, err := capture.run(ctx, executor, recoverOnly, pullCredential)
	if err != nil {
		return preparationRecord{}, err
	}
	return j.recordReceipt(ctx, installation, key, workflow, role, receipt)
}

func preparationReceiptDigest(value any) string {
	body, _ := json.Marshal(value)
	return "memql-id:" + string(id.NewUntracked().FromString("installation-preparation-artifact-v1:"+string(body)))
}

func (j *preparationJournal) recordVerified(ctx context.Context, installation, key, workflow, role string, proof verifiedSourceCapture) (preparationRecord, error) {
	return j.withRecord(ctx, installation, key, workflow, func(r *preparationRecord) error {
		entry, found := r.Captures[role]
		if !found || !entry.Started || entry.Receipt == nil {
			return errors.New("source verification requires its durable capture receipt")
		}
		capture, err := newSourceCapture(r.Scope.Captures[role])
		if err != nil {
			return err
		}
		if proof.scope != capture.digest || proof.receipt.IntentID != entry.Receipt.IntentID || !internalDigest.MatchString(proof.source.Digest()) || !sameJSON(proof.source.Spec(), capture.render) {
			return errors.New("verified source belongs to another preparation capture")
		}
		digest := preparationReceiptDigest(proof.receipt)
		if entry.SourceDigest != "" && (entry.SourceDigest != proof.source.Digest() || entry.ReceiptDigest != digest) {
			return errors.New("recorded private source evidence changed")
		}
		entry.SourceDigest, entry.ReceiptDigest = proof.source.Digest(), digest
		r.Captures[role] = entry
		return nil
	})
}

// Always reread/pin/open on a fresh native verifier, even if an earlier replica
// saved these digests. A persisted digest cannot manufacture a ClosedSource.
func (j *preparationJournal) verifyCapture(ctx context.Context, installation, key, workflow, role string, files sourceCaptureFiles) (verifiedSourceCapture, error) {
	r, capture, err := j.captureRecord(ctx, installation, key, workflow, role)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	entry := r.Captures[role]
	if entry.Receipt == nil {
		return verifiedSourceCapture{}, errors.New("source verification requires its durable capture receipt")
	}
	proof, err := capture.verify(ctx, files, *entry.Receipt)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	if _, err = j.recordVerified(ctx, installation, key, workflow, role, proof); err != nil {
		return verifiedSourceCapture{}, err
	}
	return proof, nil
}

// No Job acknowledgement is possible before both the result and verified
// private artifact binding are committed. Lost acknowledgements are retried as
// cleanup of the exact recorded Job, never as source execution.
func (j *preparationJournal) acknowledgeCapture(ctx context.Context, installation, key, workflow, role string, executor sourceCaptureExecutor) (preparationRecord, error) {
	r, capture, err := j.captureRecord(ctx, installation, key, workflow, role)
	if err != nil {
		return preparationRecord{}, err
	}
	entry := r.Captures[role]
	if entry.Receipt == nil || entry.SourceDigest == "" {
		return preparationRecord{}, errors.New("source acknowledgement requires durable verified evidence")
	}
	if entry.Acknowledged {
		return r, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = capture.acknowledge(ctx, executor, *entry.Receipt); err != nil {
		return preparationRecord{}, err
	}
	return j.withRecord(ctx, installation, key, workflow, func(current *preparationRecord) error {
		stored := current.Captures[role]
		if stored.Receipt == nil || *stored.Receipt != *entry.Receipt || stored.SourceDigest != entry.SourceDigest || stored.ReceiptDigest != entry.ReceiptDigest {
			return errChanged
		}
		stored.Acknowledged = true
		current.Captures[role] = stored
		return nil
	})
}

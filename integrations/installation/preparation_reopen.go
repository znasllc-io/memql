package installation

import (
	"context"
	"errors"
	"github.com/znasllc-io/memql/integrations/argocd"
	"time"
)

// reopenPromotedCapture obtains a new native closure proof for an existing
// retained source. It cannot dispatch a producer, acknowledge a Job, update the
// preparing journal or restore a released reference. The current installation
// head and immutable preparation/plan relationship fence both sides of the
// external read; cancellation or replacement discards the resulting proof.
func (j *preparationJournal) reopenPromotedCapture(ctx context.Context, installation, preparation, plan, role string, files sourceCaptureFiles) (verifiedSourceCapture, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	record, capture, err := j.promotedCapture(ctx, installation, preparation, plan, role)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	entry := record.Captures[role]
	proof, err := reopenSourceGraph(ctx, record, role, files)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	if proof.scope != capture.digest || proof.source.Digest() != entry.SourceDigest ||
		preparationReceiptDigest(proof.receipt) != entry.ReceiptDigest ||
		!sameJSON(proof.source.Spec(), capture.render) {
		return verifiedSourceCapture{}, errors.New("retained source differs from its promoted native binding")
	}
	fresh, _, err := j.promotedCapture(ctx, installation, preparation, plan, role)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	if !sameJSON(record, fresh) {
		return verifiedSourceCapture{}, errors.New("promoted source journal changed during verification")
	}
	if err := ctx.Err(); err != nil {
		return verifiedSourceCapture{}, err
	}
	return proof, nil
}

func (j *preparationJournal) promotedCapture(ctx context.Context, installation, preparation, plan, role string) (preparationRecord, sourceCapture, error) {
	actor, err := preparationActor(ctx)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	if !identifier.MatchString(installation) || !internalDigest.MatchString(preparation) || !internalDigest.MatchString(plan) || !captureStepKey.MatchString(role) {
		return preparationRecord{}, sourceCapture{}, errors.New("promoted source requires exact native installation identities and role")
	}
	db, err := j.database()
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	defer tx.Rollback()
	active, preparing, epoch, err := lockPreparationHead(ctx, tx, installation)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	if active != plan || preparing != "" {
		return preparationRecord{}, sourceCapture{}, errChanged
	}
	r, err := readPreparation(ctx, tx, installation, preparation)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	if r.State != "promoted" || r.PromotedPlanID != plan || actor != r.Scope.RequestedBy {
		return preparationRecord{}, sourceCapture{}, errors.New("retained source is outside the original requester's promoted preparation")
	}
	parent, err := promotedRevision(ctx, tx, r)
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	if parent.ID != active || parent.SlotEpoch != epoch || (parent.State != "prepared" && parent.State != "applying" && parent.State != "reverting") {
		return preparationRecord{}, sourceCapture{}, errChanged
	}
	_, root := r.Scope.Captures[role]
	entry, found := r.Captures[role]
	if !root || !found || !entry.Acknowledged || entry.Receipt == nil || !internalDigest.MatchString(entry.SourceDigest) || !internalDigest.MatchString(entry.ReceiptDigest) {
		return preparationRecord{}, sourceCapture{}, errors.New("promoted source has no exact retained receipt")
	}
	capture, err := newSourceCapture(r.Scope.Captures[role])
	if err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	if err := tx.Commit(); err != nil {
		return preparationRecord{}, sourceCapture{}, err
	}
	return r, capture, nil
}

// This read-only path is deliberately independent of the preparation writer.
// It can recover a graph after promotion on a replica with no local fragments.
func reopenSourceGraph(ctx context.Context, r preparationRecord, root string, files sourceCaptureFiles) (verifiedSourceCapture, error) {
	spec, err := preparationCaptureSpec(r, root)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	capture, err := newSourceCapture(spec)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	entry := r.Captures[root]
	if entry.Receipt == nil {
		return verifiedSourceCapture{}, errors.New("retained graph has no root receipt")
	}
	proof, err := capture.verify(ctx, files, *entry.Receipt)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	if proof.fragment.Digest() == "" {
		return proof, nil
	}
	if proof.fragment.Digest() != entry.FragmentDigest || preparationReceiptDigest(proof.receipt) != entry.ReceiptDigest {
		return verifiedSourceCapture{}, errors.New("retained root fragment changed")
	}
	dependencies := []argocd.SourceFragment{}
	expected := map[string]bool{}
	for _, dependency := range proof.fragment.Dependencies() {
		key := dependencyCaptureKey(root, dependency)
		expected[key] = true
		retained, found := r.Captures[key]
		if !found || retained.Parent != root || retained.Receipt == nil || !retained.Acknowledged {
			return verifiedSourceCapture{}, errors.New("retained graph has an unfinished dependency")
		}
		spec, err := preparationCaptureSpec(r, key)
		if err != nil {
			return verifiedSourceCapture{}, err
		}
		capture, err := newSourceCapture(spec)
		if err != nil {
			return verifiedSourceCapture{}, err
		}
		fragment, err := capture.verify(ctx, files, *retained.Receipt)
		if err != nil {
			return verifiedSourceCapture{}, err
		}
		if fragment.fragment.Digest() == "" || fragment.fragment.Digest() != retained.FragmentDigest || preparationReceiptDigest(fragment.receipt) != retained.ReceiptDigest {
			return verifiedSourceCapture{}, errors.New("retained dependency fragment changed")
		}
		dependencies = append(dependencies, fragment.fragment)
	}
	for key, entry := range r.Captures {
		if entry.Parent == root && !expected[key] {
			return verifiedSourceCapture{}, errors.New("retained graph has an extra dependency")
		}
	}
	closed, err := argocd.AssembleSourceClosure(ctx, proof.fragment, dependencies)
	if err != nil {
		return verifiedSourceCapture{}, err
	}
	proof.source, proof.fragment = closed, argocd.SourceFragment{}
	return proof, nil
}

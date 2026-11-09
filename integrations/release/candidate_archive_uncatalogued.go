package release

import (
	"context"
	"crypto/ed25519"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// candidateSupersessionProof is what releaseArchiveWithoutCatalog records in
// place of catalog evidence: every declared destination's outcome, a fresh read
// of each copy the candidate actually published, and the catalogued release
// that supersedes each component. Like archival's proof it is minted only by
// native read-only verification; no request or editable row supplies one.
type candidateSupersessionProof struct {
	CandidateID  string                             `json:"candidateId"`
	ApprovalID   string                             `json:"approvalId"`
	ObservedAt   time.Time                          `json:"observedAt"`
	Destinations []candidateUncataloguedDestination `json:"destinations"`
	Targets      []candidateArchiveTarget           `json:"targets"`
	SupersededBy map[string]string                  `json:"supersededBy"`
}

// verifyUncataloguedInputs reads back every destination the snapshot recorded
// as verified, and nothing else: a destination whose publication never
// completed has no canonical copy to read, so it is never read and needs no
// credential. The owner's call resolves the destinations (archivalTargets) only
// at the first copy to read, and a failure to resolve them refuses the whole
// operation there. That resolution is archival's: a legacy candidate's
// configuration is matched only for the destinations read, while a
// profile-bound candidate's profile must then still authorize every
// destination it selected, read or not.
func (p *candidatePreparer) verifyUncataloguedInputs(ctx context.Context, snapshot candidateUncataloguedSnapshot) (candidateSupersessionProof, error) {
	record := snapshot.record
	proof := candidateSupersessionProof{CandidateID: record.ID, ApprovalID: record.ApprovalID, ObservedAt: snapshot.observedAt,
		Destinations: slices.Clone(snapshot.destinations), Targets: []candidateArchiveTarget{}, SupersededBy: maps.Clone(snapshot.supersededBy)}
	checkedFiles := map[string]bool{}
	for _, outcome := range snapshot.destinations {
		if outcome.Outcome != uncataloguedVerified {
			continue
		}
		if p != nil && p.targetReader == nil && p.archivalTargets != nil {
			reader, err := p.archivalTargets(ctx)
			if err != nil {
				return proof, err
			}
			p.targetReader = reader
		}
		if p == nil || p.targetReader == nil {
			return proof, errors.New("published copy verification is unavailable")
		}
		index := slices.IndexFunc(record.Manifest.Destinations, func(d pl.ReleaseDestination) bool {
			return d.TargetID == outcome.TargetID && d.Component == outcome.Component && d.Artifact == outcome.Artifact
		})
		if index < 0 {
			return proof, errors.New("recorded destination is outside the approved candidate")
		}
		var bound *candidateDraftRecord
		if release, ok := snapshot.bound[outcome.TargetID]; ok {
			bound = &release
		}
		target, err := p.verifyPublishedDestination(ctx, record, record.Manifest.Destinations[index], bound, checkedFiles)
		if err != nil {
			return proof, err
		}
		proof.Targets = append(proof.Targets, target)
	}
	return proof, nil
}

// archiveWithoutCatalog is the owner's terminal path for an approved candidate
// that has no signed catalog. Like archive, it commits the permanent fence
// before releasing anything: if the reply or this process is lost, any replica
// rerunning it finds the candidate archived on the supersession basis and
// resumes releasing the same references without reading a provider again, and
// storage tombstones each reference so a delayed preparer cannot pin it again.
// It releases duplicate or abandoned build inputs only, never a published
// download, image, tag, approval, catalog or publication receipt.
func (p *candidatePreparer) archiveWithoutCatalog(ctx context.Context, key, publisher string, trust map[string]ed25519.PublicKey) (candidateRecord, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return candidateRecord{}, err
	}
	if p == nil || p.ledger == nil || p.library == nil {
		return candidateRecord{}, errors.New("archiving without a catalog is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	snapshot, err := p.ledger.uncataloguedSnapshot(ctx, key, publisher, trust)
	if err != nil {
		return candidateRecord{}, err
	}
	record := snapshot.record
	if record.State != "archived" {
		proof, err := p.verifyUncataloguedInputs(ctx, snapshot)
		if err != nil {
			return candidateRecord{}, err
		}
		if record, err = p.ledger.archiveWithoutCatalog(ctx, snapshot, proof, publisher, trust); err != nil {
			return candidateRecord{}, err
		}
	}
	var releaseErr error
	for _, ref := range candidateArtifactReferences(record.Manifest, record.ID) {
		releaseErr = errors.Join(releaseErr, p.library.ReleaseRunFileReference(auth.ContextWithInternalOrigin(ctx), ref))
	}
	if releaseErr == nil {
		releaseErr = p.ledger.recordArchiveInputsReleased(ctx, record.ID)
	}
	return record, releaseErr
}

// The preparer starts with the ledger and library alone. The destinations a
// published copy is read through are resolved as archival resolves them,
// through the candidate's release profile or the current configuration
// (archivalCandidateFor), and only when verification reaches a copy to read. A
// candidate that published nothing needs neither, so a removed profile or
// configuration cannot keep its inputs retained.
// TestArchiveWithoutCatalogResolvesDestinationsOnlyToReadACopy holds both sides
// for a candidate of the legacy configuration; no test yet drives a
// profile-bound candidate through this handler.
func (i *Integration) handleCandidateArchiveWithoutCatalog(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	key := asString(args["candidateId"])
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	p.archivalTargets = func(ctx context.Context) (*candidateTargetReader, error) {
		archival, err := i.archivalCandidateFor(ctx, key)
		if err != nil {
			return nil, err
		}
		return archival.targetReader, nil
	}
	_, config, trust, err := i.catalog(ctx)
	if err != nil {
		return nil, err
	}
	record, err := p.archiveWithoutCatalog(ctx, key, config.Publisher, trust)
	if err != nil {
		return nil, err
	}
	return resultNode("candidate", record.ID, record)
}

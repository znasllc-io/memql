package release

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/npmregistry"
	"github.com/znasllc-io/memql/integrations/ociregistry"
)

// Archival is a terminal resource lifecycle operation, not version selection.
// The installed DSL chooses which history to archive and when. This operation
// can release only duplicate build inputs, never canonical published assets,
// images, tags, signatures, or publication receipts.
func (p *candidatePreparer) archive(ctx context.Context, key, publisher string, trust map[string]ed25519.PublicKey) (candidateRecord, error) {
	if _, err := candidateOwner(ctx); err != nil {
		return candidateRecord{}, err
	}
	if p == nil || p.ledger == nil || p.library == nil || p.targetReader == nil {
		return candidateRecord{}, errors.New("published input archival is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	snapshot, err := p.ledger.archiveSnapshot(ctx, key, publisher, trust)
	if err != nil {
		return candidateRecord{}, err
	}
	record := snapshot.record
	if record.State != "archived" {
		proof, err := p.verifyPublishedInputs(ctx, snapshot)
		if err != nil {
			return candidateRecord{}, err
		}
		record, err = p.ledger.archive(ctx, snapshot, proof, publisher, trust)
		if err != nil {
			return candidateRecord{}, err
		}
	}
	// Commit the permanent fence first. If the response or this process is
	// lost, another replica resumes releasing the same references. Storage
	// tombstones each reference, so a delayed preparer cannot pin it again.
	var releaseErr error
	for _, ref := range candidateArtifactReferences(record.Manifest, record.ID) {
		releaseErr = errors.Join(releaseErr, p.library.ReleaseRunFileReference(auth.ContextWithInternalOrigin(ctx), ref))
	}
	if releaseErr == nil {
		releaseErr = p.ledger.recordArchiveInputsReleased(ctx, record.ID)
	}
	return record, releaseErr
}

func (p *candidatePreparer) verifyPublishedInputs(ctx context.Context, snapshot candidateArchiveSnapshot) (candidateArchiveProof, error) {
	record := snapshot.record
	proof := candidateArchiveProof{CandidateID: record.ID, ApprovalID: record.ApprovalID, CatalogDigest: snapshot.digest,
		ObservedAt: snapshot.observedAt, Targets: []candidateArchiveTarget{}}
	drafts, err := p.ledger.drafts(ctx, record.ID)
	if err != nil {
		return proof, err
	}
	checkedFiles := map[string]bool{}
	for _, destination := range record.Manifest.Destinations {
		var bound *candidateDraftRecord
		if p.targetReader.files[destination.TargetID].ID != "" {
			for j := range drafts {
				if slices.Contains(drafts[j].Targets, destination.TargetID) {
					if bound != nil {
						return proof, errors.New("published release has ambiguous native identity")
					}
					bound = &drafts[j]
				}
			}
		}
		target, err := p.verifyPublishedDestination(ctx, record, destination, bound, checkedFiles)
		if err != nil {
			return proof, err
		}
		proof.Targets = append(proof.Targets, target)
	}
	return proof, nil
}

// verifyPublishedDestination freshly reads one destination's canonical copy
// through the destination the current profile or configuration still authorizes
// at its approved digest: an OCI image by digest, a whole published release by
// bytes (once per release, recorded in checkedFiles), or an npm tarball. bound
// is the published release record a file destination belongs to; archival finds
// it among the candidate's own drafts, and archiving without a catalog may find
// it under a renewal successor. A destination the current configuration no
// longer declares refuses, so credentials are only ever used where the operator
// still authorizes them.
func (p *candidatePreparer) verifyPublishedDestination(ctx context.Context, record candidateRecord, destination pl.ReleaseDestination, bound *candidateDraftRecord, checkedFiles map[string]bool) (candidateArchiveTarget, error) {
	publication, err := candidatePublicationFor(record, destination.TargetID, destination.Component, destination.Artifact)
	if err != nil {
		return candidateArchiveTarget{}, err
	}
	a := publication.Artifact
	switch {
	case p.targetReader.targets[destination.TargetID].ID != "":
		target, err := p.targetReader.authenticatedRegistry(ctx, destination)
		if err != nil {
			return candidateArchiveTarget{}, err
		}
		verifier, err := ociregistry.NewAvailabilityVerifier(target)
		if err != nil {
			return candidateArchiveTarget{}, err
		}
		available, err := verifier.Check(ctx, a.ImageDigest, a.Platform, ociregistry.AvailabilityLimits{})
		if err != nil {
			return candidateArchiveTarget{}, err
		}
		receipt, err := available.Receipt()
		if err != nil || receipt.ImageDigest != a.ImageDigest || receipt.Platform != a.Platform {
			return candidateArchiveTarget{}, errors.New("published image availability differs from the sealed artifact")
		}
	case p.targetReader.files[destination.TargetID].ID != "":
		plan, err := p.targetReader.draftPlan(ctx, record, destination.TargetID)
		if err != nil {
			return candidateArchiveTarget{}, err
		}
		if !checkedFiles[plan.ResourceKey] {
			if bound == nil || bound.State != "published" || bound.Receipt == nil || plan.Target.Draft == nil {
				return candidateArchiveTarget{}, errors.New("published release requires its complete native lifecycle receipt")
			}
			client, err := p.targetReader.fileLifecycle(ctx, destination)
			if err != nil {
				return candidateArchiveTarget{}, err
			}
			fresh, err := client.ObservePublished(ctx, bound.ReleaseID, *plan.Target.Draft, plan.marker(*bound), plan.expected())
			if err != nil {
				return candidateArchiveTarget{}, err
			}
			if fresh.ReleaseID != bound.ReleaseID || fresh.Draft || !slices.Equal(fresh.Assets, bound.Receipt.Assets) {
				return candidateArchiveTarget{}, errors.New("published release inventory changed since native publication")
			}
			checkedFiles[plan.ResourceKey] = true
		}
	case p.targetReader.npm[destination.TargetID].ID != "":
		target, _, err := p.targetReader.resolveNPM(ctx, destination)
		if err != nil {
			return candidateArchiveTarget{}, err
		}
		client, err := p.targetReader.npmPublisher(ctx, destination)
		if err != nil {
			return candidateArchiveTarget{}, err
		}
		_, err = client.Observe(ctx, npmregistry.Expected{Name: target.PackageName, Version: target.Version, SHA256: a.Digest, Size: a.Size})
		if err != nil {
			return candidateArchiveTarget{}, err
		}
	default:
		return candidateArchiveTarget{}, errors.New("archive requires the current configured destination for every published artifact")
	}
	return candidateArchiveTarget{TargetID: destination.TargetID, Digest: a.Digest, ImageDigest: a.ImageDigest, Platform: a.Platform, Size: a.Size}, nil
}

// archivalCandidateFor builds the preparer archival verifies with. configuredCandidateFor
// keeps every new effect on the engine and resolver that prepared it, and the deployment
// a release exists for moves that engine, normally long before the 48 hours archival
// waits, so it refused every bound candidate that shipped. Archival selects nothing and
// publishes nothing. It asks only for what verifyPublishedInputs reads: destinations the
// current profile still authorizes, which resolve to the digests the candidate was
// approved with. The binding it takes them from was verified when it was read.
func (i *Integration) archivalCandidateFor(ctx context.Context, key string) (*candidatePreparer, error) {
	p, err := i.candidateStorage(ctx)
	if err != nil {
		return nil, err
	}
	b, err := p.ledger.profileBinding(ctx, key, true)
	if errors.Is(err, sql.ErrNoRows) {
		return i.configuredCandidate(ctx)
	}
	if err != nil {
		return nil, err
	}
	profiles, err := i.candidateProfiles(ctx)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(profiles, func(profile candidateProfile) bool { return profile.Name == b.ProfileName })
	if index < 0 {
		return nil, errors.New("release profile is absent or no longer authorized")
	}
	if p.targetReader, err = i.profileTargets(profiles[index], b); err != nil {
		return nil, err
	}
	return p, nil
}

func (i *Integration) handleCandidateArchive(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	p, err := i.archivalCandidateFor(ctx, asString(args["candidateId"]))
	if err != nil {
		return nil, err
	}
	_, config, trust, err := i.catalog(ctx)
	if err != nil {
		return nil, err
	}
	record, err := p.archive(ctx, asString(args["candidateId"]), config.Publisher, trust)
	if err != nil {
		return nil, err
	}
	return resultNode("candidate", record.ID, record)
}

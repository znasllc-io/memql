package release

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

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
// completed has no canonical copy to read, so it is never resolved against the
// current configuration and needs no credential.
func (p *candidatePreparer) verifyUncataloguedInputs(ctx context.Context, snapshot candidateUncataloguedSnapshot) (candidateSupersessionProof, error) {
	record := snapshot.record
	proof := candidateSupersessionProof{CandidateID: record.ID, ApprovalID: record.ApprovalID, ObservedAt: snapshot.observedAt,
		Destinations: slices.Clone(snapshot.destinations), Targets: []candidateArchiveTarget{}, SupersededBy: maps.Clone(snapshot.supersededBy)}
	checkedFiles := map[string]bool{}
	for _, outcome := range snapshot.destinations {
		if outcome.Outcome != uncataloguedVerified {
			continue
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

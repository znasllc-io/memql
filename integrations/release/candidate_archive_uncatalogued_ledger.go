package release

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// An approved candidate that never had a signed catalog can neither retire
// (it has publication intents) nor archive (archival verifies against the
// catalog), so its retention pins would last forever. releaseArchiveWithoutCatalog
// is the terminal path for it, chosen by the owner on 2026-11-08 for six October
// candidates and for every future renewal predecessor and abandoned attempt:
// supersession by a catalogued release of every component (supersedingCatalogs),
// fresh verification of every copy the candidate actually published, and nothing
// still running. docs/superpowers/specs/2026-11-08-release-archive-without-catalog-design.md
// records the decision; release-candidates.md describes the operation.

// uncataloguedQuietPeriod is how long a pending publication intent, or a draft
// claimed for creation or promotion, must have been unchanged before the
// candidate may archive without a catalog. The longest publication operation is
// bounded at releasePublicationOperationLimit (70 minutes) and both lifecycle
// workflows at one hour, so no writer of this candidate can still be acting.
// The fence then makes the ledger refuse any later completion, because every
// effect requires an approved candidate under the same row lock.
// TestArchiveWithoutCatalogQuietPeriodExceedsEveryPublicationBound holds it there.
const uncataloguedQuietPeriod = 2 * time.Hour

// renewalHopLimit bounds the succession chain a predecessor's release is
// followed through. Each renewal needs a fresh owner approval, so a real chain
// is short; the bound keeps a corrupted journal from looping.
const renewalHopLimit = 16

// The recorded outcome of one declared destination.
const (
	// The destination's publication completed and its canonical copy is read
	// back freshly before the fence.
	uncataloguedVerified = "verified"
	// A file was uploaded to a release that was never published; there is no
	// public copy to read.
	uncataloguedUploaded = "uploaded"
	// The publication began and never completed. The quiet period guarantees
	// its writer is gone.
	uncataloguedPending = "pending"
	// Nothing was ever attempted for the destination under this approval.
	uncataloguedNotBegun = "notBegun"
)

type candidateUncataloguedDestination struct {
	TargetID  string `json:"targetId"`
	Component string `json:"component"`
	Artifact  string `json:"artifact"`
	Outcome   string `json:"outcome"`
}

// The snapshot is a short authority transaction, never a lock held during
// network verification; the fence transaction re-derives every field and
// refuses unless they are unchanged.
type candidateUncataloguedSnapshot struct {
	record       candidateRecord
	destinations []candidateUncataloguedDestination
	// bound names, per verified file destination, the published release that
	// holds its canonical copy: this candidate's own, or a renewal successor's.
	bound        map[string]candidateDraftRecord
	supersededBy map[string]string
	observedAt   time.Time
}

func (l *candidateLedger) uncataloguedSnapshot(ctx context.Context, key, publisher string, trust map[string]ed25519.PublicKey) (candidateUncataloguedSnapshot, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidateUncataloguedSnapshot{}, err
	}
	db, err := l.database()
	if err != nil {
		return candidateUncataloguedSnapshot{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return candidateUncataloguedSnapshot{}, err
	}
	defer tx.Rollback()
	snapshot, err := readUncatalogued(ctx, tx, owner, key, publisher, trust)
	if err != nil {
		return candidateUncataloguedSnapshot{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&snapshot.observedAt); err != nil {
		return candidateUncataloguedSnapshot{}, err
	}
	return snapshot, tx.Commit()
}

// readUncatalogued reads everything the snapshot and the fence must agree on,
// under the candidate row lock the transaction takes first.
func readUncatalogued(ctx context.Context, tx *sql.Tx, owner, key, publisher string, trust map[string]ed25519.PublicKey) (candidateUncataloguedSnapshot, error) {
	rec, err := readCandidate(ctx, tx, owner, key)
	if err != nil {
		return candidateUncataloguedSnapshot{}, err
	}
	if rec.State != "approved" {
		return candidateUncataloguedSnapshot{}, errors.New("only an approved candidate can archive without a catalog")
	}
	rec, err = readCandidateApproval(ctx, tx, owner, rec)
	if err != nil {
		return candidateUncataloguedSnapshot{}, err
	}
	var catalogued bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM release_catalog WHERE candidate_id=$1)`, rec.ID).Scan(&catalogued); err != nil {
		return candidateUncataloguedSnapshot{}, err
	}
	if catalogued {
		return candidateUncataloguedSnapshot{}, errors.New("candidate has a signed catalog; archive it with releaseArchiveCandidate")
	}
	supersededBy, err := supersedingCatalogs(ctx, tx, owner, rec, publisher, trust)
	if err != nil {
		return candidateUncataloguedSnapshot{}, err
	}
	if err := requireQuietPublication(ctx, tx, owner, rec.ID); err != nil {
		return candidateUncataloguedSnapshot{}, err
	}
	destinations, bound, err := classifyUncatalogued(ctx, tx, owner, rec)
	if err != nil {
		return candidateUncataloguedSnapshot{}, err
	}
	return candidateUncataloguedSnapshot{record: rec, destinations: destinations, bound: bound, supersededBy: supersededBy}, nil
}

func requireQuietPublication(ctx context.Context, tx *sql.Tx, owner, key string) error {
	var uploading bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM release_draft_intents WHERE candidate_id=$1 AND owner_user_id=$2 AND upload_lease_until>clock_timestamp())`, key, owner).Scan(&uploading); err != nil {
		return err
	}
	if uploading {
		return errDraftUploadRunning
	}
	var recent bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM release_publication_intents WHERE candidate_id=$1 AND state='pending'
 AND updated_at>clock_timestamp()-($3::bigint*interval '1 millisecond'))
OR EXISTS(SELECT 1 FROM release_draft_intents WHERE candidate_id=$1 AND owner_user_id=$2 AND state IN ('creating','promoting')
 AND updated_at>clock_timestamp()-($3::bigint*interval '1 millisecond'))`, key, owner, uncataloguedQuietPeriod.Milliseconds()).Scan(&recent)
	if err != nil {
		return err
	}
	if recent {
		return errors.New("a publication effect of this candidate may still be running; archive it once nothing has changed for two hours")
	}
	return nil
}

// classifyUncatalogued records every declared destination's outcome, sorted by
// target, component and artifact. The snapshot and the fence both call it, so
// any change to an intent or a release between them shows as a difference.
func classifyUncatalogued(ctx context.Context, tx *sql.Tx, owner string, rec candidateRecord) ([]candidateUncataloguedDestination, map[string]candidateDraftRecord, error) {
	out := make([]candidateUncataloguedDestination, 0, len(rec.Manifest.Destinations))
	bound := map[string]candidateDraftRecord{}
	for _, d := range rec.Manifest.Destinations {
		entry, err := candidatePublicationFor(rec, d.TargetID, d.Component, d.Artifact)
		if err != nil {
			return nil, nil, err
		}
		outcome := candidateUncataloguedDestination{TargetID: d.TargetID, Component: d.Component, Artifact: d.Artifact}
		var state, approval string
		var snapshot, receipt []byte
		err = tx.QueryRowContext(ctx, `SELECT state,approval_id,target_snapshot,receipt FROM release_publication_intents WHERE candidate_id=$1 AND target_id=$2 AND component_name=$3 AND artifact_name=$4`, rec.ID, d.TargetID, d.Component, d.Artifact).Scan(&state, &approval, &snapshot, &receipt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			outcome.Outcome = uncataloguedNotBegun
		case err != nil:
			return nil, nil, err
		case approval != rec.ApprovalID:
			return nil, nil, errors.New("publication intent belongs to another approval")
		case state == "pending":
			outcome.Outcome = uncataloguedPending
		case state == "complete":
			var proof candidatePublicationReceipt
			want := candidatePublicationReceipt{TargetDigest: d.TargetDigest, ArchiveDigest: entry.Artifact.Digest, ImageDigest: entry.Artifact.ImageDigest, Platform: entry.Artifact.Platform}
			if json.Unmarshal(receipt, &proof) != nil || proof != want {
				return nil, nil, errors.New("stored publication proof disagrees with candidate")
			}
			var target candidatePublicationTarget
			if err := decodeCandidateObject(snapshot, &target); err != nil {
				return nil, nil, err
			}
			if err := target.validate(d); err != nil {
				return nil, nil, err
			}
			outcome.Outcome = uncataloguedVerified
			if target.File != nil {
				release, err := boundRelease(ctx, tx, owner, rec, entry, *target.File)
				if err != nil {
					return nil, nil, err
				}
				if release.State == "published" {
					bound[d.TargetID] = release
				} else {
					outcome.Outcome = uncataloguedUploaded
				}
			}
		default:
			return nil, nil, errors.New("publication intent has an invalid state")
		}
		out = append(out, outcome)
	}
	slices.SortFunc(out, func(a, b candidateUncataloguedDestination) int {
		return cmp.Or(cmp.Compare(a.TargetID, b.TargetID), cmp.Compare(a.Component, b.Component), cmp.Compare(a.Artifact, b.Artifact))
	})
	return out, bound, nil
}

// boundRelease reads the release a complete file publication went to. The
// release is identified from the target frozen in its intent, so no current
// configuration is consulted to learn whether it was ever published.
//
// A renewal moves the binding to the successor, keeping the release ID, the
// recovery marker and the plan: an unchanged file release has the same
// destinations and artifacts under both approvals (unchangedFileRelease), so
// the successor's stored plan is the predecessor's plan too. The row is read
// with that stored plan, whose digest readDraft checks, and is accepted only
// when the succession journal leads there and the plan covers this exact
// destination and artifact.
func boundRelease(ctx context.Context, tx *sql.Tx, owner string, rec candidateRecord, entry candidatePublication, target candidateFileTarget) (candidateDraftRecord, error) {
	var holder, approval string
	var body []byte
	err := tx.QueryRowContext(ctx, `SELECT candidate_id,approval_id,plan FROM release_draft_intents WHERE resource_key=$1 AND owner_user_id=$2`, draftResourceKey(target), owner).Scan(&holder, &approval, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return candidateDraftRecord{}, errors.New("file publication has no native release record")
	}
	if err != nil {
		return candidateDraftRecord{}, err
	}
	if holder != rec.ID {
		if err := requireRenewedInto(ctx, tx, rec.ID, holder); err != nil {
			return candidateDraftRecord{}, err
		}
	}
	var approvedBy string
	if err := tx.QueryRowContext(ctx, `SELECT approval_id,approved_by FROM release_candidate_approvals WHERE candidate_id=$1`, holder).Scan(&approval, &approvedBy); err != nil || approvedBy != owner {
		return candidateDraftRecord{}, errors.New("release record's candidate approval is inconsistent")
	}
	var plan candidateDraftPlan
	if err := decodeCandidateObject(body, &plan); err != nil {
		return candidateDraftRecord{}, err
	}
	covered := false
	for _, a := range plan.Assets {
		if a.Destination == entry.Target && a.Expected.Name == target.AssetName && a.Expected.SHA256 == entry.Artifact.Digest && a.Expected.Size == entry.Artifact.Size {
			covered = true
		}
	}
	if !covered {
		return candidateDraftRecord{}, errors.New("release record does not cover this candidate's published file")
	}
	return readDraft(ctx, tx, owner, holder, approval, plan)
}

func requireRenewedInto(ctx context.Context, tx *sql.Tx, from, to string) error {
	for range renewalHopLimit {
		var next string
		err := tx.QueryRowContext(ctx, `SELECT candidate_id FROM release_candidate_successions WHERE previous_candidate_id=$1`, from).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		if next == to {
			return nil
		}
		from = next
	}
	return errors.New("file publication's release is bound to a candidate outside this candidate's renewals")
}

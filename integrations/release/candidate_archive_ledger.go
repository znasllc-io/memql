package release

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// This proof is minted by native read-only provider verification. No request,
// editable graph row, or historical publication receipt can supply one.
type candidateArchiveProof struct {
	CandidateID   string                   `json:"candidateId"`
	ApprovalID    string                   `json:"approvalId"`
	CatalogDigest string                   `json:"catalogDigest"`
	ObservedAt    time.Time                `json:"observedAt"`
	Targets       []candidateArchiveTarget `json:"targets"`
}

type candidateArchiveTarget struct {
	TargetID    string `json:"targetId"`
	Digest      string `json:"digest"`
	ImageDigest string `json:"imageDigest,omitempty"`
	Platform    string `json:"platform"`
	Size        int64  `json:"size"`
}

type candidateArchiveSnapshot struct {
	record     candidateRecord
	release    pl.PublishedRelease
	digest     string
	observedAt time.Time
}

func candidateArchiveCatalog(ctx context.Context, tx *sql.Tx, owner string, rec candidateRecord, publisher string, trust map[string]ed25519.PublicKey) (pl.PublishedRelease, string, error) {
	var effects int
	var complete bool
	err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(bool_and(state='complete' AND approval_id=$2),false)
FROM release_publication_intents WHERE candidate_id=$1`, rec.ID, rec.ApprovalID).Scan(&effects, &complete)
	if err != nil || effects != len(rec.Manifest.Destinations) || !complete {
		return pl.PublishedRelease{}, "", errors.New("archive requires complete native receipts for exactly the declared destinations")
	}
	var unfinishedDraft bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM release_draft_intents WHERE candidate_id=$1 AND state<>'published')`, rec.ID).Scan(&unfinishedDraft); err != nil {
		return pl.PublishedRelease{}, "", err
	}
	if unfinishedDraft {
		return pl.PublishedRelease{}, "", errors.New("archive cannot release inputs of an unfinished publication")
	}
	projection, err := publicCandidate(ctx, tx, owner, rec, publisher)
	if err != nil {
		return projection, "", err
	}
	_, digest, err := pl.CanonicalPublishedRelease(projection)
	if err != nil {
		return projection, "", err
	}
	var envelope []byte
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT envelope,catalog_digest FROM release_catalog WHERE candidate_id=$1 AND publisher=$2`, rec.ID, publisher).Scan(&envelope, &stored)
	if err != nil {
		return projection, "", err
	}
	verified, err := pl.VerifyPublishedRelease(envelope, publisher, trust)
	if err != nil || stored != digest || verified.Digest() != digest {
		return projection, "", errors.New("archive requires the sealed catalog to match complete native publication evidence")
	}
	return projection, digest, nil
}

func archiveProofMatches(rec candidateRecord, digest string, proof candidateArchiveProof) bool {
	if proof.CandidateID != rec.ID || proof.ApprovalID != rec.ApprovalID || proof.CatalogDigest != digest || proof.ObservedAt.IsZero() || len(proof.Targets) != len(rec.Manifest.Destinations) {
		return false
	}
	seen := map[string]bool{}
	for _, target := range proof.Targets {
		if seen[target.TargetID] {
			return false
		}
		seen[target.TargetID] = true
		matched := false
		for _, d := range rec.Manifest.Destinations {
			if d.TargetID != target.TargetID {
				continue
			}
			publication, err := candidatePublicationFor(rec, d.TargetID, d.Component, d.Artifact)
			if err != nil {
				return false
			}
			a := publication.Artifact
			matched = target == (candidateArchiveTarget{TargetID: d.TargetID, Digest: a.Digest, ImageDigest: a.ImageDigest, Platform: a.Platform, Size: a.Size})
		}
		if !matched {
			return false
		}
	}
	return true
}

func readCandidateArchive(ctx context.Context, tx *sql.Tx, owner string, rec candidateRecord, digest string) error {
	var approval, stored string
	var verified, archived time.Time
	var body []byte
	err := tx.QueryRowContext(ctx, `SELECT approval_id,catalog_digest,verified_at,archived_at,receipt
FROM release_candidate_archives WHERE candidate_id=$1 AND owner_user_id=$2`, rec.ID, owner).Scan(&approval, &stored, &verified, &archived, &body)
	if err != nil {
		return err
	}
	var proof candidateArchiveProof
	if decodeCandidateObject(body, &proof) != nil || approval != rec.ApprovalID || stored != digest ||
		!verified.Equal(proof.ObservedAt) || archived.Before(verified) || !archiveProofMatches(rec, digest, proof) {
		return errors.New("candidate archive journal differs from its verified publication")
	}
	return nil
}

// Snapshot is a short authority transaction, never a lock held during network
// verification. The fence transaction revalidates the same sealed identity.
func (l *candidateLedger) archiveSnapshot(ctx context.Context, key, publisher string, trust map[string]ed25519.PublicKey) (candidateArchiveSnapshot, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidateArchiveSnapshot{}, err
	}
	db, err := l.database()
	if err != nil {
		return candidateArchiveSnapshot{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return candidateArchiveSnapshot{}, err
	}
	defer tx.Rollback()
	rec, err := readCandidate(ctx, tx, owner, key)
	if err != nil {
		return candidateArchiveSnapshot{}, err
	}
	if rec.State != "approved" && rec.State != "archived" {
		return candidateArchiveSnapshot{}, errors.New("only a completely published, sealed candidate can archive its build inputs")
	}
	rec, err = readCandidateApproval(ctx, tx, owner, rec)
	if err != nil {
		return candidateArchiveSnapshot{}, err
	}
	projection, digest, err := candidateArchiveCatalog(ctx, tx, owner, rec, publisher, trust)
	if err != nil {
		return candidateArchiveSnapshot{}, err
	}
	if rec.State == "archived" {
		if err := readCandidateArchive(ctx, tx, owner, rec, digest); err != nil {
			return candidateArchiveSnapshot{}, err
		}
	}
	var observedAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&observedAt); err != nil {
		return candidateArchiveSnapshot{}, err
	}
	return candidateArchiveSnapshot{record: rec, release: projection, digest: digest, observedAt: observedAt}, tx.Commit()
}

// Archive permanently fences publication under the same candidate row lock as
// beginPublication. Pending/uncertain effects, corrupt receipts, missing seals
// and stale observations all refuse before any input reference is released.
func (l *candidateLedger) archive(ctx context.Context, snapshot candidateArchiveSnapshot, proof candidateArchiveProof, publisher string, trust map[string]ed25519.PublicKey) (candidateRecord, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return candidateRecord{}, err
	}
	db, err := l.database()
	if err != nil {
		return candidateRecord{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return candidateRecord{}, err
	}
	defer tx.Rollback()
	rec, err := readCandidate(ctx, tx, owner, snapshot.record.ID)
	if err != nil {
		return candidateRecord{}, err
	}
	if rec.State != "approved" && rec.State != "archived" {
		return candidateRecord{}, errors.New("candidate is no longer available for archival")
	}
	rec, err = readCandidateApproval(ctx, tx, owner, rec)
	if err != nil {
		return candidateRecord{}, err
	}
	_, digest, err := candidateArchiveCatalog(ctx, tx, owner, rec, publisher, trust)
	if err != nil {
		return candidateRecord{}, err
	}
	if digest != snapshot.digest || rec.ApprovalID != snapshot.record.ApprovalID {
		return candidateRecord{}, errors.New("candidate publication changed during availability verification")
	}
	if rec.State == "archived" {
		if err := readCandidateArchive(ctx, tx, owner, rec, digest); err != nil {
			return candidateRecord{}, err
		}
		return rec, tx.Commit()
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return candidateRecord{}, err
	}
	if !archiveProofMatches(rec, digest, proof) || !proof.ObservedAt.Equal(snapshot.observedAt) || proof.ObservedAt.After(now) || now.Sub(proof.ObservedAt) > 5*time.Minute {
		return candidateRecord{}, errors.New("archival requires fresh native availability proof for every published target")
	}
	body, err := json.Marshal(proof)
	if err != nil {
		return candidateRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO release_candidate_archives(candidate_id,owner_user_id,approval_id,catalog_digest,verified_at,receipt)
VALUES($1,$2,$3,$4,$5,$6::jsonb)`, rec.ID, owner, rec.ApprovalID, digest, proof.ObservedAt, string(body))
	if err != nil {
		return candidateRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE release_candidates SET state='archived',updated_at=clock_timestamp() WHERE candidate_id=$1`, rec.ID); err != nil {
		return candidateRecord{}, err
	}
	rec.State = "archived"
	return rec, tx.Commit()
}

func (l *candidateLedger) recordArchiveInputsReleased(ctx context.Context, key string) error {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return err
	}
	db, err := l.database()
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE release_candidate_archives a
SET inputs_released_at=COALESCE(inputs_released_at,clock_timestamp())
WHERE a.candidate_id=$1 AND a.owner_user_id=$2
AND EXISTS(SELECT 1 FROM release_candidates c WHERE c.candidate_id=a.candidate_id AND c.owner_user_id=$2 AND c.state='archived')`, key, owner)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("archive reference completion requires its permanent owner-scoped fence")
	}
	return nil
}

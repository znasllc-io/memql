package release

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

func catalogReader(ctx context.Context) error {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.IsAnonymous || ac.Synthetic || ac.RoleStandIn || memql.BareShortId(ac.UserId) == "" || (ac.Role != auth.RoleOwner && ac.Role != auth.RoleAdmin && ac.Role != auth.RoleDeveloper) {
		return errors.New("release catalog requires an identified developer, admin or owner")
	}
	return nil
}

// publicCandidate is a mandatory evidence projection, not workflow policy.
// All approved effects must have native completion proof. File uploads alone
// are insufficient: their exact native release must also be published. No
// current operator target, caller manifest or receipt map participates.
func publicCandidate(ctx context.Context, tx *sql.Tx, owner string, rec candidateRecord, publisher string) (pl.PublishedRelease, error) {
	provenance, err := pl.ReleaseProvenanceDigest(rec.Manifest)
	if err != nil {
		return pl.PublishedRelease{}, err
	}
	out := pl.PublishedRelease{FormatVersion: 1, Publisher: publisher, CandidateID: rec.ID, ApprovalID: rec.ApprovalID, WorkflowDigest: rec.Manifest.WorkflowDigest, ProvenanceDigest: provenance, Compatibility: rec.Manifest.Compatibility}
	locations := map[string][]pl.PublishedLocation{}
	targets := candidateTargetReader{targets: map[string]candidateRegistryTarget{}, files: map[string]candidateFileTarget{}}
	for _, d := range rec.Manifest.Destinations {
		target, proof, err := readPublicationTarget(ctx, tx, rec.ID, rec.ApprovalID, d)
		if err != nil {
			return out, err
		}
		pub, err := candidatePublicationFor(rec, d.TargetID, d.Component, d.Artifact)
		if err != nil {
			return out, err
		}
		want := candidatePublicationReceipt{TargetDigest: d.TargetDigest, ArchiveDigest: pub.Artifact.Digest, ImageDigest: pub.Artifact.ImageDigest, Platform: pub.Artifact.Platform}
		if proof != want {
			return out, errors.New("catalog publication receipt differs from approved artifact")
		}
		if target.Registry != nil {
			if pub.Artifact.Kind != "oci" {
				return out, errors.New("catalog target kind differs from artifact")
			}
			t := *target.Registry
			targets.targets[t.ID] = t
			key := d.Component + "/" + d.Artifact
			locations[key] = append(locations[key], pl.PublishedLocation{Kind: "oci", Origin: t.Origin, Repository: t.Repository})
		} else {
			if pub.Artifact.Kind != "file" {
				return out, errors.New("catalog target kind differs from artifact")
			}
			targets.files[target.File.ID] = *target.File
		}
	}
	for _, d := range rec.Manifest.Destinations {
		t, ok := targets.files[d.TargetID]
		if !ok {
			continue
		}
		plan, err := targets.draftPlan(ctx, rec, d.TargetID)
		if err != nil {
			return out, err
		}
		draft, err := readDraft(ctx, tx, owner, rec.ID, rec.ApprovalID, plan)
		if err != nil {
			return out, err
		}
		if draft.State != "published" || draft.Receipt == nil {
			return out, errors.New("catalog requires verified publication of every file release")
		}
		var assetID int64
		for _, a := range draft.Receipt.Assets {
			if a.Name == t.AssetName {
				assetID = a.ID
			}
		}
		if assetID <= 0 {
			return out, errors.New("published file has no verified remote asset identity")
		}
		key := d.Component + "/" + d.Artifact
		locations[key] = append(locations[key], pl.PublishedLocation{Kind: "github-release", Origin: t.APIOrigin, Repository: t.Repository, ReleaseID: draft.ReleaseID, AssetID: assetID, AssetName: t.AssetName, Tag: t.Tag})
	}
	for _, c := range rec.Manifest.Components {
		component := pl.PublishedComponent{Name: c.Name, Version: c.Version, Repository: c.Repository, Commit: c.Commit, Artifacts: []pl.PublishedArtifact{}}
		for _, a := range c.Artifacts {
			component.Artifacts = append(component.Artifacts, pl.PublishedArtifact{Name: a.Name, Kind: a.Kind, Platform: a.Platform, Digest: a.Digest, Size: a.Size, ImageDigest: a.ImageDigest, Locations: locations[c.Name+"/"+a.Name]})
		}
		out.Components = append(out.Components, component)
	}
	_, _, err = pl.CanonicalPublishedRelease(out)
	return out, err
}

func (l *candidateLedger) sealCatalog(ctx context.Context, key, approvalID, publisher, keyID string, private ed25519.PrivateKey, trusted map[string]ed25519.PublicKey) ([]byte, error) {
	tx, owner, err := l.draftTransaction(ctx, key, approvalID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rec, err := readCandidate(ctx, tx, owner, key)
	if err != nil {
		return nil, err
	}
	rec, err = readCandidateApproval(ctx, tx, owner, rec)
	if err != nil {
		return nil, err
	}
	projection, err := publicCandidate(ctx, tx, owner, rec, publisher)
	if err != nil {
		return nil, err
	}
	_, digest, err := pl.CanonicalPublishedRelease(projection)
	if err != nil {
		return nil, err
	}
	var old []byte
	var oldDigest, oldPublisher string
	err = tx.QueryRowContext(ctx, `SELECT catalog_digest,publisher,envelope FROM release_catalog WHERE candidate_id=$1`, key).Scan(&oldDigest, &oldPublisher, &old)
	if err == nil {
		verified, e := pl.VerifyPublishedRelease(old, publisher, trusted)
		if e != nil || oldPublisher != publisher || oldDigest != digest || verified.Digest() != digest {
			return nil, errors.New("stored catalog differs from native publication or configured trust")
		}
		return old, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	body, err := pl.SignPublishedRelease(projection, keyID, private)
	if err != nil {
		return nil, err
	}
	verified, err := pl.VerifyPublishedRelease(body, publisher, trusted)
	if err != nil || verified.Digest() != digest {
		return nil, errors.New("catalog signing key does not match configured trust")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO release_catalog(candidate_id,publisher,catalog_digest,envelope) VALUES($1,$2,$3,$4)`, key, publisher, digest, body)
	if err != nil {
		return nil, err
	}
	return body, tx.Commit()
}

// PublishedCatalog reads only the public signed record. Installation callers
// pass independently configured publisher trust. This entry never reads the
// private candidate/Library journal or accepts an approval boolean.
func PublishedCatalog(ctx context.Context, db *sql.DB, key, publisher string, trusted map[string]ed25519.PublicKey) (pl.VerifiedPublishedRelease, error) {
	_, verified, err := readCatalog(ctx, db, key, publisher, trusted)
	return verified, err
}

func readCatalog(ctx context.Context, db *sql.DB, key, publisher string, trusted map[string]ed25519.PublicKey) ([]byte, pl.VerifiedPublishedRelease, error) {
	var out pl.VerifiedPublishedRelease
	if err := catalogReader(ctx); err != nil {
		return nil, out, err
	}
	if db == nil || len(key) != 71 || key[:7] != "sha256:" || !candidateArtifactDigest(key[7:]) {
		return nil, out, errors.New("catalog read requires storage and an exact candidate identity")
	}
	var body []byte
	var digest string
	err := db.QueryRowContext(ctx, `SELECT envelope,catalog_digest FROM release_catalog WHERE candidate_id=$1 AND publisher=$2`, key, publisher).Scan(&body, &digest)
	if err != nil {
		return nil, out, err
	}
	out, err = pl.VerifyPublishedRelease(body, publisher, trusted)
	if err != nil {
		return nil, out, err
	}
	projection, err := out.Release()
	if err != nil || out.Digest() != digest || projection.CandidateID != key {
		return nil, pl.VerifiedPublishedRelease{}, errors.New("catalog record identity differs from signed evidence")
	}
	return body, out, nil
}

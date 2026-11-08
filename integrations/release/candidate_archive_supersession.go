package release

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// Supersession is what stands in for a catalog when an approved candidate
// never had one (releaseArchiveWithoutCatalog). A later or equal release of
// the same component carrying a signed catalog says the release line moved on:
// whatever this candidate did or did not publish, an installation can find that
// component's newer published record, so this candidate's build inputs are no
// longer the only way to one. It is also what keeps the newest release of a
// component from skipping its catalog through this path: nothing newer than the
// newest release has a catalog. The same version counts, because a renewal
// successor and a later attempt that did publish both carry it.

// supersedingCatalogLimit bounds the catalog rows read per component. The
// newest come first, and a catalog of the same component at a lower version
// cannot supersede, so a release line with more catalogs than this at or above
// the candidate's version still finds one among the newest.
const supersedingCatalogLimit = 256

// catalogSupersedes reports whether a verified catalog payload lists the
// component at the same or a higher version. Versions compare numerically per
// part; a version either side cannot parse never supersedes.
func catalogSupersedes(published pl.PublishedRelease, component pl.ReleaseComponent) bool {
	want, ok := normalizeVersion(component.Version)
	if !ok {
		return false
	}
	for _, c := range published.Components {
		if c.Name != component.Name {
			continue
		}
		if got, ok := normalizeVersion(c.Version); ok && !want.newer(got) {
			return true
		}
	}
	return false
}

type supersedingCatalogRow struct {
	candidateID, digest string
	envelope            []byte
}

// supersedingCatalogs returns, for every component in the candidate, another
// candidate of the same owner whose signed catalog supersedes it, or an error
// naming the first component with none.
//
// The manifest join only narrows which rows are read. A row whose manifest was
// altered so it no longer names the component is skipped, which can only
// refuse an archive. The decision reads the signed payload, and a row that does
// not verify under the configured publisher trust, or whose payload names a
// different candidate or digest than its row, is skipped rather than trusted.
func supersedingCatalogs(ctx context.Context, tx *sql.Tx, owner string, rec candidateRecord, publisher string, trust map[string]ed25519.PublicKey) (map[string]string, error) {
	out := map[string]string{}
	for _, component := range rec.Manifest.Components {
		if out[component.Name] != "" {
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT p.candidate_id,p.catalog_digest,p.envelope FROM release_catalog p
JOIN release_candidates c ON c.candidate_id=p.candidate_id
WHERE c.owner_user_id=$1 AND p.publisher=$2 AND p.candidate_id<>$3
 AND c.manifest->'components' @> jsonb_build_array(jsonb_build_object('name',$4::text))
ORDER BY p.created_at DESC,p.candidate_id DESC LIMIT $5`, owner, publisher, rec.ID, component.Name, supersedingCatalogLimit)
		if err != nil {
			return nil, err
		}
		// Read every row before verifying: the transaction's connection holds
		// one result set at a time.
		var found []supersedingCatalogRow
		for rows.Next() {
			var row supersedingCatalogRow
			if err := rows.Scan(&row.candidateID, &row.digest, &row.envelope); err != nil {
				rows.Close()
				return nil, err
			}
			found = append(found, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, row := range found {
			if row.supersedes(component, publisher, trust) {
				out[component.Name] = row.candidateID
				break
			}
		}
		if out[component.Name] == "" {
			return nil, fmt.Errorf("archiving without a catalog requires a later or equal release of %s %s with a signed catalog", component.Name, component.Version)
		}
	}
	return out, nil
}

// supersedes verifies one catalog row under the configured publisher trust and
// decides from its signed payload alone.
func (row supersedingCatalogRow) supersedes(component pl.ReleaseComponent, publisher string, trust map[string]ed25519.PublicKey) bool {
	verified, err := pl.VerifyPublishedRelease(row.envelope, publisher, trust)
	if err != nil || verified.Digest() != row.digest {
		return false
	}
	projection, err := verified.Release()
	return err == nil && projection.CandidateID == row.candidateID && catalogSupersedes(projection, component)
}

// requireSupersedingCatalog is the fence's check of the catalog the snapshot
// named for one component: still this owner's, still verifying, still
// superseding. Catalogs are immutable, so this re-reads the proof rather than
// repeating the search, which a catalog sealed since could answer differently.
func requireSupersedingCatalog(ctx context.Context, tx *sql.Tx, owner string, rec candidateRecord, component pl.ReleaseComponent, candidateID, publisher string, trust map[string]ed25519.PublicKey) error {
	row := supersedingCatalogRow{candidateID: candidateID}
	err := tx.QueryRowContext(ctx, `SELECT p.catalog_digest,p.envelope FROM release_catalog p
JOIN release_candidates c ON c.candidate_id=p.candidate_id
WHERE c.owner_user_id=$1 AND p.publisher=$2 AND p.candidate_id=$3 AND p.candidate_id<>$4`, owner, publisher, candidateID, rec.ID).Scan(&row.digest, &row.envelope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err != nil || !row.supersedes(component, publisher, trust) {
		return fmt.Errorf("the catalogued release recorded as superseding %s %s no longer does", component.Name, component.Version)
	}
	return nil
}

package release

import (
	"os"
	"strings"
	"testing"
)

const archiveSupersessionDown = "../../component/database/memory-nodes/migrations/20261108210000_release_archive_supersession.down.sql"

// The archive journal holds two kinds of evidence. A catalog-basis row names
// the signed catalog it was verified against; a supersession-basis row has no
// catalog by definition. The constraint keeps either from passing as the other.
func TestArchiveBasisPairsCatalogDigestWithCatalogBasis(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	p, c, _, _ := candidatePublicationFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, basis string
		digest      any
		accepted    bool
	}{
		{"supersession with a catalog digest", "supersession", digest, false},
		{"catalog without a digest", "catalog", nil, false},
		{"an unknown basis", "operator", nil, false},
		{"catalog with a digest", "catalog", digest, true},
		{"supersession without a digest", "supersession", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			_, err = tx.ExecContext(t.Context(), `INSERT INTO release_candidate_archives(candidate_id,owner_user_id,approval_id,catalog_digest,verified_at,receipt,basis)
VALUES($1,$2,$3,$4,clock_timestamp(),'{}'::jsonb,$5)`, request.CandidateID, c.OwnerUserID, request.ApprovalID, tc.digest, tc.basis)
			if (err == nil) != tc.accepted {
				t.Fatalf("basis %q with digest %v: accepted=%v, want %v (%v)", tc.basis, tc.digest, err == nil, tc.accepted, err)
			}
		})
	}
}

func TestArchiveSupersessionRollbackRefusesWhileEvidenceExists(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	p, c, _, _ := candidatePublicationFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	rollback, err := os.ReadFile(archiveSupersessionDown)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("refuses with supersession evidence", func(t *testing.T) {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO release_candidate_archives(candidate_id,owner_user_id,approval_id,verified_at,receipt,basis)
VALUES($1,$2,$3,clock_timestamp(),'{}'::jsonb,'supersession')`, request.CandidateID, c.OwnerUserID, request.ApprovalID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), string(rollback)); err == nil || !strings.Contains(err.Error(), "release archive supersession evidence is not empty") {
			t.Fatal("schema rollback erased supersession archive evidence", err)
		}
	})
	t.Run("restores the catalog-only journal without it", func(t *testing.T) {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		// Another test's evidence must not decide this one; the delete rolls back.
		if _, err := tx.ExecContext(t.Context(), `DELETE FROM release_candidate_archives WHERE basis<>'catalog'`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), string(rollback)); err != nil {
			t.Fatal("schema rollback failed on catalog-only evidence", err)
		}
		var required bool
		if err := tx.QueryRowContext(t.Context(), `SELECT attnotnull FROM pg_attribute WHERE attrelid='release_candidate_archives'::regclass AND attname='catalog_digest'`).Scan(&required); err != nil || !required {
			t.Fatal("schema rollback left the catalog digest optional", required, err)
		}
	})
}

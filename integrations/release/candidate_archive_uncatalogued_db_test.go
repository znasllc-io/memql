package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
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

func TestSupersedingCatalogsCountOnlyTheOwnersVerifiedCatalogs(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	pa, ca, _, _ := candidatePublicationFixture(t, db, provider.server.URL)
	a := preparePublication(t, db, pa, ca)
	record, err := pa.ledger.get(ownerCtx(), a.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	owner := ca.OwnerUserID
	find := func(t *testing.T, trust map[string]ed25519.PublicKey, tamper string) (map[string]string, error) {
		t.Helper()
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if tamper != "" {
			if _, err := tx.ExecContext(t.Context(), tamper); err != nil {
				t.Fatal(err)
			}
		}
		return supersedingCatalogs(t.Context(), tx, owner, record, "fixture", trust)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := find(t, map[string]ed25519.PublicKey{"archive-test": pub}, ""); err == nil || !strings.Contains(err.Error(), "engine") {
		t.Fatal("a candidate with no catalogued release of its component was superseded", got, err)
	}
	pb, cb, _, _ := candidatePublicationFixture(t, db, provider.server.URL)
	b := preparePublication(t, db, pb, cb)
	if _, err := pb.publish(ownerCtx(), b); err != nil {
		t.Fatal(err)
	}
	trust := sealArchiveFixture(t, pb, b)
	got, err := find(t, trust, "")
	if err != nil || len(got) != 1 || got["engine"] != b.CandidateID {
		t.Fatal("the owner's verified catalog of the same component did not supersede", got, err)
	}
	if got, err := find(t, trust, "UPDATE release_candidates SET owner_user_id='another-owner' WHERE candidate_id='"+b.CandidateID+"'"); err == nil {
		t.Fatal("another owner's catalog superseded this owner's candidate", got)
	}
	if got, err := find(t, map[string]ed25519.PublicKey{"archive-test": pub}, ""); err == nil {
		t.Fatal("a catalog that fails the configured trust superseded", got)
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

// supersedingFixture publishes and seals a second engine candidate of the same
// owner, the catalogued release that supersedes every engine 0.25.0 candidate
// a test then archives without a catalog. The trust it returns verifies only
// this catalog, so no other test's catalog can supersede for this test.
func supersedingFixture(t *testing.T, db *sql.DB, provider *archiveRegistry) map[string]ed25519.PublicKey {
	t.Helper()
	p, c, _, _ := candidatePublicationFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	return sealArchiveFixture(t, p, request)
}

func classifyFor(t *testing.T, db *sql.DB, p *candidatePreparer, key string) ([]candidateUncataloguedDestination, map[string]candidateDraftRecord, error) {
	t.Helper()
	rec, err := p.ledger.get(ownerCtx(), key)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	return classifyUncatalogued(ownerCtx(), tx, rec.Manifest.OwnerUserID, rec)
}

func ageIntents(t *testing.T, db *sql.DB, key, age string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE release_publication_intents SET updated_at=clock_timestamp()-$2::interval WHERE candidate_id=$1`, key, age); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveWithoutCatalogQuietPeriodExceedsEveryPublicationBound(t *testing.T) {
	if uncataloguedQuietPeriod <= releasePublicationOperationLimit || uncataloguedQuietPeriod <= time.Hour {
		t.Fatalf("quiet period %v does not exceed the longest publication bound (%v) and the one-hour lifecycle workflows", uncataloguedQuietPeriod, releasePublicationOperationLimit)
	}
}

func TestUncataloguedSnapshotRefusesBeforeAnyProviderRead(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	trust := supersedingFixture(t, db, provider)
	refuses := func(t *testing.T, p *candidatePreparer, library *candidateLibraryFixture, key string, trust map[string]ed25519.PublicKey, want string) {
		t.Helper()
		before := provider.reads.Load()
		if _, err := p.ledger.uncataloguedSnapshot(ownerCtx(), key, "fixture", trust); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("snapshot did not refuse with %q: %v", want, err)
		}
		if provider.reads.Load() != before || (library != nil && !library.pins[key]) {
			t.Fatal("a refused snapshot read a provider or released a pin")
		}
	}
	t.Run("ready", func(t *testing.T) {
		p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
		r, err := p.prepare(ownerCtx(), c)
		if err != nil {
			t.Fatal(err)
		}
		cleanupCandidate(t, db, r.ID)
		refuses(t, p, library, r.ID, trust, "approved")
	})
	t.Run("retired", func(t *testing.T) {
		p, c, _, _ := candidatePublicationFixture(t, db, provider.server.URL)
		r, err := p.prepare(ownerCtx(), c)
		if err != nil {
			t.Fatal(err)
		}
		cleanupCandidate(t, db, r.ID)
		if _, err := p.retire(ownerCtx(), r.ID); err != nil {
			t.Fatal(err)
		}
		refuses(t, p, nil, r.ID, trust, "approved")
	})
	t.Run("catalogued", func(t *testing.T) {
		p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
		request := preparePublication(t, db, p, c)
		if _, err := p.publish(ownerCtx(), request); err != nil {
			t.Fatal(err)
		}
		own := sealArchiveFixture(t, p, request)
		refuses(t, p, library, request.CandidateID, own, "releaseArchiveCandidate")
	})
	t.Run("no supersession", func(t *testing.T) {
		p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
		request := preparePublication(t, db, p, c)
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		refuses(t, p, library, request.CandidateID, map[string]ed25519.PublicKey{"archive-test": pub}, "signed catalog")
	})
	t.Run("pending 1h59m", func(t *testing.T) {
		p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
		request := preparePublication(t, db, p, c)
		if _, err := p.ledger.beginPublication(ownerCtx(), request.CandidateID, request.ApprovalID, request.TargetID, request.Component, request.Artifact); err != nil {
			t.Fatal(err)
		}
		ageIntents(t, db, request.CandidateID, "1 hour 59 minutes")
		refuses(t, p, library, request.CandidateID, trust, "may still be running")
	})
	t.Run("pending 2h1m is quiet", func(t *testing.T) {
		p, c, _, _ := candidatePublicationFixture(t, db, provider.server.URL)
		request := preparePublication(t, db, p, c)
		if _, err := p.ledger.beginPublication(ownerCtx(), request.CandidateID, request.ApprovalID, request.TargetID, request.Component, request.Artifact); err != nil {
			t.Fatal(err)
		}
		ageIntents(t, db, request.CandidateID, "2 hours 1 minute")
		snapshot, err := p.ledger.uncataloguedSnapshot(ownerCtx(), request.CandidateID, "fixture", trust)
		if err != nil || len(snapshot.destinations) != 1 || snapshot.destinations[0].Outcome != "pending" || snapshot.supersededBy["engine"] == "" || snapshot.observedAt.IsZero() {
			t.Fatal("a pending effect quiet for longer than the bound did not snapshot", snapshot.destinations, err)
		}
	})
	t.Run("live upload lease", func(t *testing.T) {
		p, c, _ := candidateDraftWorkflowFixture(t, db)
		library := p.library.(*candidateLibraryFixture)
		publication := preparePublication(t, db, p, c)
		if _, err := p.draft(ownerCtx(), draftRequest(publication), false); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE release_draft_intents SET upload_lease_id='held',upload_lease_until=clock_timestamp()+interval '1 minute' WHERE candidate_id=$1`, publication.CandidateID); err != nil {
			t.Fatal(err)
		}
		refuses(t, p, library, publication.CandidateID, trust, "still running")
	})
}

func TestUncataloguedClassificationRecordsEveryDeclaredDestination(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	t.Run("registry", func(t *testing.T) {
		p, c, _, _ := candidatePublicationFixture(t, db, provider.server.URL)
		request := preparePublication(t, db, p, c)
		expect := func(want string) {
			t.Helper()
			got, bound, err := classifyFor(t, db, p, request.CandidateID)
			if err != nil || len(got) != 1 || got[0] != (candidateUncataloguedDestination{TargetID: "registry", Component: "engine", Artifact: "image", Outcome: want}) || len(bound) != 0 {
				t.Fatalf("classified %v (bound %v, %v), want %s", got, bound, err, want)
			}
		}
		expect("notBegun")
		if _, err := p.ledger.beginPublication(ownerCtx(), request.CandidateID, request.ApprovalID, request.TargetID, request.Component, request.Artifact); err != nil {
			t.Fatal(err)
		}
		ageIntents(t, db, request.CandidateID, "2 hours 1 minute")
		expect("pending")
		if _, err := p.publish(ownerCtx(), request); err != nil {
			t.Fatal(err)
		}
		expect("verified")
	})
	t.Run("file", func(t *testing.T) {
		p, c, _ := candidateDraftWorkflowFixture(t, db)
		publication := preparePublication(t, db, p, c)
		request := draftRequest(publication)
		if _, err := p.draft(ownerCtx(), request, false); err != nil {
			t.Fatal(err)
		}
		if _, err := p.publish(ownerCtx(), publication); err != nil {
			t.Fatal(err)
		}
		got, bound, err := classifyFor(t, db, p, publication.CandidateID)
		if err != nil || len(got) != 1 || got[0].Outcome != "uploaded" || len(bound) != 0 {
			t.Fatal("an upload to a release never published was not recorded as uploaded", got, bound, err)
		}
		if _, err := p.draft(ownerCtx(), request, true); err != nil {
			t.Fatal(err)
		}
		got, bound, err = classifyFor(t, db, p, publication.CandidateID)
		if err != nil || len(got) != 1 || got[0].Outcome != "verified" || bound["registry"].State != "published" || bound["registry"].ReleaseID != 73 || bound["registry"].CandidateID != publication.CandidateID {
			t.Fatal("a published release was not bound for verification", got, bound, err)
		}
	})
}

// A renewal moves the draft binding to the successor and the successor's own
// renewal moves it again, so the predecessor's uploaded asset lives on a release
// two hops away. Only the succession journal may lead there.
func TestUncataloguedClassificationFollowsATwoHopRenewal(t *testing.T) {
	db := candidateTestDB(t)
	first, manifest, _ := candidateDraftWorkflowFixture(t, db)
	a := preparePublication(t, db, first, manifest)
	if _, err := first.draft(ownerCtx(), draftRequest(a), false); err != nil {
		t.Fatal(err)
	}
	if _, err := first.publish(ownerCtx(), a); err != nil {
		t.Fatal(err)
	}
	renew := func(prior *candidatePreparer, revision string) (*candidatePreparer, candidatePublishRequest) {
		t.Helper()
		next := freshDraftHost(prior, db)
		next.engineRevision = func() string { return strings.Repeat(revision, 40) }
		request := preparePublication(t, db, next, manifest)
		if _, err := next.draft(ownerCtx(), draftRequest(request), false); err != nil {
			t.Fatal("renewal failed", err)
		}
		return next, request
	}
	second, b := renew(first, "e")
	third, c := renew(second, "f")
	if _, err := third.publish(ownerCtx(), c); err != nil {
		t.Fatal(err)
	}
	if _, err := third.draft(ownerCtx(), draftRequest(c), true); err != nil {
		t.Fatal(err)
	}
	got, bound, err := classifyFor(t, db, first, a.CandidateID)
	if err != nil || len(got) != 1 || got[0].Outcome != "verified" || bound["registry"].CandidateID != c.CandidateID || bound["registry"].State != "published" {
		t.Fatal("the predecessor did not reach its release through two renewals", got, bound, err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM release_candidate_successions WHERE previous_candidate_id=$1`, b.CandidateID); err != nil {
		t.Fatal(err)
	}
	rec, err := first.ledger.get(ownerCtx(), a.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := classifyUncatalogued(ownerCtx(), tx, rec.Manifest.OwnerUserID, rec); err == nil {
		t.Fatal("a release bound outside the candidate's renewals was accepted", got)
	}
}

package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
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

// twoRegistryFixture publishes the same image to a second registry target, so a
// candidate can hold a complete destination beside one that never began.
func twoRegistryFixture(t *testing.T, db *sql.DB, origin string) (*candidatePreparer, pl.ReleaseCandidate, *candidateLibraryFixture) {
	t.Helper()
	p, c, library, _ := candidatePublicationFixture(t, db, origin)
	mirror := p.targetReader.targets["registry"]
	mirror.ID, mirror.Repository = "registry-2", mirror.Repository+"-mirror"
	p.targetReader.targets[mirror.ID] = mirror
	_, _, digest, err := mirror.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c.Destinations = append(c.Destinations, pl.ReleaseDestination{TargetID: mirror.ID, TargetDigest: digest, Component: "engine", Artifact: "image", Operation: "publish"})
	return p, c, library
}

// withMovedTarget returns a host whose current configuration moved one target's
// repository, so that target no longer resolves to its approved digest.
func withMovedTarget(p *candidatePreparer, targetID string) *candidatePreparer {
	reader := *p.targetReader
	reader.targets = maps.Clone(p.targetReader.targets)
	moved := reader.targets[targetID]
	moved.Repository += "-moved"
	reader.targets[targetID] = moved
	other := *p
	other.targetReader = &reader
	return &other
}

func TestArchiveWithoutCatalogRefusesAnUnavailablePublishedImage(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	trust := supersedingFixture(t, db, provider)
	p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.ledger.uncataloguedSnapshot(ownerCtx(), request.CandidateID, "fixture", trust)
	if err != nil {
		t.Fatal(err)
	}
	provider.unusable.Store(true)
	if _, err := p.verifyUncataloguedInputs(ownerCtx(), snapshot); err == nil || !library.pins[request.CandidateID] {
		t.Fatal("an unavailable published image passed verification", err)
	}
	if record, err := p.ledger.get(ownerCtx(), request.CandidateID); err != nil || record.State != "approved" {
		t.Fatal("failed verification changed the candidate", record.State, err)
	}
	provider.unusable.Store(false)
	proof, err := p.verifyUncataloguedInputs(ownerCtx(), snapshot)
	a := c.Components[0].Artifacts[0]
	if err != nil || len(proof.Targets) != 1 || proof.Targets[0] != (candidateArchiveTarget{TargetID: "registry", Digest: a.Digest, ImageDigest: a.ImageDigest, Platform: a.Platform, Size: a.Size}) {
		t.Fatal("an available published image did not verify", proof.Targets, err)
	}
}

func TestArchiveWithoutCatalogReadsNoProviderForUnpublishedEffects(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	trust := supersedingFixture(t, db, provider)
	p, c, library := twoRegistryFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	if _, err := p.ledger.beginPublication(ownerCtx(), request.CandidateID, request.ApprovalID, "registry", "engine", "image"); err != nil {
		t.Fatal(err)
	}
	ageIntents(t, db, request.CandidateID, "2 hours 1 minute")
	snapshot, err := p.ledger.uncataloguedSnapshot(ownerCtx(), request.CandidateID, "fixture", trust)
	if err != nil {
		t.Fatal(err)
	}
	before := provider.reads.Load()
	proof, err := p.verifyUncataloguedInputs(ownerCtx(), snapshot)
	want := []candidateUncataloguedDestination{
		{TargetID: "registry", Component: "engine", Artifact: "image", Outcome: "pending"},
		{TargetID: "registry-2", Component: "engine", Artifact: "image", Outcome: "notBegun"},
	}
	if err != nil || len(proof.Targets) != 0 || !slices.Equal(proof.Destinations, want) || provider.reads.Load() != before {
		t.Fatal("effects that never completed reached a provider or were misrecorded", proof.Destinations, err)
	}
	if proof.CandidateID != request.CandidateID || proof.ApprovalID != request.ApprovalID || !proof.ObservedAt.Equal(snapshot.observedAt) || proof.SupersededBy["engine"] == "" || !library.pins[request.CandidateID] {
		t.Fatal("the proof did not carry the snapshot's identity", proof)
	}
}

func TestArchiveWithoutCatalogVerifiesARenewedPredecessorThroughItsSuccessor(t *testing.T) {
	db := candidateTestDB(t)
	first, manifest, remote := candidateDraftWorkflowFixture(t, db)
	a := preparePublication(t, db, first, manifest)
	if _, err := first.draft(ownerCtx(), draftRequest(a), false); err != nil {
		t.Fatal(err)
	}
	if _, err := first.publish(ownerCtx(), a); err != nil {
		t.Fatal(err)
	}
	next := freshDraftHost(first, db)
	next.engineRevision = func() string { return strings.Repeat("e", 40) }
	b := preparePublication(t, db, next, manifest)
	if _, err := next.draft(ownerCtx(), draftRequest(b), false); err != nil {
		t.Fatal(err)
	}
	if _, err := next.publish(ownerCtx(), b); err != nil {
		t.Fatal(err)
	}
	if _, err := next.draft(ownerCtx(), draftRequest(b), true); err != nil {
		t.Fatal(err)
	}
	trust := sealArchiveFixture(t, next, b)
	snapshot, err := first.ledger.uncataloguedSnapshot(ownerCtx(), a.CandidateID, "fixture", trust)
	if err != nil || len(snapshot.destinations) != 1 || snapshot.destinations[0].Outcome != "verified" || snapshot.bound["registry"].CandidateID != b.CandidateID || snapshot.supersededBy["engine"] != b.CandidateID {
		t.Fatal("the predecessor did not snapshot its successor's release", snapshot.destinations, err)
	}
	remote.asset.mu.Lock()
	remote.asset.corrupt = true
	remote.asset.mu.Unlock()
	if _, err := first.verifyUncataloguedInputs(ownerCtx(), snapshot); err == nil {
		t.Fatal("a changed asset on the successor's release passed verification")
	}
	remote.asset.mu.Lock()
	remote.asset.corrupt = false
	remote.asset.mu.Unlock()
	proof, err := first.verifyUncataloguedInputs(ownerCtx(), snapshot)
	if err != nil || len(proof.Targets) != 1 || proof.Targets[0].TargetID != "registry" {
		t.Fatal("the successor's published release did not verify the predecessor", proof.Targets, err)
	}
	if remote.creates != 1 || remote.tags != 1 || remote.promotes != 1 || remote.asset.uploads != 1 {
		t.Fatal("verification repeated a release effect", remote.creates, remote.tags, remote.promotes, remote.asset.uploads)
	}
}

func TestArchiveWithoutCatalogResolvesOnlyDestinationsItReads(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	trust := supersedingFixture(t, db, provider)
	p, c, _ := twoRegistryFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.ledger.uncataloguedSnapshot(ownerCtx(), request.CandidateID, "fixture", trust)
	if err != nil {
		t.Fatal(err)
	}
	if proof, err := withMovedTarget(p, "registry-2").verifyUncataloguedInputs(ownerCtx(), snapshot); err != nil || len(proof.Targets) != 1 {
		t.Fatal("a moved target that needs no read blocked verification", proof.Targets, err)
	}
	if _, err := withMovedTarget(p, "registry").verifyUncataloguedInputs(ownerCtx(), snapshot); err == nil || !strings.Contains(err.Error(), "changed since candidate review") {
		t.Fatal("a published destination verified against a moved target", err)
	}
}

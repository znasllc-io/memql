package release

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

func sealArchiveFixture(t *testing.T, p *candidatePreparer, request candidatePublishRequest) map[string]ed25519.PublicKey {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust := map[string]ed25519.PublicKey{"archive-test": pub}
	if _, err := p.ledger.sealCatalog(ownerCtx(), request.CandidateID, request.ApprovalID, "fixture", "archive-test", private, trust); err != nil {
		t.Fatal(err)
	}
	return trust
}

type archiveRegistry struct {
	server   *httptest.Server
	unusable atomic.Bool
	reads    atomic.Int64
	writes   atomic.Int64
}

func archiveRegistryFixture(t *testing.T) *archiveRegistry {
	t.Helper()
	f := &archiveRegistry{}
	base := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			f.reads.Add(1)
			if f.unusable.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		} else {
			f.writes.Add(1)
		}
		base.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestCandidateArchiveOwnerWallPrecedesPorts(t *testing.T) {
	p := &candidatePreparer{ledger: &candidateLedger{db: func() *sql.DB { t.Fatal("non-owner reached archive storage"); return nil }}}
	for _, ctx := range []context.Context{context.Background(), actorContext(auth.RoleAdmin), actorContext(auth.RoleDeveloper)} {
		if _, err := p.archive(ctx, "candidate", "fixture", nil); err == nil {
			t.Fatal("non-owner archived candidate")
		}
		if _, err := p.ledger.archiveSnapshot(ctx, "candidate", "fixture", nil); err == nil {
			t.Fatal("non-owner read archive inputs")
		}
		if _, err := p.ledger.archive(ctx, candidateArchiveSnapshot{}, candidateArchiveProof{}, "fixture", nil); err == nil {
			t.Fatal("non-owner wrote archive fence")
		}
	}
}

func TestCandidateArchiveRetainsCanonicalHistoryAndOtherReferencesAcrossReplicas(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	trust := sealArchiveFixture(t, p, request)
	library.pins["another-candidate"] = true
	beforeWrites, beforeOpens := provider.writes.Load(), library.opens
	var wg sync.WaitGroup
	failures := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other := freshDraftHost(p, candidateTestDB(t))
			record, err := other.archive(ownerCtx(), request.CandidateID, "fixture", trust)
			if err == nil && (record.State != "archived" || record.ApprovalID != request.ApprovalID) {
				err = errors.New("archive changed candidate identity")
			}
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if library.pins[request.CandidateID] || !library.pins["another-candidate"] || !library.released[request.CandidateID] || provider.writes.Load() != beforeWrites || library.opens != beforeOpens {
		t.Fatal("archive rewrote canonical bytes, reopened build inputs or released another reference")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM release_candidate_archives WHERE candidate_id=$1`, request.CandidateID).Scan(&count); err != nil || count != 1 {
		t.Fatal("replicas did not share one archive fence", count, err)
	}
	if _, err := PublishedCatalog(ownerCtx(), db, request.CandidateID, "fixture", trust); err != nil {
		t.Fatal("public catalog no longer works without build inputs", err)
	}
	effects, err := p.ledger.effects(ownerCtx(), request.CandidateID)
	if err != nil || len(effects) != 1 || effects[0].State != "complete" {
		t.Fatal("publication history was lost", err)
	}
	for name, operation := range map[string]func() error{
		"prepare": func() error { _, err := p.prepare(ownerCtx(), c); return err },
		"approve": func() error { _, err := p.approve(ownerCtx(), request.CandidateID); return err },
		"publish": func() error { _, err := p.publish(ownerCtx(), request); return err },
		"ready":   func() error { _, err := p.ledger.ready(ownerCtx(), request.CandidateID); return err },
		"begin publication": func() error {
			_, err := p.ledger.beginPublication(ownerCtx(), request.CandidateID, request.ApprovalID, request.TargetID, request.Component, request.Artifact)
			return err
		},
	} {
		if operation() == nil {
			t.Fatal("archived candidate resumed", name)
		}
	}
	for _, ref := range candidateArtifactReferences(c, request.CandidateID) {
		if _, err := library.PinRunFileReceipts(auth.ContextWithInternalOrigin(ownerCtx()), ref); err == nil {
			t.Fatal("delayed preparer repinned archived evidence")
		}
	}
	rollback, err := os.ReadFile("../../component/database/memory-nodes/migrations/20261020022000_release_candidate_archives.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.ExecContext(t.Context(), string(rollback))
	_ = tx.Rollback()
	if err == nil || !strings.Contains(err.Error(), "release archive evidence is not empty") {
		t.Fatal("schema rollback erased archive evidence", err)
	}
}

type interruptedArchiveLibrary struct{ CandidateLibrary }

func (l interruptedArchiveLibrary) ReleaseRunFileReference(context.Context, pipelinesteps.RunFileReference) error {
	return errors.New("simulated process loss after native archive fence")
}

func TestCandidateArchiveResumesPinReleaseAfterLostResponse(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	trust := sealArchiveFixture(t, p, request)
	first := *p
	first.library = interruptedArchiveLibrary{library}
	if record, err := first.archive(ownerCtx(), request.CandidateID, "fixture", trust); err == nil || record.State != "archived" || !library.pins[request.CandidateID] {
		t.Fatal("interrupted archive lost its fence or pending pin", record.State, err)
	}
	provider.unusable.Store(true)
	before := provider.reads.Load()
	other := freshDraftHost(p, candidateTestDB(t))
	if _, err := other.archive(ownerCtx(), request.CandidateID, "fixture", trust); err != nil || library.pins[request.CandidateID] || provider.reads.Load() != before {
		t.Fatal("new host did not resume the already committed reference release", err)
	}
}

func TestCandidateArchiveRefusesHistoricalReceiptWithoutFreshAvailability(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	trust := sealArchiveFixture(t, p, request)
	provider.unusable.Store(true)
	if _, err := p.archive(ownerCtx(), request.CandidateID, "fixture", trust); err == nil || !library.pins[request.CandidateID] || len(library.released) != 0 {
		t.Fatal("unavailable published bytes allowed archival", err)
	}
	record, err := p.ledger.get(ownerCtx(), request.CandidateID)
	if err != nil || record.State != "approved" {
		t.Fatal("failed availability changed candidate state", err)
	}
}

func TestCandidateArchiveRefusesIncompleteUnsealedOrChangedPublication(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	trust := map[string]ed25519.PublicKey{"archive-test": pub}
	refuse := func() {
		t.Helper()
		before := provider.reads.Load()
		if _, err := p.archive(ownerCtx(), request.CandidateID, "fixture", trust); err == nil || provider.reads.Load() != before || !library.pins[request.CandidateID] {
			t.Fatal("incomplete evidence reached external observation or released pins", err)
		}
	}
	refuse()
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	refuse() // Complete publication without a signed catalog is insufficient.
	trust = sealArchiveFixture(t, p, request)
	if _, err := db.Exec(`UPDATE release_publication_intents SET state='pending',receipt=NULL WHERE candidate_id=$1`, request.CandidateID); err != nil {
		t.Fatal(err)
	}
	refuse() // A catalog cannot legitimize a now-inconsistent native journal.
}

func TestCandidateArchiveProofCannotCrossOwnerCandidateOrFreshnessBoundary(t *testing.T) {
	db := candidateTestDB(t)
	provider := archiveRegistryFixture(t)
	p, c, library, _ := candidatePublicationFixture(t, db, provider.server.URL)
	request := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	trust := sealArchiveFixture(t, p, request)
	snapshot, err := p.ledger.archiveSnapshot(ownerCtx(), request.CandidateID, "fixture", trust)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := p.verifyPublishedInputs(ownerCtx(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"owner", "candidate", "approval", "catalog", "stale", "future", "missing target", "wrong digest"} {
		t.Run(mode, func(t *testing.T) {
			input := proof
			input.Targets = append([]candidateArchiveTarget(nil), proof.Targets...)
			ctx := ownerCtx()
			snap := snapshot
			switch mode {
			case "owner":
				ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "another-owner", Role: auth.RoleOwner})
			case "candidate":
				input.CandidateID = "sha256:" + strings.Repeat("f", 64)
			case "approval":
				input.ApprovalID = "another-approval"
			case "catalog":
				input.CatalogDigest = "sha256:" + strings.Repeat("f", 64)
			case "stale":
				input.ObservedAt = input.ObservedAt.Add(-6 * time.Minute)
				snap.observedAt = input.ObservedAt
			case "future":
				input.ObservedAt = input.ObservedAt.Add(time.Minute)
				snap.observedAt = input.ObservedAt
			case "missing target":
				input.Targets = nil
			case "wrong digest":
				input.Targets[0].Digest = "sha256:" + strings.Repeat("f", 64)
			}
			if _, err := p.ledger.archive(ctx, snap, input, "fixture", trust); err == nil || !library.pins[request.CandidateID] {
				t.Fatal("altered availability proof was accepted", mode, err)
			}
		})
	}
}

func TestCandidateArchiveFilesRequiresFreshPublishedBytesAndPreservesDraftHistory(t *testing.T) {
	db := candidateTestDB(t)
	p, c, fixture := candidateDraftWorkflowFixture(t, db)
	library := p.library.(*candidateLibraryFixture)
	publication := preparePublication(t, db, p, c)
	request := draftRequest(publication)
	if _, err := p.draft(ownerCtx(), request, false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.publish(ownerCtx(), publication); err != nil {
		t.Fatal(err)
	}
	if _, err := p.draft(ownerCtx(), request, true); err != nil {
		t.Fatal(err)
	}
	trust := sealArchiveFixture(t, p, publication)
	fixture.asset.mu.Lock()
	fixture.asset.corrupt = true
	fixture.asset.mu.Unlock()
	if _, err := freshDraftHost(p, candidateTestDB(t)).archive(ownerCtx(), publication.CandidateID, "fixture", trust); err == nil || !library.pins[publication.CandidateID] {
		t.Fatal("changed published file released its retained build input", err)
	}
	fixture.asset.mu.Lock()
	fixture.asset.corrupt = false
	fixture.asset.mu.Unlock()
	beforeOpens := library.opens
	other := freshDraftHost(p, candidateTestDB(t))
	if record, err := other.archive(ownerCtx(), publication.CandidateID, "fixture", trust); err != nil || record.State != "archived" || library.pins[publication.CandidateID] {
		t.Fatal("published file could not archive its verified duplicate", record.State, err)
	}
	if library.opens != beforeOpens || fixture.creates != 1 || fixture.tags != 1 || fixture.promotes != 1 || fixture.asset.uploads != 1 {
		t.Fatal("archival reopened an input or repeated a publication effect")
	}
	history, err := other.ledger.drafts(ownerCtx(), publication.CandidateID)
	if err != nil || len(history) != 1 || history[0].State != "published" || history[0].ReleaseID != 73 {
		t.Fatal("archival erased the native release history", history, err)
	}
	if _, err := PublishedCatalog(ownerCtx(), db, publication.CandidateID, "fixture", trust); err != nil {
		t.Fatal("archival broke the published file catalog", err)
	}
	for _, promote := range []bool{false, true} {
		if _, err := other.draft(ownerCtx(), request, promote); err == nil {
			t.Fatal("archived release entered a mutable draft workflow", promote)
		}
	}
}

func TestCandidateArchiveNPMRequiresFreshCanonicalArchiveWithoutRepublishing(t *testing.T) {
	db := candidateTestDB(t)
	p, c, library, writes, corrupt := npmCandidateFixture(t, db)
	request := preparePublication(t, db, p, c)
	if _, err := p.publish(ownerCtx(), request); err != nil {
		t.Fatal(err)
	}
	trust := sealArchiveFixture(t, p, request)
	corrupt.Store(true)
	if _, err := freshDraftHost(p, candidateTestDB(t)).archive(ownerCtx(), request.CandidateID, "fixture", trust); err == nil || !library.pins[request.CandidateID] {
		t.Fatal("changed npm archive released its retained build input", err)
	}
	corrupt.Store(false)
	beforeOpens := library.opens
	if record, err := freshDraftHost(p, candidateTestDB(t)).archive(ownerCtx(), request.CandidateID, "fixture", trust); err != nil || record.State != "archived" || library.pins[request.CandidateID] {
		t.Fatal("published npm archive could not release its verified duplicate", record.State, err)
	}
	if writes.Load() != 1 || library.opens != beforeOpens {
		t.Fatal("archival republished npm or reopened its build input")
	}
	if _, err := PublishedCatalog(ownerCtx(), db, request.CandidateID, "fixture", trust); err != nil {
		t.Fatal("archival broke the published npm catalog", err)
	}
}

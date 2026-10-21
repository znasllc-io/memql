package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestCandidateDraftRenewalAfterEngineUpgradePreservesEffectsAcrossHosts(t *testing.T) {
	for _, uploaded := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-upload", true: "after-upload"}[uploaded], func(t *testing.T) {
			db := candidateTestDB(t)
			old, manifest, remote := candidateDraftWorkflowFixture(t, db)
			previous := preparePublication(t, db, old, manifest)
			binding, err := old.draft(ownerCtx(), draftRequest(previous), false)
			if err != nil {
				t.Fatal(err)
			}
			if uploaded {
				if _, err := old.publish(ownerCtx(), previous); err != nil {
					t.Fatal(err)
				}
			}
			next := freshDraftHost(old, db)
			next.engineRevision = func() string { return strings.Repeat("e", 40) }
			if _, err := next.draft(ownerCtx(), draftRequest(previous), false); err == nil {
				t.Fatal("engine change reused the old approval")
			}
			replacement := preparePublication(t, db, next, manifest)
			if replacement.CandidateID == previous.CandidateID || replacement.ApprovalID == previous.ApprovalID {
				t.Fatal("upgrade did not require a separately verified and approved candidate")
			}
			renewed, err := next.draft(ownerCtx(), draftRequest(replacement), false)
			if err != nil {
				t.Fatal("fresh approval could not resume the unchanged draft", err)
			}
			if renewed.IntentID != binding.IntentID || renewed.ReleaseID != binding.ReleaseID || renewed.State != "ready" {
				t.Fatal("renewal replaced the external effect identity", renewed)
			}
			peer := freshDraftHost(next, db)
			if _, err := peer.publish(ownerCtx(), replacement); err != nil {
				t.Fatal(err)
			}
			if _, err := peer.draft(ownerCtx(), draftRequest(replacement), true); err != nil {
				t.Fatal(err)
			}
			if remote.creates != 1 || remote.tags != 1 || remote.asset.uploads != 1 || remote.promotes != 1 {
				t.Fatal("renewal repeated an external effect", remote.creates, remote.tags, remote.asset.uploads, remote.promotes)
			}
			if _, err := old.draft(ownerCtx(), draftRequest(previous), false); err == nil {
				t.Fatal("the previous host reclaimed the renewed draft")
			}
		})
	}
}

func renewalRecords(t *testing.T, db *sql.DB, p *candidatePreparer, prior candidatePublishRequest) (candidateRecord, candidateRecord, candidateDraftPlan) {
	t.Helper()
	old, err := p.ledger.get(ownerCtx(), prior.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.targetReader.draftPlan(ownerCtx(), old, prior.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	manifest := old.Manifest
	manifest.WorkflowDigest = "sha256:" + strings.Repeat("f", 64)
	next := approveRenewalRecord(t, db, p.ledger, manifest)
	return old, next, plan
}

func approveRenewalRecord(t *testing.T, db *sql.DB, ledger *candidateLedger, manifest pl.ReleaseCandidate) candidateRecord {
	t.Helper()
	next, err := ledger.prepare(ownerCtx(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, next.ID)
	if _, err := ledger.ready(ownerCtx(), next.ID); err != nil {
		t.Fatal(err)
	}
	next, err = ledger.approve(ownerCtx(), next.ID)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestCandidateDraftRenewalRefusesUnsafeStateWithoutPartialTransfer(t *testing.T) {
	for _, scenario := range []string{"wrong-approval", "old-approval", "no-owner", "pending-upload", "creating", "promoting", "published", "corrupt-proof", "changed-plan", "new-effects"} {
		t.Run(scenario, func(t *testing.T) {
			db := candidateTestDB(t)
			p, c, _ := candidateDraftWorkflowFixture(t, db)
			prior := preparePublication(t, db, p, c)
			if _, err := p.draft(ownerCtx(), draftRequest(prior), false); err != nil {
				t.Fatal(err)
			}
			old, next, plan := renewalRecords(t, db, p, prior)
			ctx, approval := ownerCtx(), next.ApprovalID
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := db.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "wrong-approval":
				approval = "unrelated"
			case "old-approval":
				approval = old.ApprovalID
			case "no-owner":
				ctx = context.Background()
			case "pending-upload", "new-effects":
				candidate := old
				if scenario == "new-effects" {
					candidate = next
				}
				if _, err := p.ledger.beginPublication(ctx, candidate.ID, candidate.ApprovalID, prior.TargetID, prior.Component, prior.Artifact); err != nil {
					t.Fatal(err)
				}
			case "creating":
				exec(`UPDATE release_draft_intents SET state='creating',release_id=NULL WHERE candidate_id=$1`, old.ID)
			case "promoting":
				exec(`UPDATE release_draft_intents SET state=$2 WHERE candidate_id=$1`, old.ID, scenario)
			case "published":
				if _, err := p.publish(ctx, prior); err != nil {
					t.Fatal(err)
				}
				if _, err := p.draft(ctx, draftRequest(prior), true); err != nil {
					t.Fatal(err)
				}
			case "corrupt-proof":
				if _, err := p.publish(ctx, prior); err != nil {
					t.Fatal(err)
				}
				exec(`UPDATE release_publication_intents SET receipt='{}'::jsonb WHERE candidate_id=$1`, old.ID)
			case "changed-plan":
				plan.Target.Tag = "different"
			}
			if _, err := p.ledger.beginReviewedDraft(ctx, next.ID, approval, plan); err == nil {
				t.Fatal("unsafe renewal admitted")
			}
			var current string
			var successions int
			if err := db.QueryRow(`SELECT candidate_id FROM release_draft_intents WHERE resource_key=$1`, plan.ResourceKey).Scan(&current); err != nil || current != old.ID {
				t.Fatal("failed renewal changed authority", current, err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM release_candidate_successions WHERE previous_candidate_id=$1`, old.ID).Scan(&successions); err != nil || successions != 0 {
				t.Fatal("failed renewal left a succession", successions, err)
			}
		})
	}
}

func TestCandidateDraftRenewalCannotBroadenApprovedRelease(t *testing.T) {
	db := candidateTestDB(t)
	p, c, _ := candidateDraftWorkflowFixture(t, db)
	prior := preparePublication(t, db, p, c)
	if _, err := p.draft(ownerCtx(), draftRequest(prior), false); err != nil {
		t.Fatal(err)
	}
	old, _, plan := renewalRecords(t, db, p, prior)
	for name, change := range map[string]func(*pl.ReleaseCandidate){
		"artifact": func(c *pl.ReleaseCandidate) {
			c.Components[0].Artifacts[0].Digest = "sha256:" + strings.Repeat("a", 64)
		},
		"source":      func(c *pl.ReleaseCandidate) { c.Components[0].Commit = strings.Repeat("c", 40) },
		"version":     func(c *pl.ReleaseCandidate) { c.Components[0].Version = "0.25.1" },
		"destination": func(c *pl.ReleaseCandidate) { c.Destinations[0].TargetDigest = "sha256:" + strings.Repeat("b", 64) },
		"evidence":    func(c *pl.ReleaseCandidate) { c.Evidence[0].ReceiptDigest = "sha256:" + strings.Repeat("c", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			body, _ := json.Marshal(old.Manifest)
			var manifest pl.ReleaseCandidate
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest.WorkflowDigest = "sha256:" + strings.Repeat("e", 64)
			change(&manifest)
			next := approveRenewalRecord(t, db, p.ledger, manifest)
			if _, err := p.ledger.beginReviewedDraft(ownerCtx(), next.ID, next.ApprovalID, plan); err == nil {
				t.Fatal("fresh approval broadened an existing release")
			}
		})
	}
}

func TestCandidateDraftRenewalSerializesHostsAndPermanentlyFencesOldAuthority(t *testing.T) {
	db := candidateTestDB(t)
	p, c, remote := candidateDraftWorkflowFixture(t, db)
	prior := preparePublication(t, db, p, c)
	if _, err := p.draft(ownerCtx(), draftRequest(prior), false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.publish(ownerCtx(), prior); err != nil {
		t.Fatal(err)
	}
	old, next, plan := renewalRecords(t, db, p, prior)
	ctx, cancel := context.WithTimeout(ownerCtx(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := freshDraftHost(p, db).ledger.beginReviewedDraft(ctx, next.ID, next.ApprovalID, plan)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.ledger.beginReviewedDraft(ctx, old.ID, old.ApprovalID, plan); err == nil {
		t.Fatal("old host reclaimed authority")
	}
	if _, err := p.ledger.beginPublication(ctx, old.ID, old.ApprovalID, prior.TargetID, prior.Component, prior.Artifact); err == nil {
		t.Fatal("old host started publication")
	}
	if err := p.ledger.withDraftUpload(ctx, old.ID, old.ApprovalID, plan, func(int64) error { t.Fatal("old host wrote remotely"); return nil }); err == nil {
		t.Fatal("old upload authority remained valid")
	}
	if _, err := p.ledger.drafts(ctx, old.ID); err != nil {
		t.Fatal("historical reads stopped working", err)
	}
	var successions, receipts int
	if err := db.QueryRow(`SELECT count(*) FROM release_candidate_successions WHERE previous_candidate_id=$1 AND candidate_id=$2`, old.ID, next.ID).Scan(&successions); err != nil || successions != 1 {
		t.Fatal(successions, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM release_publication_intents WHERE candidate_id IN ($1,$2) AND state='complete'`, old.ID, next.ID).Scan(&receipts); err != nil || receipts != 2 {
		t.Fatal("receipt history was lost or duplicated", receipts, err)
	}
	if remote.creates != 1 || remote.asset.uploads != 1 {
		t.Fatal("renewal repeated remote effects")
	}
}

func TestCandidateDraftRenewalWaitsForInFlightUploadAndRefusesItsPendingReceipt(t *testing.T) {
	db := candidateTestDB(t)
	p, c, _ := candidateDraftWorkflowFixture(t, db)
	prior := preparePublication(t, db, p, c)
	if _, err := p.draft(ownerCtx(), draftRequest(prior), false); err != nil {
		t.Fatal(err)
	}
	old, next, plan := renewalRecords(t, db, p, prior)
	if _, err := p.ledger.beginPublication(ownerCtx(), old.ID, old.ApprovalID, prior.TargetID, prior.Component, prior.Artifact); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(ownerCtx(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	written, renewed := make(chan error, 1), make(chan error, 1)
	go func() {
		written <- p.ledger.withDraftUpload(ctx, old.ID, old.ApprovalID, plan, func(int64) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-written:
		t.Fatal("upload did not enter", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		_, err := freshDraftHost(p, db).ledger.beginReviewedDraft(ctx, next.ID, next.ApprovalID, plan)
		renewed <- err
	}()
	select {
	case err := <-renewed:
		t.Fatal("renewal crossed an active upload", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := <-renewed; err == nil || !strings.Contains(err.Error(), "unfinished publication") {
		t.Fatal("renewal ignored an unrecorded upload result", err)
	}
}

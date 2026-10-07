package release

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

func candidateTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "candidate approval journal", dbtest.DSN(), err)
	}
	return db
}

func candidateTestManifest() pl.ReleaseCandidate {
	ac, _ := auth.AccessFromContext(ownerCtx())
	d := "sha256:" + strings.Repeat("a", 64)
	return pl.ReleaseCandidate{FormatVersion: 1, OwnerUserID: memql.BareShortId(ac.UserId), WorkflowDigest: d,
		Components:   []pl.ReleaseComponent{{Name: "engine", Version: "0.25.0", Repository: "acme/engine", Commit: strings.Repeat("b", 40), Artifacts: []pl.ReleaseArtifact{{Name: "image", Kind: "oci", Platform: "linux/arm64", Digest: d, Size: 10, ImageDigest: d, Receipt: pl.ReleaseReceiptReference{WorkRunID: id.NewShortId(), StepKey: "build.image", Attempt: 1, IntentID: strings.Repeat("c", 64), ReceiptDigest: d, DefinitionDigest: d}}}}},
		Evidence:     []pl.ReleaseEvidence{{Name: "checks", Component: "engine", WorkRunID: id.NewShortId(), StepKey: "checks.all", Attempt: 1, ReceiptID: id.NewShortId(), ReceiptDigest: d, DefinitionDigest: d}},
		Destinations: []pl.ReleaseDestination{{TargetID: "registry", TargetDigest: d, Component: "engine", Artifact: "image", Operation: "publish"}},
	}
}

func cleanupCandidate(t *testing.T, db *sql.DB, key string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM release_draft_intents WHERE candidate_id=$1`, key); err != nil {
			t.Error(err)
		}
		if _, err := db.Exec(`DELETE FROM release_publication_intents WHERE candidate_id=$1`, key); err != nil {
			t.Error(err)
		}
		if _, err := db.Exec(`DELETE FROM release_candidate_approvals WHERE candidate_id=$1`, key); err != nil {
			t.Error(err)
		}
		if _, err := db.Exec(`DELETE FROM release_candidates WHERE candidate_id=$1`, key); err != nil {
			t.Error(err)
		}
	})
}

func TestCandidateLedgerOwnerWallPrecedesDatabase(t *testing.T) {
	ledger := candidateLedger{db: func() *sql.DB { t.Fatal("unapproved actor reached database"); return nil }}
	for _, ctx := range []context.Context{context.Background(), actorContext(auth.RoleDeveloper), actorContext(auth.RoleAdmin)} {
		if _, err := ledger.prepare(ctx, candidateTestManifest()); err == nil {
			t.Fatal("non-owner prepared a candidate")
		}
		if _, err := ledger.approve(ctx, "anything"); err == nil {
			t.Fatal("non-owner approved a candidate")
		}
		if _, err := ledger.ready(ctx, "anything"); err == nil {
			t.Fatal("non-owner marked candidate ready")
		}
		if _, err := ledger.retire(ctx, "anything"); err == nil {
			t.Fatal("non-owner retired a candidate")
		}
	}
}

func TestCandidateLedgerApprovalSurvivesReplicasAndCannotBroaden(t *testing.T) {
	db := candidateTestDB(t)
	first := candidateLedger{db: func() *sql.DB { return db }}
	other := candidateLedger{db: func() *sql.DB { return db }}
	candidate := candidateTestManifest()
	prepared, err := first.prepare(ownerCtx(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, prepared.ID)
	if prepared.State != "preparing" || prepared.ApprovalID != "" {
		t.Fatal("preparation fabricated approval")
	}
	if _, err := other.approve(ownerCtx(), prepared.ID); err == nil {
		t.Fatal("unverified candidate approved")
	}
	if _, err := first.ready(ownerCtx(), prepared.ID); err != nil {
		t.Fatal(err)
	}
	replies := make(chan candidateRecord, 8)
	failures := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := other.approve(ownerCtx(), prepared.ID)
			if e != nil {
				failures <- e
			} else {
				replies <- r
			}
		}()
	}
	wg.Wait()
	close(replies)
	close(failures)
	for e := range failures {
		t.Error(e)
	}
	approval := ""
	for r := range replies {
		if r.State != "approved" || r.ApprovalID == "" {
			t.Fatal("approval did not persist")
		}
		if approval != "" && approval != r.ApprovalID {
			t.Fatal("replicas minted multiple approvals")
		}
		approval = r.ApprovalID
	}
	if approval == "" {
		t.Fatal("no approval")
	}
	candidate.Destinations[0].TargetID = "different-registry"
	changed, err := other.prepare(ownerCtx(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, changed.ID)
	if changed.ID == prepared.ID || changed.State != "preparing" || changed.ApprovalID != "" {
		t.Fatal("changed target reused approval")
	}
	if _, err := other.approve(ownerCtx(), changed.ID); err == nil {
		t.Fatal("changed inputs acquired prior verification")
	}
	if _, err := first.retire(ownerCtx(), prepared.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ready(ownerCtx(), prepared.ID); err == nil {
		t.Fatal("late verifier resurrected retired candidate")
	}
	if _, err := other.approve(ownerCtx(), prepared.ID); err == nil {
		t.Fatal("late approval resurrected retired candidate")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM release_candidate_approvals WHERE candidate_id=$1`, prepared.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("retirement discarded approval history", err)
	}
}

func TestCandidateLedgerRefusesChangedStoredManifestBeforeApproval(t *testing.T) {
	db := candidateTestDB(t)
	ledger := candidateLedger{db: func() *sql.DB { return db }}
	rec, err := ledger.prepare(ownerCtx(), candidateTestManifest())
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, rec.ID)
	if _, err := ledger.ready(ownerCtx(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE release_candidates SET manifest=jsonb_set(manifest,'{destinations,0,targetId}','"attacker-registry"'::jsonb) WHERE candidate_id=$1`, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.approve(ownerCtx(), rec.ID); err == nil {
		t.Fatal("tampered manifest accepted under old digest")
	}
	var approval string
	err = db.QueryRow(`SELECT approval_id FROM release_candidate_approvals WHERE candidate_id=$1`, rec.ID).Scan(&approval)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("tampered candidate wrote approval", err)
	}
}

func TestCandidateLedgerPublicationIntentsSurviveLostRepliesAndFenceRetirement(t *testing.T) {
	db := candidateTestDB(t)
	first := candidateLedger{db: func() *sql.DB { return db }}
	replacement := candidateLedger{db: func() *sql.DB { return db }}
	c := candidateTestManifest()
	prepared, err := first.prepare(ownerCtx(), c)
	if err != nil {
		t.Fatal(err)
	}
	cleanupCandidate(t, db, prepared.ID)
	if _, err := first.ready(ownerCtx(), prepared.ID); err != nil {
		t.Fatal(err)
	}
	approved, err := first.approve(ownerCtx(), prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	begin := func(l *candidateLedger) (candidatePublication, error) {
		return l.beginPublication(ownerCtx(), approved.ID, approved.ApprovalID, "registry", "engine", "image")
	}
	original, err := begin(&first)
	if err != nil {
		t.Fatal(err)
	}
	// The first process may die after COMMIT but before observing its answer.
	recovered, err := begin(&replacement)
	if err != nil {
		t.Fatal(err)
	}
	if original.EffectID == "" || original.EffectID != recovered.EffectID || recovered.State != "pending" {
		t.Fatal("lost reply minted a new publication intent")
	}
	if _, err := replacement.retire(ownerCtx(), approved.ID); err == nil {
		t.Fatal("retirement discarded an uncertain publication")
	}
	for _, args := range [][3]string{{"other", "engine", "image"}, {"registry", "other", "image"}, {"registry", "engine", "other"}} {
		if _, err := replacement.beginPublication(ownerCtx(), approved.ID, approved.ApprovalID, args[0], args[1], args[2]); err == nil {
			t.Fatal("publication escaped approved target/artifact set")
		}
	}
	if _, err := replacement.beginPublication(ownerCtx(), approved.ID, "other-approval", "registry", "engine", "image"); err == nil {
		t.Fatal("publication used an unrelated approval")
	}
	good := candidatePublicationReceipt{TargetDigest: recovered.Target.TargetDigest, ArchiveDigest: recovered.Artifact.Digest, ImageDigest: recovered.Artifact.ImageDigest, Platform: recovered.Artifact.Platform}
	wrong := good
	wrong.ArchiveDigest = "sha256:" + strings.Repeat("f", 64)
	if err := replacement.finishPublication(ownerCtx(), recovered, wrong); err == nil {
		t.Fatal("different artifact marked published")
	}
	if err := replacement.finishPublication(ownerCtx(), recovered, good); err != nil {
		t.Fatal(err)
	}
	if err := first.finishPublication(ownerCtx(), original, good); err != nil {
		t.Fatal("lost completion reply was not recoverable", err)
	}
	complete, err := begin(&replacement)
	if err != nil || complete.State != "complete" || complete.EffectID != original.EffectID {
		t.Fatal("publication receipt did not survive replacement", err)
	}
	if _, err := db.Exec(`UPDATE release_publication_intents SET receipt=jsonb_set(receipt,'{platform}','"linux/amd64"'::jsonb) WHERE effect_id=$1`, original.EffectID); err != nil {
		t.Fatal(err)
	}
	if _, err := begin(&replacement); err == nil {
		t.Fatal("corrupt stored publication receipt accepted")
	}
}

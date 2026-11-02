package release

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// draftUploadFixture is one approved file candidate with its draft bound, ready
// for an upload to enter the ledger.
func draftUploadFixture(t *testing.T, db *sql.DB) (*candidatePreparer, candidateRecord, candidatePublishRequest, candidateDraftPlan) {
	t.Helper()
	p, c, _, _ := candidateFilePublicationFixture(t, db)
	return draftUploadFixtureFor(t, db, p, c)
}

func draftUploadFixtureFor(t *testing.T, db *sql.DB, p *candidatePreparer, c pl.ReleaseCandidate) (*candidatePreparer, candidateRecord, candidatePublishRequest, candidateDraftPlan) {
	t.Helper()
	pub := preparePublication(t, db, p, c)
	rec, err := p.ledger.get(ownerCtx(), pub.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.targetReader.draftPlan(ownerCtx(), rec, pub.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ledger.beginDraft(ownerCtx(), rec.ID, pub.ApprovalID, plan); err != nil {
		t.Fatal(err)
	}
	return p, rec, pub, plan
}

// TestDraftUploadHoldsNoTransactionAcrossTheUpload is the request pool's own
// rule, at a limit a test can wait for. That pool ends a session idle in a
// transaction after 60 s, and a page read refuses after 8 s, so an upload holding
// one would fail after it had succeeded and blind the Releases screen meanwhile.
func TestDraftUploadHoldsNoTransactionAcrossTheUpload(t *testing.T) {
	db := candidateTestDB(t)
	p, rec, pub, plan := draftUploadFixture(t, db)
	tight := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"idle_in_transaction_session_timeout": "300"})))
	t.Cleanup(func() { tight.Close() })
	ledger := &candidateLedger{db: func() *sql.DB { return tight }}
	err := ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(context.Context, int64) error {
		time.Sleep(time.Second)
		read, cancel := context.WithTimeout(ownerCtx(), 500*time.Millisecond)
		defer cancel()
		if _, err := p.ledger.get(read, rec.ID); err != nil {
			return fmt.Errorf("the candidate stayed locked through the upload: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal("an upload longer than the idle limit failed:", err)
	}
}

func TestDraftUploadExcludesAnotherWriterUntilItFinishes(t *testing.T) {
	db := candidateTestDB(t)
	p, rec, pub, plan := draftUploadFixture(t, db)
	other := freshDraftHost(p, db)
	err := p.ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(context.Context, int64) error {
		secondEntered := false
		err := other.ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(context.Context, int64) error {
			secondEntered = true
			return nil
		})
		if err == nil || secondEntered {
			t.Fatal("a second upload entered while the first held the release")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(context.Context, int64) error { return nil }); err != nil {
		t.Fatal("a finished upload kept the release", err)
	}
}

func TestDraftUploadLeaseExpiresForAHolderThatDied(t *testing.T) {
	db := candidateTestDB(t)
	p, rec, pub, plan := draftUploadFixture(t, db)
	// A process that died mid-upload runs no close and no renewal.
	if _, err := p.ledger.claimDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan); err != nil {
		t.Fatal(err)
	}
	if err := freshDraftHost(p, db).ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(context.Context, int64) error { return nil }); err == nil {
		t.Fatal("a live lease admitted another writer")
	}
	if _, err := db.Exec(`UPDATE release_draft_intents SET upload_lease_until='epoch' WHERE resource_key=$1`, plan.ResourceKey); err != nil {
		t.Fatal(err)
	}
	if err := freshDraftHost(p, db).ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(context.Context, int64) error { return nil }); err != nil {
		t.Fatal("an expired lease still held the release", err)
	}
}

// Rolling the schema back during an upload would remove the fence around it.
func TestDraftUploadLeaseRollbackRefusesWhileAnUploadRuns(t *testing.T) {
	db := candidateTestDB(t)
	p, rec, pub, plan := draftUploadFixture(t, db)
	body, err := os.ReadFile("../../component/database/memory-nodes/migrations/20261101180000_release_draft_upload_lease.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(context.Context, int64) error {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(t.Context(), string(body)); err == nil || !strings.Contains(err.Error(), "still running") {
			t.Fatal("a running upload's fence could be rolled back", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// setLeaseTiming shortens both durable leases to what a test can wait for.
func setLeaseTiming(t *testing.T, ttl, renew time.Duration) {
	t.Helper()
	build, upload := buildLeaseTiming, uploadLeaseTiming
	buildLeaseTiming, uploadLeaseTiming = leaseTiming{ttl: ttl, renew: renew}, leaseTiming{ttl: ttl, renew: renew}
	t.Cleanup(func() { buildLeaseTiming, uploadLeaseTiming = build, upload })
}

func TestDraftUploadLeaseIsRenewedForAsLongAsTheUploadRuns(t *testing.T) {
	setLeaseTiming(t, 600*time.Millisecond, 100*time.Millisecond)
	db := candidateTestDB(t)
	p, rec, pub, plan := draftUploadFixture(t, db)
	other := freshDraftHost(p, db)
	err := p.ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(ctx context.Context, _ int64) error {
		// Outlive the lease several times over; it must still be held at the end.
		time.Sleep(2 * time.Second)
		if err := other.ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(context.Context, int64) error { return nil }); err == nil {
			t.Fatal("the lease lapsed during a live upload")
		}
		return ctx.Err()
	})
	if err != nil {
		t.Fatal("a renewed lease cut off its own upload:", err)
	}
}

func TestDraftUploadStopsWhenItsLeaseIsNoLongerHeld(t *testing.T) {
	setLeaseTiming(t, 600*time.Millisecond, 100*time.Millisecond)
	db := candidateTestDB(t)
	p, rec, pub, plan := draftUploadFixture(t, db)
	err := p.ledger.withDraftUpload(ownerCtx(), rec.ID, pub.ApprovalID, plan, func(ctx context.Context, _ int64) error {
		// Another holder took the lease after this one was presumed dead.
		if _, err := db.Exec(`UPDATE release_draft_intents SET upload_lease_id='another-holder' WHERE resource_key=$1`, plan.ResourceKey); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			t.Fatal("an upload kept running after losing its lease")
			return nil
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("the lost lease did not reach the upload", err)
	}
	var holder string
	if err := db.QueryRow(`SELECT upload_lease_id FROM release_draft_intents WHERE resource_key=$1`, plan.ResourceKey).Scan(&holder); err != nil || holder != "another-holder" {
		t.Fatal("the old holder released a lease it no longer held", holder, err)
	}
}

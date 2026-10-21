package pipelinerun

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type artifactRetentionLibraryFixture struct {
	ArtifactRetentionLibrary
	t                 *testing.T
	db                *sql.DB
	owner, work, keep string
	failPurge         bool
	effects           int
	adopted           string
}

func (l *artifactRetentionLibraryFixture) AdoptLegacyRunLog(ctx context.Context, s pipelinesteps.RunFileReceiptScope, file string) (pipelinesteps.StoredFileReceipt, error) {
	l.check(ctx, s)
	l.adopted = file
	return pipelinesteps.StoredFileReceipt{FileID: file, IntentID: "verified-legacy-receipt", Size: 11}, nil
}

func TestArtifactRetentionLegacyLogUsesTheOwnedCompletedStep(t *testing.T) {
	h, l, _ := artifactRetentionHarness(t, 0)
	h.step["key"] = "tests.unit"
	h.step["logFileId"] = "v1:library:file:legacy-log"
	got, err := h.peer.AdoptLegacyLog(personCtx(h.owner), h.run.ID, "tests.unit")
	if err != nil || got.FileID != "legacy-log" || l.adopted != "legacy-log" {
		t.Fatalf("adoption %+v %v", got, err)
	}
	l.adopted = ""
	if _, err := h.i.AdoptLegacyLog(personCtx(otherID), h.run.ID, "tests.unit"); err == nil || l.adopted != "" {
		t.Fatal("stranger reached legacy log", err)
	}
	h.step["attempt"] = 1.5
	if _, err := h.i.AdoptLegacyLog(personCtx(h.owner), h.run.ID, "tests.unit"); err == nil || l.adopted != "" {
		t.Fatal("fractional producer attempt adopted", err)
	}
	h.step["attempt"] = 1
	h.work["status"] = "running"
	if _, err := h.i.AdoptLegacyLog(personCtx(h.owner), h.run.ID, "tests.unit"); err == nil || l.adopted != "" {
		t.Fatal("active producer adopted", err)
	}
}

func (l *artifactRetentionLibraryFixture) check(ctx context.Context, s pipelinesteps.RunFileReceiptScope) {
	l.t.Helper()
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor.UserId != l.owner || !auth.OriginFromContext(ctx).IsInternal() || s.OwnerUserID != l.owner || s.WorkRunID != l.work || s.Attempt != 1 || s.StepKey != "tests.unit" {
		l.t.Fatal("artifact operation escaped its owner/producer scope", s)
	}
}

func (l *artifactRetentionLibraryFixture) FenceRunFileScope(ctx context.Context, s pipelinesteps.RunFileReceiptScope) error {
	l.check(ctx, s)
	return nil
}

func (l *artifactRetentionLibraryFixture) RetireRunFile(ctx context.Context, s pipelinesteps.RunFileReceiptScope, key string) (pipelinesteps.RetiredFileReceipt, error) {
	l.check(ctx, s)
	if key == l.keep {
		return pipelinesteps.RetiredFileReceipt{}, pipelinesteps.ErrArtifactRetained
	}
	res, err := l.db.ExecContext(ctx, `UPDATE pipeline_artifact_uploads SET state='retired' WHERE intent_id=$1 AND state<>'retired'`, key)
	if err != nil {
		return pipelinesteps.RetiredFileReceipt{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return pipelinesteps.RetiredFileReceipt{}, err
	}
	l.effects += int(n)
	return pipelinesteps.RetiredFileReceipt{IntentID: key, TombstoneETag: "confirmed"}, nil
}

func (l *artifactRetentionLibraryFixture) PurgeRetiredRunFile(ctx context.Context, s pipelinesteps.RunFileReceiptScope, key string) error {
	l.check(ctx, s)
	if l.failPurge {
		return errors.New("lost Library cleanup reply")
	}
	_, err := l.db.ExecContext(ctx, `UPDATE pipeline_artifact_uploads SET library_purged_at=clock_timestamp() WHERE intent_id=$1 AND state='retired'`, key)
	return err
}

func artifactRetentionHarness(t *testing.T, count int) (*sarifHarness, *artifactRetentionLibraryFixture, []string) {
	t.Helper()
	if _, err := dbtest.EnsureSchema(t.Context()); err != nil {
		t.Fatal(err)
	}
	h := newSARIFHarness(t)
	l := &artifactRetentionLibraryFixture{t: t, db: h.db, owner: h.owner, work: h.run.WorkRunID}
	for _, integ := range []*Integration{h.i, h.peer} {
		integ.Configure(func(d *Deps) {
			d.ArtifactRetention = &ArtifactRetentionDependencies{Database: func() *sql.DB { return h.db }, Engine: h.engine, Library: l}
		})
	}
	ids := make([]string, count)
	for n := range count {
		// Scope is isolated by a unique owner; derive the full key to avoid
		// collisions with another test's page in the shared database.
		ids[n] = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d", h.owner, n))))
		identity, _ := json.Marshal(pipelinesteps.RunFileReceiptScope{OwnerUserID: h.owner, WorkRunID: h.run.WorkRunID, StepKey: "tests.unit", Attempt: 1})
		_, err := h.db.Exec(`INSERT INTO pipeline_artifact_uploads(intent_id,owner_user_id,identity,file_id,object_key,size_bytes,generation,state)
VALUES($1,$2,$3::jsonb,$4,$5,11,1,'ready')`, ids[n], h.owner, string(identity), id.NewShortId(), "retention-test/"+ids[n])
		if err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(ids)
	t.Cleanup(func() {
		if _, err := h.db.Exec(`DELETE FROM pipeline_artifact_retention_queue WHERE owner_user_id=$1`, h.owner); err != nil {
			t.Error(err)
		}
		_, err := h.db.Exec(`DELETE FROM pipeline_artifact_uploads WHERE owner_user_id=$1`, h.owner)
		if err != nil {
			t.Error(err)
		}
	})
	return h, l, ids
}

func prioritizeArtifactRetentionTest(t *testing.T, h *sarifHarness) {
	t.Helper()
	if _, err := h.db.Exec(`UPDATE pipeline_artifact_retention_queue SET visited_at='-infinity' WHERE owner_user_id=$1`, h.owner); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactRetentionScheduledPolicyAndOwnerScope(t *testing.T) {
	for _, tc := range []struct {
		name          string
		event         pipelines.Event
		conclusion    string
		age           time.Duration
		active, purge bool
	}{
		{"young success", pipelines.EventManual, ConclusionSuccess, 47 * time.Hour, false, false},
		{"old success", pipelines.EventManual, ConclusionSuccess, 48 * time.Hour, false, true},
		{"young failure", pipelines.EventManual, ConclusionFailure, 6 * 24 * time.Hour, false, false},
		{"old failure", pipelines.EventManual, ConclusionFailure, 7 * 24 * time.Hour, false, true},
		{"young release", pipelines.EventRelease, ConclusionSuccess, 13 * 24 * time.Hour, false, false},
		{"old release", pipelines.EventRelease, ConclusionSuccess, 14 * 24 * time.Hour, false, true},
		{"active producer", pipelines.EventManual, ConclusionSuccess, 60 * 24 * time.Hour, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, l, _ := artifactRetentionHarness(t, 1)
			h.run.Event, h.run.Conclusion, h.run.FinishedAt = tc.event, tc.conclusion, testNow.Add(-tc.age)
			h.h.store.addRun(h.run)
			if tc.active {
				h.work["status"] = "running"
			}
			prioritizeArtifactRetentionTest(t, h)
			if _, err := h.i.SweepArtifacts(personCtx(h.owner)); err == nil {
				t.Fatal("external caller reached maintenance")
			}
			out, err := h.peer.SweepArtifacts(auth.ContextWithInternalOrigin(t.Context()))
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.purge {
				want = 1
			}
			if l.effects != want || out["purged"] != want {
				t.Fatalf("policy result %+v effects=%d want=%d", out, l.effects, want)
			}
		})
	}
}

func TestArtifactRetentionScheduledCursorSurvivesHostReplacement(t *testing.T) {
	h, l, ids := artifactRetentionHarness(t, 33)
	l.keep = ids[0]
	h.run.FinishedAt = testNow.Add(-48 * time.Hour)
	h.h.store.addRun(h.run)
	prioritizeArtifactRetentionTest(t, h)
	ctx := auth.ContextWithInternalOrigin(t.Context())
	first, err := h.i.SweepArtifacts(ctx)
	if err != nil || first["purged"] != 31 {
		t.Fatalf("first page %+v %v", first, err)
	}
	prioritizeArtifactRetentionTest(t, h)
	second, err := h.peer.SweepArtifacts(ctx)
	if err != nil || second["purged"] != 1 {
		t.Fatalf("second page %+v %v", second, err)
	}
	// The next cycle returns to retained files. Releasing the consumer later
	// must not strand them behind the end cursor.
	l.keep = ""
	prioritizeArtifactRetentionTest(t, h)
	third, err := h.peer.SweepArtifacts(ctx)
	if err != nil || third["purged"] != 1 || l.effects != 33 {
		t.Fatalf("retained retry %+v %v effects=%d", third, err, l.effects)
	}
}

func TestArtifactRetentionPagesAndPreviewAreOwnerScoped(t *testing.T) {
	h, l, ids := artifactRetentionHarness(t, 33)
	l.keep = ids[0]
	preview, err := h.i.RetireArtifacts(personCtx(h.owner), h.run.ID, "", true)
	if err != nil || preview.Inspected != 32 || !preview.HasMore || l.effects != 0 {
		t.Fatalf("preview %+v %v", preview, err)
	}
	if _, err := h.peer.RetireArtifacts(personCtx(otherID), h.run.ID, "", false); err == nil {
		t.Fatal("another owner reached cleanup")
	}
	h.work["status"] = "running"
	if _, err := h.peer.RetireArtifacts(personCtx(h.owner), h.run.ID, "", false); err == nil {
		t.Fatal("active producer reached cleanup")
	}
	h.work["status"] = "succeeded"
	first, err := h.i.RetireArtifacts(personCtx(h.owner), h.run.ID, "", false)
	if err != nil || first.Retained != 1 || first.Purged != 31 || !first.HasMore {
		t.Fatalf("first page %+v %v", first, err)
	}
	second, err := h.peer.RetireArtifacts(personCtx(h.owner), h.run.ID, first.AfterID, false)
	if err != nil || second.Purged != 1 || second.HasMore || l.effects != 32 {
		t.Fatalf("next host %+v %v effects=%d", second, err, l.effects)
	}
}

func TestArtifactRetentionResumesConfirmedRetirementOnAnotherHost(t *testing.T) {
	h, first, _ := artifactRetentionHarness(t, 1)
	first.failPurge = true
	if _, err := h.i.RetireArtifacts(personCtx(h.owner), h.run.ID, "", false); err == nil {
		t.Fatal("unconfirmed presentation cleanup reported success")
	}
	second := &artifactRetentionLibraryFixture{t: t, db: h.db, owner: h.owner, work: h.run.WorkRunID}
	h.peer.Configure(func(d *Deps) { copy := *d.ArtifactRetention; copy.Library = second; d.ArtifactRetention = &copy })
	out, err := h.peer.RetireArtifacts(personCtx(h.owner), h.run.ID, "", false)
	if err != nil || out.Purged != 1 || first.effects != 1 || second.effects != 0 {
		t.Fatalf("recovery %+v %v effects=%d/%d", out, err, first.effects, second.effects)
	}
}

func TestArtifactRetentionCannotForgeACompletedPage(t *testing.T) {
	h, l, _ := artifactRetentionHarness(t, 1)
	a := workflowSource(t, `@template
automation retirePipelineArtifacts {
 args { dryRun bool! }
 return builtin pipelineRetentionReport()
}`)
	h.i.Configure(func(d *Deps) { d.LoadWorkflow = workflowLoader(a) })
	if _, err := h.i.RetireArtifacts(personCtx(h.owner), h.run.ID, "", false); err == nil || l.effects != 0 {
		t.Fatal("recipe forged completion", err, l.effects)
	}
}

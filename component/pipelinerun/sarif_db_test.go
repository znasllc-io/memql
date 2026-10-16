package pipelinerun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type sarifRemoteFixture struct {
	*fakeGitHub
	posts, gets            atomic.Int32
	state                  string
	uploadErr, errorOnRead error
	entered, release       chan struct{}
	onUpload               func()
}

func (g *sarifRemoteFixture) UploadSARIF(ctx context.Context, token, repository string, report githubapp.SARIFAnalysis) (githubapp.SARIFUpload, error) {
	g.posts.Add(1)
	if g.onUpload != nil {
		g.onUpload()
	}
	if g.entered != nil {
		close(g.entered)
		<-g.release
	}
	h := sha256.Sum256(report.SARIF)
	return githubapp.SARIFUpload{ID: "upload-42", SARIFSHA256: hex.EncodeToString(h[:])}, g.uploadErr
}
func (g *sarifRemoteFixture) SARIFUploadStatus(context.Context, string, string, string) (githubapp.SARIFProcessing, error) {
	g.gets.Add(1)
	return githubapp.SARIFProcessing{Status: g.state, Errors: []string{"invalid report"}}, g.errorOnRead
}

type sarifLibraryFixture struct {
	SARIFLibrary
	t                     *testing.T
	receipt               pipelinesteps.StoredFileReceipt
	body                  []byte
	pins, opens, releases atomic.Int32
	onPin                 func()
}

func (l *sarifLibraryFixture) ReleaseRunFileReference(ctx context.Context, ref pipelinesteps.RunFileReference) error {
	l.assertScope(ctx, ref.Scope)
	l.releases.Add(1)
	return nil
}

func (l *sarifLibraryFixture) assertScope(ctx context.Context, s pipelinesteps.RunFileReceiptScope) {
	l.t.Helper()
	if auth.OriginFromContext(ctx) != auth.OriginInternal || s.OwnerUserID != l.receipt.OwnerUserID || s.WorkRunID != l.receipt.WorkRunID || s.StepKey != l.receipt.StepKey || s.Attempt != l.receipt.Attempt {
		l.t.Fatal("wrong artifact authority", s)
	}
}
func (l *sarifLibraryFixture) ReadRunFileReceipts(ctx context.Context, s pipelinesteps.RunFileReceiptScope, ids []string) ([]pipelinesteps.StoredFileReceipt, error) {
	l.assertScope(ctx, s)
	if len(ids) != 1 || ids[0] != l.receipt.IntentID {
		return nil, errors.New("wrong artifact identity")
	}
	return []pipelinesteps.StoredFileReceipt{l.receipt}, nil
}
func (l *sarifLibraryFixture) PinRunFileReceipts(ctx context.Context, ref pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	l.pins.Add(1)
	if l.onPin != nil {
		l.onPin()
	}
	return l.ReadRunFileReceipts(ctx, ref.Scope, ref.IntentIDs)
}
func (l *sarifLibraryFixture) OpenRunFileReceipt(ctx context.Context, s pipelinesteps.RunFileReceiptScope, key string) (pipelinesteps.StoredFileReceipt, io.ReadCloser, error) {
	l.assertScope(ctx, s)
	l.opens.Add(1)
	return l.receipt, io.NopCloser(bytes.NewReader(l.body)), nil
}

type sarifHarness struct {
	i, peer    *Integration
	h          *harness
	engine     *recordingEngine
	remote     *sarifRemoteFixture
	library    *sarifLibraryFixture
	run        Run
	db         *sql.DB
	owner      string
	work, step map[string]any
}

func newSARIFHarness(t *testing.T) *sarifHarness {
	t.Helper()
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "SARIF journal", dbtest.DSN(), err)
	}
	migration, err := os.ReadFile("../database/memory-nodes/migrations/20261016101000_pipeline_sarif.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	owner := id.NewShortId()
	runID := id.NewShortId()
	workID := id.NewShortId()
	p := testPipeline(DeliveryWebhook)
	p.OwnerUserID = owner
	h.store.addPipeline(p)
	r := Run{ID: runID, OwnerUserID: owner, PipelineID: p.ID, Repository: p.Repository, SHA: shaA, Mode: pipelines.ModeFull, Event: pipelines.EventSchedule, WorkRunID: workID, Status: StatusCompleted, Conclusion: "success", HeadBranch: "main", Attempt: 1, StartedAt: testNow}
	h.store.addRun(r)
	body := []byte(`{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"fixture"}},"results":[]}]}`)
	digest := sha256.Sum256(body)
	intent := strings.Repeat("c", 64)
	l := &sarifLibraryFixture{t: t, body: body, receipt: pipelinesteps.StoredFileReceipt{OwnerUserID: owner, WorkRunID: workID, StepKey: "analysis.go", Attempt: 1, IntentID: intent, FileID: id.NewShortId(), Path: ".reports/go.sarif", ETag: "immutable-etag", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(body))}}
	fp := "work-definition-v2:" + strings.Repeat("a", 64)
	work := map[string]any{"id": workID, "ownerUserId": owner, "status": "succeeded", "mode": "live", "templateFingerprint": fp, "triggeredBy": "pipeline:full", "input": map[string]any{"repository": r.Repository, "sha": r.SHA, "mode": "full", "event": "schedule", "pipelineId": p.ID, "pipelineRunId": r.ID, "attempt": 1}}
	step := map[string]any{"id": id.NewShortId(), "createdAt": "2026-10-16T10:00:00Z", "finishedAt": "2026-10-16T10:01:00Z", "ownerUserId": owner, "runId": workID, "key": "analysis.go", "status": "done", "stepType": "exec", "kind": "deterministic", "attempt": 1, "call": map[string]any{"construct": "pipeline", "pipelineKind": "command", "definitionFingerprint": fp}, "result": map[string]any{"status": "succeeded", "metadata": map[string]any{"exitCode": 0, "artifactIntentIds": []string{intent}}}}
	engine := newRecordingEngine()
	engine.answers["workRunForOwner"] = []any{work}
	engine.answers["workStepForOwnerRun"] = []any{step}
	g := &sarifRemoteFixture{fakeGitHub: h.github, state: "pending"}
	d := h.integ.snapshot()
	d.GitHub = g
	d.SARIF = &SARIFDependencies{Engine: engine, Database: func() *sql.DB { return db }, Library: l}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM pipeline_sarif_publications WHERE owner_user_id=$1`, owner); err != nil {
			t.Error(err)
		}
	})
	return &sarifHarness{i: New(d), peer: New(d), h: h, engine: engine, remote: g, library: l, run: r, db: db, owner: owner, work: work, step: step}
}
func (h *sarifHarness) publish(i *Integration) (SARIFPublication, error) {
	return i.PublishSARIF(personCtx(h.owner), h.run.ID, "analysis.go", ".reports/go.sarif")
}

func TestSARIFAcceptedUploadResumesOnAnotherHostWithoutAnotherPost(t *testing.T) {
	h := newSARIFHarness(t)
	first, err := h.publish(h.i)
	if err != nil || first.State != "accepted" || first.UploadID != "upload-42" {
		t.Fatalf("first %+v %v", first, err)
	}
	h.remote.state = "complete"
	second, err := h.publish(h.peer)
	if err != nil || second.State != "complete" || second.ID != first.ID || h.remote.posts.Load() != 1 {
		t.Fatalf("resumed %+v %v posts=%d", second, err, h.remote.posts.Load())
	}
	third, err := h.publish(h.i)
	if err != nil || third.State != "complete" || h.remote.gets.Load() != 2 || h.library.opens.Load() != 1 {
		t.Fatal("completed receipt repeated an effect", third, err)
	}
	if h.library.releases.Load() == 0 {
		t.Fatal("terminal publication retained its artifact pin forever")
	}
	h.library.body = nil // The retained receipt still works after object retention.
	fourth, err := h.peer.ReadSARIF(personCtx(h.owner), first.ID)
	if err != nil || fourth.State != "complete" || h.library.opens.Load() != 1 {
		t.Fatal(fourth, err)
	}
	if _, err := h.peer.ReadSARIF(personCtx(otherID), first.ID); err == nil {
		t.Fatal("another owner read the publication")
	}
	for _, call := range h.engine.recorded() {
		if call.origin == auth.OriginInternal || !call.fresh || call.actor.UserId != h.owner {
			t.Fatal("journal read escalated or cached", call)
		}
	}
}

func TestSARIFAcceptedPostWithLostJournalWriteDoesNotPostFromAnotherHost(t *testing.T) {
	h := newSARIFHarness(t)
	firstDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { firstDB.Close() })
	h.i.Configure(func(d *Deps) {
		copied := *d.SARIF
		copied.Database = func() *sql.DB { return firstDB }
		d.SARIF = &copied
	})
	h.remote.onUpload = func() { firstDB.Close() }
	if _, err := h.publish(h.i); err == nil {
		t.Fatal("lost journal write was hidden")
	}
	r, err := h.publish(h.peer)
	if err != nil || r.State != "uncertain" || h.remote.posts.Load() != 1 {
		t.Fatal("fresh host repeated a possibly accepted upload", r, err)
	}
	if h.library.releases.Load() != 0 {
		t.Fatal("uncertain upload lost its investigation evidence")
	}
}

func TestSARIFConcurrentHostsClaimOnlyOnePost(t *testing.T) {
	h := newSARIFHarness(t)
	h.remote.entered = make(chan struct{})
	h.remote.release = make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := h.publish(h.i); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-h.remote.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("POST was not reached")
	}
	r, err := h.publish(h.peer)
	close(h.remote.release)
	wg.Wait()
	if err != nil || r.State != "uncertain" || h.remote.posts.Load() != 1 {
		t.Fatal("second host repeated an unresolved POST", r, err)
	}
}

func TestSARIFLostReplyNeverRetriesAndKnownReceiptSurvivesStatusFailure(t *testing.T) {
	t.Run("lost upload reply", func(t *testing.T) {
		h := newSARIFHarness(t)
		h.remote.uploadErr = githubapp.ErrSARIFUploadUncertain
		r, err := h.publish(h.i)
		if err != nil || r.State != "uncertain" {
			t.Fatal(r, err)
		}
		r, err = h.publish(h.peer)
		if err != nil || r.State != "uncertain" || h.remote.posts.Load() != 1 || h.remote.gets.Load() != 0 {
			t.Fatal(r, err)
		}
	})
	t.Run("status unavailable", func(t *testing.T) {
		h := newSARIFHarness(t)
		h.remote.errorOnRead = errors.New("status unavailable")
		if _, err := h.publish(h.i); err == nil {
			t.Fatal("status failure hidden")
		}
		h.remote.errorOnRead = nil
		h.remote.state = "complete"
		r, err := h.publish(h.peer)
		if err != nil || r.State != "complete" || h.remote.posts.Load() != 1 {
			t.Fatal(r, err)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		h := newSARIFHarness(t)
		h.remote.uploadErr = errors.New("permission denied")
		r, err := h.publish(h.i)
		if err != nil || r.State != "rejected" {
			t.Fatal(r, err)
		}
		r, err = h.publish(h.peer)
		if err != nil || r.State != "rejected" || h.remote.posts.Load() != 1 {
			t.Fatal(r, err)
		}
	})
	t.Run("ingestion failed", func(t *testing.T) {
		h := newSARIFHarness(t)
		h.remote.state = "failed"
		r, err := h.publish(h.i)
		if err != nil || r.State != "failed" || len(r.Errors) != 1 {
			t.Fatal(r, err)
		}
	})
}

func TestSARIFRefusalsPrecedeExternalWrites(t *testing.T) {
	for _, name := range []string{"stranger", "wrong work source", "wrong work run", "failed scanner", "failed run", "changed bytes", "wrong path", "changed installation", "revoked while reading artifact"} {
		t.Run(name, func(t *testing.T) {
			h := newSARIFHarness(t)
			caller := h.owner
			filePath := ".reports/go.sarif"
			switch name {
			case "stranger":
				caller = otherID
			case "wrong work source":
				h.work["input"].(map[string]any)["sha"] = shaC
			case "wrong work run":
				h.work["input"].(map[string]any)["pipelineRunId"] = "other"
			case "failed scanner":
				h.step["status"] = "failed"
			case "failed run":
				r := h.run
				r.Conclusion = "failure"
				h.h.store.addRun(r)
			case "changed bytes":
				h.library.body = []byte("modified")
			case "wrong path":
				filePath = ".reports/missing.sarif"
			case "changed installation":
				h.remote.installationID = 8
			case "revoked while reading artifact":
				h.library.onPin = func() { p, _ := h.h.store.pipeline(h.run.PipelineID); p.CredentialID = ""; h.h.store.addPipeline(p) }
			}
			if _, err := h.i.PublishSARIF(personCtx(caller), h.run.ID, "analysis.go", filePath); err == nil {
				t.Fatal("unsafe upload accepted")
			}
			if h.remote.posts.Load() != 0 || h.remote.gets.Load() != 0 {
				t.Fatal("refusal reached remote effect")
			}
		})
	}
}

func TestSARIFRefUsesTheExactRunEvent(t *testing.T) {
	for _, tc := range []struct {
		r    Run
		want string
	}{
		{Run{Event: pipelines.EventRelease, Version: "v1.2.3"}, "refs/tags/v1.2.3"},
		{Run{Event: pipelines.EventPullRequest, PullRequest: 42}, "refs/pull/42/head"},
		{Run{Event: pipelines.EventMergeGroup, HeadBranch: "refs/heads/gh-readonly-queue/main/pr-42"}, "refs/heads/gh-readonly-queue/main/pr-42"},
		{Run{Event: pipelines.EventSchedule, HeadBranch: "main"}, "refs/heads/main"},
	} {
		got, err := sarifRef(tc.r)
		if err != nil || got != tc.want {
			t.Fatal(got, err)
		}
	}
	if _, err := sarifRef(Run{Event: pipelines.EventRelease, Version: "../main"}); err == nil {
		t.Fatal("invalid ref accepted")
	}
}

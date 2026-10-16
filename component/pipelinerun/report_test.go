package pipelinerun

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

const reportManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: analysis
      steps:
        - name: go
          run: scan
          artifacts: [.reports]
    - name: reports
      runAfterFailure: true
      steps:
        - name: go
          sarif: {step: analysis.go, path: .reports/go.sarif}
          timeout: 1m
`

// Read the actual real-workjournal writes with owner/fresh-read assertions.
// fakeWork stores mutation arguments; a real engine stamps these intrinsic
// fields, which this adapter supplies without inventing execution outcomes.
type reportJournalReader struct {
	h    *sarifHarness
	work *fakeWork
}

func (e reportJournalReader) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || ac.UserId != e.h.owner || !memql.FreshReadFromContext(ctx) || auth.OriginFromContext(ctx).IsInternal() {
		return nil, errors.New("report read escaped fresh owner authority")
	}
	var row map[string]any
	switch constructOf(query) {
	case "workRunForOwner":
		row = e.work.run(e.h.library.receipt.WorkRunID)
		row["id"] = row["runId"]
	case "workStepForOwnerRun":
		for _, candidate := range e.work.stepRows(bareID(e.h.library.receipt.WorkRunID)) {
			if candidate["key"] == "analysis.go" {
				row = candidate
				break
			}
		}
		if row == nil {
			return nil, errors.New("scanner receipt is missing from this attempt")
		}
		row["id"] = row["stepId"]
		row["createdAt"] = "2026-10-16T10:00:00Z"
	default:
		return nil, errors.New("unexpected report read")
	}
	row["ownerUserId"] = e.h.owner
	return memql.NewResultWithOutput([]any{row}), nil
}

func newReportDriveHarness(t *testing.T, state string) (*sarifHarness, *driveHarness) {
	t.Helper()
	h := newSARIFHarness(t)
	dh := newDriveHarness(t, reportManifest)
	dh.p.OwnerUserID = h.owner
	dh.store.addPipeline(dh.p)
	h.remote.fakeGitHub = dh.github
	h.remote.state = state
	if state == "uncertain" {
		h.remote.uploadErr = githubapp.ErrSARIFUploadUncertain
	}
	dh.integ.Configure(func(d *Deps) {
		d.GitHub = h.remote
		d.SARIF = &SARIFDependencies{Engine: reportJournalReader{h, dh.work}, Database: func() *sql.DB { return h.db }, Library: h.library}
	})
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.StepKey != "analysis.go" || len(req.Secrets) != 0 {
			t.Error("report reached worker or scanner received credentials", req.StepKey)
		}
		h.library.receipt.WorkRunID = req.WorkRunID
		r := passed(req)
		r.ArtifactIntentIDs = []string{h.library.receipt.IntentID}
		if state == "scanner failed" {
			r.Status = pipelines.OutcomeFailed
			r.ExitCode = 1
		}
		return r, nil
	}
	return h, dh
}

func TestReportStepPublishesOnlyTheJournaledScannerAndGatesTheCheck(t *testing.T) {
	for _, state := range []string{"complete", "failed", "uncertain", "scanner failed"} {
		t.Run(state, func(t *testing.T) {
			h, dh := newReportDriveHarness(t, state)
			run := dh.openRun(t, pushOpening())
			deliver(t, dh.integ, run)
			got, _ := dh.store.run(run.ID)
			want := ConclusionFailure
			if state == "complete" {
				want = ConclusionSuccess
			}
			if got.Conclusion != want {
				t.Fatalf("report state %s gave %+v\n%s", state, got, dh.logs.String())
			}
			if !slices.Equal(dh.exec.sentKeys(), []string{"analysis.go"}) {
				t.Fatal("report consumed worker capacity", dh.exec.sentKeys())
			}
			posts := int32(1)
			if state == "scanner failed" {
				posts = 0
			}
			if h.remote.posts.Load() != posts {
				t.Fatalf("posts=%d want=%d\n%s", h.remote.posts.Load(), posts, dh.logs.String())
			}
		})
	}
}

func TestReportRecipeOnlyObservesAcceptedUploads(t *testing.T) {
	for _, state := range []string{"accepted", "complete", "uncertain", "rejected", "failed"} {
		reads := 0
		got, err := workflowhost.Run(context.Background(), "pipelineSARIFDelivery", nil, workflowhost.Options{Operations: map[string]workflowhost.Operation{
			"pipelineReportPublish": func(context.Context, map[string]any) (any, error) { return map[string]any{"state": state}, nil },
			"pipelineReportObserve": func(_ context.Context, args map[string]any) (any, error) {
				reads++
				if rowInt(args, "waitSeconds") < 1 {
					t.Error("unbounded poll")
				}
				return map[string]any{"state": "complete"}, nil
			},
		}})
		if err != nil {
			t.Fatal(err)
		}
		wantReads := 0
		if state == "accepted" {
			wantReads = 1
		}
		if reads != wantReads || got == nil {
			t.Fatal("recipe repeated a terminal or uncertain effect", state, reads, got)
		}
	}
}

func TestSARIFPublicationIdentitySurvivesRunFailureOnAnotherHost(t *testing.T) {
	h := newSARIFHarness(t)
	r := h.run
	r.Status, r.Conclusion = StatusInProgress, ""
	h.h.store.addRun(r)
	h.work["status"] = "running"
	first, err := h.i.PublishSARIF(personCtx(h.owner), r.ID, "analysis.go", ".reports/go.sarif")
	if err != nil || first.State != "accepted" {
		t.Fatal(first, err)
	}
	r.Status, r.Conclusion = StatusCompleted, ConclusionFailure
	h.h.store.addRun(r)
	h.work["status"] = "failed"
	h.remote.state = "complete"
	second, err := h.peer.PublishSARIF(personCtx(h.owner), r.ID, "analysis.go", ".reports/go.sarif")
	if err != nil || second.ID != first.ID || second.State != "complete" || h.remote.posts.Load() != 1 {
		t.Fatal("completion reposted or lost upload", first, second, err)
	}
}

func TestReportDriverRecoveryUsesTheAcceptedUploadOnAnotherReplica(t *testing.T) {
	h, dh := newReportDriveHarness(t, "pending")
	run := dh.openRun(t, pushOpening())
	h.remote.onUpload = func() { dh.integ.driving(run.ID).lose() }
	deliver(t, dh.integ, run)
	prior, _ := dh.store.run(run.ID)
	if prior.Status == StatusCompleted || h.remote.posts.Load() != 1 {
		t.Fatalf("lost driver concluded or lost its effect: %+v posts=%d", prior, h.remote.posts.Load())
	}
	h.remote.onUpload = nil
	h.remote.state = "complete"
	peer := dh.peer("agent-b")
	peer.Configure(func(d *Deps) {
		d.Now = func() time.Time { return testNow.Add(leaseStaleAfter + time.Second) }
		d.GitHub = h.remote
		d.SARIF = dh.integ.snapshot().SARIF
	})
	if err := peer.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, peer)
	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess || got.DriverNodeID != "agent-b" || h.remote.posts.Load() != 1 {
		t.Fatalf("replacement lost or repeated accepted publication: %+v posts=%d\n%s", got, h.remote.posts.Load(), dh.logs.String())
	}
	if !slices.Equal(dh.exec.sentKeys(), []string{"analysis.go"}) {
		t.Fatal("driver recovery repeated scan", dh.exec.sentKeys())
	}
}

func TestReportRecipeCannotManufactureSuccessWithoutAnUpload(t *testing.T) {
	h, dh := newReportDriveHarness(t, "complete")
	a := workflowSource(t, `@template
 automation pipelineSARIFDelivery { return {state: "complete"} }`)
	dh.integ.Configure(func(d *Deps) { d.LoadWorkflow = workflowLoader(a) })
	run := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure || h.remote.posts.Load() != 0 {
		t.Fatal("recipe invented a delivery", got)
	}
}

func TestFailedOnlyReportRetryProducesArtifactsForItsOwnAttempt(t *testing.T) {
	h, dh := newReportDriveHarness(t, "failed")
	first := dh.openRun(t, pushOpening())
	deliver(t, dh.integ, first)
	if got, _ := dh.store.run(first.ID); got.Conclusion != ConclusionFailure {
		t.Fatal("failed ingestion was not recorded", got)
	}
	firstWorkRun := h.library.receipt.WorkRunID
	h.remote.state = "complete"
	next, err := dh.integ.Rerun(personCtx(h.owner), first.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	deliver(t, dh.integ, next)
	got, _ := dh.store.run(next.ID)
	if got.Conclusion != ConclusionSuccess || h.remote.posts.Load() != 2 || h.library.receipt.WorkRunID == firstWorkRun {
		t.Fatalf("retry did not prove delivery from its own scan: %+v posts=%d\n%s", got, h.remote.posts.Load(), dh.logs.String())
	}
	if !slices.Equal(dh.exec.sentKeys(), []string{"analysis.go", "analysis.go"}) {
		t.Fatal("new attempt carried a scanner without its own artifact receipt", dh.exec.sentKeys())
	}
}

func TestCancellingReportDeliveryDoesNotConcludeSuccess(t *testing.T) {
	h, dh := newReportDriveHarness(t, "pending")
	run := dh.openRun(t, pushOpening())
	h.remote.onUpload = func() { dh.integ.driving(run.ID).cancel() }
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionCancelled || h.remote.posts.Load() != 1 {
		t.Fatalf("cancelled report concluded incorrectly: %+v posts=%d\n%s", got, h.remote.posts.Load(), dh.logs.String())
	}
}

package pipelinerun

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// gate_discipline_test.go -- the gate discipline (fakes_test.go) is a property
// of EVERY test in this package, because the shared fake gate and the shared
// fake GitHub both enforce it. This file holds the two things that make that
// claim worth something: the checker is shown to report what it exists to
// report (a checker that reports nothing passes everything), and every gated
// path is walked once, in one place, so a path no other test reaches is not
// silently exempt.

// disciplineRecorder captures the discipline's failures instead of failing
// the test, so the checker itself can be asserted on.
type disciplineRecorder struct {
	testing.TB
	mu       sync.Mutex
	failures []string
}

func (r *disciplineRecorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *disciplineRecorder) took() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.failures
	r.failures = nil
	return out
}

func TestTheGateDisciplineIsChecked(t *testing.T) {
	rec := &disciplineRecorder{TB: t}
	gate := newKeyedGate(rec)
	gh := newFakeGitHub(rec)
	ctx := context.Background()
	open := OpenGateKey("acme/shop@" + shaA + ":affected:pull_request")

	for _, c := range []struct {
		name string
		do   func() error
		want string // "" is no failure
	}{
		{"a GitHub call outside every gate", func() error {
			_, _, err := gh.InstallationToken(ctx, credID, ownerID, repoName)
			return err
		}, ""},
		{"two gates one after the other", func() error {
			if err := gate.run(ctx, RunGateKey("r1"), func(context.Context) error { return nil }); err != nil {
				return err
			}
			return gate.run(ctx, RepositoryGateKey(repoName), func(context.Context) error { return nil })
		}, ""},
		{"a gate under a gate", func() error {
			return gate.run(ctx, RepositoryGateKey(repoName), func(gctx context.Context) error {
				return gate.run(gctx, open, func(context.Context) error { return nil })
			})
		}, "was asked for while"},
		{"a token minted under a gate", func() error {
			return gate.run(ctx, open, func(gctx context.Context) error {
				_, _, err := gh.InstallationToken(gctx, credID, ownerID, repoName)
				return err
			})
		}, "InstallationToken was called while"},
		{"a check run moved under a run's gate", func() error {
			return gate.run(ctx, RunGateKey("r1"), func(gctx context.Context) error {
				return gh.UpdateCheckRun(gctx, "tok", repoName, 9, githubapp.CheckRun{})
			})
		}, "UpdateCheckRun was called while"},
		{"a check run created under a repository's gate", func() error {
			return gate.run(ctx, RepositoryGateKey(repoName), func(gctx context.Context) error {
				_, err := gh.CreateCheckRun(gctx, "tok", repoName, githubapp.CheckRun{})
				return err
			})
		}, "CreateCheckRun was called while"},
		{"the check-run create under the run key's open gate", func() error {
			return gate.run(ctx, open, func(gctx context.Context) error {
				_, err := gh.CreateCheckRun(gctx, "tok", repoName, githubapp.CheckRun{})
				return err
			})
		}, ""},
	} {
		if err := c.do(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := rec.took()
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("%s: reported %v, want nothing", c.name, got)
		case c.want != "" && (len(got) != 1 || !strings.Contains(got[0], c.want)):
			t.Errorf("%s: reported %v, want one failure saying %q", c.name, got, c.want)
		}
	}
}

// TestEveryGatedPathKeepsTheGateDiscipline walks every path that takes a gate
// -- the opening half and the driver -- on the shared fakes, which fail the
// test on a nested gate or a GitHub call under one. Most of these paths have
// tests of their own that now run under the same fakes; this one exists so
// that a path without one is not exempt, and it asserts each path reached
// its gate, so a path that stopped short of the gate cannot pass it vacuously.
func TestEveryGatedPathKeepsTheGateDiscipline(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	p := dh.p
	sawKey := func(t *testing.T, before int, want string) {
		t.Helper()
		for _, k := range dh.gate.seen()[before:] {
			if k == want {
				return
			}
		}
		t.Errorf("the path never took %q (took %v)", want, dh.gate.seen()[before:])
	}

	t.Run("a delivery opens a run, and a fork's is refused", func(t *testing.T) {
		before := len(dh.gate.seen())
		res := trigger(t, dh.harness, "pull_request", "d-pr", prDelivery(t, "opened", 42, shaA, repoName, testInstallation))
		if len(res.Opened) != 1 {
			t.Fatalf("opened %+v", res)
		}
		sawKey(t, before, OpenGateKey(res.Opened[0].RunKey))
		fork := trigger(t, dh.harness, "pull_request", "d-fork", prDelivery(t, "opened", 7, shaB, "mallory/shop", testInstallation))
		if len(fork.Opened) != 1 || fork.Opened[0].RefusalCode != pipelines.CodeForkRefused {
			t.Fatalf("fork %+v", fork)
		}
	})

	t.Run("a release resolves its tag and opens", func(t *testing.T) {
		dh.github.mu.Lock()
		dh.github.commits[repoName+"@refs/tags/v1.0.0"] = headAnswer{SHA: shaC}
		dh.github.mu.Unlock()
		res := trigger(t, dh.harness, "release", "d-rel", releaseDelivery(t, "v1.0.0", testInstallation))
		if len(res.Opened) != 1 {
			t.Fatalf("opened %+v", res)
		}
	})

	t.Run("a driven run, concluded, teaches the timings", func(t *testing.T) {
		// Only a step that declares shards teaches the table (timings.go), and the
		// merge is what takes the repository's gate here.
		dh.setTree(shaA, timingManifest)
		defer dh.setTree(shaA, driveManifest)
		dh.addTimedPackages()
		dh.exec.mu.Lock()
		dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
			res := passed(req)
			res.Timings = map[string]float64{"acme.test/shop": 1.5}
			return res, nil
		}
		dh.exec.mu.Unlock()
		before := len(dh.gate.seen())
		run := dh.openRun(t, pushOpening())
		deliver(t, dh.integ, run)
		got, _ := dh.store.run(run.ID)
		if got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess {
			rows, _ := dh.store.WorkSteps(context.Background(), got.WorkRunID)
			t.Fatalf("run %s/%s, steps %+v\n%s", got.Status, got.Conclusion, rows, dh.logs.String())
		}
		sawKey(t, before, RunGateKey(run.ID))
		sawKey(t, before, RepositoryGateKey(repoName))
		if learned, _ := dh.store.pipeline(p.ID); learned.Timings["acme.test/shop"] != 1.5 {
			t.Errorf("the full run taught the table: %v", learned.Timings)
		}
	})

	t.Run("a re-run and a cancel", func(t *testing.T) {
		done := dh.store.allRuns()
		var finished Run
		for _, r := range done {
			if r.Finished() && r.RefusalCode == "" {
				finished = r
			}
		}
		next, err := dh.integ.Rerun(personCtx(ownerID), finished.ID, false)
		if err != nil {
			t.Fatalf("rerun: %v", err)
		}
		before := len(dh.gate.seen())
		if err := dh.integ.RequestCancel(context.Background(), next.ID, ownerID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		sawKey(t, before, RunGateKey(next.ID))
		if got, _ := dh.store.run(next.ID); got.Conclusion != ConclusionCancelled {
			t.Errorf("the undriven re-run is concluded cancelled: %+v", got)
		}
	})

	t.Run("the poll: a baseline, then a moved head", func(t *testing.T) {
		polled := p
		polled.ID, polled.PackageID, polled.Repository, polled.Delivery = PipelineIDFor("pkg-polled"), "pkg-polled", "acme/polled", DeliveryPoll
		dh.store.addPipeline(polled)
		dh.github.mu.Lock()
		dh.github.heads["acme/polled@main"] = headAnswer{SHA: shaA}
		dh.github.mu.Unlock()
		before := len(dh.gate.seen())
		if _, err := dh.integ.Poll(automationCtx()); err != nil {
			t.Fatalf("poll: %v", err)
		}
		sawKey(t, before, RepositoryGateKey("acme/polled"))
		dh.github.mu.Lock()
		dh.github.heads["acme/polled@main"] = headAnswer{SHA: shaB}
		dh.github.mu.Unlock()
		res, err := dh.integ.Poll(automationCtx())
		if err != nil || len(res.Opened) != 1 {
			t.Fatalf("poll: %+v %v", res, err)
		}
		waitDrives(t, dh.integ)
	})

	t.Run("a push supersedes its pull request's earlier run", func(t *testing.T) {
		// The first delivery's run of #42, still queued: no agent claimed it.
		var earlier Run
		for _, r := range dh.store.allRuns() {
			if r.PullRequest == 42 && r.Mode == pipelines.ModeAffected && !r.Finished() {
				earlier = r
			}
		}
		if earlier.ID == "" {
			t.Fatalf("no unfinished run of #42 is left to supersede: %+v", dh.store.allRuns())
		}
		before, readsBefore := len(dh.gate.seen()), len(dh.github.headReads())
		dh.github.setPullHead(repoName, 42, shaD)
		res := trigger(t, dh.harness, "pull_request", "d-pr-push", prDelivery(t, "synchronize", 42, shaD, repoName, testInstallation))
		if len(res.Opened) != 1 {
			t.Fatalf("opened %+v", res)
		}
		sawKey(t, before, OpenGateKey(res.Opened[0].RunKey))
		sawKey(t, before, RunGateKey(earlier.ID))
		if reads := dh.github.headReads()[readsBefore:]; len(reads) != 1 {
			t.Errorf("the push read #42's head from GitHub once, ungated: %v", reads)
		}
		if got, _ := dh.store.run(earlier.ID); got.Conclusion != ConclusionCancelled || got.CancelledBy != "superseded by "+res.Opened[0].ID {
			t.Errorf("the superseded run is concluded cancelled, by the push's run: %s by %q", got.Conclusion, got.CancelledBy)
		}
	})

	t.Run("a force-push back to the superseded head runs it again (R37)", func(t *testing.T) {
		// #42's run at shaA, superseded above by the push to shaD.
		var superseded Run
		for _, r := range dh.store.allRuns() {
			if r.PullRequest == 42 && r.SHA == shaA && r.Mode == pipelines.ModeAffected && strings.HasPrefix(r.CancelledBy, "superseded by ") {
				superseded = r
			}
		}
		if superseded.ID == "" {
			t.Fatalf("no superseded run of #42 at shaA: %+v", dh.store.allRuns())
		}
		// Where the gate sequence stood at each head read.
		var (
			mu     sync.Mutex
			atRead []int
		)
		dh.integ.Configure(func(d *Deps) {
			d.GitHub = hookedGitHub{fakeGitHub: dh.github, afterHeadRead: func(context.Context) {
				mu.Lock()
				defer mu.Unlock()
				atRead = append(atRead, len(dh.gate.seen()))
			}}
		})
		before, readsBefore := len(dh.gate.seen()), len(dh.github.headReads())
		dh.github.setPullHead(repoName, 42, shaA)
		res := trigger(t, dh.harness, "pull_request", "d-pr-back", prDelivery(t, "synchronize", 42, shaA, repoName, testInstallation))
		if len(res.Opened) != 1 || res.Opened[0].RunKey != superseded.RunKey || res.Opened[0].Attempt != superseded.Attempt+1 {
			t.Fatalf("the force-push back opens the key's next attempt: %+v", res)
		}
		openKey := OpenGateKey(superseded.RunKey)
		sawKey(t, before, openKey)
		gateAt := before + slices.Index(dh.gate.seen()[before:], openKey)
		mu.Lock()
		defer mu.Unlock()
		if reads := dh.github.headReads()[readsBefore:]; len(reads) != 2 || len(atRead) != 2 {
			t.Fatalf("want the dedup's head read and the supersede's: %v (at %v)", reads, atRead)
		}
		if atRead[0] > gateAt {
			t.Errorf("the dedup's head read came after the open gate %q was taken (read at %d, gate at %d)", openKey, atRead[0], gateAt)
		}
		if atRead[1] <= gateAt {
			t.Errorf("the supersede's head read came before the open gate (read at %d, gate at %d)", atRead[1], gateAt)
		}
	})

	t.Run("connect, reconnect elsewhere, disconnect", func(t *testing.T) {
		h := connectHarness(t, connectManifest)
		before := len(h.gate.seen())
		if _, err := connect(h, personCtx(ownerID), ConnectRequest{}); err != nil {
			t.Fatalf("connect: %v", err)
		}
		for _, k := range h.gate.seen()[before:] {
			if k != RepositoryGateKey(repoName) {
				t.Errorf("connect took %q; a connect takes the repository's gate and no other", k)
			}
		}
		moved := testPipeline(DeliveryPoll)
		moved.Repository, moved.Heads = "acme/old-shop", map[string]string{"branch:main": shaC}
		h.store.addPipeline(moved)
		if _, err := connect(h, personCtx(ownerID), ConnectRequest{Delivery: DeliveryPoll}); err != nil {
			t.Fatalf("reconnect: %v", err)
		}
		if _, err := h.integ.Disconnect(personCtx(ownerID), PipelineIDFor(packageID)); err != nil {
			t.Fatalf("disconnect: %v", err)
		}
	})
}

// TestThePushLaneKeepsTheGateDiscipline walks every path the push lane adds
// (push_lane.go) on the shared fakes, which fail the test on a nested gate or
// a call to GitHub under one: a push that supersedes a waiting push run, a
// claim the lane holds, recovery's lane pass, and the hand-off a concluded
// drive makes. Each asserts it reached the gate it is about, so a path that
// stopped short cannot pass vacuously.
func TestThePushLaneKeepsTheGateDiscipline(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	optInPushLane(t, dh.integ)
	p := dh.p
	policy := laneSnapshot(t, dh.integ, p)
	sawKey := func(t *testing.T, before int, want string) {
		t.Helper()
		if !slices.Contains(dh.gate.seen()[before:], want) {
			t.Errorf("the path never took %q (took %v)", want, dh.gate.seen()[before:])
		}
	}
	executing := drivenBy(pushRunAt(p, shaA, 601, policy), "agent-b")
	waiting := pushRunAt(p, shaB, 602, policy)
	dh.store.addRun(executing)
	dh.store.addRun(waiting)

	t.Run("a push supersedes the waiting push run it covers", func(t *testing.T) {
		dh.github.setAncestor(repoName, shaB, shaC)
		before := len(dh.gate.seen())
		opened := pushToMain(t, dh.harness, "d-lane-push", shaB, shaC)
		sawKey(t, before, OpenGateKey(opened.RunKey))
		sawKey(t, before, RunGateKey(waiting.ID))
		if got, _ := dh.store.run(waiting.ID); got.CancelledBy != supersededPrefix+opened.ID {
			t.Errorf("the waiting push is superseded by the new one: %+v", got)
		}
	})

	t.Run("a claim the lane holds", func(t *testing.T) {
		held := pushRunAt(p, shaC, 603, policy)
		held.ID = "lane-held"
		dh.store.addRun(held)
		before := len(dh.gate.seen())
		if _, ok, err := dh.integ.claim(context.Background(), dh.integ.snapshot(), held.ID); err != nil || ok {
			t.Fatalf("held: %v %v", ok, err)
		}
		if keys := dh.gate.seen()[before:]; !slices.Equal(keys, []string{RunGateKey(held.ID)}) {
			t.Errorf("a held claim takes its own run's gate and no other: %v", keys)
		}
		dh.store.mu.Lock()
		delete(dh.store.runs, held.ID)
		dh.store.mu.Unlock()
	})

	t.Run("recovery supersedes a late push toward the head's run", func(t *testing.T) {
		// shaC's run, opened above, is the head's; shaD's delivery came late.
		late := pushRunAt(p, shaD, 604, policy)
		late.QueuedAt = testNow.Add(-time.Minute)
		dh.store.addRun(late)
		dh.github.setAncestor(repoName, shaD, shaC)
		before := len(dh.gate.seen())
		if err := dh.integ.RecoverRuns(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitDrives(t, dh.integ)
		sawKey(t, before, RunGateKey(late.ID))
		if got, _ := dh.store.run(late.ID); got.Conclusion != ConclusionCancelled {
			t.Errorf("the late push is superseded: %+v", got)
		}
	})

	t.Run("a concluded drive hands the lane on", func(t *testing.T) {
		// The run ahead finishes on agent-b; the head's run (shaC) waits.
		dh.store.mu.Lock()
		r := dh.store.runs[executing.ID]
		r.Status, r.Conclusion, r.FinishedAt = StatusCompleted, ConclusionSuccess, testNow
		dh.store.runs[executing.ID] = r
		dh.store.mu.Unlock()
		dh.setTree(shaC, driveManifest)
		var head Run
		for _, run := range dh.store.allRuns() {
			if run.SHA == shaC && run.Event == pipelines.EventPush {
				head = run
			}
		}
		// A run of its own concludes here, and its drive hands the lane on.
		ahead := pushRunAt(p, shaBase, 600, policy)
		ahead.ID = "lane-ahead"
		dh.store.addRun(ahead)
		dh.setTree(shaBase, driveManifest)
		before := len(dh.gate.seen())
		dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, ahead))
		waitDrives(t, dh.integ)
		sawKey(t, before, RunGateKey(ahead.ID))
		sawKey(t, before, RunGateKey(head.ID))
		if got, _ := dh.store.run(head.ID); got.Conclusion != ConclusionSuccess {
			t.Errorf("the head's push ran once the lane was handed on: %s/%s", got.Status, got.Conclusion)
		}
	})
}

package pipelinerun

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// supersede_test.go -- a new push to a pull request stops the pull request's
// earlier affected-mode runs, and nothing else (design record D11; epic
// memql#5478, Task 13 / memql#5496).
//
// Every test that asserts a run was LEFT ALONE also holds a run the same push
// does stop: a push that stopped nothing at all would otherwise pass them.

// shaD is a fourth head: a pull request pushed more than twice.
const shaD = "4444444444444444444444444444444444444444"

// prRun is pull request pr's run of head sha on p, as an earlier push left
// it: affected, attempt 1, queued ten minutes ago by a webhook, reporting
// check run checkRunID, and claimed by no agent yet.
func prRun(p Pipeline, pr int, sha string, checkRunID int64) Run {
	key := pipelines.RunKey(p.Repository, sha, pipelines.ModeAffected, pipelines.EventPullRequest)
	return Run{
		ID: RunIDFor(p.ID, key, 1), OwnerUserID: p.OwnerUserID, AccountID: p.AccountID, PipelineID: p.ID,
		Repository: p.Repository, SHA: sha, Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest,
		RunKey: key, Attempt: 1, Trigger: TriggerWebhook, PullRequest: pr, BaseSHA: shaBase, HeadBranch: "cart-badge",
		Version: sha, Status: StatusQueued, CheckRunID: checkRunID, CheckRunState: CheckRunWritten,
		QueuedAt: testNow.Add(-tenMinutes),
	}
}

// drivenBy is r claimed and in progress on node.
func drivenBy(r Run, node string) Run {
	r.Status, r.DriverNodeID, r.StartedAt, r.DriverHeartbeatAt = StatusInProgress, node, testNow.Add(-5*time.Minute), testNow
	return r
}

// push42 is a push to pull request #42's branch, delivered: GitHub's
// synchronize, to head sha. It answers the run the push opened.
func push42(t *testing.T, h *harness, deliveryID, sha string) Run {
	t.Helper()
	res := trigger(t, h, "pull_request", deliveryID, prDelivery(t, "synchronize", 42, sha, repoName, testInstallation))
	if len(res.Opened) != 1 {
		t.Fatalf("the push to #42 opened %d runs (existing %d, skipped %+v); want its head's one", len(res.Opened), len(res.Existing), res.Skipped)
	}
	return res.Opened[0]
}

// wantAsked holds the run id to having been asked to stop by the run whose id
// is by's: the ask and who asked recorded. A driven run stays in progress --
// its driver concludes it -- so the status is the caller's to check.
func wantAsked(t *testing.T, h *harness, id, supersededBy string) Run {
	t.Helper()
	got, ok := h.store.run(id)
	if !ok {
		t.Fatalf("no run %s", id)
	}
	if want := "superseded by " + supersededBy; !got.CancelRequested || got.CancelledBy != want {
		t.Errorf("run %s: cancelRequested %v cancelledBy %q; want the ask, by %q", id, got.CancelRequested, got.CancelledBy, want)
	}
	return got
}

// wantUntouched holds a run to exactly what it was: never asked to stop, its
// status and conclusion unmoved, and its gate never taken -- so not even asked
// and refused.
func wantUntouched(t *testing.T, h *harness, before Run) {
	t.Helper()
	got, _ := h.store.run(before.ID)
	if got.CancelRequested || got.CancelledBy != before.CancelledBy || got.Status != before.Status || got.Conclusion != before.Conclusion {
		t.Errorf("run %s (%s #%d %s) was touched: %s/%s cancelRequested %v by %q",
			before.ID, before.Mode, before.PullRequest, before.SHA[:7], got.Status, got.Conclusion, got.CancelRequested, got.CancelledBy)
	}
	for _, k := range h.gate.seen() {
		if k == RunGateKey(before.ID) {
			t.Errorf("run %s's gate was taken: something asked it to stop", before.ID)
			return
		}
	}
}

// checkRunWrites is every write GitHub saw to check run id.
func checkRunWrites(h *harness, id int64) []checkWrite {
	var out []checkWrite
	for _, u := range h.github.updatedRuns() {
		if u.ID == id {
			out = append(out, u)
		}
	}
	return out
}

func TestANewPushCancelsTheRunningAffectedRunOfTheSamePullRequest(t *testing.T) {
	t.Run("a webhook delivers the push", func(t *testing.T) {
		h := newHarness(t)
		p := testPipeline(DeliveryWebhook)
		h.store.addPipeline(p)
		// Two earlier heads of #42, neither finished: one an agent on another
		// replica drives, one no agent has claimed yet.
		driven := drivenBy(prRun(p, 42, shaA, 501), "agent-b")
		queued := prRun(p, 42, shaB, 502)
		h.store.addRun(driven)
		h.store.addRun(queued)

		opened := push42(t, h, "d-push", shaC)

		// The driven run is ASKED: the row records it, and the replica driving
		// it stops what is executing and concludes it.
		if got := wantAsked(t, h, driven.ID, opened.ID); got.Status != StatusInProgress || got.Conclusion != "" {
			t.Errorf("a run another replica drives is concluded by its driver, not by the push: %s/%s", got.Status, got.Conclusion)
		}
		if w := checkRunWrites(h, 501); len(w) != 0 {
			t.Errorf("the driven run's check run is its driver's to move: %+v", w)
		}
		// The queued run nobody drives is CONCLUDED, its check run with it.
		got := wantAsked(t, h, queued.ID, opened.ID)
		if got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled || got.FinishedAt.IsZero() {
			t.Errorf("a queued run nobody drives is concluded cancelled on the spot: %s/%s finished %v", got.Status, got.Conclusion, got.FinishedAt)
		}
		if w := checkRunWrites(h, 502); len(w) != 1 || w[0].Run.Status != "completed" || w[0].Run.Conclusion != "cancelled" {
			t.Errorf("its check run is moved to cancelled: %+v", w)
		}
		// The new head's run is the pull request's run now, and untouched.
		if now, _ := h.store.run(opened.ID); now.Status != StatusQueued || now.CancelRequested || now.PullRequest != 42 || now.SHA != shaC {
			t.Errorf("the new head's run = %+v", now)
		}
	})

	t.Run("the poll sees the push", func(t *testing.T) {
		h := newHarness(t)
		p := testPipeline(DeliveryPoll)
		p.Heads = map[string]string{"branch:main": shaBase, "pr:42": shaA}
		h.store.addPipeline(p)
		h.github.heads[repoName+"@main"] = headAnswer{SHA: shaBase}
		h.github.pulls[repoName] = []githubapp.PullRequestHead{
			{Number: 42, Title: "Show the cart count", HeadSHA: shaC, HeadRef: "cart-badge", HeadRepository: repoName, BaseSHA: shaBase},
		}
		driven := drivenBy(prRun(p, 42, shaA, 501), "agent-b")
		h.store.addRun(driven)

		res := poll(t, h)
		if len(res.Opened) != 1 || res.Opened[0].SHA != shaC || res.Opened[0].Trigger != TriggerPoll {
			t.Fatalf("the poll opens #42's new head: %+v", res)
		}
		wantAsked(t, h, driven.ID, res.Opened[0].ID)
	})
}

// A full run is never stopped by a push: the default branch's push and the
// merge queue's run run to their end. The merge queue's run here CARRIES the
// pull request it merges, and is queued with no driver -- the run a cancel
// would conclude on the spot -- so nothing but its mode keeps it running.
func TestAFullRunIsNeverCancelledByAPush(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	pushKey := pipelines.RunKey(repoName, shaA, pipelines.ModeFull, pipelines.EventPush)
	mainPush := drivenBy(Run{
		ID: RunIDFor(p.ID, pushKey, 1), OwnerUserID: p.OwnerUserID, PipelineID: p.ID, Repository: repoName, SHA: shaA,
		Mode: pipelines.ModeFull, Event: pipelines.EventPush, RunKey: pushKey, Attempt: 1, Trigger: TriggerWebhook,
		HeadBranch: "main", CheckRunID: 601, CheckRunState: CheckRunWritten, QueuedAt: testNow.Add(-tenMinutes),
	}, "agent-b")
	queueKey := pipelines.RunKey(repoName, shaB, pipelines.ModeFull, pipelines.EventMergeGroup)
	mergeQueue := Run{
		ID: RunIDFor(p.ID, queueKey, 1), OwnerUserID: p.OwnerUserID, PipelineID: p.ID, Repository: repoName, SHA: shaB,
		Mode: pipelines.ModeFull, Event: pipelines.EventMergeGroup, RunKey: queueKey, Attempt: 1, Trigger: TriggerWebhook,
		PullRequest: 42, Status: StatusQueued, CheckRunID: 602, CheckRunState: CheckRunWritten, QueuedAt: testNow.Add(-tenMinutes),
	}
	// The control: #42's own earlier affected run, which the same push stops.
	earlier := drivenBy(prRun(p, 42, shaC, 603), "agent-b")
	for _, r := range []Run{mainPush, mergeQueue, earlier} {
		h.store.addRun(r)
	}

	opened := push42(t, h, "d-push", shaD)

	wantUntouched(t, h, mainPush)
	wantUntouched(t, h, mergeQueue)
	if w := checkRunWrites(h, 602); len(w) != 0 {
		t.Errorf("the merge queue's check run was moved: %+v", w)
	}
	wantAsked(t, h, earlier.ID, opened.ID)
}

func TestAnotherPullRequestsRunIsUntouched(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	// #43, mid-run, and #44, queued: other pull requests' heads.
	other := drivenBy(prRun(p, 43, shaA, 701), "agent-b")
	queuedOther := prRun(p, 44, shaB, 702)
	// #42 of the repository's previous pipeline, disconnected and still
	// running: the same number, and another pipeline's run all the same.
	old := testPipeline(DeliveryWebhook)
	old.ID, old.PackageID, old.Status = PipelineIDFor("pkg-old"), "pkg-old", PipelineDisconnected
	h.store.addPipeline(old)
	predecessor := drivenBy(prRun(old, 42, shaB, 703), "agent-b")
	// The control: #42's own earlier run.
	earlier := drivenBy(prRun(p, 42, shaC, 704), "agent-b")
	for _, r := range []Run{other, queuedOther, predecessor, earlier} {
		h.store.addRun(r)
	}

	opened := push42(t, h, "d-push", shaD)

	wantUntouched(t, h, other)
	wantUntouched(t, h, queuedOther)
	wantUntouched(t, h, predecessor)
	wantAsked(t, h, earlier.ID, opened.ID)
}

// A second sighting of a head -- a redelivered webhook, or a webhook and the
// poll for one head -- opens nothing, and so supersedes nothing: the run that
// answers it superseded what it superseded when it opened.
func TestADedupHitCancelsNothing(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	// The poll opened #42's head at shaC a minute ago; the run of the head
	// before it is still going -- that opening's ask never landed.
	earlier := drivenBy(prRun(p, 42, shaA, 501), "agent-b")
	seen := prRun(p, 42, shaC, 502)
	seen.Trigger, seen.QueuedAt = TriggerPoll, testNow.Add(-time.Minute)
	h.store.addRun(earlier)
	h.store.addRun(seen)

	body := prDelivery(t, "synchronize", 42, shaC, repoName, testInstallation)
	for _, delivery := range []string{"d-same-head", "d-same-head"} { // the webhook, then its redelivery
		res := trigger(t, h, "pull_request", delivery, body)
		if len(res.Opened) != 0 || len(res.Existing) != 1 || res.Existing[0].ID != seen.ID {
			t.Fatalf("a second sighting of shaC opens nothing: %+v", res)
		}
	}
	wantUntouched(t, h, earlier)
	wantUntouched(t, h, seen)

	// The control: the next head's push does stop both.
	opened := push42(t, h, "d-next", shaD)
	wantAsked(t, h, earlier.ID, opened.ID)
	wantAsked(t, h, seen.ID, opened.ID)
}

// A re-run is not a push. Re-running an earlier head asks for that commit's
// answer again; it must not stop the run of the pull request's CURRENT head,
// whose check run is the one the pull request shows.
func TestARerunSupersedesNothing(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	ended := prRun(p, 42, shaA, 501)
	ended.Status, ended.Conclusion, ended.FinishedAt = StatusCompleted, ConclusionCancelled, testNow.Add(-time.Minute)
	ended.CancelRequested, ended.CancelledBy = true, "superseded by an earlier run"
	current := drivenBy(prRun(p, 42, shaB, 502), "agent-b")
	h.store.addRun(ended)
	h.store.addRun(current)

	// Somebody re-runs the earlier head's check run on GitHub.
	res := trigger(t, h, "check_run", "d-rerun", checkRunDelivery(t, 501, shaA, testInstallation))
	if len(res.Opened) != 1 {
		t.Fatalf("the re-request opens the earlier head's next attempt: %+v", res)
	}
	if rerun := res.Opened[0]; rerun.Trigger != TriggerRerun || rerun.Mode != pipelines.ModeAffected || rerun.PullRequest != 42 {
		t.Fatalf("an affected re-run of #42: %+v", rerun)
	}
	wantUntouched(t, h, current)

	// The control: a push to #42 does stop the current head's run -- and the
	// re-run beside it, an earlier head's answer the pull request no longer
	// waits on.
	opened := push42(t, h, "d-push", shaC)
	wantAsked(t, h, current.ID, opened.ID)
	wantAsked(t, h, res.Opened[0].ID, opened.ID)
}

// Two pushes to one pull request opened at the same moment on two replicas
// must never stop each other: the opening reads the pull request's runs
// BEFORE its own exists, so a run created while it is being created -- here,
// between its read and its own create -- is not one it supersedes.
func TestARunCreatedWhileThePushOpensIsNotItsToCancel(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	earlier := drivenBy(prRun(p, 42, shaA, 501), "agent-b")
	h.store.addRun(earlier)
	concurrent := drivenBy(prRun(p, 42, shaB, 502), "agent-b")
	concurrent.QueuedAt = testNow
	var once sync.Once
	h.integ.Configure(func(d *Deps) {
		d.GitHub = createHook{fakeGitHub: h.github, before: func() { once.Do(func() { h.store.addRun(concurrent) }) }}
	})

	opened := push42(t, h, "d-push", shaC)

	if _, ok := h.store.run(concurrent.ID); !ok {
		t.Fatalf("the concurrent run was never created -- the test checks nothing")
	}
	wantUntouched(t, h, concurrent)
	wantAsked(t, h, earlier.ID, opened.ID)
}

// createHook is the fake GitHub with a hook run as each check run is created:
// what another replica does while this one opens a run.
type createHook struct {
	*fakeGitHub
	before func()
}

func (g createHook) CreateCheckRun(ctx context.Context, token, repository string, run githubapp.CheckRun) (int64, error) {
	if g.before != nil {
		g.before()
	}
	return g.fakeGitHub.CreateCheckRun(ctx, token, repository, run)
}

// A read or a cancel that fails is logged and fails nothing: the new head's
// run opened, and an earlier run left going costs a runner slot, not a wrong
// answer.
func TestASupersedeThatFailsDoesNotFailTheOpen(t *testing.T) {
	for name, c := range map[string]struct {
		fail func(s *memStore)
		says string
	}{
		"the pull request's runs cannot be read": {func(s *memStore) { s.failRunsForPullRequest = errors.New("database unavailable") }, "could not be read"},
		"the earlier run cannot be asked":        {func(s *memStore) { s.failUpdateRun = errors.New("database unavailable") }, "could not be cancelled"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			logs := &lockedBuffer{}
			h.integ.Configure(func(d *Deps) { d.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
			p := testPipeline(DeliveryWebhook)
			h.store.addPipeline(p)
			earlier := drivenBy(prRun(p, 42, shaA, 501), "agent-b")
			h.store.addRun(earlier)
			c.fail(h.store)

			res, err := h.integ.Trigger(automationCtx(), h.stage(t, "pull_request", "d-push", prDelivery(t, "synchronize", 42, shaC, repoName, testInstallation)))
			if err != nil || len(res.Opened) != 1 {
				t.Fatalf("the push opens its run whatever became of the supersede: %+v %v", res, err)
			}
			if got, _ := h.store.run(earlier.ID); got.CancelRequested {
				t.Errorf("the earlier run reads as asked: %+v", got)
			}
			if out := logs.String(); !strings.Contains(out, c.says) || !strings.Contains(out, "level=WARN") {
				t.Errorf("the failure is logged as a warning saying %q:\n%s", c.says, out)
			}
		})
	}
}

// THE GATE DISCIPLINE (fakes_test.go): the opening's gate is released before
// any superseded run is asked to stop. RequestCancel takes each run's own
// gate, and concludes a queued run nobody drives with a check-run write; asked
// under the open gate, that is a nested gate and a call to GitHub under one.
// The shared fakes report into a recorder here rather than failing the test,
// so the test can say what it saw and prove it saw the paths it is about.
func TestCancelOnPushHoldsNoGateWhileCallingRequestCancel(t *testing.T) {
	h := newHarness(t)
	rec := &disciplineRecorder{TB: t}
	h.gate.t, h.github.t = rec, rec
	var (
		mu               sync.Mutex
		asks             []gateEvent
		signals          []string
		signalledHolding []string
	)
	inner := h.gate.run
	h.integ.Configure(func(d *Deps) {
		d.Gate = func(ctx context.Context, key string, fn func(context.Context) error) error {
			held, _ := gateHeldOn(ctx)
			mu.Lock()
			asks = append(asks, gateEvent{key: key, enter: true, held: held})
			mu.Unlock()
			defer func() {
				mu.Lock()
				asks = append(asks, gateEvent{key: key})
				mu.Unlock()
			}()
			return inner(ctx, key, fn)
		}
		// This node drives one of the superseded runs: RequestCancel signals
		// its driver, on the context it was called on.
		d.SignalCancel = func(ctx context.Context, runID string) {
			held, _ := gateHeldOn(ctx)
			mu.Lock()
			defer mu.Unlock()
			signals = append(signals, runID)
			if held != "" {
				signalledHolding = append(signalledHolding, held)
			}
		}
	})
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	queued := prRun(p, 42, shaA, 501)                          // concluded on the spot: a check-run write
	drivenHere := drivenBy(prRun(p, 42, shaB, 502), "agent-a") // this node's driver is signalled
	h.store.addRun(queued)
	h.store.addRun(drivenHere)

	opened := push42(t, h, "d-push", shaC)

	if got := rec.took(); len(got) != 0 {
		t.Errorf("the gate discipline was broken:\n  %s", strings.Join(got, "\n  "))
	}
	mu.Lock()
	defer mu.Unlock()
	openKey := OpenGateKey(opened.RunKey)
	released := slices.IndexFunc(asks, func(e gateEvent) bool { return !e.enter && e.key == openKey })
	if released < 0 {
		t.Fatalf("the open never released %q: %+v", openKey, asks)
	}
	for _, id := range []string{queued.ID, drivenHere.ID} {
		asked := slices.IndexFunc(asks, func(e gateEvent) bool { return e.enter && e.key == RunGateKey(id) })
		switch {
		case asked < 0:
			t.Errorf("run %s was never asked to stop: %+v", id, asks)
		case asked < released:
			t.Errorf("run %s's gate was asked for before the open gate was released: %+v", id, asks)
		case asks[asked].held != "":
			t.Errorf("run %s's gate was asked for while %q was held", id, asks[asked].held)
		}
	}
	if !slices.Equal(signals, []string{drivenHere.ID}) || len(signalledHolding) != 0 {
		t.Errorf("this node's driver is signalled once, with no gate held: signals %v, held %v", signals, signalledHolding)
	}
	if w := checkRunWrites(h, 501); len(w) != 1 || w[0].Run.Conclusion != "cancelled" {
		t.Errorf("the concluded run's check run was written -- with no gate held, or the recorder would say so: %+v", w)
	}
	if got, _ := h.store.run(queued.ID); got.Conclusion != ConclusionCancelled {
		t.Errorf("the queued run is concluded cancelled: %+v", got)
	}
	wantAsked(t, h, drivenHere.ID, opened.ID)
}

// gateEvent is one gate asked for (enter, with the gate already held on the
// asking context) or released.
type gateEvent struct {
	key   string
	enter bool
	held  string
}

// The whole path on a real driver: a push stops the run this node is driving
// at once -- the runner told to cancel, the run concluded cancelled -- and
// the new head's run is left queued.
func TestASupersededRunThisNodeDrivesStopsAtOnce(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	release := make(chan struct{})
	defer close(release)
	blockTests(dh, release)
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")

	next := prOpening()
	next.SHA, next.Title = shaB, "Show the cart count, fixed"
	pushed := dh.openRun(t, next)
	waitDrives(t, dh.integ)

	if cancels := dh.exec.cancelled(); !slices.Equal(cancels, []string{run.ID}) {
		t.Errorf("the runner is told to cancel the superseded run once: %v", cancels)
	}
	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled || got.CancelledBy != "superseded by "+pushed.ID {
		t.Errorf("the superseded run = %s/%s by %q", got.Status, got.Conclusion, got.CancelledBy)
	}
	if now, _ := dh.store.run(pushed.ID); now.Status != StatusQueued || now.CancelRequested {
		t.Errorf("the new head's run is left queued for a driver: %+v", now)
	}
}

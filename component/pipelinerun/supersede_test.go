package pipelinerun

import (
	"context"
	"errors"
	"fmt"
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

// shaHex is a head spelled with hex LETTERS, which every other test SHA lacks:
// the only one whose case can differ between two spellings.
const shaHex = "abcdef0123456789abcdef0123456789abcdef01"

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

// push42 is a push to pull request #42's branch: GitHub's head moves to sha,
// then its synchronize is delivered. It answers the run the push opened.
func push42(t *testing.T, h *harness, deliveryID, sha string) Run {
	t.Helper()
	h.github.setPullHead(repoName, 42, sha)
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

// wantUntouched holds a run to exactly what it was: no new ask to stop, its
// status and conclusion unmoved, and its gate never taken -- so not even asked
// and refused.
func wantUntouched(t *testing.T, h *harness, before Run) {
	t.Helper()
	got, _ := h.store.run(before.ID)
	if got.CancelRequested != before.CancelRequested || got.CancelledBy != before.CancelledBy || got.Status != before.Status || got.Conclusion != before.Conclusion {
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

// logLine is the first line of out that says phrase, or "" -- so a test holds
// a phrase and its LEVEL to one line, not to two lines that each say one.
func logLine(out, phrase string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, phrase) {
			return line
		}
	}
	return ""
}

// wantLogged holds out to a line saying phrase at level ("INFO", "WARN").
func wantLogged(t *testing.T, out, phrase, level string) {
	t.Helper()
	if line := logLine(out, phrase); line == "" || !strings.Contains(line, "level="+level) {
		t.Errorf("want one %s line saying %q, got line %q in:\n%s", level, phrase, line, out)
	}
}

// supersededAt is pull request pr's run of head sha after the run byID
// superseded it: concluded cancelled, or -- stopping -- still in progress on
// agent-b with the ask on its row.
func supersededAt(p Pipeline, pr int, sha string, checkRunID int64, byID string, stopping bool) Run {
	r := prRun(p, pr, sha, checkRunID)
	if stopping {
		r = drivenBy(r, "agent-b")
	} else {
		r.Status, r.Conclusion, r.FinishedAt = StatusCompleted, ConclusionCancelled, testNow.Add(-time.Minute)
	}
	r.CancelRequested, r.CancelledBy = true, "superseded by "+byID
	return r
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
	// An ordinary second sighting asks GitHub nothing: only a key whose newest
	// attempt a newer push superseded asks whether its head came back (R37).
	if reads := h.github.headReads(); len(reads) != 0 {
		t.Errorf("a dedup hit on a run nobody superseded read the head: %v", reads)
	}

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

// R34: processing order is not push order. A delivery for an OLDER head that
// arrives late -- redelivered, or a trigger retried after a gate timeout --
// opens its run after the newer head's; it must not stop the run of the head
// the pull request shows. GitHub is asked for the pull request's head, and an
// opening that is not it stops nothing.
func TestALateOpeningForAnOlderHeadStopsNothing(t *testing.T) {
	h := newHarness(t)
	logs := &lockedBuffer{}
	h.integ.Configure(func(d *Deps) { d.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	// #42's head on GitHub is shaB, and its run is going.
	current := drivenBy(prRun(p, 42, shaB, 502), "agent-b")
	h.store.addRun(current)
	h.github.setPullHead(repoName, 42, shaB)

	// The delivery of the push BEFORE it, to shaA, arrives now.
	res := trigger(t, h, "pull_request", "d-late", prDelivery(t, "synchronize", 42, shaA, repoName, testInstallation))
	if len(res.Opened) != 1 {
		t.Fatalf("the late delivery still opens its own head's run: %+v", res)
	}
	late := res.Opened[0]

	wantUntouched(t, h, current)
	if reads := h.github.headReads(); !slices.Equal(reads, []string{repoName + "#42"}) {
		t.Errorf("GitHub is asked for #42's head once: %v", reads)
	}
	wantLogged(t, logs.String(), "has moved on", "INFO")

	// The control: the next push is the head GitHub names, and stops both.
	opened := push42(t, h, "d-next", shaC)
	wantAsked(t, h, current.ID, opened.ID)
	wantAsked(t, h, late.ID, opened.ID)
}

// Whatever keeps the head from being KNOWN stops nothing: GitHub refusing the
// read, no installation token to ask with, an answer naming no head. Each is
// a warning, and the run opens regardless.
func TestTheHeadReadFailingStopsNothing(t *testing.T) {
	for name, c := range map[string]struct {
		break_  func(g *fakeGitHub)
		says    string
		noReads bool
	}{
		"GitHub refuses the read": {func(g *fakeGitHub) { g.pullHeadErr = errStatus(502) }, "head could not be read", false},
		"no installation token":   {func(g *fakeGitHub) { g.tokenErr = errors.New("reconnect_required") }, "no installation token", true},
		"GitHub names no head":    {func(g *fakeGitHub) { g.setPullHead(repoName, 42, "") }, "named no head", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			logs := &lockedBuffer{}
			h.integ.Configure(func(d *Deps) { d.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
			p := testPipeline(DeliveryWebhook)
			h.store.addPipeline(p)
			earlier := drivenBy(prRun(p, 42, shaA, 501), "agent-b")
			h.store.addRun(earlier)
			h.github.setPullHead(repoName, 42, shaC)
			c.break_(h.github)

			res, err := h.integ.Trigger(automationCtx(), h.stage(t, "pull_request", "d-push", prDelivery(t, "synchronize", 42, shaC, repoName, testInstallation)))
			if err != nil || len(res.Opened) != 1 {
				t.Fatalf("the push opens its run whatever GitHub said of its head: %+v %v", res, err)
			}
			wantUntouched(t, h, earlier)
			if reads := h.github.headReads(); (len(reads) == 0) != c.noReads {
				t.Errorf("head reads %v", reads)
			}
			// The phrase and its level on ONE line: the opening's own
			// warning about its check run is another line, at WARN too.
			wantLogged(t, logs.String(), c.says, "WARN")
		})
	}
}

// GitHub's head is compared as this package compares every SHA -- trimmed and
// lower-cased -- so the same commit spelled in upper case by one side is the
// same head, and the push it belongs to supersedes. The SHA carries hex
// letters: an all-digit SHA reads the same in either case and would test
// nothing.
func TestTheHeadIsComparedAsThePackageComparesSHAs(t *testing.T) {
	if strings.ToUpper(shaHex) == shaHex {
		t.Fatalf("%s has no letters to change case -- the test checks nothing", shaHex)
	}
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	earlier := drivenBy(prRun(p, 42, shaA, 501), "agent-b")
	h.store.addRun(earlier)

	h.github.setPullHead(repoName, 42, " "+strings.ToUpper(shaHex)+" ")
	res := trigger(t, h, "pull_request", "d-push", prDelivery(t, "synchronize", 42, shaHex, repoName, testInstallation))
	if len(res.Opened) != 1 || res.Opened[0].SHA != shaHex {
		t.Fatalf("opened %+v", res)
	}
	wantAsked(t, h, earlier.ID, res.Opened[0].ID)
}

// The common case -- nothing unfinished that the push would stop -- costs no
// call to GitHub. A full run carrying the pull request and another pull
// request's run come back from the read and pass no rule, and are not reason
// enough to ask.
func TestNothingSupersedableMakesNoGitHubCall(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	queueKey := pipelines.RunKey(repoName, shaB, pipelines.ModeFull, pipelines.EventMergeGroup)
	mergeQueue := Run{
		ID: RunIDFor(p.ID, queueKey, 1), OwnerUserID: p.OwnerUserID, PipelineID: p.ID, Repository: repoName, SHA: shaB,
		Mode: pipelines.ModeFull, Event: pipelines.EventMergeGroup, RunKey: queueKey, Attempt: 1, Trigger: TriggerWebhook,
		PullRequest: 42, Status: StatusQueued, CheckRunID: 602, CheckRunState: CheckRunWritten, QueuedAt: testNow.Add(-tenMinutes),
	}
	other := drivenBy(prRun(p, 43, shaA, 701), "agent-b")
	h.store.addRun(mergeQueue)
	h.store.addRun(other)

	first := push42(t, h, "d-first", shaC)
	if reads := h.github.headReads(); len(reads) != 0 {
		t.Errorf("a push with nothing to stop asked GitHub for the head: %v", reads)
	}
	wantUntouched(t, h, mergeQueue)
	wantUntouched(t, h, other)

	// The control: the next push has #42's first run to stop, and asks once.
	second := push42(t, h, "d-second", shaD)
	if reads := h.github.headReads(); len(reads) != 1 {
		t.Errorf("a push with a run to stop asks GitHub once: %v", reads)
	}
	wantAsked(t, h, first.ID, second.ID)
}

// A NEWER push landing while this one opens -- pushed just after GitHub named
// this opening's head, and its run created on another replica -- is never this
// opening's to stop: the opening stops only runs it read before its own run
// existed, and it read them before it asked GitHub.
func TestANewerPushLandingWhileThisOneOpensIsNotItsToCancel(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	earlier := drivenBy(prRun(p, 42, shaA, 501), "agent-b")
	h.store.addRun(earlier)
	newer := drivenBy(prRun(p, 42, shaD, 504), "agent-b")
	newer.QueuedAt = testNow
	var once sync.Once
	h.integ.Configure(func(d *Deps) {
		d.GitHub = hookedGitHub{fakeGitHub: h.github, afterHeadRead: func(context.Context) {
			once.Do(func() {
				h.github.setPullHead(repoName, 42, shaD)
				h.store.addRun(newer)
			})
		}}
	})

	opened := push42(t, h, "d-push", shaC)

	if _, ok := h.store.run(newer.ID); !ok {
		t.Fatalf("the newer push's run was never created -- the test checks nothing")
	}
	wantUntouched(t, h, newer)
	wantAsked(t, h, earlier.ID, opened.ID)
}

// hookedGitHub is the fake GitHub with a hook run after each head read: what
// GitHub and another replica do while this one is asking.
type hookedGitHub struct {
	*fakeGitHub
	afterHeadRead func(ctx context.Context)
}

func (g hookedGitHub) PullRequestHead(ctx context.Context, token, repository string, number int) (githubapp.PullRequestHead, error) {
	head, err := g.fakeGitHub.PullRequestHead(ctx, token, repository, number)
	if g.afterHeadRead != nil {
		g.afterHeadRead(ctx)
	}
	return head, err
}

// A read or a cancel that fails is logged and fails nothing: the new head's
// run opened, and an earlier run left going costs a runner slot, not a wrong
// answer.
func TestASupersedeThatFailsDoesNotFailTheOpen(t *testing.T) {
	for name, c := range map[string]struct {
		fail func(s *memStore)
		says string
	}{
		"the pull request's runs cannot be read": {func(s *memStore) { s.failRunsForPullRequest = errors.New("database unavailable") }, "earlier runs could not be read"},
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
			h.github.setPullHead(repoName, 42, shaC)
			c.fail(h.store)

			res, err := h.integ.Trigger(automationCtx(), h.stage(t, "pull_request", "d-push", prDelivery(t, "synchronize", 42, shaC, repoName, testInstallation)))
			if err != nil || len(res.Opened) != 1 {
				t.Fatalf("the push opens its run whatever became of the supersede: %+v %v", res, err)
			}
			if got, _ := h.store.run(earlier.ID); got.CancelRequested {
				t.Errorf("the earlier run reads as asked: %+v", got)
			}
			wantLogged(t, logs.String(), c.says, "WARN")
		})
	}
}

// THE GATE DISCIPLINE (fakes_test.go): the opening's gate is released before
// GitHub is asked for the pull request's head and before any superseded run
// is asked to stop. RequestCancel takes each run's own gate, and concludes a
// queued run nobody drives with a check-run write; asked under the open gate,
// those -- and the head read -- would be a nested gate and calls to GitHub
// under one. The shared fakes report into a recorder here rather than failing
// the test, so the test can say what it saw and prove it saw the paths it is
// about.
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
		// The head read, in the same sequence as the gates.
		d.GitHub = hookedGitHub{fakeGitHub: h.github, afterHeadRead: func(ctx context.Context) {
			held, _ := gateHeldOn(ctx)
			mu.Lock()
			defer mu.Unlock()
			asks = append(asks, gateEvent{key: headRead, enter: true, held: held})
		}}
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
	read := slices.IndexFunc(asks, func(e gateEvent) bool { return e.key == headRead })
	switch {
	case read < 0:
		t.Fatalf("GitHub was never asked for the pull request's head: %+v", asks)
	case read < released:
		t.Errorf("the head was read before the open gate was released: %+v", asks)
	case asks[read].held != "":
		t.Errorf("the head was read while %q was held", asks[read].held)
	}
	if reads := h.github.headReads(); len(reads) != 1 {
		t.Errorf("the head is read once for the push, whatever it stops: %v", reads)
	}
	for _, id := range []string{queued.ID, drivenHere.ID} {
		asked := slices.IndexFunc(asks, func(e gateEvent) bool { return e.enter && e.key == RunGateKey(id) })
		switch {
		case asked < 0:
			t.Errorf("run %s was never asked to stop: %+v", id, asks)
		case asked < released || asked < read:
			t.Errorf("run %s's gate was asked for before the open gate was released and the head known: %+v", id, asks)
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
// asking context) or released -- or, keyed headRead, the pull request's head
// read from GitHub, in the same sequence.
type gateEvent struct {
	key   string
	enter bool
	held  string
}

// headRead keys the head read in a gateEvent sequence: GitHub, not a gate.
const headRead = "github:PullRequestHead"

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
	dh.github.setPullHead(repoName, 42, shaB)
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

// ---------------------------------------------------------------------------
// R37: a superseded run does not answer its own head coming back
// ---------------------------------------------------------------------------

// A force-push BACK to a head a newer push superseded: the head's run was
// cancelled -- or is still stopping -- as superseded, and would answer the
// delivery as a second sighting, leaving the head the pull request shows with
// a cancelled check and no run coming while the dropped commit's run goes on.
// When GitHub names the commit as the pull request's head again, the key
// opens its NEXT attempt instead, and that run supersedes the dropped one.
func TestAForcePushBackToASupersededHeadRunsItAgain(t *testing.T) {
	for name, stopping := range map[string]bool{"its run concluded cancelled": false, "its run still stopping": true} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			logs := &lockedBuffer{}
			h.integ.Configure(func(d *Deps) { d.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
			p := testPipeline(DeliveryWebhook)
			h.store.addPipeline(p)
			dropped := drivenBy(prRun(p, 42, shaB, 502), "agent-b")
			back := supersededAt(p, 42, shaA, 501, dropped.ID, stopping)
			h.store.addRun(back)
			h.store.addRun(dropped)

			// The developer force-pushes #42 back to shaA.
			again := push42(t, h, "d-back", shaA)

			if again.RunKey != back.RunKey || again.Attempt != 2 || again.ID != RunIDFor(p.ID, back.RunKey, 2) {
				t.Errorf("the key's NEXT attempt opens: key %q attempt %d id %s", again.RunKey, again.Attempt, again.ID)
			}
			if again.Trigger != TriggerWebhook || again.RerunOf != "" || again.Status != StatusQueued || again.PullRequest != 42 {
				t.Errorf("it is the push's own run, queued, not a re-run: %+v", again)
			}
			wantAsked(t, h, dropped.ID, again.ID)
			if got, _ := h.store.run(back.ID); got.CancelledBy != back.CancelledBy || got.Attempt != 1 {
				t.Errorf("the superseded run stays as it was stopped: %+v", got)
			}
			// One read decides the dedup, before the gate; one decides the
			// supersede, after it.
			if reads := h.github.headReads(); len(reads) != 2 {
				t.Errorf("head reads %v; want the dedup's and the supersede's", reads)
			}
			wantLogged(t, logs.String(), "runs again", "INFO")
		})
	}
}

// The same delivery while GitHub's head is ELSEWHERE -- a late redelivery of a
// head a newer push superseded -- is answered by the superseded run, as a
// second sighting always is: nothing opens, nothing is stopped.
func TestALateRedeliveryOfASupersededHeadOpensNothing(t *testing.T) {
	h := newHarness(t)
	logs := &lockedBuffer{}
	h.integ.Configure(func(d *Deps) { d.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	current := drivenBy(prRun(p, 42, shaB, 502), "agent-b")
	superseded := supersededAt(p, 42, shaA, 501, current.ID, false)
	h.store.addRun(superseded)
	h.store.addRun(current)
	h.github.setPullHead(repoName, 42, shaB)

	res := trigger(t, h, "pull_request", "d-late", prDelivery(t, "synchronize", 42, shaA, repoName, testInstallation))
	if len(res.Opened) != 0 || len(res.Existing) != 1 || res.Existing[0].ID != superseded.ID {
		t.Fatalf("the superseded run answers a head GitHub no longer names: %+v", res)
	}
	wantUntouched(t, h, current)
	wantUntouched(t, h, superseded)
	if reads := h.github.headReads(); len(reads) != 1 {
		t.Errorf("one head read decides the dedup: %v", reads)
	}
	wantLogged(t, logs.String(), "answered by its superseded run", "INFO")

	// The control: GitHub naming shaA again makes the same delivery run it.
	h.github.setPullHead(repoName, 42, shaA)
	again := trigger(t, h, "pull_request", "d-late", prDelivery(t, "synchronize", 42, shaA, repoName, testInstallation))
	if len(again.Opened) != 1 || again.Opened[0].Attempt != 2 {
		t.Fatalf("with its head back, the commit runs again: %+v", again)
	}
	wantAsked(t, h, current.ID, again.Opened[0].ID)
}

// Whatever keeps the head from being KNOWN leaves the superseded run
// answering, as today: nothing re-runs, nothing is stopped, and a warning
// says why on its own line -- with the error that made it so, where there is
// one. The failed MINT matters most: on this path the opening returns at once,
// so no check-run warning records it either.
func TestTheHeadUnknownLeavesTheSupersededRunAnswering(t *testing.T) {
	for name, c := range map[string]struct {
		break_ func(g *fakeGitHub)
		says   string
		why    string // the error the same line carries; "" for none
	}{
		"GitHub refuses the read": {func(g *fakeGitHub) { g.pullHeadErr = errStatus(502) }, "head could not be read from GitHub, so the superseded run answers", "GitHub answered HTTP 502"},
		"no installation token":   {func(g *fakeGitHub) { g.tokenErr = errors.New("reconnect_required: the grant was revoked") }, "no installation token to ask GitHub whether", "reconnect_required: the grant was revoked"},
		"GitHub names no head":    {func(g *fakeGitHub) { g.setPullHead(repoName, 42, "") }, "named no head for the pull request, so the superseded run answers", ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			logs := &lockedBuffer{}
			h.integ.Configure(func(d *Deps) { d.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
			p := testPipeline(DeliveryWebhook)
			h.store.addPipeline(p)
			current := drivenBy(prRun(p, 42, shaB, 502), "agent-b")
			superseded := supersededAt(p, 42, shaA, 501, current.ID, false)
			h.store.addRun(superseded)
			h.store.addRun(current)
			h.github.setPullHead(repoName, 42, shaA)
			c.break_(h.github)

			res := trigger(t, h, "pull_request", "d-back", prDelivery(t, "synchronize", 42, shaA, repoName, testInstallation))
			if len(res.Opened) != 0 || len(res.Existing) != 1 || res.Existing[0].ID != superseded.ID {
				t.Fatalf("an unknown head leaves the superseded run answering: %+v", res)
			}
			wantUntouched(t, h, current)
			wantLogged(t, logs.String(), c.says, "WARN")
			if line := logLine(logs.String(), c.says); c.why != "" && !strings.Contains(line, c.why) {
				t.Errorf("the warning's own line says why (%q): %q", c.why, line)
			}
		})
	}
}

// A run a PERSON cancelled answers its head as it always has -- the cancel
// was somebody's decision, not a push's -- and costs no head read.
func TestAPersonsCancelStillAnswersItsHead(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	cancelled := prRun(p, 42, shaA, 501)
	cancelled.Status, cancelled.Conclusion, cancelled.FinishedAt = StatusCompleted, ConclusionCancelled, testNow.Add(-time.Minute)
	cancelled.CancelRequested, cancelled.CancelledBy = true, "v1:identity:user:"+ownerID
	h.store.addRun(cancelled)
	h.github.setPullHead(repoName, 42, shaA)

	res := trigger(t, h, "pull_request", "d-again", prDelivery(t, "synchronize", 42, shaA, repoName, testInstallation))
	if len(res.Opened) != 0 || len(res.Existing) != 1 || res.Existing[0].ID != cancelled.ID {
		t.Fatalf("a person's cancel answers its head: %+v", res)
	}
	if reads := h.github.headReads(); len(reads) != 0 {
		t.Errorf("a run nobody superseded asked GitHub for the head: %v", reads)
	}
}

// The poll sees a force-push back too -- it opens the head GitHub's list
// names -- and the superseded run does not answer it there either.
func TestThePollSeesAForcePushBackToo(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryPoll)
	p.Heads = map[string]string{"branch:main": shaBase, "pr:42": shaB}
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaBase}
	h.github.setPullHead(repoName, 42, shaA)
	dropped := drivenBy(prRun(p, 42, shaB, 502), "agent-b")
	back := supersededAt(p, 42, shaA, 501, dropped.ID, false)
	h.store.addRun(back)
	h.store.addRun(dropped)

	res := poll(t, h)
	if len(res.Opened) != 1 || res.Opened[0].RunKey != back.RunKey || res.Opened[0].Attempt != 2 || res.Opened[0].Trigger != TriggerPoll {
		t.Fatalf("the poll opens the head's next attempt: %+v", res)
	}
	wantAsked(t, h, dropped.ID, res.Opened[0].ID)
}

// THE GATE DISCIPLINE for R37's read: GitHub is asked whether the head came
// back BEFORE the open gate is taken, and the gated dedup read reuses the
// answer -- nothing GitHub is asked under the gate. The supersede's own head
// read follows the gate's release, and only then is the dropped run asked.
func TestTheHeadComingBackIsReadBeforeTheGate(t *testing.T) {
	h := newHarness(t)
	rec := &disciplineRecorder{TB: t}
	h.gate.t, h.github.t = rec, rec
	var (
		mu   sync.Mutex
		asks []gateEvent
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
		d.GitHub = hookedGitHub{fakeGitHub: h.github, afterHeadRead: func(ctx context.Context) {
			held, _ := gateHeldOn(ctx)
			mu.Lock()
			defer mu.Unlock()
			asks = append(asks, gateEvent{key: headRead, enter: true, held: held})
		}}
	})
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	dropped := prRun(p, 42, shaB, 502)
	back := supersededAt(p, 42, shaA, 501, dropped.ID, false)
	h.store.addRun(back)
	h.store.addRun(dropped)

	again := push42(t, h, "d-back", shaA)

	if got := rec.took(); len(got) != 0 {
		t.Errorf("the gate discipline was broken:\n  %s", strings.Join(got, "\n  "))
	}
	mu.Lock()
	defer mu.Unlock()
	openKey := OpenGateKey(again.RunKey)
	entered := slices.IndexFunc(asks, func(e gateEvent) bool { return e.enter && e.key == openKey })
	released := slices.IndexFunc(asks, func(e gateEvent) bool { return !e.enter && e.key == openKey })
	var reads []int
	for i, e := range asks {
		if e.key == headRead {
			reads = append(reads, i)
			if e.held != "" {
				t.Errorf("the head was read while %q was held", e.held)
			}
		}
	}
	if entered < 0 || released < 0 || len(reads) != 2 {
		t.Fatalf("want the open gate taken and released and two head reads: %+v", asks)
	}
	if reads[0] > entered {
		t.Errorf("the dedup's head read came after the open gate was taken: %+v", asks)
	}
	if reads[1] < released {
		t.Errorf("the supersede's head read came before the open gate was released: %+v", asks)
	}
	stopped := slices.IndexFunc(asks, func(e gateEvent) bool { return e.enter && e.key == RunGateKey(dropped.ID) })
	if stopped < reads[1] {
		t.Errorf("the dropped run was asked to stop before the supersede read the head: %+v", asks)
	}
}

// R37 is answered once, before the gate: when ANOTHER replica opens the key's
// run between this opening's two dedup reads, the gated read finds it and
// answers -- this opening opened nothing, so it supersedes nothing and asks
// GitHub nothing (the other replica's opening did both).
func TestARunAnotherReplicaOpenedBetweenTheReadsAnswersAndSupersedesNothing(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	earlier := drivenBy(prRun(p, 42, shaA, 501), "agent-b")
	h.store.addRun(earlier)
	theirs := prRun(p, 42, shaC, 503)
	theirs.QueuedAt = testNow
	inner := h.gate.run
	var once sync.Once
	h.integ.Configure(func(d *Deps) {
		d.Gate = func(ctx context.Context, key string, fn func(context.Context) error) error {
			if key == OpenGateKey(theirs.RunKey) {
				once.Do(func() { h.store.addRun(theirs) })
			}
			return inner(ctx, key, fn)
		}
	})
	h.github.setPullHead(repoName, 42, shaC)

	res := trigger(t, h, "pull_request", "d-push", prDelivery(t, "synchronize", 42, shaC, repoName, testInstallation))
	if len(res.Opened) != 0 || len(res.Existing) != 1 || res.Existing[0].ID != theirs.ID {
		t.Fatalf("the gated read answers with the other replica's run: %+v", res)
	}
	wantUntouched(t, h, earlier)
	if reads := h.github.headReads(); len(reads) != 0 {
		t.Errorf("an opening that opened nothing asked GitHub for the head: %v", reads)
	}
}

// A run that finished on its own before it could be asked -- it passed as the
// push read the head -- keeps its answer, and the skip is said like every
// other: at info, on its own line.
func TestARunThatFinishedBeforeItsAskIsPassedOverAndSaid(t *testing.T) {
	h := newHarness(t)
	logs := &lockedBuffer{}
	h.integ.Configure(func(d *Deps) { d.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	earlier := drivenBy(prRun(p, 42, shaA, 501), "agent-b")
	h.store.addRun(earlier)
	h.integ.Configure(func(d *Deps) {
		d.GitHub = hookedGitHub{fakeGitHub: h.github, afterHeadRead: func(context.Context) {
			h.store.mu.Lock()
			defer h.store.mu.Unlock()
			r := h.store.runs[earlier.ID]
			r.Status, r.Conclusion = StatusCompleted, ConclusionSuccess
			h.store.runs[earlier.ID] = r
		}}
	})

	push42(t, h, "d-push", shaC)

	if got, _ := h.store.run(earlier.ID); got.Conclusion != ConclusionSuccess || got.CancelRequested {
		t.Errorf("a run that finished keeps its own answer: %s cancelRequested %v", got.Conclusion, got.CancelRequested)
	}
	wantLogged(t, logs.String(), "had finished before it could be asked", "INFO")
}

// A FORK never re-runs a superseded commit (R37 leaves forks out): a fork's
// pull request whose head is a commit this repository's own pull request ran
// -- and a newer push then superseded -- is answered by that superseded run,
// as before. Asking GitHub would be pointless, a fork runs nothing; opening
// past the superseded run would open a REFUSED fork attempt, whose failing
// check run would sit on the commit.
func TestAForkNeverRerunsASupersededCommit(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	// #42, from this repository, ran shaA; a push to shaB superseded it.
	current := drivenBy(prRun(p, 42, shaB, 502), "agent-b")
	superseded := supersededAt(p, 42, shaA, 501, current.ID, false)
	h.store.addRun(superseded)
	h.store.addRun(current)
	// A fork's pull request #7 has that same commit as its head, and GitHub
	// says so.
	h.github.setPullHead(repoName, 7, shaA)

	res := trigger(t, h, "pull_request", "d-fork", prDelivery(t, "synchronize", 7, shaA, "mallory/shop", testInstallation))
	if len(res.Opened) != 0 || len(res.Existing) != 1 || res.Existing[0].ID != superseded.ID {
		var opened, checks []string
		for _, r := range res.Opened {
			opened = append(opened, fmt.Sprintf("attempt %d of %s, %s/%s %s", r.Attempt, r.RunKey, r.Status, r.Conclusion, r.RefusalCode))
		}
		for _, c := range h.github.createdRuns() {
			checks = append(checks, c.Run.Status+"/"+c.Run.Conclusion)
		}
		t.Fatalf("the fork's delivery is answered by the superseded run, and opens nothing: opened %v, existing %d, check runs written %v",
			opened, len(res.Existing), checks)
	}
	if reads := h.github.headReads(); len(reads) != 0 {
		t.Errorf("a fork's delivery asked GitHub for a head: %v", reads)
	}
	if created := h.github.createdRuns(); len(created) != 0 {
		t.Errorf("a fork's delivery wrote a check run: %+v", created)
	}
	if n := len(h.store.allRuns()); n != 2 {
		t.Errorf("runs = %d, want the two there were", n)
	}
	wantUntouched(t, h, current)
}

// The head answer, asked once before the gate, is about ONE attempt -- the
// superseded attempt the ungated read judged -- and frees only that attempt.
// Here another replica opened the key's second attempt for the same
// force-push back between this opening's two reads, and a newer push has
// superseded THAT one already: GitHub's answer said nothing about attempt 2,
// so attempt 2 answers and no third attempt opens.
func TestTheHeadAnswerFreesOnlyTheAttemptItWasAsked(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	dropped := drivenBy(prRun(p, 42, shaB, 502), "agent-b")
	back := supersededAt(p, 42, shaA, 501, dropped.ID, false)
	h.store.addRun(back)
	h.store.addRun(dropped)
	h.github.setPullHead(repoName, 42, shaA)
	second := supersededAt(p, 42, shaA, 503, "a-run-of-shaC", false)
	second.ID, second.Attempt, second.QueuedAt = RunIDFor(p.ID, back.RunKey, 2), 2, testNow
	inner := h.gate.run
	var once sync.Once
	h.integ.Configure(func(d *Deps) {
		d.Gate = func(ctx context.Context, key string, fn func(context.Context) error) error {
			if key == OpenGateKey(back.RunKey) {
				once.Do(func() {
					h.store.addRun(second)
					h.github.setPullHead(repoName, 42, shaC)
				})
			}
			return inner(ctx, key, fn)
		}
	})

	res := trigger(t, h, "pull_request", "d-back", prDelivery(t, "synchronize", 42, shaA, repoName, testInstallation))
	if len(res.Opened) != 0 || len(res.Existing) != 1 || res.Existing[0].ID != second.ID {
		t.Fatalf("the attempt the answer was not about answers; nothing opens: %+v", res)
	}
	if n := len(h.store.allRuns()); n != 3 {
		t.Errorf("runs = %d, want no third attempt", n)
	}
	if reads := h.github.headReads(); len(reads) != 1 {
		t.Errorf("one head read decided the dedup, and nothing asked again: %v", reads)
	}
	wantUntouched(t, h, dropped)
}

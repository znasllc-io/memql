package pipelinerun

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// poll_test.go -- the polling delivery (D4, D11; decision 13).

func poll(t *testing.T, h *harness) PollResult {
	t.Helper()
	res, err := h.integ.Poll(automationCtx())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	return res
}

func TestTheFirstPollRecordsABaselineAndOpensNothing(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryPoll)
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA, Message: "Initial"}
	h.github.pulls[repoName] = []githubapp.PullRequestHead{
		{Number: 5, Title: "Cart", HeadSHA: shaB, HeadRef: "cart", HeadRepository: repoName, BaseSHA: shaA},
	}

	res := poll(t, h)
	if res.Baselines != 1 || len(res.Opened) != 0 {
		t.Fatalf("the first poll records and opens nothing: baselines %d opened %d", res.Baselines, len(res.Opened))
	}
	got, _ := h.store.pipeline(p.ID)
	want := map[string]string{"branch:main": shaA, "pr:5": shaB}
	if len(got.Heads) != len(want) || got.Heads["branch:main"] != shaA || got.Heads["pr:5"] != shaB {
		t.Errorf("heads = %v, want %v", got.Heads, want)
	}
	if n := len(h.github.createdRuns()); n != 0 {
		t.Errorf("check runs = %d", n)
	}
	if n := len(h.store.allRuns()); n != 0 {
		t.Errorf("runs = %d", n)
	}
}

func TestAMovedHeadAndANewPullRequestOpenRuns(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryPoll)
	p.Heads = map[string]string{"branch:main": shaA, "pr:5": shaB, "pr:6": shaC}
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaB, Message: "Land it\n\nbody"}
	h.github.pulls[repoName] = []githubapp.PullRequestHead{
		// Unchanged: nothing.
		{Number: 5, Title: "Cart", HeadSHA: shaB, HeadRef: "cart", HeadRepository: repoName, BaseSHA: shaA},
		// New, from this repository: an affected run.
		{Number: 8, Title: "Badge", HeadSHA: shaC, HeadRef: "badge", HeadRepository: "Acme/Shop", BaseSHA: shaA},
		// New, from a fork: refused.
		{Number: 9, Title: "Hack", HeadSHA: shaA, HeadRef: "main", HeadRepository: "mallory/shop", BaseSHA: shaA},
		// #6 closed: its key drops out.
	}

	res := poll(t, h)
	if len(res.Opened) != 3 {
		t.Fatalf("opened %d, want the push, #8 and #9's refusal (%+v)", len(res.Opened), res)
	}
	byEvent := map[string]Run{}
	for _, r := range res.Opened {
		key := string(r.Event)
		if r.PullRequest > 0 {
			key += ":" + strconv.Itoa(r.PullRequest)
		}
		byEvent[key] = r
	}
	push := byEvent["push"]
	if push.SHA != shaB || push.BaseSHA != shaA || push.Mode != pipelines.ModeFull || push.Trigger != TriggerPoll || push.Title != "Land it" {
		t.Errorf("push run = %+v", push)
	}
	pr8 := byEvent["pull_request:8"]
	if pr8.SHA != shaC || pr8.Mode != pipelines.ModeAffected || pr8.Status != StatusQueued || pr8.HeadBranch != "badge" {
		t.Errorf("#8 = %+v", pr8)
	}
	pr9 := byEvent["pull_request:9"]
	if pr9.Status != StatusCompleted || pr9.RefusalCode != pipelines.CodeForkRefused {
		t.Errorf("a fork's head is refused by the poll exactly as by a delivery: %+v", pr9)
	}
	got, _ := h.store.pipeline(p.ID)
	want := map[string]string{"branch:main": shaB, "pr:5": shaB, "pr:8": shaC, "pr:9": shaA}
	if len(got.Heads) != len(want) {
		t.Fatalf("heads = %v, want %v (closed #6 dropped)", got.Heads, want)
	}
	for k, v := range want {
		if got.Heads[k] != v {
			t.Errorf("heads[%s] = %q, want %q", k, got.Heads[k], v)
		}
	}
}

// Review Focus 1 across the two deliveries: a webhook and a poll for the same
// head are one run and one check run, whichever comes first.
func TestAWebhookAndAPollForTheSameHeadAreOneRun(t *testing.T) {
	for _, order := range []string{"webhook first", "poll first"} {
		t.Run(order, func(t *testing.T) {
			h := newHarness(t)
			p := testPipeline(DeliveryPoll)
			p.Heads = map[string]string{"branch:main": shaA, "pr:5": shaA}
			h.store.addPipeline(p)
			h.github.heads[repoName+"@main"] = headAnswer{SHA: shaB}
			h.github.pulls[repoName] = []githubapp.PullRequestHead{
				{Number: 5, HeadSHA: shaC, HeadRef: "cart-badge", HeadRepository: repoName, BaseSHA: shaBase},
			}
			webhook := func() {
				trigger(t, h, "push", "d-push", pushDelivery(t, "refs/heads/main", shaA, shaB, testInstallation))
				trigger(t, h, "pull_request", "d-pr", prDelivery(t, "synchronize", 5, shaC, repoName, testInstallation))
			}
			if order == "webhook first" {
				webhook()
				poll(t, h)
			} else {
				poll(t, h)
				webhook()
			}
			if n := len(h.store.allRuns()); n != 2 {
				t.Errorf("runs = %d, want 2 (the push and the pull request, once each)", n)
			}
			if n := len(h.github.createdRuns()); n != 2 {
				t.Errorf("check runs = %d, want 2", n)
			}
		})
	}
}

func TestAnUnchangedRepositoryWritesNothing(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryPoll)
	p.Heads = map[string]string{"branch:main": shaA}
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA}

	res := poll(t, h)
	if len(res.Opened) != 0 || res.Baselines != 0 {
		t.Fatalf("nothing moved: %+v", res)
	}
	if n := len(h.store.pipelineUpdates); n != 0 {
		t.Errorf("an unchanged head is not rewritten: %d pipeline writes", n)
	}
}

// A head whose run could not be opened keeps its previous value, so the next
// poll asks again instead of the change being lost.
func TestAFailedOpenKeepsThePreviousHead(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryPoll)
	p.Heads = map[string]string{"branch:main": shaA}
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaB}
	h.github.pulls[repoName] = []githubapp.PullRequestHead{
		{Number: 3, HeadSHA: shaC, HeadRef: "x", HeadRepository: repoName},
	}
	h.store.failCreateRun = errors.New("database unavailable")

	res := poll(t, h)
	if len(res.Failed) != 1 {
		t.Fatalf("the failure is reported: %+v", res)
	}
	got, _ := h.store.pipeline(p.ID)
	if got.Heads["branch:main"] != shaA {
		t.Errorf("the default head must stay at the last one a run answered: %v", got.Heads)
	}
	if _, kept := got.Heads["pr:3"]; kept {
		t.Errorf("a new pull request whose run did not open must not be recorded as seen: %v", got.Heads)
	}

	h.store.failCreateRun = nil
	res = poll(t, h)
	if len(res.Opened) != 2 {
		t.Errorf("the next poll opens what the last one could not: opened %d", len(res.Opened))
	}
}

// The pull requests could not be read -- GitHub answered 502, or the list
// timed out. The default branch is a separate question with its own answer:
// a push the poll did see still opens its run, every pull request's head
// stays as the last poll recorded it (a read that failed proves nothing about
// which are closed), and the failure is reported and said.
func TestAFailedPullRequestReadStillRunsTheDefaultBranch(t *testing.T) {
	h := newHarness(t)
	logs := &lockedBuffer{}
	h.integ.Configure(func(d *Deps) { d.Logger = slog.New(slog.NewTextHandler(logs, nil)) })
	p := testPipeline(DeliveryPoll)
	p.Heads = map[string]string{"branch:main": shaA, "pr:5": shaB}
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaB, Message: "Land it"}
	h.github.pullsErr = errStatus(502)

	res := poll(t, h)
	if len(res.Opened) != 1 || res.Opened[0].Event != pipelines.EventPush || res.Opened[0].SHA != shaB {
		t.Fatalf("the moved default branch opens its push run: %+v", res.Opened)
	}
	if len(res.Failed) != 1 || res.Failed[0].PipelineID != p.ID || !strings.Contains(res.Failed[0].Reason, "pull requests") {
		t.Errorf("the failed read is reported: %+v", res.Failed)
	}
	if !strings.Contains(logs.String(), "could not be polled") || !strings.Contains(logs.String(), "pull requests") {
		t.Errorf("and said in the log:\n%s", logs.String())
	}
	got, _ := h.store.pipeline(p.ID)
	if got.Heads["branch:main"] != shaB || got.Heads["pr:5"] != shaB || len(got.Heads) != 2 {
		t.Errorf("heads = %v; want the branch moved and the pull request kept as it was", got.Heads)
	}

	// The next poll that can read them carries on from there.
	h.github.pullsErr = nil
	h.github.pulls[repoName] = []githubapp.PullRequestHead{{Number: 5, HeadSHA: shaC, HeadRef: "cart", HeadRepository: repoName}}
	res = poll(t, h)
	if len(res.Opened) != 1 || res.Opened[0].PullRequest != 5 || len(res.Failed) != 0 {
		t.Errorf("the next poll opens the moved pull request: %+v", res)
	}
}

// A FIRST poll whose pull requests cannot be read records no baseline: a
// baseline without them would make every open pull request look new at the
// next poll, and open a run for each -- the flood the baseline exists to
// prevent. It reports the failure and leaves the baseline to the next poll.
func TestAFirstPollThatCannotReadPullRequestsRecordsNoBaseline(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryPoll)
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA}
	h.github.pulls[repoName] = []githubapp.PullRequestHead{{Number: 5, HeadSHA: shaB, HeadRef: "cart", HeadRepository: repoName}}
	h.github.pullsErr = errStatus(502)

	res := poll(t, h)
	if res.Baselines != 0 || len(res.Opened) != 0 || len(res.Failed) != 1 {
		t.Fatalf("no baseline, nothing opened, the failure reported: %+v", res)
	}
	if got, _ := h.store.pipeline(p.ID); len(got.Heads) != 0 {
		t.Errorf("heads = %v, want none recorded", got.Heads)
	}

	h.github.pullsErr = nil
	res = poll(t, h)
	if res.Baselines != 1 || len(res.Opened) != 0 {
		t.Errorf("the next poll records the whole baseline and opens nothing: %+v", res)
	}
	if got, _ := h.store.pipeline(p.ID); got.Heads["pr:5"] != shaB || got.Heads["branch:main"] != shaA {
		t.Errorf("heads = %v", got.Heads)
	}
}

// A default branch renamed on GitHub since connect: the poll asks which
// branch is the default now, records it, and treats its head as a baseline --
// a rename is not a push -- rather than failing every minute.
func TestARenamedDefaultBranchIsFollowedNotRun(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryPoll)
	p.DefaultBranch = "master"
	p.Heads = map[string]string{"branch:master": shaA}
	h.store.addPipeline(p)
	h.github.repos[repoName] = githubapp.RepositoryInfo{FullName: repoName, DefaultBranch: "main"}
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaB}

	res := poll(t, h)
	if len(res.Failed) != 0 || len(res.Opened) != 0 {
		t.Fatalf("a rename opens nothing and fails nothing: %+v", res)
	}
	got, _ := h.store.pipeline(p.ID)
	if got.DefaultBranch != "main" || got.Heads["branch:main"] != shaB || len(got.Heads) != 1 {
		t.Errorf("default branch %q heads %v; want main at the new head, the old key gone", got.DefaultBranch, got.Heads)
	}
}

func TestOnlyPolledPipelinesArePolled(t *testing.T) {
	h := newHarness(t)
	webhook := testPipeline(DeliveryWebhook)
	h.store.addPipeline(webhook)

	res := poll(t, h)
	if res.Pipelines != 0 || len(h.github.tokenMints) != 0 {
		t.Errorf("a webhook pipeline is not polled: %+v, %d mints", res, len(h.github.tokenMints))
	}
}

// The poll writes its heads under the repository's gate -- the key every
// writer of the pipeline row takes -- and takes it only to write: a baseline
// is one gated section, after every GitHub read (the fakes fail any GitHub
// call made under a gate).
func TestThePollWritesItsHeadsUnderTheRepositorysGate(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryPoll)
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA}

	poll(t, h)
	if seen := h.gate.seen(); len(seen) != 1 || seen[0] != RepositoryGateKey(repoName) {
		t.Errorf("gate keys = %v, want exactly %q", seen, RepositoryGateKey(repoName))
	}
}

// The heads write is a compare-and-swap against the row the poll read: a
// writer that moved the row while the poll talked to GitHub -- here a
// reconnect to another repository, which clears the heads -- wins, and the
// poll's stale heads are not written over it.
func TestAPollDoesNotWriteHeadsOverARowThatMovedMeanwhile(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryPoll)
	p.Heads = map[string]string{"branch:main": shaA}
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaB}
	// The reconnect lands between the poll's reads of GitHub and its write.
	h.github.onBranchHead = func() {
		moved := p
		moved.Repository, moved.Heads = "acme/elsewhere", map[string]string{}
		h.store.addPipeline(moved)
	}

	res := poll(t, h)
	if len(res.Opened) != 1 {
		t.Fatalf("the push the poll saw still opens: %+v", res)
	}
	got, _ := h.store.pipeline(p.ID)
	if got.Repository != "acme/elsewhere" || len(got.Heads) != 0 {
		t.Errorf("the moved row keeps what its writer wrote: %s %v", got.Repository, got.Heads)
	}
	for _, u := range h.store.pipelineUpdates {
		if u.Patch.Heads != nil {
			t.Errorf("the poll wrote heads over a row that moved: %v", *u.Patch.Heads)
		}
	}
}

func TestThePollCallsTheRecoveryHook(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.integ.Configure(func(d *Deps) {
		d.Recover = func(context.Context) error { calls++; return errors.New("stranded") }
	})
	res := poll(t, h)
	if calls != 1 {
		t.Errorf("recover ran %d times, want once after polling", calls)
	}
	if res.RecoverError != "stranded" {
		t.Errorf("the hook's failure is reported: %q", res.RecoverError)
	}

	// Nil is a no-op: every node that does not drive.
	h.integ.Configure(func(d *Deps) { d.Recover = nil })
	if _, err := h.integ.Poll(automationCtx()); err != nil {
		t.Errorf("poll with no hook: %v", err)
	}
}

func TestAPipelineThatCannotBePolledDoesNotStopTheOthers(t *testing.T) {
	h := newHarness(t)
	broken := testPipeline(DeliveryPoll)
	broken.ID, broken.PackageID = PipelineIDFor("pkg-broken"), "pkg-broken"
	broken.Repository = "acme/gone" // no head on the fake: 404
	h.store.addPipeline(broken)
	ok := testPipeline(DeliveryPoll)
	h.store.addPipeline(ok)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA}

	res := poll(t, h)
	if len(res.Failed) != 1 || res.Failed[0].PipelineID != broken.ID {
		t.Errorf("failed = %+v", res.Failed)
	}
	if res.Baselines != 1 {
		t.Errorf("the healthy pipeline still recorded its baseline: %d", res.Baselines)
	}
}

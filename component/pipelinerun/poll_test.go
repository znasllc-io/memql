package pipelinerun

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// poll_test.go -- the polling delivery (D4, D11; decision 13).

func poll(t *testing.T, h *harness) PollResult {
	t.Helper()
	res, err := h.integ.Poll(context.Background())
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

func TestThePollHoldsThePipelinesGate(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryPoll)
	h.store.addPipeline(p)
	h.github.heads[repoName+"@main"] = headAnswer{SHA: shaA}

	poll(t, h)
	seen := h.gate.seen()
	if len(seen) == 0 || seen[0] != PipelineGateKey(p.ID) {
		t.Errorf("gate keys = %v, want %q first", seen, PipelineGateKey(p.ID))
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
	if _, err := h.integ.Poll(context.Background()); err != nil {
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

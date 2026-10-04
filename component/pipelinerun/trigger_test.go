package pipelinerun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// trigger_test.go -- a staged GitHub delivery becomes runs (design record
// D4-D6, plan decisions 1, 3, 4, 14, 15; Review Focus 1 and 3).

// trigger stages body as the inbound receiver would and hands the trigger the
// row's id, on the context the shipped automation calls it on.
func trigger(t *testing.T, h *harness, event, deliveryID, body string) TriggerResult {
	t.Helper()
	res, err := h.integ.Trigger(automationCtx(), h.stage(t, event, deliveryID, body))
	if err != nil {
		t.Fatalf("trigger %s: %v", event, err)
	}
	return res
}

// THE CRITICAL ONE (memql#5488 review): a builtin is callable by any
// signed-in client -- @sdk has no engine effect -- so the trigger and the poll
// refuse every call that did not arrive with internal origin, through the
// handler and through the Go method alike, and they refuse it before anything
// is read, minted or written.
func TestTheTriggerAndThePollRefuseAClient(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	h.store.addPipeline(func() Pipeline {
		p := testPipeline(DeliveryPoll)
		p.ID, p.PackageID, p.Repository = PipelineIDFor("pkg-polled"), "pkg-polled", "acme/polled"
		p.Heads = map[string]string{"branch:main": shaA}
		return p
	}())
	h.github.heads["acme/polled@main"] = headAnswer{SHA: shaB}
	id := h.stage(t, "pull_request", "d1", prDelivery(t, "opened", 42, shaA, repoName, testInstallation))

	for name, ctx := range map[string]context.Context{
		"a signed-in person": personCtx(ownerID),
		"a cluster owner":    clusterOwnerCtx(ownerID),
		"nobody":             context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.integ.Trigger(ctx, id); !errors.Is(err, ErrClientOrigin) {
				t.Errorf("Trigger: %v, want ErrClientOrigin", err)
			}
			if _, err := h.integ.handleTrigger(ctx, map[string]any{"inboundRequestId": id}, 0); !errors.Is(err, ErrClientOrigin) {
				t.Errorf("the trigger capability: %v, want ErrClientOrigin", err)
			}
			if _, err := h.integ.Poll(ctx); !errors.Is(err, ErrClientOrigin) {
				t.Errorf("Poll: %v, want ErrClientOrigin", err)
			}
			if _, err := h.integ.handlePoll(ctx, nil, 0); !errors.Is(err, ErrClientOrigin) {
				t.Errorf("the poll capability: %v, want ErrClientOrigin", err)
			}
		})
	}
	if n := len(h.store.allRuns()); n != 0 {
		t.Errorf("a refused call opened %d runs", n)
	}
	if n := len(h.github.tokenMints) + len(h.github.createdRuns()); n != 0 {
		t.Errorf("a refused call reached GitHub %d times", n)
	}
	if n := len(h.store.pipelineUpdates); n != 0 {
		t.Errorf("a refused poll wrote %d pipeline rows", n)
	}

	// The control: the same delivery and the same poll, on the automation's
	// context, do what they are for -- so the refusals above are about the
	// origin and nothing else.
	if res, err := h.integ.Trigger(automationCtx(), id); err != nil || len(res.Opened) != 1 {
		t.Fatalf("the automation's trigger: %+v %v", res, err)
	}
	if res, err := h.integ.Poll(automationCtx()); err != nil || len(res.Opened) != 1 {
		t.Fatalf("the automation's poll: %+v %v", res, err)
	}
}

// The trigger acts on the STAGED ROW and on nothing its caller hands it: a
// body, headers or a source passed as arguments -- which the automation no
// longer passes, and a caller could forge -- change nothing about what opens.
func TestAForgedBodyInTheArgumentsHasNoEffect(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	id := h.stage(t, "pull_request", "d1", prDelivery(t, "opened", 42, shaA, repoName, testInstallation))

	nodes, err := h.integ.handleTrigger(automationCtx(), map[string]any{
		"inboundRequestId": id,
		"source":           "github",
		"body":             pushDelivery(t, "refs/heads/main", shaA, shaC, testInstallation),
		"headersJson":      deliveryHeadersJSON(t, "push", "forged"),
	}, 0)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	runs := h.store.allRuns()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want the staged delivery's one", len(runs))
	}
	if r := runs[0]; r.Event != pipelines.EventPullRequest || r.SHA != shaA || r.PullRequest != 42 || r.DeliveryID != "d1" {
		t.Errorf("the run is the staged row's pull request, not the forged push: %+v", r)
	}
	for _, r := range runs {
		if r.SHA == shaC {
			t.Errorf("the forged body opened a run at %s", shaC)
		}
	}
	if payload := decodeNode(t, nodes); payload["event"] != "pull_request" || payload["inboundRequestId"] != id {
		t.Errorf("answer = %v", payload)
	}
}

// A delivery the receiver did not verify -- the github source configured to
// sign nothing -- is anybody's body, and is refused before anything is
// minted, opened or written.
func TestAnUnsignedDeliveryIsRefused(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	id := h.stageFrom(t, githubSource, false,
		prDelivery(t, "opened", 42, shaA, repoName, testInstallation), deliveryHeadersJSON(t, "pull_request", "d1"))

	if _, err := h.integ.Trigger(automationCtx(), id); !errors.Is(err, ErrDeliveryUnverified) {
		t.Fatalf("err = %v, want ErrDeliveryUnverified", err)
	}
	if n := len(h.store.allRuns()); n != 0 {
		t.Errorf("an unsigned delivery opened %d runs", n)
	}
	if n := len(h.github.tokenMints) + len(h.github.createdRuns()); n != 0 {
		t.Errorf("an unsigned delivery reached GitHub %d times", n)
	}
}

func TestATriggerForNoStagedRowIsAnError(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	if _, err := h.integ.Trigger(automationCtx(), "inbound-nobody-staged"); err == nil {
		t.Errorf("a delivery id no row answers for must be an error, not a silent no-op")
	}
	if _, err := h.integ.Trigger(automationCtx(), "  "); err == nil {
		t.Errorf("no delivery id is an error")
	}
}

func TestAPullRequestDeliveryOpensOneQueuedRunWithItsCheckRun(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)

	res := trigger(t, h, "pull_request", "delivery-1", prDelivery(t, "opened", 42, shaA, repoName, testInstallation))
	if len(res.Opened) != 1 {
		t.Fatalf("one delivery, one run: opened %d (%+v)", len(res.Opened), res)
	}
	run, ok := h.store.run(res.Opened[0].ID)
	if !ok {
		t.Fatalf("the opened run %q is not in the store", res.Opened[0].ID)
	}
	key := pipelines.RunKey(repoName, shaA, pipelines.ModeAffected, pipelines.EventPullRequest)
	for name, got := range map[string][2]string{
		"status":     {run.Status, StatusQueued},
		"mode":       {string(run.Mode), string(pipelines.ModeAffected)},
		"event":      {string(run.Event), string(pipelines.EventPullRequest)},
		"runKey":     {run.RunKey, key},
		"trigger":    {run.Trigger, TriggerWebhook},
		"deliveryId": {run.DeliveryID, "delivery-1"},
		"sha":        {run.SHA, shaA},
		"baseSha":    {run.BaseSHA, shaBase},
		"headBranch": {run.HeadBranch, "cart-badge"},
		"version":    {run.Version, shaA},
		"pipelineId": {run.PipelineID, p.ID},
		"owner":      {run.OwnerUserID, p.OwnerUserID},
		"accountId":  {run.AccountID, p.AccountID},
		"state":      {run.CheckRunState, CheckRunWritten},
		"id":         {run.ID, RunIDFor(p.ID, key, 1)},
	} {
		if got[0] != got[1] {
			t.Errorf("%s = %q, want %q", name, got[0], got[1])
		}
	}
	if run.Attempt != 1 || run.PullRequest != 42 {
		t.Errorf("attempt %d, pull request %d; want 1 and 42", run.Attempt, run.PullRequest)
	}

	created := h.github.createdRuns()
	if len(created) != 1 {
		t.Fatalf("one run, one check run: GitHub saw %d creates", len(created))
	}
	cr := created[0]
	if run.CheckRunID != cr.ID {
		t.Errorf("the run row names check run %d; GitHub created %d", run.CheckRunID, cr.ID)
	}
	if cr.Run.Name != "MemQL / shop" || cr.Run.HeadSHA != shaA || cr.Run.Status != "queued" {
		t.Errorf("check run = %+v", cr.Run)
	}
	if cr.Run.ExternalID != run.ID {
		t.Errorf("external_id = %q, want the run id %q", cr.Run.ExternalID, run.ID)
	}
	if want := pipelines.RunPageURL(testOSOrigin, run.ID); cr.Run.DetailsURL != want {
		t.Errorf("details_url = %q, want %q", cr.Run.DetailsURL, want)
	}
	if cr.Run.Conclusion != "" || cr.Run.Output == nil || cr.Run.Output.Title != "Queued" {
		t.Errorf("a queued check run carries no conclusion and says Queued: %+v", cr.Run)
	}
	if cr.Token != "ghs_"+credID || cr.Repository != repoName {
		t.Errorf("the check run went to %q under %q; want %q under the owner's grant", cr.Repository, cr.Token, repoName)
	}
	mints := h.github.tokenMints
	if len(mints) == 0 || mints[0].CredentialID != credID || mints[0].Owner != p.OwnerUserID {
		t.Errorf("the token must be minted through the pipeline OWNER's grant: %+v", mints)
	}
}

// Review Focus 1: a webhook redelivered with the same body is one run and
// one check run.
func TestARedeliveredWebhookOpensOneRunAndOneCheckRun(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	body := prDelivery(t, "synchronize", 42, shaA, repoName, testInstallation)

	first := trigger(t, h, "pull_request", "delivery-1", body)
	second := trigger(t, h, "pull_request", "delivery-1", body)
	if len(first.Opened) != 1 || len(second.Opened) != 0 || len(second.Existing) != 1 {
		t.Fatalf("first opened %d, second opened %d with %d existing; want 1, 0, 1", len(first.Opened), len(second.Opened), len(second.Existing))
	}
	if second.Existing[0].ID != first.Opened[0].ID {
		t.Errorf("the redelivery must answer with the run already open")
	}
	if n := len(h.store.allRuns()); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
	if n := len(h.github.createdRuns()); n != 1 {
		t.Errorf("check runs created = %d, want 1", n)
	}
}

// Review Focus 1, under contention: every opener of one head serializes on
// the run key's gate, so N at once are still one run and one check run.
func TestConcurrentDeliveriesOfOneHeadOpenOneRun(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	id := h.stage(t, "push", "d", pushDelivery(t, "refs/heads/main", shaA, shaB, testInstallation))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.integ.Trigger(automationCtx(), id); err != nil {
				t.Errorf("trigger: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := len(h.store.allRuns()); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
	if n := len(h.github.createdRuns()); n != 1 {
		t.Errorf("check runs created = %d, want 1", n)
	}
	key := pipelines.RunKey(repoName, shaB, pipelines.ModeFull, pipelines.EventPush)
	found := false
	for _, k := range h.gate.seen() {
		if k == OpenGateKey(key) {
			found = true
		}
	}
	if !found {
		t.Errorf("the open must run under the run key's gate %q; the gate saw %v", OpenGateKey(key), h.gate.seen())
	}
}

// Decision 1: the merge queue's full run and the default branch's push run
// of one SHA are TWO runs -- the event keeps them apart.
func TestTheMergeQueueAndThePushOfOneSHAAreTwoRuns(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))

	mg := trigger(t, h, "merge_group", "d1", mergeGroupDelivery(t, shaB, testInstallation))
	push := trigger(t, h, "push", "d2", pushDelivery(t, "refs/heads/main", shaA, shaB, testInstallation))
	if len(mg.Opened) != 1 || len(push.Opened) != 1 {
		t.Fatalf("merge group opened %d, push opened %d; want one each", len(mg.Opened), len(push.Opened))
	}
	if mg.Opened[0].Mode != pipelines.ModeFull || push.Opened[0].Mode != pipelines.ModeFull {
		t.Errorf("both run full: %s, %s", mg.Opened[0].Mode, push.Opened[0].Mode)
	}
	if mg.Opened[0].RunKey == push.Opened[0].RunKey {
		t.Errorf("one key for two events: %q", mg.Opened[0].RunKey)
	}
	if push.Opened[0].BaseSHA != shaA || push.Opened[0].Title != "Land the cart badge" {
		t.Errorf("push run base %q title %q", push.Opened[0].BaseSHA, push.Opened[0].Title)
	}
}

// D6 and decision 15: a fork's pull request is REFUSED, never queued, and its
// check run FAILS.
func TestAForkPullRequestIsRefusedWithAFailingCheckRun(t *testing.T) {
	for name, head := range map[string]string{
		"another repository": "mallory/shop",
		"a deleted fork":     "",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.store.addPipeline(testPipeline(DeliveryWebhook))
			res := trigger(t, h, "pull_request", "d1", prDelivery(t, "opened", 7, shaA, head, testInstallation))
			if len(res.Opened) != 1 {
				t.Fatalf("opened %d, want one refused run", len(res.Opened))
			}
			run, _ := h.store.run(res.Opened[0].ID)
			if run.Status != StatusCompleted || run.Conclusion != ConclusionRefused || run.RefusalCode != pipelines.CodeForkRefused {
				t.Fatalf("a fork's run is completed and refused with %s; got %s/%s/%s", pipelines.CodeForkRefused, run.Status, run.Conclusion, run.RefusalCode)
			}
			if run.RefusalMessage == "" || run.FinishedAt.IsZero() {
				t.Errorf("a refusal says why and when: %+v", run)
			}
			created := h.github.createdRuns()
			if len(created) != 1 {
				t.Fatalf("check runs = %d, want 1", len(created))
			}
			cr := created[0].Run
			if cr.Status != "completed" || cr.Conclusion != "failure" {
				t.Errorf("a fork's check run is completed with failure (neutral would satisfy a required check): %s/%s", cr.Status, cr.Conclusion)
			}
			if cr.Output == nil || cr.Output.Title != "Refused: pull request from a fork" {
				t.Errorf("title = %+v", cr.Output)
			}
			if cr.CompletedAt.IsZero() {
				t.Errorf("a completed check run carries its completion time")
			}
		})
	}
}

// Decision 3: a re-requested check run is the NEXT ATTEMPT of the run it
// reports, in that run's mode and event.
func TestACheckRunRerequestOpensTheNextAttemptOfTheOriginal(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	key := pipelines.RunKey(repoName, shaA, pipelines.ModeAffected, pipelines.EventPullRequest)
	original := Run{
		ID: RunIDFor(p.ID, key, 1), OwnerUserID: p.OwnerUserID, PipelineID: p.ID, Repository: repoName, SHA: shaA,
		Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest, RunKey: key, Attempt: 1, Trigger: TriggerWebhook,
		PullRequest: 42, HeadBranch: "cart-badge", BaseSHA: shaBase, Title: "Show the cart count", Version: shaA,
		Status: StatusCompleted, Conclusion: ConclusionFailure, CheckRunID: 4242, CheckRunState: CheckRunWritten,
		QueuedAt: testNow.Add(-tenMinutes),
	}
	h.store.addRun(original)

	res := trigger(t, h, "check_run", "d9", checkRunDelivery(t, 4242, shaA, testInstallation))
	if len(res.Opened) != 1 {
		t.Fatalf("opened %d, want the next attempt (%+v)", len(res.Opened), res)
	}
	next, _ := h.store.run(res.Opened[0].ID)
	if next.Attempt != 2 || next.RunKey != key || next.Mode != pipelines.ModeAffected || next.Event != pipelines.EventPullRequest {
		t.Errorf("next attempt = %d key %q mode %s event %s", next.Attempt, next.RunKey, next.Mode, next.Event)
	}
	if next.Trigger != TriggerRerun || next.RerunOf != original.ID || next.ID != RunIDFor(p.ID, key, 2) {
		t.Errorf("trigger %q rerunOf %q id %q", next.Trigger, next.RerunOf, next.ID)
	}
	if next.PullRequest != 42 || next.HeadBranch != "cart-badge" || next.Status != StatusQueued {
		t.Errorf("the re-run carries the original's facts and is queued: %+v", next)
	}
	if orig, _ := h.store.run(original.ID); orig.Status != StatusCompleted || orig.Conclusion != ConclusionFailure {
		t.Errorf("the original stays exactly as it ended: %+v", orig)
	}
	if n := len(h.github.createdRuns()); n != 1 {
		t.Errorf("the re-run gets a check run of its own: %d created", n)
	}

	// The same re-request, redelivered: one attempt, not two.
	again := trigger(t, h, "check_run", "d9", checkRunDelivery(t, 4242, shaA, testInstallation))
	if len(again.Opened) != 0 {
		t.Errorf("a redelivered re-request opened %d more attempts", len(again.Opened))
	}
}

func TestACheckSuiteRerequestRerunsTheNewestRunAtTheSHA(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	mgKey := pipelines.RunKey(repoName, shaB, pipelines.ModeFull, pipelines.EventMergeGroup)
	pushKey := pipelines.RunKey(repoName, shaB, pipelines.ModeFull, pipelines.EventPush)
	h.store.addRun(Run{ID: RunIDFor(p.ID, mgKey, 1), OwnerUserID: p.OwnerUserID, PipelineID: p.ID, Repository: repoName, SHA: shaB,
		Mode: pipelines.ModeFull, Event: pipelines.EventMergeGroup, RunKey: mgKey, Attempt: 1, Status: StatusCompleted,
		Conclusion: ConclusionSuccess, QueuedAt: testNow.Add(-2 * tenMinutes)})
	h.store.addRun(Run{ID: RunIDFor(p.ID, pushKey, 1), OwnerUserID: p.OwnerUserID, PipelineID: p.ID, Repository: repoName, SHA: shaB,
		Mode: pipelines.ModeFull, Event: pipelines.EventPush, RunKey: pushKey, Attempt: 1, Status: StatusCompleted,
		Conclusion: ConclusionFailure, QueuedAt: testNow.Add(-tenMinutes)})

	res := trigger(t, h, "check_suite", "d3", checkSuiteDelivery(t, shaB, testInstallation))
	if len(res.Opened) != 1 {
		t.Fatalf("opened %d, want one", len(res.Opened))
	}
	if got := res.Opened[0]; got.RunKey != pushKey || got.Attempt != 2 || got.Event != pipelines.EventPush {
		t.Errorf("the newest run at the SHA is the push's: re-ran key %q attempt %d", got.RunKey, got.Attempt)
	}
}

func TestARerequestOfARunStillGoingOpensNothing(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	key := pipelines.RunKey(repoName, shaA, pipelines.ModeFull, pipelines.EventPush)
	h.store.addRun(Run{ID: RunIDFor(p.ID, key, 1), OwnerUserID: p.OwnerUserID, PipelineID: p.ID, Repository: repoName, SHA: shaA,
		Mode: pipelines.ModeFull, Event: pipelines.EventPush, RunKey: key, Attempt: 1, Status: StatusInProgress,
		CheckRunID: 77, QueuedAt: testNow})

	res := trigger(t, h, "check_run", "d4", checkRunDelivery(t, 77, shaA, testInstallation))
	if len(res.Opened) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("a run still going is not re-run: %+v", res)
	}
}

func TestAReleaseResolvesItsTagToTheCommit(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	h.github.commits[repoName+"@tags/v1.4.0"] = headAnswer{SHA: shaC, Message: "Release 1.4"}

	res := trigger(t, h, "release", "d5", releaseDelivery(t, "v1.4.0", testInstallation))
	if len(res.Opened) != 1 {
		t.Fatalf("opened %d (%+v)", len(res.Opened), res)
	}
	run := res.Opened[0]
	if run.SHA != shaC || run.Event != pipelines.EventRelease || run.Mode != pipelines.ModeFull {
		t.Errorf("release run sha %s event %s mode %s", run.SHA, run.Event, run.Mode)
	}
	if run.Version != "v1.4.0" {
		t.Errorf("MEMQL_VERSION is the release tag: %q", run.Version)
	}
	if run.Title != "Shop v1.4.0" {
		t.Errorf("title = %q", run.Title)
	}
}

func TestADeliveryThroughAnotherInstallationIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook)) // installation 7

	res := trigger(t, h, "pull_request", "d6", prDelivery(t, "opened", 42, shaA, repoName, 99))
	if len(res.Opened) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("a delivery through installation 99 must not drive a pipeline connected through 7: %+v", res)
	}
	if !strings.Contains(res.Skipped[0].Reason, "installation 99") {
		t.Errorf("reason = %q", res.Skipped[0].Reason)
	}
	if n := len(h.store.allRuns()); n != 0 {
		t.Errorf("runs = %d", n)
	}
}

func TestDeliveriesPipelinesDoNotRunOnAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	for name, c := range map[string]struct{ source, event, body string }{
		"another source":        {"shopify-1", "push", pushDelivery(t, "refs/heads/main", shaA, shaB, testInstallation)},
		"a closed pull request": {"github", "pull_request", prDelivery(t, "closed", 42, shaA, repoName, testInstallation)},
		"another branch":        {"github", "push", pushDelivery(t, "refs/heads/feature", shaA, shaB, testInstallation)},
		"a header that disagrees with the body": {"github", "push",
			prDelivery(t, "opened", 42, shaA, repoName, testInstallation)},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := h.integ.Trigger(automationCtx(), h.stageFrom(t, c.source, true, c.body, deliveryHeadersJSON(t, c.event, "d")))
			if err != nil {
				t.Fatalf("an ignored delivery is not an error: %v", err)
			}
			if res.Ignored == "" || len(res.Opened) != 0 {
				t.Errorf("want ignored with nothing opened: %+v", res)
			}
		})
	}
	if n := len(h.store.allRuns()); n != 0 {
		t.Errorf("runs = %d", n)
	}
}

// Review Focus 3: an app installed before pipelines answers every check-run
// write with 403. The run still opens, carrying the state and the note.
func TestA403OnTheCheckRunStillOpensTheRun(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	h.github.createErr = &githubapp.StatusError{Status: 403, Endpoint: "/repos/acme/shop/check-runs"}

	res := trigger(t, h, "pull_request", "d1", prDelivery(t, "opened", 42, shaA, repoName, testInstallation))
	if len(res.Opened) != 1 {
		t.Fatalf("the run must open regardless: %+v", res)
	}
	run, _ := h.store.run(res.Opened[0].ID)
	if run.Status != StatusQueued {
		t.Errorf("status = %q; a refused check run never stops the run", run.Status)
	}
	if run.CheckRunState != CheckRunRefused || run.CheckRunID != 0 {
		t.Errorf("checkRunState %q id %d; want refused and none", run.CheckRunState, run.CheckRunID)
	}
	if len(run.Notes) != 1 || run.Notes[0].Code != pipelines.CodeCheckPermission || run.Notes[0].Message == "" {
		t.Errorf("notes = %+v; want one %s with its sentence", run.Notes, pipelines.CodeCheckPermission)
	}
	if class, ok := pipelines.ClassOf(pipelines.CodeCheckPermission); !ok || class != pipelines.ClassNote {
		t.Errorf("the code must be a catalogued note")
	}
}

func TestARateLimited403IsNotAMissingPermission(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	h.github.createErr = &githubapp.StatusError{Status: 403, RateLimited: true}

	res := trigger(t, h, "pull_request", "d1", prDelivery(t, "opened", 42, shaA, repoName, testInstallation))
	run, _ := h.store.run(res.Opened[0].ID)
	if run.CheckRunState != CheckRunUnavailable || len(run.Notes) != 0 {
		t.Errorf("a rate limit is unavailable with no permission note: %q %+v", run.CheckRunState, run.Notes)
	}
}

func TestNoTokenOpensTheRunWithTheCheckRunUnavailable(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	h.github.tokenErr = errors.New("reconnect_required: GitHub refused the grant")

	res := trigger(t, h, "pull_request", "d1", prDelivery(t, "opened", 42, shaA, repoName, testInstallation))
	if len(res.Opened) != 1 {
		t.Fatalf("opened %d", len(res.Opened))
	}
	run, _ := h.store.run(res.Opened[0].ID)
	if run.Status != StatusQueued || run.CheckRunState != CheckRunUnavailable {
		t.Errorf("status %q checkRunState %q", run.Status, run.CheckRunState)
	}
	if n := len(h.github.createdRuns()); n != 0 {
		t.Errorf("no token, no check run: %d created", n)
	}
}

// The dedup is PER PIPELINE: a disconnected predecessor's run of a head is
// not the new pipeline's, so it must not stop the new pipeline opening its
// own -- and the two are different rows, because the run id carries the
// pipeline. The new pipeline's own redelivery still dedups.
func TestAPredecessorsRunDoesNotBlockANewPipelinesRun(t *testing.T) {
	h := newHarness(t)
	old := testPipeline(DeliveryWebhook)
	old.ID, old.PackageID, old.OwnerUserID = PipelineIDFor("pkg-old"), "pkg-old", "v1:identity:user:"+otherID
	old.Status = PipelineDisconnected
	h.store.addPipeline(old)
	key := pipelines.RunKey(repoName, shaA, pipelines.ModeAffected, pipelines.EventPullRequest)
	predecessor := Run{
		ID: RunIDFor(old.ID, key, 1), OwnerUserID: old.OwnerUserID,
		// Canonical, as a relationship field reads back.
		PipelineID: "v1:pipelines:pipeline:" + old.ID,
		Repository: repoName, SHA: shaA, Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest,
		RunKey: key, Attempt: 1, Status: StatusCompleted, Conclusion: ConclusionSuccess, QueuedAt: testNow.Add(-tenMinutes),
	}
	h.store.addRun(predecessor)
	current := testPipeline(DeliveryWebhook)
	h.store.addPipeline(current)

	body := prDelivery(t, "opened", 42, shaA, repoName, testInstallation)
	res := trigger(t, h, "pull_request", "d1", body)
	if len(res.Opened) != 1 || len(res.Existing) != 0 {
		t.Fatalf("opened %d existing %d; the predecessor's run must not answer for the new pipeline", len(res.Opened), len(res.Existing))
	}
	opened := res.Opened[0]
	if !sameID(opened.PipelineID, current.ID) || opened.Attempt != 1 || opened.ID == predecessor.ID {
		t.Errorf("the new pipeline's first attempt is a row of its own: %+v", opened)
	}
	if got, _ := h.store.run(predecessor.ID); got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess || got.OwnerUserID != old.OwnerUserID {
		t.Errorf("the predecessor's run is untouched: %+v", got)
	}
	again := trigger(t, h, "pull_request", "d1", body)
	if len(again.Opened) != 0 || len(again.Existing) != 1 || again.Existing[0].ID != opened.ID {
		t.Errorf("the new pipeline's own redelivery dedups: %+v", again)
	}
}

func TestADisconnectedPipelineOpensNothing(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	p.Status = PipelineDisconnected
	h.store.addPipeline(p)

	res := trigger(t, h, "pull_request", "d1", prDelivery(t, "opened", 42, shaA, repoName, testInstallation))
	if len(res.Opened) != 0 {
		t.Fatalf("a disconnected pipeline opened %d runs", len(res.Opened))
	}
}

func TestTheTriggerFailsClosedWithoutAGate(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	h.gate.refuse = errors.New("github: lifecycle database unavailable")

	_, err := h.integ.Trigger(automationCtx(), h.stage(t, "pull_request", "d1", prDelivery(t, "opened", 42, shaA, repoName, testInstallation)))
	if err == nil {
		t.Fatalf("no gate, no run: the open must refuse")
	}
	if n := len(h.store.allRuns()); n != 0 {
		t.Errorf("runs = %d", n)
	}
}

func TestTheTriggerCapabilityAnswersWhatItDid(t *testing.T) {
	h := newHarness(t)
	h.store.addPipeline(testPipeline(DeliveryWebhook))
	id := h.stage(t, "pull_request", "d1", prDelivery(t, "opened", 42, shaA, repoName, testInstallation))
	nodes, err := h.integ.handleTrigger(automationCtx(), map[string]any{"inboundRequestId": id}, 0)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	payload := decodeNode(t, nodes)
	opened, _ := payload["opened"].([]any)
	if len(opened) != 1 || payload["event"] != "pull_request" || payload["repository"] != repoName {
		t.Errorf("answer = %v", payload)
	}
	if payload["inboundRequestId"] != id || payload["source"] != githubSource {
		t.Errorf("the answer names the staged row it read: %v", payload)
	}
}

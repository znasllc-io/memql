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
	"github.com/znasllc-io/memql/component/pipelines"
)

// push_lane_test.go -- a push to the default branch waits for the push run
// ahead of it, and only the newest waiting push survives (design record
// 2026-11-05 D4; push_lane.go). The opening half -- a new push superseding the
// waiting runs it covers -- is in supersede_test.go beside the pull request's.
//
// Every test that asserts a run was LEFT ALONE also holds a run the same pass
// does move, or the control after it: a lane that did nothing at all would
// otherwise pass them.

// laneOptIn is the installed supersession policy an installation writes to
// opt in: the portable rule for pull requests, and full-mode pushes a delivery
// or the poll opened.
const laneOptIn = `@template
automation pipelineSupersessionPolicy {
 args { event string! mode string! previousMode string! trigger string! }
 return (args.event == "pull_request" && args.mode == "affected" && args.previousMode == "affected" && args.trigger != "rerun") || (args.event == "push" && args.mode == "full" && args.previousMode == "full" && (args.trigger == "webhook" || args.trigger == "poll"))
}`

// optInPushLane installs laneOptIn on integ: every run it opens from now on
// saves it in its policy snapshot, which is what makes the run a lane member.
func optInPushLane(t *testing.T, integ *Integration) {
	t.Helper()
	a := workflowSource(t, laneOptIn)
	integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
}

// laneSnapshot is the policy snapshot a push h's integration opened now would
// save: opted in when the installation is, the portable one when it is not.
func laneSnapshot(t *testing.T, integ *Integration, p Pipeline) map[string]any {
	t.Helper()
	s, err := capturePipelinePolicy(context.Background(), integ.snapshot(), p, Opening{Event: pipelines.EventPush, Trigger: TriggerWebhook})
	if err != nil {
		t.Fatalf("capturing the delivery policy: %v", err)
	}
	return s.Map()
}

// pushRunAt is a push to main at sha as an earlier delivery left it: full
// mode, attempt 1, queued ten minutes ago by a webhook, reporting check run
// checkRunID, claimed by no agent, under policy.
func pushRunAt(p Pipeline, sha string, checkRunID int64, policy map[string]any) Run {
	key := pipelines.RunKey(p.Repository, sha, pipelines.ModeFull, pipelines.EventPush)
	return Run{
		ID: RunIDFor(p.ID, key, 1), OwnerUserID: p.OwnerUserID, AccountID: p.AccountID, PipelineID: p.ID,
		Repository: p.Repository, SHA: sha, Mode: pipelines.ModeFull, Event: pipelines.EventPush,
		RunKey: key, Attempt: 1, Trigger: TriggerWebhook, HeadBranch: "main", BaseSHA: shaBase,
		Version: sha, Status: StatusQueued, CheckRunID: checkRunID, CheckRunState: CheckRunWritten,
		QueuedAt: testNow.Add(-tenMinutes), PolicySnapshot: policy,
	}
}

// pushToMain is a push of sha to main: GitHub's head moves to it, then its
// push is delivered. It answers the run the push opened.
func pushToMain(t *testing.T, h *harness, deliveryID, before, sha string) Run {
	t.Helper()
	h.github.setBranchHead(repoName, "main", sha)
	res := trigger(t, h, "push", deliveryID, pushDelivery(t, "refs/heads/main", before, sha, testInstallation))
	if len(res.Opened) != 1 {
		t.Fatalf("the push to main opened %d runs (existing %d, skipped %+v); want its commit's one", len(res.Opened), len(res.Existing), res.Skipped)
	}
	return res.Opened[0]
}

// withLogs points integ's logger at a buffer the test reads.
func withLogs(integ *Integration) *lockedBuffer {
	logs := &lockedBuffer{}
	integ.Configure(func(d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	return logs
}

// agentPeer is a second agent replica over h's rows, gate and GitHub: another
// node of one cluster, with no memory of this one.
func agentPeer(h *harness, node string) *Integration {
	return New(Deps{
		Store: h.store, GitHub: h.github, Gate: h.gate.run, NodeID: node,
		OSOrigin: func() string { return testOSOrigin }, Now: func() time.Time { return testNow },
		Logger: slog.New(slog.DiscardHandler),
	})
}

// gateTaken reports whether key's gate was ever asked for.
func gateTaken(g *keyedGate, key string) bool { return slices.Contains(g.seen(), key) }

// ---------------------------------------------------------------------------
// Who is in the lane
// ---------------------------------------------------------------------------

// A lane member is a full push a delivery or the poll opened, under a policy
// that opted it in. Nothing else is: not a re-run, a manual run, a release, a
// pull request's run, a qualification -- and nothing at all when the
// installation did not opt in.
func TestOnlyAPushTheInstallationOptedInJoinsTheLane(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	portable := laneSnapshot(t, h.integ, p)
	optInPushLane(t, h.integ)
	optedIn := laneSnapshot(t, h.integ, p)
	ctx := context.Background()

	member := pushRunAt(p, shaA, 501, optedIn)
	if !inPushLane(ctx, member) {
		t.Fatalf("a webhook's full push under the opt-in is a lane member")
	}
	polled := member
	polled.Trigger = TriggerPoll
	if !inPushLane(ctx, polled) {
		t.Errorf("the poll's full push under the opt-in is a lane member")
	}
	for name, change := range map[string]func(*Run){
		"the portable policy":  func(r *Run) { r.PolicySnapshot = portable },
		"no policy snapshot":   func(r *Run) { r.PolicySnapshot = nil },
		"a re-run":             func(r *Run) { r.Trigger, r.RerunOf = TriggerRerun, "an-earlier-run" },
		"a manual run":         func(r *Run) { r.Trigger = TriggerManual },
		"a scheduled scan":     func(r *Run) { r.Event, r.Trigger = pipelines.EventSchedule, TriggerSchedule },
		"a release":            func(r *Run) { r.Event = pipelines.EventRelease },
		"an affected push":     func(r *Run) { r.Mode = pipelines.ModeAffected },
		"a pull request's run": func(r *Run) { r.Event, r.PullRequest = pipelines.EventPullRequest, 42 },
		"a qualification":      func(r *Run) { r.Qualification = map[string]any{"requestId": "q1"} },
	} {
		r := member
		change(&r)
		if inPushLane(ctx, r) {
			t.Errorf("%s joined the push lane", name)
		}
	}
}

// ---------------------------------------------------------------------------
// The claim holds a waiting push while another executes
// ---------------------------------------------------------------------------

// A waiting lane run is not claimed while another push run of the pipeline is
// executing: nothing is written, so it stays queued with no driver and its
// time limit, counted from its start, has not begun. A run that has STARTED is
// taken over whatever holds the lane -- a takeover after a lost replica is
// never blocked -- and a push of another pipeline is not this lane's.
func TestAWaitingPushIsNotClaimedWhileAnotherExecutes(t *testing.T) {
	h := newHarness(t)
	optInPushLane(t, h.integ)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	policy := laneSnapshot(t, h.integ, p)
	executing := drivenBy(pushRunAt(p, shaB, 602, policy), "agent-c")
	stranded := drivenBy(pushRunAt(p, shaA, 601, policy), "agent-b")
	stranded.DriverHeartbeatAt = testNow.Add(-tenMinutes) // agent-b is gone
	waiting := pushRunAt(p, shaC, 603, policy)
	other := testPipeline(DeliveryWebhook)
	other.ID, other.PackageID, other.Repository = PipelineIDFor("pkg-other"), "pkg-other", "acme/other"
	h.store.addPipeline(other)
	elsewhere := pushRunAt(other, shaD, 604, policy)
	for _, r := range []Run{executing, stranded, waiting, elsewhere} {
		h.store.addRun(r)
	}
	ctx, d := context.Background(), h.integ.snapshot()

	if _, ok, err := h.integ.claim(ctx, d, waiting.ID); err != nil || ok {
		t.Fatalf("a waiting push is held while another executes: claimed %v, err %v", ok, err)
	}
	if got, _ := h.store.run(waiting.ID); got.Status != StatusQueued || got.DriverNodeID != "" || !got.StartedAt.IsZero() || got.DriverLeaseID != "" {
		t.Errorf("a held run is written nothing: %+v", got)
	}
	if fresh := h.store.freshCalls("RunsUnfinishedForPipelineEvent"); len(fresh) != 1 || !fresh[0] {
		t.Errorf("the lane is read once, fresh, under the run's gate: %v", fresh)
	}
	if claimed, ok, err := h.integ.claim(ctx, d, stranded.ID); err != nil || !ok || claimed.DriverNodeID != "agent-a" {
		t.Errorf("a started run is taken over whatever holds the lane: %+v %v %v", claimed, ok, err)
	}
	if _, ok, err := h.integ.claim(ctx, d, elsewhere.ID); err != nil || !ok {
		t.Errorf("another pipeline's push is not held by this one's lane: %v %v", ok, err)
	}

	// The control: once the run ahead has finished, the waiting run is claimed.
	h.store.mu.Lock()
	for _, id := range []string{executing.ID, stranded.ID} {
		r := h.store.runs[id]
		r.Status, r.Conclusion, r.FinishedAt = StatusCompleted, ConclusionSuccess, testNow
		h.store.runs[id] = r
	}
	h.store.mu.Unlock()
	if claimed, ok, err := h.integ.claim(ctx, d, waiting.ID); err != nil || !ok || claimed.DriverNodeID != "agent-a" {
		t.Errorf("with the lane free, the waiting push is claimed: %+v %v %v", claimed, ok, err)
	}
}

// Claimed a moment ago and not started yet holds the lane too -- the replica
// that claimed it is starting it -- while a claim whose lease went stale
// before it started holds nothing.
func TestAFreshClaimHoldsTheLaneAndAStaleOneDoesNot(t *testing.T) {
	h := newHarness(t)
	optInPushLane(t, h.integ)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	policy := laneSnapshot(t, h.integ, p)
	starting := pushRunAt(p, shaA, 601, policy)
	starting.DriverNodeID, starting.DriverLeaseID, starting.DriverHeartbeatAt = "agent-b", "lease-b", testNow
	waiting := pushRunAt(p, shaB, 602, policy)
	h.store.addRun(starting)
	h.store.addRun(waiting)
	ctx, d := context.Background(), h.integ.snapshot()

	if _, ok, err := h.integ.claim(ctx, d, waiting.ID); err != nil || ok {
		t.Fatalf("a push claimed a moment ago holds the lane: claimed %v, err %v", ok, err)
	}
	h.store.mu.Lock()
	r := h.store.runs[starting.ID]
	r.DriverHeartbeatAt = testNow.Add(-leaseStaleAfter)
	h.store.runs[starting.ID] = r
	h.store.mu.Unlock()
	if _, ok, err := h.integ.claim(ctx, d, waiting.ID); err != nil || !ok {
		t.Errorf("a claim whose lease went stale before it started holds nothing: claimed %v, err %v", ok, err)
	}
}

// A lane that cannot be read is not a free lane: the claim fails, nothing is
// written, and the run stays queued for recovery to try again.
func TestALaneThatCannotBeReadLeavesTheRunQueued(t *testing.T) {
	h := newHarness(t)
	optInPushLane(t, h.integ)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	waiting := pushRunAt(p, shaA, 601, laneSnapshot(t, h.integ, p))
	h.store.addRun(waiting)
	h.store.mu.Lock()
	h.store.failRunsForPipelineEvent = errors.New("database unavailable")
	h.store.mu.Unlock()

	if _, ok, err := h.integ.claim(context.Background(), h.integ.snapshot(), waiting.ID); err == nil || ok {
		t.Fatalf("an unreadable lane fails the claim: claimed %v, err %v", ok, err)
	}
	if got, _ := h.store.run(waiting.ID); got.DriverNodeID != "" || got.Status != StatusQueued {
		t.Errorf("nothing is written: %+v", got)
	}
}

// Without the installation's opt-in the claim asks nothing of the lane: a push
// is claimed while another push of the pipeline executes, as before.
func TestWithoutTheOptInTheClaimDoesNotReadTheLane(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	policy := laneSnapshot(t, h.integ, p)
	h.store.addRun(drivenBy(pushRunAt(p, shaA, 601, policy), "agent-b"))
	queued := pushRunAt(p, shaB, 602, policy)
	h.store.addRun(queued)

	if _, ok, err := h.integ.claim(context.Background(), h.integ.snapshot(), queued.ID); err != nil || !ok {
		t.Fatalf("a push is claimed beside another, as before: %v %v", ok, err)
	}
	if fresh := h.store.freshCalls("RunsUnfinishedForPipelineEvent"); len(fresh) != 0 {
		t.Errorf("the lane was read for a run outside it: %v", fresh)
	}
}

// ---------------------------------------------------------------------------
// The whole of it on a driver: a waiting push starts when the run ahead ends
// ---------------------------------------------------------------------------

// THE HOP: the run ahead executes on agent-a; the new push's created event
// reaches agent-b, which holds it -- nothing claimed, nothing started -- and
// when the run ahead concludes on agent-a, agent-a starts the waiting run at
// once, without waiting a minute for recovery.
func TestAWaitingPushStartsWhenTheRunAheadOfItConcludes(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	optInPushLane(t, dh.integ)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	blockTests(dh, release)
	dh.setTree(shaB, driveManifest)

	ahead := dh.openRun(t, pushOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, ahead))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")

	next := pushOpening()
	next.SHA, next.BaseSHA, next.Title = shaB, shaA, "Land the cart, fixed"
	dh.github.setBranchHead(repoName, "main", shaB)
	waiting := dh.openRun(t, next)
	peer := dh.peer("agent-b")
	peer.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, waiting))
	waitDrives(t, peer)

	if got, _ := dh.store.run(waiting.ID); got.Status != StatusQueued || got.DriverNodeID != "" || !got.StartedAt.IsZero() {
		t.Fatalf("the waiting push was claimed while the run ahead executes: %+v", got)
	}
	if sent := dh.exec.sent(); len(sent) != 3 || slices.ContainsFunc(sent, func(r pipelines.StepRequest) bool { return r.RunID == waiting.ID }) {
		t.Fatalf("only the run ahead's steps were handed to the runner: %v", dh.exec.sentKeys())
	}

	close(release)
	released = true
	waitDrives(t, dh.integ)

	first, _ := dh.store.run(ahead.ID)
	second, _ := dh.store.run(waiting.ID)
	if first.Conclusion != ConclusionSuccess || second.Conclusion != ConclusionSuccess {
		t.Fatalf("both runs pass, the second after the first: %s, %s\n%s", first.Conclusion, second.Conclusion, dh.logs.String())
	}
	if second.DriverNodeID != "agent-a" {
		t.Errorf("the replica that concluded the run ahead started the waiting one: driven by %q", second.DriverNodeID)
	}
	var order []string
	for _, req := range dh.exec.sent() {
		if len(order) == 0 || order[len(order)-1] != req.RunID {
			order = append(order, req.RunID)
		}
	}
	if !slices.Equal(order, []string{ahead.ID, waiting.ID}) {
		t.Errorf("the runner was handed the run ahead's steps, then the waiting run's: %v", order)
	}
	// Nothing here called RecoverRuns: the replica that concluded the run
	// ahead started the waiting one itself.
}

// Without the opt-in nothing changes: the second push starts at once beside
// the first, on whichever replica hears it, and nothing is superseded.
func TestWithoutTheOptInEveryPushStartsAtOnce(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	blockTests(dh, release)
	dh.setTree(shaB, driveManifest)

	first := dh.openRun(t, pushOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, first))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")

	next := pushOpening()
	next.SHA, next.BaseSHA = shaB, shaA
	dh.github.setBranchHead(repoName, "main", shaB)
	dh.github.setAncestor(repoName, shaA, shaB)
	second := dh.openRun(t, next)
	peer := dh.peer("agent-b")
	peer.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, second))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")

	if got, _ := dh.store.run(second.ID); got.Status != StatusInProgress || got.DriverNodeID != "agent-b" {
		t.Errorf("the second push started at once on the replica that heard it: %+v", got)
	}
	if got, _ := dh.store.run(first.ID); got.CancelRequested || got.Status != StatusInProgress {
		t.Errorf("the first push is untouched: %+v", got)
	}
	if reads := dh.github.branchHeadReads(); len(reads) != 0 {
		t.Errorf("nothing asked GitHub for the default branch's head: %v", reads)
	}
	if reads := dh.github.ancestryReads(); len(reads) != 0 {
		t.Errorf("nothing compared commits: %v", reads)
	}
	if fresh := dh.store.freshCalls("RunsUnfinishedForPipelineEvent"); len(fresh) != 0 {
		t.Errorf("nothing read the push lane: %v", fresh)
	}
	close(release)
	released = true
	waitDrives(t, dh.integ)
	waitDrives(t, peer)
}

// ---------------------------------------------------------------------------
// Recovery: the lane's, not the every-minute claim's
// ---------------------------------------------------------------------------

// laneRecoveryHarness is an agent node over an opted-in installation, with
// its logs in a buffer.
func laneRecoveryHarness(t *testing.T) (*driveHarness, map[string]any) {
	t.Helper()
	dh := newDriveHarness(t, driveManifest)
	optInPushLane(t, dh.integ)
	return dh, laneSnapshot(t, dh.integ, dh.p)
}

// While a push run executes, recovery leaves the waiting push runs alone: no
// claim, no log line every minute. (The default branch's head here is the
// waiting run's own commit, so there is nothing to supersede either.)
func TestRecoveryLeavesWaitingPushesAloneWhileTheLaneIsHeld(t *testing.T) {
	dh, policy := laneRecoveryHarness(t)
	executing := drivenBy(pushRunAt(dh.p, shaA, 601, policy), "agent-b")
	waiting := pushRunAt(dh.p, shaB, 602, policy)
	waiting.QueuedAt = testNow.Add(-5 * time.Minute)
	dh.store.addRun(executing)
	dh.store.addRun(waiting)
	dh.github.setBranchHead(repoName, "main", shaB)

	for range 3 {
		if err := dh.integ.RecoverRuns(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitDrives(t, dh.integ)
	}
	if gateTaken(dh.gate, RunGateKey(waiting.ID)) {
		t.Errorf("recovery tried to claim a push waiting behind the lane")
	}
	if got, _ := dh.store.run(waiting.ID); got.Status != StatusQueued || got.DriverNodeID != "" || got.CancelRequested {
		t.Errorf("the waiting push is untouched: %+v", got)
	}
	if line := logLine(dh.logs.String(), waiting.ID); line != "" {
		t.Errorf("recovery logged about the waiting run: %s", line)
	}

	// The control: once the run ahead has finished, the next pass starts it.
	dh.store.mu.Lock()
	r := dh.store.runs[executing.ID]
	r.Status, r.Conclusion, r.FinishedAt = StatusCompleted, ConclusionSuccess, testNow
	dh.store.runs[executing.ID] = r
	dh.store.mu.Unlock()
	dh.setTree(shaB, driveManifest)
	if err := dh.integ.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, dh.integ)
	if got, _ := dh.store.run(waiting.ID); got.Conclusion != ConclusionSuccess {
		t.Errorf("with the lane free, recovery starts the waiting push: %s/%s", got.Status, got.Conclusion)
	}
	wantLogged(t, dh.logs.String(), "the push lane is free; starting its newest waiting run", "INFO")
}

// With the lane free, recovery first supersedes the waiting runs the head's
// run covers, and starts the newest waiting run -- one per pipeline.
func TestRecoveryStartsOnlyTheNewestWaitingPush(t *testing.T) {
	t.Run("the older waiting push is a proven ancestor of the head", func(t *testing.T) {
		dh, policy := laneRecoveryHarness(t)
		older := pushRunAt(dh.p, shaA, 601, policy)
		newer := pushRunAt(dh.p, shaB, 602, policy)
		newer.QueuedAt = testNow.Add(-5 * time.Minute)
		dh.store.addRun(older)
		dh.store.addRun(newer)
		dh.setTree(shaB, driveManifest)
		dh.github.setBranchHead(repoName, "main", shaB)
		dh.github.setAncestor(repoName, shaA, shaB)

		if err := dh.integ.RecoverRuns(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitDrives(t, dh.integ)

		got := wantAsked(t, dh.harness, older.ID, newer.ID)
		if got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled || got.DriverNodeID != "" {
			t.Errorf("the older waiting push is concluded superseded, never claimed: %+v", got)
		}
		if got, _ := dh.store.run(newer.ID); got.Conclusion != ConclusionSuccess {
			t.Errorf("the head's run is started and runs: %s/%s", got.Status, got.Conclusion)
		}
		for _, req := range dh.exec.sent() {
			if req.RunID != newer.ID {
				t.Errorf("only the newest push ran: %s of %s", req.StepKey, req.RunID)
			}
		}
	})

	t.Run("the older waiting push is not proven an ancestor", func(t *testing.T) {
		dh, policy := laneRecoveryHarness(t)
		older := pushRunAt(dh.p, shaA, 601, policy)
		newer := pushRunAt(dh.p, shaB, 602, policy)
		newer.QueuedAt = testNow.Add(-5 * time.Minute)
		dh.store.addRun(older)
		dh.store.addRun(newer)
		dh.setTree(shaB, driveManifest)
		dh.github.setBranchHead(repoName, "main", shaB)
		dh.github.setNotAncestor(repoName, shaA, shaB)

		if err := dh.integ.RecoverRuns(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitDrives(t, dh.integ)

		// Neither is superseded; the newest runs first, and the other only
		// once it has concluded -- one at a time.
		var order []string
		for _, req := range dh.exec.sent() {
			if len(order) == 0 || order[len(order)-1] != req.RunID {
				order = append(order, req.RunID)
			}
		}
		if !slices.Equal(order, []string{newer.ID, older.ID}) {
			t.Errorf("the newest push ran first, then the other: %v", order)
		}
		for _, id := range []string{older.ID, newer.ID} {
			if got, _ := dh.store.run(id); got.CancelRequested || got.Conclusion != ConclusionSuccess {
				t.Errorf("run %s: %s/%s cancelled %v", id, got.Status, got.Conclusion, got.CancelRequested)
			}
		}
		wantLogged(t, dh.logs.String(), "is not a proven ancestor of the default branch's head", "INFO")
	})
}

// OUT OF ORDER: a delivery for an older push opens its run AFTER the head's
// run started -- its own opening superseded nothing, because the head had
// moved on. Recovery supersedes it: the head's run exists and covers it.
func TestRecoverySupersedesAPushOpenedLateForAnOlderCommit(t *testing.T) {
	dh, policy := laneRecoveryHarness(t)
	head := drivenBy(pushRunAt(dh.p, shaC, 603, policy), "agent-b")
	late := pushRunAt(dh.p, shaB, 602, policy)
	late.QueuedAt = testNow.Add(-time.Minute)
	dh.store.addRun(head)
	dh.store.addRun(late)
	dh.github.setBranchHead(repoName, "main", shaC)
	dh.github.setAncestor(repoName, shaB, shaC)

	if err := dh.integ.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, dh.integ)

	got := wantAsked(t, dh.harness, late.ID, head.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled {
		t.Errorf("the late push is concluded superseded by the head's run: %+v", got)
	}
	if w := checkRunWrites(dh.harness, 602); len(w) != 1 || w[0].Run.Conclusion != "cancelled" {
		t.Errorf("its check run is concluded cancelled: %+v", w)
	}
	if now, _ := dh.store.run(head.ID); now.CancelRequested || now.Status != StatusInProgress {
		t.Errorf("the head's run executes on: %+v", now)
	}
}

// A head whose run does not exist yet covers nothing: its delivery may never
// come, and a waiting run superseded toward it would leave nothing to test the
// commits between. And a head GitHub would not name supersedes nothing either.
func TestRecoverySupersedesNothingTowardAHeadWithNoRun(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(g *fakeGitHub)
		says  string
		level string
	}{
		"the head has no run yet": {func(g *fakeGitHub) {}, "", ""},
		"the head cannot be read": {func(g *fakeGitHub) { g.branchHeadErr = errStatus(502) }, "the default branch's head could not be read", "WARN"},
	} {
		t.Run(name, func(t *testing.T) {
			dh, policy := laneRecoveryHarness(t)
			executing := drivenBy(pushRunAt(dh.p, shaBase, 600, policy), "agent-b")
			older := pushRunAt(dh.p, shaA, 601, policy)
			newer := pushRunAt(dh.p, shaB, 602, policy)
			newer.QueuedAt = testNow.Add(-5 * time.Minute)
			for _, r := range []Run{executing, older, newer} {
				dh.store.addRun(r)
			}
			dh.github.setBranchHead(repoName, "main", shaC)
			dh.github.setAncestor(repoName, shaA, shaC)
			dh.github.setAncestor(repoName, shaB, shaC)
			c.setup(dh.github)

			if err := dh.integ.RecoverRuns(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitDrives(t, dh.integ)
			for _, r := range []Run{older, newer} {
				wantUntouched(t, dh.harness, r)
			}
			if reads := dh.github.ancestryReads(); len(reads) != 0 {
				t.Errorf("nothing was compared toward a head with no run: %v", reads)
			}
			if c.says != "" {
				wantLogged(t, dh.logs.String(), c.says, c.level)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The claim and a supersede share the run's gate
// ---------------------------------------------------------------------------

// ACROSS REPLICAS: agent-b claims a waiting push while agent-a's supersede
// asks it to stop only if it is still waiting. Both decide under the run's
// gate on a fresh read, so exactly one of them wins: a run is either claimed
// and left to finish, or concluded superseded and never claimed -- never both,
// and never neither. Each order is held first, then the two raced.
func TestAClaimAndASupersedeOfOneRunNeverBothWin(t *testing.T) {
	h := newHarness(t)
	logs := withLogs(h.integ)
	optInPushLane(t, h.integ)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	policy := laneSnapshot(t, h.integ, p)
	peer := agentPeer(h, "agent-b")
	ctx := context.Background()
	round := 0
	// waitingRun adds a fresh waiting push, alone in its lane.
	waitingRun := func() Run {
		round++
		r := pushRunAt(p, shaA, 601, policy)
		r.ID = fmt.Sprintf("%s-round-%d", r.ID, round)
		h.store.addRun(r)
		return r
	}
	// settle takes the round's run out of the lane, so the next round's claim
	// races its supersede rather than waiting behind this one.
	settle := func(r Run) {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		done := h.store.runs[r.ID]
		done.Status, done.Conclusion = StatusCompleted, ConclusionSuccess
		h.store.runs[r.ID] = done
	}
	// judge holds the row to whichever of the two won, and reports which.
	judge := func(t *testing.T, r Run, claimed, superseded bool) string {
		t.Helper()
		got, _ := h.store.run(r.ID)
		switch {
		case claimed && !superseded:
			if got.DriverNodeID != "agent-b" || got.Finished() || got.CancelRequested {
				t.Errorf("the claim won and the run reads %+v", got)
			}
			return "claim"
		case superseded && !claimed:
			if got.DriverNodeID != "" || got.Conclusion != ConclusionCancelled || !strings.HasPrefix(got.CancelledBy, supersededPrefix) {
				t.Errorf("the supersede won and the run reads %+v", got)
			}
			return "supersede"
		}
		t.Errorf("claimed %v and superseded %v; exactly one must win", claimed, superseded)
		return ""
	}

	t.Run("the claim first", func(t *testing.T) {
		r := waitingRun()
		_, claimed, err := peer.claim(ctx, peer.snapshot(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, superseded, err := h.integ.cancelIfWaiting(ctx, h.integ.snapshot(), r.ID, supersededPrefix+"run-of-shaC")
		if err != nil {
			t.Fatal(err)
		}
		if judge(t, r, claimed, superseded) != "claim" {
			t.Errorf("the claim that came first won")
		}
		wantLogged(t, logs.String(), "already executing; it finishes", "INFO")
		settle(r)
	})

	t.Run("the supersede first", func(t *testing.T) {
		r := waitingRun()
		_, superseded, err := h.integ.cancelIfWaiting(ctx, h.integ.snapshot(), r.ID, supersededPrefix+"run-of-shaC")
		if err != nil {
			t.Fatal(err)
		}
		_, claimed, err := peer.claim(ctx, peer.snapshot(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if judge(t, r, claimed, superseded) != "supersede" {
			t.Errorf("the supersede that came first won")
		}
		settle(r)
	})

	t.Run("the two at once", func(t *testing.T) {
		won := map[string]int{}
		for i := range 40 {
			r := waitingRun()
			// Each side lets the other ahead on alternate rounds, so the race
			// is run both ways round rather than always one way.
			yield := func(side int) {
				if i%2 == side {
					time.Sleep(time.Millisecond)
				}
			}
			var (
				wg                    sync.WaitGroup
				claimed, superseded   bool
				claimErr, supersedeEr error
				start                 = make(chan struct{})
			)
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				yield(0)
				// Membership decided, as claimAndDrive decides it before the
				// gate: what is raced is the two gated sections.
				_, claimed, claimErr = peer.claimInLane(ctx, peer.snapshot(), r.ID, true)
			}()
			go func() {
				defer wg.Done()
				<-start
				yield(1)
				_, superseded, supersedeEr = h.integ.cancelIfWaiting(ctx, h.integ.snapshot(), r.ID, supersededPrefix+"run-of-shaC")
			}()
			close(start)
			wg.Wait()
			if claimErr != nil || supersedeEr != nil {
				t.Fatalf("claim %v, supersede %v", claimErr, supersedeEr)
			}
			won[judge(t, r, claimed, superseded)]++
			settle(r)
		}
		t.Logf("raced 40 times: %v", won)
	})
}

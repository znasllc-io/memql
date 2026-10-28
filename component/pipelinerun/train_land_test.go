package pipelinerun

import (
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

// awaitingTrain departs a train carrying #7 and #8 and passes its run, so it
// waits for the owner's approval.
func awaitingTrain(t *testing.T) (*trainHarness, Train) {
	t.Helper()
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.remote.addPR(8, shaB, "success")
	th.tick(t)
	th.runFor(t, th.train(t), ConclusionSuccess)
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	tr := th.train(t)
	if tr.State != TrainAwaitingApproval {
		t.Fatalf("a passed run leaves the train %q, want awaiting approval", tr.State)
	}
	return th, tr
}

func (th *trainHarness) approve(tr Train, login, commit string) {
	th.remote.mu.Lock()
	defer th.remote.mu.Unlock()
	th.remote.reviews[tr.PullRequest] = append(th.remote.reviews[tr.PullRequest],
		githubapp.PullRequestReview{Login: login, State: "APPROVED", CommitID: commit, SubmittedAt: th.clock})
}

func TestApprovedTrainLands(t *testing.T) {
	th, tr := awaitingTrain(t)
	var asked bool
	for _, m := range th.messages() {
		asked = asked || (strings.Contains(m, "ACTION NEEDED: approve "+tr.Branch) && strings.Contains(m, "<@123456789012345678>"))
	}
	if !asked {
		t.Fatalf("no approval request mentioning the owner in %v", th.messages())
	}
	th.approve(tr, "Ada-GH", tr.HeadSHA)
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	got := th.train(t)
	if got.State != TrainLanded || got.LandedSHA == "" || got.ApprovedBy != "Ada-GH" || got.ApprovedSHA != tr.HeadSHA {
		t.Fatalf("after approval: %+v", got)
	}
	pr := th.remote.trainPRs[tr.PullRequest]
	if !pr.merged || pr.landedAs != got.LandedSHA {
		t.Errorf("the train pull request was not merged as recorded: %+v", pr)
	}
	for _, n := range []int{7, 8} {
		c := th.remote.comments[n]
		if len(c) == 0 || !strings.Contains(c[len(c)-1], "Landed on main") {
			t.Errorf("no landing comment on #%d: %v", n, c)
		}
	}
	if c := th.remote.comments[7]; !strings.Contains(c[len(c)-1], shaA[:7]) {
		t.Errorf("the landing comment on #7 does not name the commit that landed: %q", c[len(c)-1])
	}
	if _, ok := th.remote.branches[tr.Branch]; ok {
		t.Errorf("the landed train's branch was kept")
	}
	var landed bool
	for _, m := range th.messages() {
		landed = landed || strings.Contains(m, "PASSED: "+tr.Branch+" landed on main")
	}
	if !landed {
		t.Errorf("no landed message in %v", th.messages())
	}
}

func TestRebuiltTrainAsksAgain(t *testing.T) {
	th, tr := awaitingTrain(t)
	th.approve(tr, "ada-gh", "an-older-head")
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if got := th.train(t); got.State != TrainAwaitingApproval || th.remote.trainPRs[tr.PullRequest].merged {
		t.Fatalf("an approval of another commit landed the train: %+v", got)
	}
	th.approve(tr, "someone-else", tr.HeadSHA)
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if got := th.train(t); got.State != TrainAwaitingApproval {
		t.Fatalf("an approval by someone other than the owner landed the train: %+v", got)
	}
}

func TestMainMovingRebuildsAWaitingTrain(t *testing.T) {
	th, tr := awaitingTrain(t)
	const moved = "4444444444444444444444444444444444444444"
	th.github.mu.Lock()
	th.github.heads[repoName+"@main"] = headAnswer{SHA: moved}
	th.github.mu.Unlock()
	th.remote.files[moved] = th.remote.files[baseSHA]
	th.approve(tr, "ada-gh", tr.HeadSHA)
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	got := th.train(t)
	if got.State != TrainRunning || got.BaseSHA != moved || got.HeadSHA == tr.HeadSHA || got.Rebuilds != 1 {
		t.Fatalf("a train whose main moved was not rebuilt on the new main: %+v", got)
	}
	if th.remote.trainPRs[tr.PullRequest].merged {
		t.Errorf("an approval of the old head landed the train after main moved")
	}
}

func TestLandingResumesAfterAReplicaChange(t *testing.T) {
	th, tr := awaitingTrain(t)
	// A replica recorded the approval and died before merging.
	if _, ok, err := th.integ.casTrain(automationCtx(), th.integ.snapshot(), th.p, tr, TrainPatch{State: ptr(TrainLanding), ApprovedBy: ptr("ada-gh"), ApprovedSHA: ptr(tr.HeadSHA)}); err != nil || !ok {
		t.Fatalf("cas: %v %v", ok, err)
	}
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if got := th.train(t); got.State != TrainLanded || !th.remote.trainPRs[tr.PullRequest].merged {
		t.Fatalf("a landing train was not landed on the next tick: %+v", got)
	}
}

func TestReminderIsSentOncePerDeparture(t *testing.T) {
	th, tr := awaitingTrain(t)
	count := func() int {
		n := 0
		for _, m := range th.messages() {
			if strings.Contains(m, "is still waiting for your approval") {
				n++
			}
		}
		return n
	}
	th.clock = time.Date(2026, 10, 28, 15, 0, 30, 0, time.UTC)
	th.tick(t)
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if n := count(); n != 1 {
		t.Errorf("%d reminders at the next departure, want 1", n)
	}
	if got := th.train(t); got.ID != tr.ID || got.State != TrainAwaitingApproval {
		t.Errorf("a waiting train was replaced at the next departure: %+v", got)
	}
}

func TestExpressTrainCarriesOnlyTheNamedPullRequests(t *testing.T) {
	th := newTrainHarness(t)
	th.clock = time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	th.remote.addPR(7, shaA, "success")
	th.remote.addPR(8, shaB, "success")
	got, err := th.integ.TrainExpress(personCtx(ownerID), "acme/shop", []int{8})
	if err != nil {
		t.Fatalf("TrainExpress: %v", err)
	}
	if got.Departure != TrainDepartureExpress || got.State != TrainRunning || len(got.OnBoard) != 1 || got.OnBoard[0].Number != 8 {
		t.Errorf("express train = %+v", got)
	}
	if _, err := th.integ.TrainExpress(personCtx(ownerID), "acme/shop", []int{7}); err == nil {
		t.Errorf("a second express train started while the first is running")
	}
}

func TestExpressRefusesAnotherOwner(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	if _, err := th.integ.TrainExpress(personCtx(otherID), "acme/shop", []int{7}); err == nil {
		t.Fatal("someone who does not own the pipeline started an express train")
	}
	if _, err := th.integ.TrainExpress(automationCtx(), "acme/shop", []int{7}); err == nil {
		t.Fatal("an express train started with no person behind it")
	}
}

// TestExpressPullRequestNumbersAreWholeAndInRange: a pull request number
// arrives as a JSON number, so 2^63 -- one past int64 -- must be dropped, not
// narrowed to the largest int and treated as a pull request, and a fraction or
// a string is dropped rather than rounded.
func TestExpressPullRequestNumbersAreWholeAndInRange(t *testing.T) {
	got := intListArg(map[string]any{"pullRequests": []any{float64(1 << 63), 12.0, 3.5, "7", int64(5), int(9)}}, "pullRequests")
	want := []int{12, 5, 9}
	if len(got) != len(want) {
		t.Fatalf("intListArg = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("intListArg = %v, want %v", got, want)
		}
	}
}

// TestExpressClosesTheTrainItReplaces: the replaced train is cancelled, so its
// pull request must be closed and say why, or the owner sees two trains
// waiting and may approve the one that can no longer land.
func TestExpressClosesTheTrainItReplaces(t *testing.T) {
	th, waiting := awaitingTrain(t)
	th.clock = th.clock.Add(time.Minute)
	express, err := th.integ.TrainExpress(personCtx(ownerID), "acme/shop", []int{8})
	if err != nil {
		t.Fatalf("TrainExpress: %v", err)
	}
	prOpen, branchLeft, comments := th.trainLeftOnGitHub(waiting)
	if prOpen || branchLeft {
		t.Errorf("the replaced train left its pull request open=%v, branch=%v", prOpen, branchLeft)
	}
	if len(comments) == 0 || !strings.Contains(comments[len(comments)-1], express.Branch) {
		t.Errorf("comments on the replaced train's pull request: %v, want one naming %s", comments, express.Branch)
	}
	var said bool
	for _, m := range th.messages() {
		said = said || (strings.Contains(m, waiting.Branch+" was cancelled") && strings.Contains(m, express.Branch))
	}
	if !said {
		t.Errorf("the owner was asked to approve %s and never told it was replaced: %v", waiting.Branch, th.messages())
	}
}

// mergeByHand marks the train pull request merged outside the train, as the
// owner merging it on GitHub would.
func (th *trainHarness) mergeByHand(tr Train, sha string) {
	th.remote.mu.Lock()
	defer th.remote.mu.Unlock()
	pr := th.remote.trainPRs[tr.PullRequest]
	pr.open, pr.merged, pr.landedAs = false, true, sha
}

func (th *trainHarness) landedComments(n int) []string {
	th.remote.mu.Lock()
	defer th.remote.mu.Unlock()
	var out []string
	for _, c := range th.remote.comments[n] {
		if strings.Contains(c, "Landed on main") {
			out = append(out, c)
		}
	}
	return out
}

// TestAHandMergedTrainLandsFully: the owner merged the train pull request on
// GitHub instead of approving it. What landed is the owner's own doing, and
// it is recorded as a landing like any other: the merge commit, a comment on
// every pull request it carried, the branch deleted, the landed message.
func TestAHandMergedTrainLandsFully(t *testing.T) {
	th, tr := awaitingTrain(t)
	th.mergeByHand(tr, "aaaabbbbccccdddd0000000000000000aaaabbbb")
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	got := th.train(t)
	if got.State != TrainLanded || got.LandedSHA != "aaaabbbbccccdddd0000000000000000aaaabbbb" {
		t.Fatalf("state %s landed %q; want landed as the hand merge's commit", got.State, got.LandedSHA)
	}
	for _, n := range []int{7, 8} {
		if c := th.landedComments(n); len(c) != 1 || !strings.Contains(c[0], "aaaabbb") {
			t.Errorf("landing comments on #%d: %v", n, c)
		}
	}
	if _, left, _ := th.trainLeftOnGitHub(got); left {
		t.Error("the landed train's branch was left behind")
	}
	var landed bool
	for _, m := range th.messages() {
		landed = landed || strings.Contains(m, "landed on main")
	}
	if !landed {
		t.Errorf("no landed message in %v", th.messages())
	}
}

// TestAHeldTrainMergedByHandLands: a held train the owner merged by hand is
// landed, not rebuilt onto a merged pull request with a second version bump.
func TestAHeldTrainMergedByHandLands(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.tick(t)
	tr := th.train(t)
	th.runFor(t, tr, ConclusionFailure, map[string]any{"key": "checks.lint", "status": WorkStepFailed})
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if got := th.train(t); got.State != TrainHeld {
		t.Fatalf("state %s, want held", got.State)
	}
	th.mergeByHand(tr, "eeeeffff00000000000000000000000000000000")
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if got := th.train(t); got.State != TrainLanded || got.LandedSHA != "eeeeffff00000000000000000000000000000000" || got.Rebuilds != 0 {
		t.Fatalf("state %s landed %q rebuilds %d; want landed, never rebuilt", got.State, got.LandedSHA, got.Rebuilds)
	}
}

// TestLandingResumedAfterTheMergeRecordsIt: a replica merged the train and
// died before recording it. The next tick finds it merged and records the
// merge commit GitHub names, not an empty one.
func TestLandingResumedAfterTheMergeRecordsIt(t *testing.T) {
	th, tr := awaitingTrain(t)
	if _, ok, err := th.integ.casTrain(automationCtx(), th.integ.snapshot(), th.p, tr, TrainPatch{State: ptr(TrainLanding), ApprovedBy: ptr("ada-gh"), ApprovedSHA: ptr(tr.HeadSHA)}); err != nil || !ok {
		t.Fatalf("cas: %v %v", ok, err)
	}
	th.mergeByHand(tr, "1234567812345678123456781234567812345678")
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	got := th.train(t)
	if got.State != TrainLanded || got.LandedSHA != "1234567812345678123456781234567812345678" {
		t.Fatalf("state %s landed %q", got.State, got.LandedSHA)
	}
	for _, m := range th.messages() {
		if strings.Contains(m, "as .") {
			t.Errorf("a message names an empty merge commit: %s", m)
		}
	}
}

// TestAMergeRaceStillLands: another tick merged the approved train between
// this tick's read and its merge, so GitHub refuses the merge (405). The
// train is landed, not held for a refusal of what already happened.
func TestAMergeRaceStillLands(t *testing.T) {
	th, tr := awaitingTrain(t)
	th.remote.mu.Lock()
	th.remote.raceMerge = true
	th.remote.mu.Unlock()
	th.approve(tr, "ada-gh", tr.HeadSHA)
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if got := th.train(t); got.State != TrainLanded || got.LandedSHA == "" {
		t.Fatalf("state %s landed %q; want landed after the race", got.State, got.LandedSHA)
	}
}

// TestExpressRefusesWhenNothingNamedCanBoard: the owner named a pull request
// whose check is still running and a number that is not open. Nothing could
// leave, so the waiting train is left as it was and the refusal says why for
// each number.
func TestExpressRefusesWhenNothingNamedCanBoard(t *testing.T) {
	th, waiting := awaitingTrain(t)
	th.remote.addPR(9, "4444444444444444444444444444444444444444", "")
	th.clock = th.clock.Add(time.Minute)
	_, err := th.integ.TrainExpress(personCtx(ownerID), "acme/shop", []int{9, 42})
	if err == nil {
		t.Fatal("an express train with nothing able to board started")
	}
	for _, want := range []string{"#9", "has not passed", "#42", "not an open pull request"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
	if got := th.train(t); got.ID != waiting.ID || got.State != TrainAwaitingApproval {
		t.Errorf("the waiting train became %s %s; a refused express must leave it alone", got.ID, got.State)
	}
}

// TestAReplacedTrainsPullRequestsLeaveWhenTheExpressLands: an express train
// cancels the waiting train it goes ahead of, and that train's pull requests
// leave on a new train as soon as the express lands, not at the next
// scheduled departure.
func TestAReplacedTrainsPullRequestsLeaveWhenTheExpressLands(t *testing.T) {
	th, _ := awaitingTrain(t)
	th.clock = th.clock.Add(time.Minute)
	express, err := th.integ.TrainExpress(personCtx(ownerID), "acme/shop", []int{8})
	if err != nil {
		t.Fatalf("TrainExpress: %v", err)
	}
	// The express train lands; GitHub marks #8 merged with it.
	th.mergeByHand(express, "5555555555555555555555555555555555555555")
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if got := th.train(t); got.ID != express.ID || got.State != TrainLanded {
		t.Fatalf("the express train is %s %s, want landed", got.ID, got.State)
	}
	th.remote.mu.Lock()
	delete(th.remote.open, 8)
	th.remote.mu.Unlock()
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	next := th.train(t)
	if next.ID == express.ID || next.Departure != TrainDepartureScheduled || len(next.OnBoard) != 1 || next.OnBoard[0].Number != 7 {
		t.Fatalf("after the express landed, train %+v; want a new train carrying #7 at once", next)
	}
	if !next.DepartedAt.Equal(th.clock) {
		t.Errorf("the new train departed at %s, want now (%s), not the next scheduled time", next.DepartedAt, th.clock)
	}
}

// TestAnApprovalBeforeARebuildNeverLandsTheRebuiltTrain (Review Focus 2),
// end to end: the owner approves head H1; main moves, so the train is rebuilt
// to H2, runs again and asks again; H1's approval must not land H2, and the
// owner's approval of H2 does.
func TestAnApprovalBeforeARebuildNeverLandsTheRebuiltTrain(t *testing.T) {
	th, h1 := awaitingTrain(t)
	th.approve(h1, "ada-gh", h1.HeadSHA)
	const moved = "4444444444444444444444444444444444444444"
	th.github.mu.Lock()
	th.github.heads[repoName+"@main"] = headAnswer{SHA: moved}
	th.github.mu.Unlock()
	th.remote.files[moved] = th.remote.files[baseSHA]
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	h2 := th.train(t)
	if h2.State != TrainRunning || h2.HeadSHA == h1.HeadSHA {
		t.Fatalf("state %s head %s; want rebuilt to a new head", h2.State, h2.HeadSHA)
	}
	th.runFor(t, h2, ConclusionSuccess)
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	asked := 0
	for _, m := range th.messages() {
		if strings.Contains(m, "ACTION NEEDED: approve") {
			asked++
		}
	}
	if asked != 2 {
		t.Errorf("asked for approval %d times, want twice: once per head", asked)
	}
	for range 3 {
		th.clock = th.clock.Add(time.Minute)
		th.tick(t)
	}
	if got := th.train(t); got.State != TrainAwaitingApproval || th.remote.trainPRs[h2.PullRequest].merged {
		t.Fatalf("state %s merged %v; the approval of %s landed %s", got.State, th.remote.trainPRs[h2.PullRequest].merged, h1.HeadSHA, h2.HeadSHA)
	}
	th.approve(h2, "ada-gh", h2.HeadSHA)
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if got := th.train(t); got.State != TrainLanded {
		t.Fatalf("state %s; the owner's approval of the current head must land it", got.State)
	}
}

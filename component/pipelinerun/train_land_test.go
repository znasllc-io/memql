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

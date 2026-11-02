package pipelinerun

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
)

// newMainTrainHarness is a train harness whose repository releases from main.
// extra is any further settings, each with its leading comma.
func newMainTrainHarness(t *testing.T, extra string) *trainHarness {
	t.Helper()
	th := newTrainHarness(t)
	th.settings = strings.Replace(trainSettingsJSON, `"enabled":true`, `"enabled":true,"source":"main"`+extra, 1)
	return th
}

// landedEarlier records a train that landed yesterday as sha.
func (th *trainHarness) landedEarlier(t *testing.T, sha, version string) {
	t.Helper()
	departed := th.clock.Add(-23 * time.Hour)
	tr := Train{ID: "landed-yesterday", OwnerUserID: th.p.OwnerUserID, AccountID: th.p.AccountID, PipelineID: th.p.ID,
		Repository: repoName, Branch: "train/2026-10-27-1", Departure: TrainDepartureScheduled, State: TrainBuilding,
		DepartedAt: departed, UpdatedAt: departed}
	if err := th.store.CreateTrain(context.Background(), th.p.OwnerUserID, tr); err != nil {
		t.Fatalf("CreateTrain: %v", err)
	}
	if err := th.store.UpdateTrain(context.Background(), th.p.OwnerUserID, tr.ID, TrainPatch{
		State: ptr(TrainLanded), LandedSHA: ptr(sha), Version: ptr(version),
	}); err != nil {
		t.Fatalf("UpdateTrain: %v", err)
	}
}

func TestATrainFromMainLeavesWithNoPullRequests(t *testing.T) {
	th := newMainTrainHarness(t, "")
	th.tick(t)
	tr := th.train(t)
	if tr.State != TrainRunning || len(tr.OnBoard) != 0 || tr.BaseSHA != baseSHA {
		t.Fatalf("after the departure: %+v", tr)
	}
	if tr.Version != "0.31.5" {
		t.Errorf("version = %q, want the patch bump", tr.Version)
	}
	if tr.HeadSHA == "" || tr.HeadSHA == baseSHA {
		t.Errorf("head = %q: a train needs a commit of its own or GitHub has nothing to open", tr.HeadSHA)
	}
	pr := th.remote.trainPRs[tr.PullRequest]
	if pr == nil || !pr.open || pr.head != tr.Branch || pr.title != "Train "+tr.Branch {
		t.Fatalf("train pull request = %+v", pr)
	}
	if th.remote.branches[tr.Branch] != tr.HeadSHA {
		t.Errorf("the branch is at %q, the row says %q", th.remote.branches[tr.Branch], tr.HeadSHA)
	}
}

func TestATrainFromMainIgnoresOpenPullRequests(t *testing.T) {
	th := newMainTrainHarness(t, "")
	th.remote.addPR(7, shaA, "success")
	th.tick(t)
	tr := th.train(t)
	if len(tr.OnBoard) != 0 || len(th.remote.merged) != 0 {
		t.Fatalf("a train from main boarded a pull request: on board %+v, merged %v", tr.OnBoard, th.remote.merged)
	}
}

func TestATrainFromMainWithNothingNewIsCancelledQuietly(t *testing.T) {
	th := newMainTrainHarness(t, "")
	th.landedEarlier(t, baseSHA, "0.31.4")
	th.tick(t)
	tr := th.train(t)
	if tr.State != TrainCancelled || !strings.Contains(tr.HeldReason, "Nothing new on main") {
		t.Fatalf("after a departure with nothing new: %+v", tr)
	}
	if n := len(th.remote.trainPRs); n != 0 {
		t.Errorf("opened %d pull requests", n)
	}
	if msgs := th.messages(); len(msgs) != 0 {
		t.Errorf("sent %v, want nothing", msgs)
	}
	// It does not stand in the way of the next night, once main has moved.
	const next = "4444444444444444444444444444444444444444"
	th.github.mu.Lock()
	th.github.heads[repoName+"@main"] = headAnswer{SHA: next}
	th.github.mu.Unlock()
	th.remote.files[next] = th.remote.files[baseSHA]
	th.clock = th.clock.Add(6 * time.Hour)
	th.tick(t)
	if got := th.train(t); got.State != TrainRunning || got.BaseSHA != next {
		t.Fatalf("the next departure: %+v", got)
	}
}

func TestATrainFromMainKeepsAVersionAPersonChose(t *testing.T) {
	th := newMainTrainHarness(t, "")
	th.landedEarlier(t, "2222222222222222222222222222222222222222", "0.31.4")
	th.remote.files[baseSHA]["VERSION"] = []byte("0.32.0\n")
	th.tick(t)
	tr := th.train(t)
	if tr.Version != "0.32.0" {
		t.Errorf("version = %q, want the one on main", tr.Version)
	}
	if tr.HeadSHA == baseSHA {
		t.Errorf("no commit of its own")
	}
	if b, err := th.remote.FileAtRef(context.Background(), "", repoName, tr.HeadSHA, "VERSION"); err != nil || strings.TrimSpace(string(b)) != "0.32.0" {
		t.Errorf("VERSION on the train = %q, %v", b, err)
	}
}

func TestATrainFromMainWithNoVersionFileStillHasACommit(t *testing.T) {
	th := newMainTrainHarness(t, "")
	delete(th.remote.files[baseSHA], "VERSION")
	th.tick(t)
	tr := th.train(t)
	if tr.State != TrainRunning || tr.Version != "" || tr.HeadSHA == baseSHA {
		t.Fatalf("after the departure: %+v", tr)
	}
}

func TestATrainFromMainListsItsCommitsAndSaysHowManyMore(t *testing.T) {
	th := newMainTrainHarness(t, `,"landing":"automatic"`)
	const last = "2222222222222222222222222222222222222222"
	th.landedEarlier(t, last, "0.31.4")
	var commits []githubapp.ComparedCommit
	for n := range 60 {
		commits = append(commits, githubapp.ComparedCommit{SHA: fmt.Sprintf("%040d", n+1), Subject: fmt.Sprintf("change %d", n+1), Parent: last})
	}
	th.remote.commitsBetween = map[string][]githubapp.ComparedCommit{last + "..." + baseSHA: commits}
	th.tick(t)
	tr := th.train(t)
	body := th.remote.trainPRs[tr.PullRequest].body
	if !strings.Contains(body, "change 50") || strings.Contains(body, "change 51") {
		t.Errorf("body should list the first 50 commits: %s", body)
	}
	if !strings.Contains(body, "and 10 more") {
		t.Errorf("body should say how many it left out: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "approve") {
		t.Errorf("an automatic train's body asks for an approval: %s", body)
	}
	if !strings.Contains(body, "0.31.5") {
		t.Errorf("body should name the version: %s", body)
	}
}

// A commit subject is a repository's text: in a code span it cannot mention
// anyone on GitHub, and a backtick cannot close the span.
func TestATrainFromMainDoesNotLetACommitSubjectMentionAnyone(t *testing.T) {
	th := newMainTrainHarness(t, "")
	const last = "2222222222222222222222222222222222222222"
	th.landedEarlier(t, last, "0.31.4")
	th.remote.commitsBetween = map[string][]githubapp.ComparedCommit{last + "..." + baseSHA: {
		{SHA: strings.Repeat("a", 40), Subject: "bump @types/node `again`", Parent: last},
	}}
	th.tick(t)
	body := th.remote.trainPRs[th.train(t).PullRequest].body
	if !strings.Contains(body, "`bump @types/node 'again'`") {
		t.Errorf("the subject is not safely quoted: %s", body)
	}
}

func TestATrainFromMainAdoptsItsOpenPullRequest(t *testing.T) {
	th := newMainTrainHarness(t, "")
	th.tick(t)
	tr := th.train(t)
	// The tick that opened it died before recording anything about it.
	stale := th.clock.Add(-time.Hour)
	if err := th.store.UpdateTrain(context.Background(), th.p.OwnerUserID, tr.ID, TrainPatch{
		State: ptr(TrainBuilding), PullRequest: ptr(0), PullRequestURL: ptr(""), UpdatedAt: &stale,
	}); err != nil {
		t.Fatal(err)
	}
	th.tick(t)
	got := th.train(t)
	if got.State != TrainRunning || got.PullRequest != tr.PullRequest {
		t.Fatalf("after the rebuild: %+v", got)
	}
	if n := len(th.remote.trainPRs); n != 1 {
		t.Errorf("%d pull requests, want the one adopted", n)
	}
}

// moveMain puts a new commit on main, as a direct push does.
func (th *trainHarness) moveMain(sha string) {
	th.github.mu.Lock()
	th.github.heads[repoName+"@main"] = headAnswer{SHA: sha}
	th.github.mu.Unlock()
	th.remote.mu.Lock()
	th.remote.files[sha] = th.remote.files[baseSHA]
	th.remote.mu.Unlock()
}

// pushToBranch moves a train's branch, as a person's fix does.
func (th *trainHarness) pushToBranch(branch, sha string) {
	th.remote.mu.Lock()
	defer th.remote.mu.Unlock()
	th.remote.setBranch(branch, sha)
}

// otherReplica is a second node over the same store and the same GitHub.
func (th *trainHarness) otherReplica() *Integration {
	d := th.integ.snapshot()
	d.NodeID = "agent-b"
	return New(d)
}

// next advances the clock a minute and ticks.
func (th *trainHarness) next(t *testing.T) Train {
	t.Helper()
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	return th.train(t)
}

func TestAMovedMainNeverRebuildsATrainFromMain(t *testing.T) {
	const moved = "4444444444444444444444444444444444444444"
	t.Run("waiting for approval", func(t *testing.T) {
		th := newMainTrainHarness(t, "")
		th.tick(t)
		tr := th.train(t)
		th.runFor(t, tr, ConclusionSuccess)
		if got := th.next(t); got.State != TrainAwaitingApproval {
			t.Fatalf("state = %s", got.State)
		}
		th.moveMain(moved)
		got := th.next(t)
		if got.State != TrainAwaitingApproval || got.Rebuilds != 0 || got.HeadSHA != tr.HeadSHA || got.BaseSHA != baseSHA {
			t.Fatalf("after main moved: %+v", got)
		}
	})
	t.Run("held", func(t *testing.T) {
		th := newMainTrainHarness(t, "")
		th.tick(t)
		tr := th.train(t)
		th.runFor(t, tr, ConclusionCancelled)
		if got := th.next(t); got.State != TrainHeld {
			t.Fatalf("state = %s", got.State)
		}
		th.moveMain(moved)
		got := th.next(t)
		if got.State != TrainHeld || got.Rebuilds != 0 || got.HeadSHA != tr.HeadSHA {
			t.Fatalf("after main moved: %+v", got)
		}
	})
}

func TestATrainFromMainLandsAfterMainMoved(t *testing.T) {
	th := newMainTrainHarness(t, "")
	th.tick(t)
	tr := th.train(t)
	th.runFor(t, tr, ConclusionSuccess)
	th.next(t)
	th.moveMain("4444444444444444444444444444444444444444")
	th.approve(tr, "Ada-GH", tr.HeadSHA)
	got := th.next(t)
	if got.State != TrainLanded || got.LandedSHA == "" || got.ApprovedBy != "Ada-GH" {
		t.Fatalf("after the approval: %+v", got)
	}
	if !th.remote.trainPRs[tr.PullRequest].merged {
		t.Errorf("the pull request was not merged")
	}
}

func TestAnAutomaticTrainLandsWhenItsRunPasses(t *testing.T) {
	th := newMainTrainHarness(t, `,"landing":"automatic"`)
	th.tick(t)
	tr := th.train(t)
	run := th.runFor(t, tr, ConclusionSuccess)
	got := th.next(t)
	if got.State != TrainLanded || got.LandedSHA == "" || got.RunID != run.ID {
		t.Fatalf("after its run passed: %+v", got)
	}
	if got.ApprovedBy != "policy:landing-automatic" || got.ApprovedSHA != tr.HeadSHA {
		t.Errorf("approved by %q at %q; want the policy, at the head", got.ApprovedBy, got.ApprovedSHA)
	}
	for _, m := range th.messages() {
		if strings.Contains(m, "ACTION NEEDED") {
			t.Errorf("an automatic train asked for something: %s", m)
		}
	}
}

func TestTurningOnAutomaticLandingLandsATrainAlreadyWaiting(t *testing.T) {
	th := newMainTrainHarness(t, "")
	th.tick(t)
	th.runFor(t, th.train(t), ConclusionSuccess)
	if got := th.next(t); got.State != TrainAwaitingApproval {
		t.Fatalf("state = %s", got.State)
	}
	th.settings = strings.Replace(th.settings, `"source":"main"`, `"source":"main","landing":"automatic"`, 1)
	if got := th.next(t); got.State != TrainLanded {
		t.Fatalf("state = %s, want landed", got.State)
	}
}

func TestARefusedMergeHoldsATrainFromMainAndDoesNotRetryEveryMinute(t *testing.T) {
	th := newMainTrainHarness(t, `,"landing":"automatic"`)
	th.tick(t)
	th.runFor(t, th.train(t), ConclusionSuccess)
	th.remote.mu.Lock()
	th.remote.refuseMerges = true
	th.remote.mu.Unlock()
	got := th.next(t)
	if got.State != TrainHeld || !strings.Contains(got.HeldReason, "would not merge") {
		t.Fatalf("after the refusal: %+v", got)
	}
	th.remote.mu.Lock()
	attempts := th.remote.mergeAttempts
	th.remote.mu.Unlock()
	th.next(t)
	th.remote.mu.Lock()
	defer th.remote.mu.Unlock()
	if th.remote.mergeAttempts != attempts {
		t.Errorf("a held train tried to merge again: %d attempts, was %d", th.remote.mergeAttempts, attempts)
	}
}

func TestAFailedRunOnAnAutomaticTrainDoesNotLand(t *testing.T) {
	th := newMainTrainHarness(t, `,"landing":"automatic"`)
	th.tick(t)
	th.runFor(t, th.train(t), ConclusionFailure, map[string]any{"key": "checks.lint", "status": WorkStepFailed})
	got := th.next(t)
	if got.State == TrainLanding || got.State == TrainLanded {
		t.Fatalf("a failed run landed: %+v", got)
	}
}

// goPackage is the package the harness's head tree says core/num reaches.
const goPackage = "github.com/acme/shop/component/memql"

func failedStep(key string) map[string]any {
	return map[string]any{"key": key, "status": WorkStepFailed, "call": map[string]any{"packages": []any{goPackage}}}
}

func TestATestFailureOnATrainFromMainRerunsOnceThenHolds(t *testing.T) {
	th := newMainTrainHarness(t, "")
	const last = "2222222222222222222222222222222222222222"
	th.landedEarlier(t, last, "0.31.4")
	reaches := strings.Repeat("a", 40)
	misses := strings.Repeat("b", 40)
	th.remote.commitsBetween = map[string][]githubapp.ComparedCommit{last + "..." + baseSHA: {
		{SHA: reaches, Subject: "Change the number helper", Parent: last},
		{SHA: misses, Subject: "Change something else", Parent: reaches},
	}}
	th.remote.shaFiles[reaches] = []string{"core/num/num.go"}
	th.remote.shaFiles[misses] = []string{"component/other/o.go"}
	th.tick(t)
	tr := th.train(t)
	first := th.runFor(t, tr, ConclusionFailure, failedStep("tests.go-unit#1"))

	got := th.next(t)
	if !got.Retried || got.State != TrainRunning {
		t.Fatalf("after the first failure: %+v", got)
	}
	var rerun *Run
	th.store.mu.Lock()
	for _, r := range th.store.runs {
		if r.RerunOf == first.ID {
			copy := r
			rerun = &copy
		}
	}
	th.store.mu.Unlock()
	if rerun == nil || !rerun.RerunFailedOnly {
		t.Fatalf("no failed-only re-run: %+v", rerun)
	}
	var said bool
	for _, m := range th.messages() {
		said = said || (strings.Contains(m, "Re-running failed steps") && strings.Contains(m, "tests.go-unit#1"))
	}
	if !said {
		t.Errorf("no message names the step being re-run: %v", th.messages())
	}

	// The re-run fails too.
	th.store.mu.Lock()
	r := th.store.runs[rerun.ID]
	r.Status, r.Conclusion, r.WorkRunID = StatusCompleted, ConclusionFailure, "work-rerun"
	th.store.runs[rerun.ID] = r
	th.store.mu.Unlock()
	step := failedStep("tests.go-unit#1")
	step["runId"] = "work-rerun"
	th.store.work.mu.Lock()
	th.store.work.steps["work-rerun-0"] = step
	th.store.work.mu.Unlock()

	got = th.next(t)
	if got.State != TrainHeld {
		t.Fatalf("after the second failure: %+v", got)
	}
	var held string
	for _, m := range th.messages() {
		if strings.Contains(m, "is held") {
			held = m
		}
	}
	if held == "" || !strings.Contains(held, "<@123456789012345678>") {
		t.Fatalf("no held message mentioning the owner: %v", th.messages())
	}
	if !strings.Contains(held, "Change the number helper") {
		t.Errorf("the held message does not name the commit that reaches what failed: %s", held)
	}
	if strings.Contains(held, "Change something else") {
		t.Errorf("the held message names a commit that cannot reach what failed: %s", held)
	}
}

func TestPassingOnTheRerunSaysSoAndLands(t *testing.T) {
	th := newMainTrainHarness(t, `,"landing":"automatic"`)
	th.tick(t)
	tr := th.train(t)
	first := th.runFor(t, tr, ConclusionFailure, failedStep("tests.go-unit#1"))
	th.next(t)
	var rerunID string
	th.store.mu.Lock()
	for id, r := range th.store.runs {
		if r.RerunOf == first.ID {
			rerunID = id
		}
	}
	r := th.store.runs[rerunID]
	r.Status, r.Conclusion = StatusCompleted, ConclusionSuccess
	th.store.runs[rerunID] = r
	th.store.mu.Unlock()
	got := th.next(t)
	if got.State != TrainLanded {
		t.Fatalf("after the re-run passed: %+v", got)
	}
	var landed string
	for _, m := range th.messages() {
		if strings.Contains(m, "landed on main") {
			landed = m
		}
	}
	if !strings.Contains(landed, "re-run") {
		t.Errorf("the landed message does not say it passed on its re-run: %s", landed)
	}
}

func TestATestFailureOnAPullRequestTrainIsStillNeverRetried(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.remote.addPR(8, shaB, "success")
	th.remote.prFiles[7] = []string{"core/num/num.go"}
	th.remote.prFiles[8] = []string{"core/num/num.go"}
	th.tick(t)
	th.runFor(t, th.train(t), ConclusionFailure, failedStep("tests.go-unit#1"))
	got := th.next(t)
	if got.Retried || got.State != TrainHeld {
		t.Fatalf("a pull request train's failed test: %+v", got)
	}
}

func TestAPushToATrainFromMainGoesBackToRunning(t *testing.T) {
	const fix = "5555555555555555555555555555555555555555"
	cases := []struct {
		name  string
		setUp func(t *testing.T, th *trainHarness, tr Train)
	}{
		{"held", func(t *testing.T, th *trainHarness, tr Train) {
			th.runFor(t, tr, ConclusionCancelled)
			if got := th.next(t); got.State != TrainHeld {
				t.Fatalf("state = %s", got.State)
			}
		}},
		{"running", func(t *testing.T, th *trainHarness, tr Train) {}},
		{"waiting for approval", func(t *testing.T, th *trainHarness, tr Train) {
			th.runFor(t, tr, ConclusionSuccess)
			if got := th.next(t); got.State != TrainAwaitingApproval {
				t.Fatalf("state = %s", got.State)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			th := newMainTrainHarness(t, "")
			th.tick(t)
			tr := th.train(t)
			c.setUp(t, th, tr)
			th.pushToBranch(tr.Branch, fix)
			got := th.next(t)
			if got.State != TrainRunning || got.HeadSHA != fix || got.Retried || got.HeldReason != "" || got.RunID != "" {
				t.Fatalf("after the push: %+v", got)
			}
			if got.Rebuilds != 0 {
				t.Errorf("a fix counted as a rebuild: %d", got.Rebuilds)
			}
			if th.remote.branches[tr.Branch] != fix {
				t.Errorf("the coordinator moved the branch off the fix: %q", th.remote.branches[tr.Branch])
			}
		})
	}
}

func TestAnApprovalOfTheOldHeadDoesNotLandAMovedTrain(t *testing.T) {
	th := newMainTrainHarness(t, "")
	th.tick(t)
	tr := th.train(t)
	th.runFor(t, tr, ConclusionSuccess)
	th.next(t)
	th.approve(tr, "Ada-GH", tr.HeadSHA)
	th.pushToBranch(tr.Branch, "5555555555555555555555555555555555555555")
	got := th.next(t)
	if got.State != TrainRunning {
		t.Fatalf("state = %s, want back to running on the new head", got.State)
	}
	if pr := th.remote.trainPRs[tr.PullRequest]; pr.merged {
		t.Errorf("it landed on the old head's approval")
	}
}

func TestAClosedPullRequestCancelsATrainFromMain(t *testing.T) {
	th := newMainTrainHarness(t, "")
	th.tick(t)
	tr := th.train(t)
	th.runFor(t, tr, ConclusionCancelled)
	th.next(t)
	th.remote.mu.Lock()
	th.remote.trainPRs[tr.PullRequest].open = false
	delete(th.remote.branches, tr.Branch)
	th.remote.mu.Unlock()
	if got := th.next(t); got.State != TrainCancelled {
		t.Fatalf("state = %s", got.State)
	}
}

// A train from main resumes from its row on whichever replica holds the lease
// next, from any state: nothing about it lives in the process.
func TestASecondReplicaResumesATrainFromMain(t *testing.T) {
	replica := func(th *trainHarness) { th.integ = th.otherReplica() }
	t.Run("running, its run passes", func(t *testing.T) {
		th := newMainTrainHarness(t, `,"landing":"automatic"`)
		th.tick(t)
		th.runFor(t, th.train(t), ConclusionSuccess)
		replica(th)
		if got := th.next(t); got.State != TrainLanded {
			t.Fatalf("state = %s", got.State)
		}
	})
	t.Run("held, a fix arrives", func(t *testing.T) {
		th := newMainTrainHarness(t, "")
		th.tick(t)
		tr := th.train(t)
		th.runFor(t, tr, ConclusionCancelled)
		th.next(t)
		replica(th)
		th.pushToBranch(tr.Branch, "5555555555555555555555555555555555555555")
		if got := th.next(t); got.State != TrainRunning {
			t.Fatalf("state = %s", got.State)
		}
	})
	t.Run("landing, the replica died before merging", func(t *testing.T) {
		th := newMainTrainHarness(t, `,"landing":"automatic"`)
		th.tick(t)
		tr := th.train(t)
		if err := th.store.UpdateTrain(context.Background(), th.p.OwnerUserID, tr.ID, TrainPatch{State: ptr(TrainLanding)}); err != nil {
			t.Fatal(err)
		}
		replica(th)
		if got := th.next(t); got.State != TrainLanded {
			t.Fatalf("state = %s", got.State)
		}
	})
	t.Run("two ticks at once land once", func(t *testing.T) {
		th := newMainTrainHarness(t, `,"landing":"automatic"`)
		th.tick(t)
		th.runFor(t, th.train(t), ConclusionSuccess)
		th.clock = th.clock.Add(time.Minute)
		other := th.otherReplica()
		done := make(chan struct{})
		go func() { defer close(done); _, _ = other.TrainTick(automationCtx()) }()
		_, _ = th.integ.TrainTick(automationCtx())
		<-done
		th.remote.mu.Lock()
		defer th.remote.mu.Unlock()
		if th.remote.mergeAttempts > 2 {
			t.Errorf("%d merge attempts", th.remote.mergeAttempts)
		}
		var merged int
		for _, pr := range th.remote.trainPRs {
			if pr.merged {
				merged++
			}
		}
		if merged != 1 {
			t.Errorf("%d merged pull requests, want 1", merged)
		}
	})
}

// One release runs at a time, so a held one keeps tonight's from leaving, and the
// owner hears that once rather than finding out from an empty night.
func TestAHeldReleaseRemindsAtTheNextDeparture(t *testing.T) {
	th := newMainTrainHarness(t, "")
	th.tick(t)
	tr := th.train(t)
	th.runFor(t, tr, ConclusionCancelled)
	th.next(t)
	remindersOf := func() int {
		n := 0
		for _, m := range th.messages() {
			if strings.Contains(m, "is still held") {
				n++
			}
		}
		return n
	}
	if n := remindersOf(); n != 0 {
		t.Fatalf("a reminder before the next departure: %v", th.messages())
	}
	th.clock = th.clock.Add(7 * time.Hour)
	if got := th.next(t); got.ID != tr.ID || got.State != TrainHeld {
		t.Fatalf("the held release should still be the newest: %+v", got)
	}
	if n := remindersOf(); n != 1 {
		t.Fatalf("%d reminders after the next departure, want 1: %v", n, th.messages())
	}
	th.next(t)
	if n := remindersOf(); n != 1 {
		t.Errorf("%d reminders after another tick, want still 1", n)
	}
}

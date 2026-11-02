package pipelinerun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/pipelines"
)

const releaseSettings = `,"landing":"automatic","releaseProfile":"engine"`

// fakeReleaser plays the release integration: each call answers next, and records what
// it was asked.
type fakeReleaser struct {
	mu   sync.Mutex
	seen []pipelines.TrainReleaseRequest
	next func(pipelines.TrainReleaseRequest) (pipelines.TrainReleaseStatus, error)
}

func (f *fakeReleaser) AdvanceTrainRelease(ctx context.Context, r pipelines.TrainReleaseRequest) (pipelines.TrainReleaseStatus, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return pipelines.TrainReleaseStatus{}, errors.New("the fake is reached with internal origin alone, as the real one is")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, r)
	if f.next == nil {
		return pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "building"}, nil
	}
	return f.next(r)
}

func (f *fakeReleaser) answer(status pipelines.TrainReleaseStatus, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = func(pipelines.TrainReleaseRequest) (pipelines.TrainReleaseStatus, error) { return status, err }
}

func (f *fakeReleaser) last() pipelines.TrainReleaseRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[len(f.seen)-1]
}

func (th *trainHarness) allowRelease(t *testing.T, release, deploy bool) {
	t.Helper()
	if err := th.store.SetStanding(context.Background(), th.p.OwnerUserID, TrainStanding{
		ID: StandingIDFor(th.p.ID), PipelineID: th.p.ID, Repository: th.p.Repository, Release: release, Deploy: deploy,
		Authority: map[string]any{"credentialClass": "user", "roleCeiling": "owner"}, SetAt: th.clock,
	}); err != nil {
		t.Fatal(err)
	}
}

// newReleaseHarness is a repository whose trains land and publish by themselves, with a
// train that has just landed.
func newReleaseHarness(t *testing.T) (*trainHarness, *fakeReleaser) {
	t.Helper()
	th := newMainTrainHarness(t, releaseSettings)
	releaser := &fakeReleaser{}
	th.integ.Configure(func(d *Deps) { d.TrainReleaser = releaser })
	th.allowRelease(t, true, false)
	th.tick(t)
	th.runFor(t, th.train(t), ConclusionSuccess)
	return th, releaser
}

func TestALandedTrainStopsThereWithoutTheOwnersWord(t *testing.T) {
	th := newMainTrainHarness(t, releaseSettings)
	releaser := &fakeReleaser{}
	th.integ.Configure(func(d *Deps) { d.TrainReleaser = releaser })
	th.tick(t)
	th.runFor(t, th.train(t), ConclusionSuccess)
	if got := th.next(t); got.State != TrainLanded {
		t.Fatalf("state = %s", got.State)
	}
	th.next(t)
	if len(releaser.seen) != 0 {
		t.Errorf("it asked for a release with nobody's word: %+v", releaser.seen)
	}
}

func TestAWordThatSaysNoStopsAtLandedToo(t *testing.T) {
	th := newMainTrainHarness(t, releaseSettings)
	th.integ.Configure(func(d *Deps) { d.TrainReleaser = &fakeReleaser{} })
	th.allowRelease(t, false, false)
	th.tick(t)
	th.runFor(t, th.train(t), ConclusionSuccess)
	if got := th.next(t); got.State != TrainLanded {
		t.Fatalf("state = %s", got.State)
	}
}

// The release tags the commit its run tested, never the merge that landed it (D5).
func TestAReleaseIsOfTheTestedCommitUnderTheOwnersAuthority(t *testing.T) {
	th, releaser := newReleaseHarness(t)
	tested := th.train(t).HeadSHA
	got := th.next(t)
	if got.State != TrainReleasing || got.LandedSHA == "" {
		t.Fatalf("after landing: %s at %s", got.State, got.LandedSHA)
	}
	th.next(t)
	r := releaser.last()
	if r.SHA != tested || r.SHA == got.LandedSHA {
		t.Errorf("release of %s; tested %s, merged as %s", r.SHA, tested, got.LandedSHA)
	}
	if r.Profile != "engine" || r.Version != got.Version || r.OwnerUserID != th.p.OwnerUserID || r.Authority["roleCeiling"] != "owner" {
		t.Errorf("request = %+v", r)
	}
}

func TestAReleaseGoesThroughToPublished(t *testing.T) {
	th, releaser := newReleaseHarness(t)
	th.next(t)
	steps := []pipelines.TrainReleaseStatus{
		{IntentID: "intent-1", Phase: "prepared"},
		{IntentID: "intent-1", Phase: "building", RunID: "release-run"},
		{IntentID: "intent-1", Phase: "built", RunID: "release-run"},
		{IntentID: "intent-1", Phase: "ready", RunID: "release-run", CandidateID: "sha256:c"},
		{IntentID: "intent-1", Phase: "ready", RunID: "release-run", CandidateID: "sha256:c", ApprovalID: "a-1", ApprovedBy: "policy:standing-release:ada"},
		{IntentID: "intent-1", Phase: "publishing", RunID: "release-run", CandidateID: "sha256:c", ApprovalID: "a-1", PublishStarted: true},
		{IntentID: "intent-1", Phase: "publishing", RunID: "release-run", CandidateID: "sha256:c", ApprovalID: "a-1"},
	}
	for _, step := range steps {
		releaser.answer(step, nil)
		if got := th.next(t); got.State != TrainReleasing {
			t.Fatalf("at %s: state = %s (%s)", step.Phase, got.State, got.HeldReason)
		}
	}
	mid := th.train(t)
	if mid.ReleaseIntentID != "intent-1" || mid.ReleaseRunID != "release-run" || mid.ReleaseCandidateID != "sha256:c" ||
		mid.ReleaseApprovalID != "a-1" || mid.ReleaseApprovedBy != "policy:standing-release:ada" || mid.PublishAttempts != 1 {
		t.Fatalf("the row did not keep the journey: %+v", mid)
	}
	if releaser.last().IntentID != "intent-1" {
		t.Errorf("a later step did not name the saved release: %+v", releaser.last())
	}
	releaser.answer(pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "published", Tag: "v0.31.5", CatalogDigest: "sha256:d", Published: true}, nil)
	got := th.next(t)
	if got.State != TrainPublished || got.ReleaseCatalogDigest != "sha256:d" || !got.Finished() {
		t.Fatalf("published: %+v", got)
	}
	if text := strings.Join(th.messages(), "\n"); !strings.Contains(text, "Released") {
		t.Errorf("no message says it was released: %s", text)
	}
}

func TestAReleaseResumesOnAnotherReplica(t *testing.T) {
	th, releaser := newReleaseHarness(t)
	th.next(t)
	releaser.answer(pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "building", RunID: "release-run"}, nil)
	th.next(t)
	other := th.otherReplica()
	th.clock = th.clock.Add(time.Minute)
	if _, err := other.TrainTick(automationCtx()); err != nil {
		t.Fatal(err)
	}
	if r := releaser.last(); r.IntentID != "intent-1" {
		t.Errorf("the other replica started over: %+v", r)
	}
}

func TestTwoTicksAtOnceCountOnePublish(t *testing.T) {
	th, releaser := newReleaseHarness(t)
	th.next(t)
	releaser.answer(pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "publishing", PublishStarted: true}, nil)
	read := th.train(t)
	d := th.integ.snapshot()
	s := pipelines.TrainSettings{Source: pipelines.TrainSourceMain, ReleaseProfile: "engine", Landing: pipelines.TrainLandingAutomatic, Channel: "releases"}
	for range 2 {
		if err := th.integ.advanceRelease(automationCtx(), d, s, th.p, read, th.clock); err != nil {
			t.Fatal(err)
		}
	}
	if got := th.train(t); got.PublishAttempts != 1 {
		t.Errorf("publish attempts = %d: a tick that lost the row's compare-and-swap wrote anyway", got.PublishAttempts)
	}
}

func TestAFailedReleaseRunIsRetriedOnceThenHolds(t *testing.T) {
	th, releaser := newReleaseHarness(t)
	th.next(t)
	tr := th.train(t)
	run := Run{ID: "release-run", OwnerUserID: th.p.OwnerUserID, PipelineID: th.p.ID, Repository: repoName, SHA: tr.HeadSHA, Mode: pipelines.ModeFull,
		Event: pipelines.EventRelease, RunKey: "release-key", Attempt: 1, Trigger: TriggerManual, Status: StatusCompleted, Conclusion: ConclusionFailure,
		WorkRunID: "release-work", ReleaseTag: "v0.31.5", Version: "v0.31.5", HeadBranch: "main", QueuedAt: th.clock}
	th.store.addRun(run)
	failed := pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "blocked", RunID: "release-run", ReasonCode: "build_not_successful", BuildFailed: true}
	releaser.answer(failed, nil)
	got := th.next(t)
	if got.State != TrainReleasing || !got.ReleaseRetried {
		t.Fatalf("after the first failure: %s retried=%v (%s)", got.State, got.ReleaseRetried, got.HeldReason)
	}
	if text := strings.Join(th.messages(), "\n"); !strings.Contains(text, "Re-running") {
		t.Errorf("nobody was told about the re-run: %s", text)
	}
	got = th.next(t)
	if got.State != TrainHeld || !strings.Contains(got.HeldReason, "failed again") {
		t.Fatalf("after the second failure: %s (%s)", got.State, got.HeldReason)
	}
}

func TestAReleaseThatCannotGoOnHoldsAndKeepsTheNextOneBack(t *testing.T) {
	th, releaser := newReleaseHarness(t)
	th.next(t)
	releaser.answer(pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "blocked", ReasonCode: "version_prepare_required", Reason: "This version already names another commit.", Stop: true}, nil)
	got := th.next(t)
	if got.State != TrainHeld || !strings.Contains(got.HeldReason, "another commit") {
		t.Fatalf("state = %s (%s)", got.State, got.HeldReason)
	}
	held := th.train(t).ID
	th.clock = th.clock.Add(25 * time.Hour)
	th.tick(t)
	if got := th.train(t); got.ID != held {
		t.Errorf("a new train left behind a held release: %+v", got)
	}
	if text := strings.Join(th.messages(), "\n"); !strings.Contains(text, "still held") {
		t.Errorf("no reminder went out: %s", text)
	}
}

func TestAHeldReleaseGoesOnWhenAPersonFinishesIt(t *testing.T) {
	stopped := pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "blocked", ReasonCode: "build_not_successful", Stop: true, Reason: "stopped"}
	for _, tc := range []struct {
		name string
		then pipelines.TrainReleaseStatus
		want string
	}{
		{"they published it by hand", pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "published", Published: true}, TrainPublished},
		{"they re-ran what failed", pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "building"}, TrainReleasing},
		{"nothing changed", stopped, TrainHeld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th, releaser := newReleaseHarness(t)
			th.next(t)
			releaser.answer(stopped, nil)
			if got := th.next(t); got.State != TrainHeld {
				t.Fatalf("not held: %s", got.State)
			}
			releaser.answer(tc.then, nil)
			got := th.next(t)
			if got.State != tc.want {
				t.Fatalf("state = %s, want %s", got.State, tc.want)
			}
			if !releaser.last().Observe {
				t.Error("a held train may read a release and start nothing")
			}
		})
	}
}

func TestPublishingThatKeepsStoppingHolds(t *testing.T) {
	th, releaser := newReleaseHarness(t)
	th.next(t)
	for range trainPublishAttempts {
		releaser.answer(pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "publishing", PublishStarted: true}, nil)
		th.next(t)
	}
	if got := th.train(t); got.State != TrainReleasing || got.PublishAttempts != trainPublishAttempts {
		t.Fatalf("after %d publishes: %s, attempts %d", trainPublishAttempts, got.State, got.PublishAttempts)
	}
	// Past its attempts it only reads, so a publish still running is left to finish.
	releaser.answer(pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "publishing"}, nil)
	if got := th.next(t); got.State != TrainReleasing || !releaser.last().Observe {
		t.Fatalf("while a publish runs: %s, observe=%v", got.State, releaser.last().Observe)
	}
	releaser.answer(pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "uncertain", Reason: "The publication stopped."}, nil)
	got := th.next(t)
	if got.State != TrainHeld || !strings.Contains(got.HeldReason, "Publishing stopped") {
		t.Fatalf("state = %s (%s)", got.State, got.HeldReason)
	}
}

// Owner authority lost, for one: the tick refuses, and the train says so once it lasts.
func TestAStepThatKeepsFailingHoldsOnceItLasts(t *testing.T) {
	th, releaser := newReleaseHarness(t)
	th.next(t)
	releaser.answer(pipelines.TrainReleaseStatus{}, errors.New("the release owner no longer holds owner authority"))
	if got := th.next(t); got.State != TrainReleasing {
		t.Fatalf("after one failure: %s", got.State)
	}
	th.clock = th.clock.Add(trainReleaseStallWait)
	got := th.next(t)
	if got.State != TrainHeld || !strings.Contains(got.HeldReason, "no longer holds owner authority") {
		t.Fatalf("state = %s (%s)", got.State, got.HeldReason)
	}
}

func TestWithdrawingTheWordHoldsARunningRelease(t *testing.T) {
	th, _ := newReleaseHarness(t)
	th.next(t)
	th.allowRelease(t, false, false)
	got := th.next(t)
	if got.State != TrainHeld || !strings.Contains(got.HeldReason, "stopped") {
		t.Fatalf("state = %s (%s)", got.State, got.HeldReason)
	}
}

func TestStartReleaseNowRespectsAReleaseInFlightAndReplacesAHeldOne(t *testing.T) {
	th, releaser := newReleaseHarness(t)
	th.next(t)
	if _, err := th.integ.TrainExpress(personCtx(ownerID), repoName, nil); err == nil || !strings.Contains(err.Error(), "releasing") {
		t.Fatalf("while releasing: %v", err)
	}
	releaser.answer(pipelines.TrainReleaseStatus{IntentID: "intent-1", Phase: "blocked", Stop: true, Reason: "stopped"}, nil)
	held := th.next(t)
	if held.State != TrainHeld {
		t.Fatalf("not held: %s", held.State)
	}
	// Nothing new on main, and the person still wants it tried again.
	th.remote.otherFiles = nil
	fresh, err := th.integ.TrainExpress(personCtx(ownerID), repoName, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == held.ID || fresh.Replaces != held.ID {
		t.Fatalf("replaced = %+v", fresh)
	}
	th.remote.mu.Lock()
	th.github.mu.Lock()
	th.github.heads[repoName+"@main"] = headAnswer{SHA: held.LandedSHA}
	th.github.mu.Unlock()
	th.remote.files[held.LandedSHA] = map[string][]byte{"VERSION": []byte(held.Version + "\n")}
	th.remote.mu.Unlock()
	th.tick(t)
	got := th.train(t)
	if got.State != TrainRunning || got.Version == held.Version {
		t.Fatalf("the new release: %s, version %s (the spent one was %s): %s", got.State, got.Version, held.Version, got.HeldReason)
	}
}

func TestOnlyTheOwnerGivesTheWord(t *testing.T) {
	window := `,"deployment":{"repository":"acme/install","applications":["memql"],"verifyStages":["deployment"],"window":{"start":"00:00","end":"07:00"}}`
	noWindow := `,"deployment":{"repository":"acme/install","applications":["memql"],"verifyStages":["deployment"]}`
	owner := clusterOwnerCtx(ownerID)
	for _, tc := range []struct {
		name            string
		settings        string
		ctx             context.Context
		release, deploy bool
		want            string
	}{
		{"somebody else", releaseSettings, clusterOwnerCtx(otherID), true, false, "only the owner"},
		{"the pipeline's owner without the owner role", releaseSettings, personCtx(ownerID), true, false, "only the owner"},
		{"the automation, which has no person", releaseSettings, automationCtx(), true, false, "no signed-in person"},
		{"no profile to release under", `,"landing":"automatic"`, owner, true, false, "releaseProfile"},
		{"a deploy without a release", releaseSettings + window, owner, false, true, "follows an automatic release"},
		{"a deploy with no deployment", releaseSettings, owner, true, true, "deployment with a window"},
		{"a deploy with no window", releaseSettings + noWindow, owner, true, true, "deployment with a window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th := newMainTrainHarness(t, tc.settings)
			_, err := th.integ.SetStanding(tc.ctx, repoName, tc.release, tc.deploy)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if st, _ := th.store.Standing(context.Background(), th.p.ID); st != nil {
				t.Errorf("a refused word was written: %+v", st)
			}
		})
	}
	t.Run("the owner", func(t *testing.T) {
		th := newMainTrainHarness(t, releaseSettings+window)
		st, err := th.integ.SetStanding(owner, repoName, true, true)
		if err != nil {
			t.Fatal(err)
		}
		saved, _ := th.store.Standing(context.Background(), th.p.ID)
		if saved == nil || !saved.Release || !saved.Deploy || saved.Authority["roleCeiling"] != "owner" || st.ID != StandingIDFor(th.p.ID) {
			t.Fatalf("saved = %+v", saved)
		}
		// Taking the word back needs no settings at all.
		th.settings = strings.Replace(th.settings, window, "", 1)
		if _, err := th.integ.SetStanding(owner, repoName, false, false); err != nil {
			t.Fatalf("taking it back: %v", err)
		}
	})
}

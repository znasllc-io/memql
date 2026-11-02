package pipelinerun

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/pipelines"
)

const (
	installRepo = "acme/install"
	installBase = "5555555555555555555555555555555555555555"
	mainWas     = "6666666666666666666666666666666666666666"
	pinCommit   = "7777777777777777777777777777777777777777"
	releaseSHA  = "8888888888888888888888888888888888888888"
	engineWas   = "9999999999999999999999999999999999999999"
)

const deploySettings = releaseSettings + `,"deployment":{"repository":"acme/install","applications":["memql","znas-cloud"],"verifyStages":["deployment"],"window":{"start":"00:00","end":"07:00"}}`

// fakeApplication is one ArgoCD Application as the fake controller holds it.
type fakeApplication struct {
	revision string // the last successful sync's commit
	request  string // the operation's request
	synced   string // the commit that operation names
	phase    string
	healthy  bool
	settled  bool
	foreign  bool // another operation runs here
}

// fakeDeployEffects plays the release integration and the cluster.
type fakeDeployEffects struct {
	mu          sync.Mutex
	apps        map[string]*fakeApplication
	syncs       []string // <name>@<revision>, in the order they started
	terminated  []string
	release     pipelines.DeployRelease
	releaseErr  error
	pins        []pipelines.DeployPinRequest
	pinErr      error
	remote      *trainRemote
	nextPR      int
	syncErr     error
	autoSucceed bool
}

func (f *fakeDeployEffects) NewestRelease(_ context.Context, _ string, authority map[string]any, _, version string) (pipelines.DeployRelease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(authority) == 0 {
		return pipelines.DeployRelease{}, errors.New("no authority")
	}
	if version != "" && version != f.release.Version {
		return pipelines.DeployRelease{}, errors.New("not in the catalog")
	}
	return f.release, f.releaseErr
}

func (f *fakeDeployEffects) ProposeDeployPin(_ context.Context, r pipelines.DeployPinRequest) (pipelines.DeployPin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pinErr != nil {
		return pipelines.DeployPin{}, f.pinErr
	}
	f.pins = append(f.pins, r)
	f.remote.mu.Lock()
	defer f.remote.mu.Unlock()
	number := f.nextPR
	if number == 0 {
		number = 900
		f.nextPR = 900
	}
	f.remote.trainPRs[number] = &trainPR{head: "memql/update-1", base: "main", title: "Update installation", open: true}
	f.remote.branches["memql/update-1"] = pinCommit
	return pipelines.DeployPin{ProposalID: "proposal-1", PullRequest: number, PullRequestURL: "https://github.com/acme/install/pull/900", Commit: pinCommit}, nil
}

func (f *fakeDeployEffects) facts(a *fakeApplication, request string) pipelines.DeployApplicationFacts {
	out := pipelines.DeployApplicationFacts{UID: "uid", Revision: a.revision, Phase: a.phase, Healthy: a.healthy, Health: "Progressing", Settled: a.settled}
	if a.healthy {
		out.Health = "Healthy"
	}
	out.Pending = a.foreign || a.phase == "Running"
	if request != "" && a.request == request {
		out.Mine = true
		out.Succeeded = a.phase == "Succeeded"
	}
	return out
}

func (f *fakeDeployEffects) ReadApplication(_ context.Context, _, name string) (pipelines.DeployApplicationFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.apps[name]
	if !ok {
		return pipelines.DeployApplicationFacts{}, errors.New("no such Application")
	}
	return f.facts(a, ""), nil
}

func (f *fakeDeployEffects) StartSync(_ context.Context, r pipelines.DeployRequest) (pipelines.DeployApplicationFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.syncErr != nil {
		return pipelines.DeployApplicationFacts{}, f.syncErr
	}
	a := f.apps[r.Name]
	if a.request == r.RequestID {
		return f.facts(a, r.RequestID), nil
	}
	if a.foreign || a.phase == "Running" || a.phase == "Terminating" {
		return pipelines.DeployApplicationFacts{}, pipelines.ErrDeployBusy
	}
	a.request, a.synced, a.phase, a.healthy, a.settled = r.RequestID, r.Revision, "Running", false, false
	f.syncs = append(f.syncs, r.Name+"@"+r.Revision)
	if f.autoSucceed {
		a.phase, a.healthy, a.settled, a.revision = "Succeeded", true, true, r.Revision
	}
	return f.facts(a, r.RequestID), nil
}

func (f *fakeDeployEffects) TerminateSync(_ context.Context, r pipelines.DeployRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, r.Name)
	f.apps[r.Name].phase = "Failed"
	return nil
}

// finishSync is the controller finishing a sync and comparing again.
func (f *fakeDeployEffects) finishSync(name, phase string, healthy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.apps[name]
	a.phase, a.healthy, a.settled = phase, healthy, true
	if phase == "Succeeded" {
		a.revision = a.synced
	}
}

// deployHarness holds a repository that published 0.31.5, and an installation pinned to
// 0.31.4 that was last synced at mainWas.
type deployHarness struct {
	*trainHarness
	effects   *fakeDeployEffects
	install   Pipeline
	stepCount int
}

func newDeployHarness(t *testing.T) *deployHarness {
	t.Helper()
	th := newMainTrainHarness(t, deploySettings)
	h := &deployHarness{trainHarness: th}
	h.install = testPipeline(DeliveryWebhook)
	h.install.ID, h.install.PackageID, h.install.Name, h.install.Repository = "install-pipeline", "pkg-install", "install", installRepo
	th.store.addPipeline(h.install)
	th.github.mu.Lock()
	th.github.heads[installRepo+"@main"] = headAnswer{SHA: installBase}
	th.github.mu.Unlock()
	h.effects = &fakeDeployEffects{remote: th.remote, apps: map[string]*fakeApplication{
		"memql":      {revision: mainWas, phase: "Succeeded", healthy: true, settled: true},
		"znas-cloud": {revision: mainWas, phase: "Succeeded", healthy: true, settled: true},
	}, release: pipelines.DeployRelease{Version: "0.31.5", Tag: "v0.31.5", SHA: releaseSHA, CandidateID: "sha256:c", CatalogDigest: "sha256:d"}}
	th.integ.Configure(func(d *Deps) { d.DeployEffects = h.effects; d.TrainReleaser = &fakeReleaser{} })
	th.allowRelease(t, true, true)
	contract := []byte(`contracts := map[string]int{"workerHost.pipeline_step": 5}`)
	manifest := []byte("pipeline:\n  stages:\n  - name: checks\n    steps:\n    - name: unit\n")
	th.remote.mu.Lock()
	th.remote.files["main"] = map[string][]byte{"product.env": []byte("ENGINE_REF=v0.31.4\n")}
	th.remote.files["v0.31.4"] = map[string][]byte{pipelines.WorkerContractPath: contract, pipelines.ManifestPath: manifest}
	th.remote.files[releaseSHA] = map[string][]byte{pipelines.WorkerContractPath: contract, pipelines.ManifestPath: manifest}
	th.remote.dirs = map[string]map[string][]string{}
	th.remote.mu.Unlock()
	// Inside the window, with a published train as the newest.
	th.clock = time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	tr := Train{ID: "train-1", OwnerUserID: th.p.OwnerUserID, AccountID: th.p.AccountID, PipelineID: th.p.ID, Repository: repoName,
		Branch: "train/2026-10-28-1", Departure: TrainDepartureScheduled, State: TrainBuilding, DepartedAt: th.clock.Add(-4 * time.Hour), UpdatedAt: th.clock}
	if err := th.store.CreateTrain(context.Background(), th.p.OwnerUserID, tr); err != nil {
		t.Fatal(err)
	}
	if err := th.store.UpdateTrain(context.Background(), th.p.OwnerUserID, tr.ID, TrainPatch{
		State: ptr(TrainPublished), Version: ptr("0.31.5"), HeadSHA: ptr(releaseSHA), LandedSHA: ptr("merge"),
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *deployHarness) deployment(t *testing.T) Deployment {
	t.Helper()
	dep, err := h.store.LatestDeployment(context.Background(), h.p.ID)
	if err != nil || dep == nil {
		t.Fatalf("no deployment: %v", err)
	}
	return *dep
}

// step advances the clock and ticks, on another replica every other time, because the
// row is the state and any of them may take a step.
func (h *deployHarness) step(t *testing.T) Deployment {
	t.Helper()
	h.clock = h.clock.Add(time.Minute)
	h.stepCount++
	integ := h.integ
	if h.stepCount%2 == 0 {
		integ = h.otherReplica()
	}
	if _, err := integ.TrainTick(automationCtx()); err != nil {
		t.Fatalf("TrainTick: %v", err)
	}
	return h.deployment(t)
}

func (h *deployHarness) stepUntil(t *testing.T, state string) Deployment {
	t.Helper()
	for range 25 {
		if dep := h.step(t); dep.State == state {
			return dep
		}
	}
	dep := h.deployment(t)
	t.Fatalf("never reached %s: it is %s (%s)", state, dep.State, dep.Reason)
	return dep
}

// finishRunAt concludes the newest run of a pipeline at a commit, or adds one.
func (h *deployHarness) passPin(t *testing.T, conclusion string) {
	t.Helper()
	r := Run{ID: "pin-run", OwnerUserID: h.install.OwnerUserID, PipelineID: h.install.ID, Repository: installRepo, SHA: pinCommit,
		Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest, RunKey: "pin-key", Attempt: 1, Trigger: TriggerWebhook,
		PullRequest: 900, HeadBranch: "memql/update-1", Status: StatusCompleted, Conclusion: conclusion, WorkRunID: "pin-work", QueuedAt: h.clock}
	h.store.addRun(r)
}

// concludeVerification finishes the deployment's verification run.
func (h *deployHarness) concludeVerification(t *testing.T, conclusion string) Run {
	t.Helper()
	dep := h.deployment(t)
	if dep.RunID == "" {
		t.Fatalf("no verification run yet: %s", dep.State)
	}
	run, err := h.store.RunByID(context.Background(), dep.RunID)
	if err != nil || run == nil {
		t.Fatalf("run %s: %v", dep.RunID, err)
	}
	run.Status, run.Conclusion, run.FinishedAt = StatusCompleted, conclusion, h.clock
	h.store.addRun(*run)
	return *run
}

// toSyncing brings a deployment to where production starts to move.
func (h *deployHarness) toSyncing(t *testing.T) Deployment {
	t.Helper()
	h.stepUntil(t, DeploymentPinning)
	for range 5 {
		if dep := h.step(t); dep.PinSHA != "" {
			break
		}
	}
	h.passPin(t, ConclusionSuccess)
	return h.stepUntil(t, DeploymentSyncing)
}

func TestAPublishedReleaseDeploysAndItsPinLandsLast(t *testing.T) {
	h := newDeployHarness(t)
	dep := h.toSyncing(t)
	if dep.Trigger != DeploymentTriggerAutomatic || dep.RequestedBy != deployApprover {
		t.Errorf("trigger %s by %s", dep.Trigger, dep.RequestedBy)
	}
	if len(h.effects.pins) != 1 || h.effects.pins[0].ExpectedBase != installBase || h.effects.pins[0].CandidateID != "sha256:c" || h.effects.pins[0].Authority["roleCeiling"] != "owner" {
		t.Fatalf("pins = %+v", h.effects.pins)
	}
	// It reads what each Application runs before moving any of them.
	dep = h.step(t)
	if len(dep.Applications) != 2 || dep.Applications[0].Previous != mainWas || dep.Applications[1].Name != "znas-cloud" {
		t.Fatalf("applications = %+v", dep.Applications)
	}
	h.step(t) // records the request
	h.step(t) // asks
	if got := h.effects.syncs; len(got) != 1 || got[0] != "memql@"+pinCommit {
		t.Fatalf("syncs = %v: the engine goes first, at the pin", got)
	}
	// The product waits until the engine is healthy.
	h.step(t)
	if len(h.effects.syncs) != 1 {
		t.Fatalf("the product started before the engine was healthy: %v", h.effects.syncs)
	}
	h.effects.finishSync("memql", "Succeeded", true)
	h.step(t)
	h.step(t)
	h.step(t)
	if got := h.effects.syncs; len(got) != 2 || got[1] != "znas-cloud@"+pinCommit {
		t.Fatalf("syncs = %v", got)
	}
	h.effects.finishSync("znas-cloud", "Succeeded", true)
	dep = h.stepUntil(t, DeploymentVerifying)
	for range 3 {
		if dep = h.step(t); dep.RunID != "" {
			break
		}
	}
	run, err := h.store.RunByID(context.Background(), dep.RunID)
	if err != nil || run == nil {
		t.Fatal(err)
	}
	if run.SHA != pinCommit || run.Event != pipelines.EventManual || !slices.Equal(run.OnlyStages, []string{"deployment"}) || run.PipelineID != h.install.ID {
		t.Fatalf("verification run = %+v", run)
	}
	h.remote.mu.Lock()
	merged := h.remote.trainPRs[900].merged
	h.remote.mu.Unlock()
	if merged {
		t.Fatal("the pin merged before it was verified")
	}
	h.concludeVerification(t, ConclusionSuccess)
	dep = h.stepUntil(t, DeploymentDeployed)
	h.remote.mu.Lock()
	defer h.remote.mu.Unlock()
	if !h.remote.trainPRs[900].merged || dep.LandedSHA == "" {
		t.Errorf("the pin did not land: %+v", h.remote.trainPRs[900])
	}
	if !slices.Contains(h.remote.readied, 900) {
		t.Error("a draft was merged without being marked ready")
	}
	if text := strings.Join(h.messages(), "\n"); !strings.Contains(text, "is deployed") {
		t.Errorf("no message says it deployed: %s", text)
	}
}

func TestAReleaseWaitsForItsWindow(t *testing.T) {
	h := newDeployHarness(t)
	h.settings = strings.Replace(h.settings, `"start":"00:00","end":"07:00"`, `"start":"08:10","end":"08:50"`, 1)
	h.clock = time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	for range 3 {
		if dep := h.step(t); dep.State != DeploymentWaiting {
			t.Fatalf("outside the window: %s", dep.State)
		}
	}
	h.clock = time.Date(2026, 10, 29, 8, 9, 0, 0, time.UTC)
	if dep := h.step(t); dep.State != DeploymentPinning {
		t.Fatalf("when the window opened: %s (%s)", dep.State, dep.Reason)
	}
}

// Syncing the mesh restarts agent, which kills running CI, so neither waits for the
// other to fail: an automatic deploy waits for a train, and a train for a deploy.
func TestADeployWaitsForARunningTrainAndATrainForAMovingDeploy(t *testing.T) {
	t.Run("a train is running", func(t *testing.T) {
		h := newDeployHarness(t)
		running := Train{ID: "train-2", OwnerUserID: h.p.OwnerUserID, PipelineID: h.p.ID, Repository: repoName, Branch: "train/2", Departure: TrainDepartureScheduled,
			State: TrainRunning, DepartedAt: h.clock, UpdatedAt: h.clock, HeadSHA: "head", BaseSHA: "base", PullRequest: 1}
		if err := h.store.CreateTrain(context.Background(), h.p.OwnerUserID, running); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if dep := h.step(t); dep.State != DeploymentWaiting {
				t.Fatalf("under a running train: %s", dep.State)
			}
		}
	})
	t.Run("a deploy is moving", func(t *testing.T) {
		h := newDeployHarness(t)
		h.toSyncing(t)
		before := h.train(t).ID
		// A departure falls due, and no train may leave under it.
		h.clock = h.clock.Add(24 * time.Hour)
		h.step(t)
		if got := h.train(t).ID; got != before {
			t.Fatalf("a train left under a moving deploy: %s", got)
		}
	})
}

func TestAPinThatFailsItsChecksTouchesNothing(t *testing.T) {
	h := newDeployHarness(t)
	h.stepUntil(t, DeploymentPinning)
	h.step(t)
	h.step(t)
	r := Run{ID: "pin-run", OwnerUserID: h.install.OwnerUserID, PipelineID: h.install.ID, Repository: installRepo, SHA: pinCommit, Mode: pipelines.ModeAffected,
		Event: pipelines.EventPullRequest, RunKey: "pin-key", Attempt: 2, Trigger: TriggerRerun, PullRequest: 900, HeadBranch: "memql/update-1",
		Status: StatusCompleted, Conclusion: ConclusionFailure, QueuedAt: h.clock}
	h.store.addRun(r)
	dep := h.stepUntil(t, DeploymentCancelled)
	if len(h.effects.syncs) != 0 || !strings.Contains(dep.Reason, "failed its checks") {
		t.Fatalf("syncs %v, reason %q", h.effects.syncs, dep.Reason)
	}
	h.remote.mu.Lock()
	open := h.remote.trainPRs[900].open
	h.remote.mu.Unlock()
	if open {
		t.Error("the pin's pull request stayed open")
	}
	if dep.PausesAutomatic() {
		t.Error("a pin that never touched production paused automatic deploys")
	}
	if text := strings.Join(h.messages(), "\n"); !strings.Contains(text, "did not deploy") {
		t.Errorf("nobody was told: %s", text)
	}
	// The same version is not tried again by itself. The next one is.
	h.step(t)
	if got := h.deployment(t); got.ID != dep.ID {
		t.Fatalf("the same version started again: %+v", got)
	}
}

func TestAFailedVerificationGoesBackToWhatRanBefore(t *testing.T) {
	h := newDeployHarness(t)
	h.effects.autoSucceed = true
	h.toSyncing(t)
	dep := h.stepUntil(t, DeploymentVerifying)
	for range 3 {
		if dep = h.step(t); dep.RunID != "" {
			break
		}
	}
	h.concludeVerification(t, ConclusionFailure)
	dep = h.stepUntil(t, DeploymentRollingBack)
	if !strings.Contains(dep.Reason, "failed its verification") {
		t.Errorf("reason = %q", dep.Reason)
	}
	dep = h.stepUntil(t, DeploymentVerifyingRollback)
	if got := h.effects.syncs[2:]; len(got) != 2 || got[0] != "memql@"+mainWas || got[1] != "znas-cloud@"+mainWas {
		t.Fatalf("syncs back = %v, want each Application at what it ran before", got)
	}
	for range 3 {
		if dep = h.step(t); dep.RunID != "" {
			break
		}
	}
	run := h.concludeVerification(t, ConclusionSuccess)
	if run.SHA != installBase {
		t.Errorf("going back verified %s, want main as it was", run.SHA)
	}
	dep = h.stepUntil(t, DeploymentRolledBack)
	h.remote.mu.Lock()
	pr := *h.remote.trainPRs[900]
	h.remote.mu.Unlock()
	if pr.open || pr.merged {
		t.Errorf("the failed pin: %+v; main must never carry it", pr)
	}
	if !dep.PausesAutomatic() {
		t.Error("a rollback did not pause automatic deploys")
	}
	if text := strings.Join(h.messages(), "\n"); !strings.Contains(text, "was rolled back") {
		t.Errorf("nobody was told: %s", text)
	}
}

func TestARollbackPausesUntilTheOwnerResumes(t *testing.T) {
	h := newDeployHarness(t)
	h.effects.autoSucceed = true
	h.toSyncing(t)
	h.stepUntil(t, DeploymentVerifying)
	for range 3 {
		if dep := h.step(t); dep.RunID != "" {
			break
		}
	}
	h.concludeVerification(t, ConclusionFailure)
	h.stepUntil(t, DeploymentVerifyingRollback)
	for range 3 {
		if dep := h.step(t); dep.RunID != "" {
			break
		}
	}
	h.concludeVerification(t, ConclusionSuccess)
	rolled := h.stepUntil(t, DeploymentRolledBack)

	// A newer release does not deploy while paused.
	h.effects.release.Version, h.effects.release.Tag = "0.31.6", "v0.31.6"
	newer := Train{ID: "train-3", OwnerUserID: h.p.OwnerUserID, PipelineID: h.p.ID, Repository: repoName, Branch: "train/3", Departure: TrainDepartureScheduled,
		State: TrainBuilding, DepartedAt: h.clock, UpdatedAt: h.clock}
	if err := h.store.CreateTrain(context.Background(), h.p.OwnerUserID, newer); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateTrain(context.Background(), h.p.OwnerUserID, newer.ID, TrainPatch{State: ptr(TrainPublished), Version: ptr("0.31.6"), HeadSHA: ptr(releaseSHA), LandedSHA: ptr("m2")}); err != nil {
		t.Fatal(err)
	}
	h.step(t)
	if got := h.deployment(t); got.ID != rolled.ID {
		t.Fatalf("a paused repository deployed: %+v", got)
	}
	if _, err := h.integ.ResumeDeployments(personCtx(ownerID), repoName); err == nil {
		t.Fatal("someone without the owner role resumed")
	}
	if _, err := h.integ.ResumeDeployments(clusterOwnerCtx(ownerID), repoName); err != nil {
		t.Fatal(err)
	}
	if got := h.step(t); got.Version != "0.31.6" {
		t.Fatalf("after resuming: %s of %s", got.State, got.Version)
	}
}

// One Application failing leaves the ones after it alone, and goes back on the rest.
func TestAFailedSyncGoesBackOnlyOnWhatItStarted(t *testing.T) {
	h := newDeployHarness(t)
	h.toSyncing(t)
	h.step(t)
	h.step(t)
	h.step(t)
	h.effects.finishSync("memql", "Failed", false)
	dep := h.stepUntil(t, DeploymentRollingBack)
	if !strings.Contains(dep.Reason, "memql's sync ended Failed") {
		t.Errorf("reason = %q", dep.Reason)
	}
	h.effects.autoSucceed = true
	h.stepUntil(t, DeploymentVerifyingRollback)
	if got := h.effects.syncs; len(got) != 2 || got[1] != "memql@"+mainWas {
		t.Fatalf("syncs = %v: the product never moved, so it has nothing to go back from", got)
	}
}

// A sync waiting on a Deployment that never comes up runs forever, so the deadline stops
// it and goes back.
func TestASyncThatHangsIsStoppedAndGoesBack(t *testing.T) {
	h := newDeployHarness(t)
	h.toSyncing(t)
	h.step(t)
	h.step(t)
	h.step(t)
	h.clock = h.clock.Add(deploySyncDeadline)
	dep := h.step(t)
	if dep.State != DeploymentRollingBack || !strings.Contains(dep.Reason, "did not become healthy") {
		t.Fatalf("after the deadline: %s (%s)", dep.State, dep.Reason)
	}
	h.effects.autoSucceed = true
	h.stepUntil(t, DeploymentVerifyingRollback)
	if !slices.Contains(h.effects.terminated, "memql") {
		t.Errorf("the hanging sync was not stopped: %v", h.effects.terminated)
	}
}

func TestARollbackThatFailsStopsAndAsks(t *testing.T) {
	h := newDeployHarness(t)
	h.toSyncing(t)
	h.step(t)
	h.step(t)
	h.step(t)
	h.effects.finishSync("memql", "Failed", false)
	h.stepUntil(t, DeploymentRollingBack)
	for range 4 {
		h.step(t)
		if len(h.effects.syncs) == 2 {
			break
		}
	}
	h.effects.finishSync("memql", "Failed", false)
	dep := h.stepUntil(t, DeploymentStopped)
	if !strings.Contains(dep.Reason, "could not go back") || !dep.PausesAutomatic() {
		t.Fatalf("stopped: %q", dep.Reason)
	}
	text := strings.Join(h.messages(), "\n")
	if !strings.Contains(text, "stopped partway") {
		t.Errorf("nobody was told: %s", text)
	}
}

// An Application with no sync to go back to cannot be undone, so nothing starts.
func TestNothingMovesWithoutSomethingToGoBackTo(t *testing.T) {
	h := newDeployHarness(t)
	h.effects.apps["znas-cloud"].revision = ""
	h.toSyncing(t)
	dep := h.stepUntil(t, DeploymentCancelled)
	if len(h.effects.syncs) != 0 || !strings.Contains(dep.Reason, "no successful sync") {
		t.Fatalf("syncs %v, reason %q", h.effects.syncs, dep.Reason)
	}
}

// Someone else's operation on the Application is theirs to finish.
func TestASyncWaitsOutAnotherOperation(t *testing.T) {
	h := newDeployHarness(t)
	h.toSyncing(t)
	h.step(t)
	h.effects.apps["memql"].foreign = true
	h.step(t)
	dep := h.step(t)
	if dep.State != DeploymentSyncing || len(h.effects.syncs) != 0 {
		t.Fatalf("over another operation: %s, %v", dep.State, h.effects.syncs)
	}
	h.effects.apps["memql"].foreign = false
	h.step(t)
	if len(h.effects.syncs) != 1 {
		t.Fatalf("once it ended: %v", h.effects.syncs)
	}
}

func TestAGuardWaitsForTheButtonAndTablesNeverRollBack(t *testing.T) {
	h := newDeployHarness(t)
	h.remote.mu.Lock()
	h.remote.dirs["v0.31.4"] = map[string][]string{"component/database": {"database.go"}}
	h.remote.dirs[releaseSHA] = map[string][]string{"component/database": {"database.go", "20261102_things.go"}}
	h.remote.files[releaseSHA]["component/database/20261102_things.go"] = []byte("m.MustRegister(func() { db.ExecContext(ctx, `ALTER TABLE t ADD COLUMN c text`) })")
	h.remote.mu.Unlock()
	dep := h.step(t)
	if dep.State != DeploymentWaiting || len(dep.Guards) != 1 || dep.Guards[0].Code != pipelines.DeployGuardData {
		t.Fatalf("guards = %+v, state %s", dep.Guards, dep.State)
	}
	for range 2 {
		if dep = h.step(t); dep.State != DeploymentWaiting {
			t.Fatalf("it went without the button: %s", dep.State)
		}
	}
	text := strings.Join(h.messages(), "\n")
	if !strings.Contains(text, "waits for your Deploy button") || !strings.Contains(text, "tables or data") {
		t.Errorf("the owner was not told why: %s", text)
	}

	if _, err := h.integ.DeployRelease(personCtx(ownerID), repoName, ""); err == nil {
		t.Fatal("someone without the owner role pressed Deploy")
	}
	if _, err := h.integ.DeployRelease(clusterOwnerCtx(otherID), repoName, ""); err == nil {
		t.Fatal("another owner pressed Deploy on this pipeline")
	}
	pressed, err := h.integ.DeployRelease(clusterOwnerCtx(ownerID), repoName, "")
	if err != nil {
		t.Fatal(err)
	}
	if !pressed.Acknowledged || pressed.Trigger != DeploymentTriggerButton || pressed.ID != dep.ID {
		t.Fatalf("pressed = %+v", pressed)
	}
	h.toSyncing(t)
	h.step(t)
	h.step(t)
	h.step(t)
	h.effects.finishSync("memql", "Failed", false)
	dep = h.stepUntil(t, DeploymentStopped)
	if !strings.Contains(dep.Reason, "tables or data") {
		t.Errorf("reason = %q", dep.Reason)
	}
	if got := h.effects.syncs; len(got) != 1 {
		t.Errorf("syncs = %v: old code is not proven on a new schema, so nothing goes back by itself", got)
	}
}

func TestEachGuard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*deployHarness)
		code  string
	}{
		{"an index alone waits for nobody", func(h *deployHarness) {
			h.remote.dirs[releaseSHA] = map[string][]string{"component/database": {"20261102_idx.go"}}
			h.remote.files[releaseSHA]["component/database/20261102_idx.go"] = []byte("m.MustRegister(func() { db.ExecContext(ctx, `CREATE INDEX i ON t (a)`) })")
		}, ""},
		{"a SQL migration that adds a table", func(h *deployHarness) {
			h.remote.dirs[releaseSHA] = map[string][]string{"component/database/memory-nodes/migrations": {"2026_t.up.sql", "2026_t.down.sql"}}
			h.remote.files[releaseSHA]["component/database/memory-nodes/migrations/2026_t.up.sql"] = []byte("CREATE TABLE t (a int);")
		}, pipelines.DeployGuardData},
		{"a changed worker contract", func(h *deployHarness) {
			h.remote.files[releaseSHA][pipelines.WorkerContractPath] = []byte(`contracts := map[string]int{"workerHost.pipeline_step": 6}`)
		}, pipelines.DeployGuardContract},
		{"a removed pipeline step", func(h *deployHarness) {
			h.remote.files[releaseSHA][pipelines.ManifestPath] = []byte("pipeline:\n  stages:\n  - name: checks\n    steps:\n    - name: other\n")
		}, pipelines.DeployGuardStep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeployHarness(t)
			h.remote.mu.Lock()
			tc.setup(h)
			h.remote.mu.Unlock()
			dep := h.step(t)
			if tc.code == "" {
				if len(dep.Guards) != 0 {
					t.Fatalf("guards = %+v", dep.Guards)
				}
				return
			}
			if len(dep.Guards) != 1 || dep.Guards[0].Code != tc.code {
				t.Fatalf("guards = %+v", dep.Guards)
			}
		})
	}
}

// A release held back at a guard is still in the range of the next: guards read from what
// the installation runs, so a newer release cannot slip past it.
func TestANewerReleaseCarriesAWaitingOneAndItsGuards(t *testing.T) {
	h := newDeployHarness(t)
	h.remote.mu.Lock()
	h.remote.dirs[releaseSHA] = map[string][]string{"component/database": {"20261102_things.go"}}
	h.remote.files[releaseSHA]["component/database/20261102_things.go"] = []byte("m.MustRegister(func() { db.ExecContext(ctx, `DELETE FROM t`) })")
	h.remote.mu.Unlock()
	first := h.step(t)
	if len(first.Guards) != 1 {
		t.Fatalf("guards = %+v", first.Guards)
	}
	h.effects.release.Version, h.effects.release.Tag = "0.31.6", "v0.31.6"
	newer := Train{ID: "train-3", OwnerUserID: h.p.OwnerUserID, PipelineID: h.p.ID, Repository: repoName, Branch: "train/3", Departure: TrainDepartureScheduled,
		State: TrainBuilding, DepartedAt: h.clock, UpdatedAt: h.clock}
	if err := h.store.CreateTrain(context.Background(), h.p.OwnerUserID, newer); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpdateTrain(context.Background(), h.p.OwnerUserID, newer.ID, TrainPatch{State: ptr(TrainPublished), Version: ptr("0.31.6"), HeadSHA: ptr(releaseSHA), LandedSHA: ptr("m2")}); err != nil {
		t.Fatal(err)
	}
	if got := h.step(t); got.State != DeploymentCancelled {
		t.Fatalf("the waiting one: %s", got.State)
	}
	got := h.step(t)
	if got.Version != "0.31.6" || len(got.Guards) != 1 || got.State != DeploymentWaiting {
		t.Fatalf("the newer one: %+v", got)
	}
}

func TestAVersionAlreadyInstalledDoesNothing(t *testing.T) {
	h := newDeployHarness(t)
	h.remote.mu.Lock()
	h.remote.files["main"]["product.env"] = []byte("ENGINE_REF=v0.31.5\n")
	h.remote.mu.Unlock()
	h.clock = h.clock.Add(time.Minute)
	h.tick(t)
	if dep, _ := h.store.LatestDeployment(context.Background(), h.p.ID); dep != nil {
		t.Fatalf("it deployed what is installed: %+v", dep)
	}
	if _, err := h.integ.DeployRelease(clusterOwnerCtx(ownerID), repoName, ""); err == nil || !strings.Contains(err.Error(), "not newer") {
		t.Fatalf("the button: %v", err)
	}
}

func TestWithdrawingTheWordCancelsAWaitingDeploy(t *testing.T) {
	h := newDeployHarness(t)
	h.clock = time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	if dep := h.step(t); dep.State != DeploymentWaiting {
		t.Fatalf("state = %s", dep.State)
	}
	h.allowRelease(t, true, false)
	if dep := h.step(t); dep.State != DeploymentCancelled {
		t.Fatalf("state = %s", dep.State)
	}
}

func TestTheButtonNeedsNoStandingWordOrWindow(t *testing.T) {
	h := newDeployHarness(t)
	h.allowRelease(t, false, false)
	h.clock = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	pressed, err := h.integ.DeployRelease(clusterOwnerCtx(ownerID), repoName, "v0.31.5")
	if err != nil {
		t.Fatal(err)
	}
	if pressed.State != DeploymentWaiting || pressed.Trigger != DeploymentTriggerButton || len(pressed.Authority) == 0 {
		t.Fatalf("pressed = %+v", pressed)
	}
	if got := h.step(t); got.State != DeploymentPinning {
		t.Fatalf("after a tick: %s (%s)", got.State, got.Reason)
	}
	if _, err := h.integ.DeployRelease(clusterOwnerCtx(ownerID), repoName, ""); err == nil || !strings.Contains(err.Error(), "pinning") {
		t.Fatalf("while one moves: %v", err)
	}
	if _, err := h.integ.DeployRelease(clusterOwnerCtx(ownerID), repoName, "9.9.9"); err == nil {
		t.Fatal("a version not in the catalog was accepted")
	}
}

func TestGitHubRefusingAVerifiedPinIsAChoreNotARollback(t *testing.T) {
	h := newDeployHarness(t)
	h.effects.autoSucceed = true
	h.toSyncing(t)
	h.stepUntil(t, DeploymentVerifying)
	for range 3 {
		if dep := h.step(t); dep.RunID != "" {
			break
		}
	}
	h.concludeVerification(t, ConclusionSuccess)
	h.remote.mu.Lock()
	h.remote.refuseMerges = true
	h.remote.mu.Unlock()
	if dep := h.stepUntil(t, DeploymentLanding); dep.State != DeploymentLanding {
		t.Fatalf("state = %s", dep.State)
	}
	h.clock = h.clock.Add(deployLandDeadline)
	dep := h.stepUntil(t, DeploymentDeployed)
	if !strings.Contains(dep.Reason, "would not merge") || dep.LandedSHA != "" {
		t.Fatalf("deployed: %+v", dep)
	}
	if text := strings.Join(h.messages(), "\n"); !strings.Contains(text, "does not have its pin") {
		t.Errorf("nobody was told: %s", text)
	}
}

// Two ticks at once ask for one sync: the request is recorded before it is asked.
func TestTwoTicksAtOnceStartOneSync(t *testing.T) {
	h := newDeployHarness(t)
	h.toSyncing(t)
	h.step(t)
	h.step(t)
	dep := h.deployment(t)
	d := h.integ.snapshot()
	settings, err := pipelines.ParseTrainSettings(h.settings)
	if err != nil {
		t.Fatal(err)
	}
	s := settings[repoName]
	for _, integ := range []*Integration{h.integ, h.otherReplica()} {
		if err := integ.stepSyncing(automationCtx(), d, s, h.p, dep, h.clock); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.effects.syncs) != 1 {
		t.Errorf("syncs = %v", h.effects.syncs)
	}
}

// A step that fails on every tick must not log forever: a deploy that never finishes
// keeps every later train from leaving, with nobody told.
func TestAStepThatKeepsFailingEnds(t *testing.T) {
	t.Run("before anything moved", func(t *testing.T) {
		h := newDeployHarness(t)
		h.toSyncing(t)
		h.effects.mu.Lock()
		h.effects.apps = map[string]*fakeApplication{}
		h.effects.mu.Unlock()
		h.step(t)
		h.clock = h.clock.Add(deployStuckAfter)
		dep := h.step(t)
		if dep.State != DeploymentCancelled || len(h.effects.syncs) != 0 || !strings.Contains(dep.Reason, "could not go on") {
			t.Fatalf("state %s, syncs %v, reason %q", dep.State, h.effects.syncs, dep.Reason)
		}
		if dep.PausesAutomatic() {
			t.Error("a deploy that moved nothing paused automatic deploys")
		}
	})
	t.Run("while it could not read what to go back to", func(t *testing.T) {
		h := newDeployHarness(t)
		h.toSyncing(t)
		h.step(t)
		h.step(t)
		h.step(t)
		h.effects.finishSync("memql", "Failed", false)
		h.stepUntil(t, DeploymentRollingBack)
		h.effects.mu.Lock()
		h.effects.apps = map[string]*fakeApplication{}
		h.effects.mu.Unlock()
		h.step(t)
		h.clock = h.clock.Add(deployStuckAfter)
		dep := h.step(t)
		if dep.State != DeploymentStopped || !strings.Contains(dep.Reason, "could not finish going back") {
			t.Fatalf("state %s, reason %q", dep.State, dep.Reason)
		}
		if text := strings.Join(h.messages(), "\n"); !strings.Contains(text, "stopped partway") {
			t.Errorf("nobody was told: %s", text)
		}
	})
	t.Run("while it could not merge what is already running", func(t *testing.T) {
		h := newDeployHarness(t)
		h.effects.autoSucceed = true
		h.toSyncing(t)
		h.stepUntil(t, DeploymentVerifying)
		for range 3 {
			if dep := h.step(t); dep.RunID != "" {
				break
			}
		}
		h.concludeVerification(t, ConclusionSuccess)
		h.stepUntil(t, DeploymentLanding)
		h.remote.mu.Lock()
		delete(h.remote.trainPRs, 900)
		h.remote.mu.Unlock()
		h.step(t)
		h.clock = h.clock.Add(deployStuckAfter)
		dep := h.step(t)
		if dep.State != DeploymentDeployed || !strings.Contains(dep.Reason, "could not be merged") {
			t.Fatalf("state %s, reason %q", dep.State, dep.Reason)
		}
	})
}

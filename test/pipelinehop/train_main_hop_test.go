package pipelinehop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
)

// train_main_hop_test.go -- a release from main (design record
// docs/superpowers/specs/2026-11-02-nightly-release-from-main-design.md) on two
// replicas over one Postgres: the real store, the real policy capture and the
// real driver. One replica builds it, the run is opened on the bff and driven on
// the agent, and the other replica lands it, after main has moved.

// fakeTrainPR is one pull request a train opened.
type fakeTrainPR struct {
	head, base, title, body string
	open, merged            bool
	landedAs                string
}

// The train's half of GitHub, on the same fake as the run driver's.

func (h githubAt) TrainToken(ctx context.Context, credential, user, repository string) (string, int64, int64, error) {
	token, installation, err := h.InstallationToken(ctx, credential, user, repository)
	return token, installation, 4822538, err
}

func (h githubAt) TrainPullRequests(_ context.Context, token, repository, base string) ([]githubapp.TrainPullRequest, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return nil, err
	}
	var out []githubapp.TrainPullRequest
	for n, pr := range r.trainPRs {
		if pr.open && pr.base == base {
			out = append(out, githubapp.TrainPullRequest{Number: n, Title: pr.title, HeadRef: pr.head, HeadSHA: r.heads[pr.head], BaseRef: pr.base, HeadRepository: repository, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repository, n)})
		}
	}
	return out, nil
}

func (h githubAt) CheckRunConclusion(context.Context, string, string, string, string, int64) (string, string, error) {
	return "", "", nil
}

func (h githubAt) CreateBranchRef(_ context.Context, token, repository, branch, sha string) error {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return err
	}
	if _, exists := r.heads[branch]; exists {
		return &githubapp.StatusError{Status: 422, Endpoint: "/git/refs"}
	}
	r.heads[branch] = sha
	return nil
}

func (h githubAt) ResetBranchRef(_ context.Context, token, repository, branch, sha string) error {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return err
	}
	r.heads[branch] = sha
	return nil
}

func (h githubAt) DeleteBranchRef(_ context.Context, token, repository, branch string) error {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return err
	}
	delete(r.heads, branch)
	// GitHub closes an open pull request whose head branch is deleted.
	for _, pr := range r.trainPRs {
		if pr.head == branch && pr.open {
			pr.open = false
		}
	}
	return nil
}

func (h githubAt) MergeIntoBranch(context.Context, string, string, string, string, string) (githubapp.MergeOutcome, error) {
	return githubapp.MergeOutcome{}, fmt.Errorf("a train from main merges nothing onto its branch")
}

func (h githubAt) FileAtRef(_ context.Context, token, repository, ref, path string) ([]byte, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return nil, err
	}
	if sha, ok := r.heads[ref]; ok {
		ref = sha
	}
	if b, ok := r.commitFiles[ref][path]; ok {
		return b, nil
	}
	if path == "VERSION" {
		return []byte("0.1.0\n"), nil
	}
	if f, ok := r.files[path]; ok {
		return bytes.Clone(f.Data), nil
	}
	return nil, &githubapp.StatusError{Status: 404, Endpoint: "/contents/" + path}
}

func (h githubAt) PutBranchFile(_ context.Context, token, repository, branch, path, message string, content []byte) (string, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return "", err
	}
	sha := commitFor(repository, branch, message)
	if r.commitFiles == nil {
		r.commitFiles = map[string]map[string][]byte{}
	}
	r.commitFiles[sha] = map[string][]byte{path: content}
	r.commits[sha] = true
	r.heads[branch] = sha
	return sha, nil
}

func (h githubAt) AddEmptyCommit(_ context.Context, token, repository, branch, message string) (string, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return "", err
	}
	sha := commitFor(repository, branch, "empty", message)
	r.commits[sha] = true
	r.heads[branch] = sha
	return sha, nil
}

func (h githubAt) CreateTrainPullRequest(_ context.Context, token, repository, head, base, title, body string) (int, string, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return 0, "", err
	}
	if r.trainPRs == nil {
		r.trainPRs = map[int]*fakeTrainPR{}
	}
	r.nextPR++
	r.trainPRs[r.nextPR] = &fakeTrainPR{head: head, base: base, title: title, body: body, open: true}
	r.pulls = append(r.pulls, githubapp.PullRequestHead{Number: r.nextPR, HeadSHA: r.heads[head], HeadRef: head, Title: title})
	return r.nextPR, fmt.Sprintf("https://github.com/%s/pull/%d", repository, r.nextPR), nil
}

func (h githubAt) EditPullRequestBody(context.Context, string, string, int, string) error { return nil }

func (h githubAt) PullRequestReviews(context.Context, string, string, int) ([]githubapp.PullRequestReview, error) {
	return nil, nil
}

func (h githubAt) CompareFiles(ctx context.Context, token, repository, base, head string) ([]string, bool, error) {
	return h.Compare(ctx, token, repository, base, head)
}

func (h githubAt) CompareCommits(context.Context, string, string, string, string) ([]githubapp.ComparedCommit, int, error) {
	return nil, 0, nil
}

func (h githubAt) MergePullRequest(_ context.Context, token, repository string, number int, headSHA string) (string, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return "", err
	}
	pr := r.trainPRs[number]
	if pr == nil || !pr.open || r.heads[pr.head] != headSHA {
		return "", &githubapp.StatusError{Status: 409, Endpoint: "/merge"}
	}
	landed := commitFor(repository, "landed", headSHA)
	pr.open, pr.merged, pr.landedAs = false, true, landed
	r.commits[landed] = true
	r.heads["main"] = landed
	return landed, nil
}

func (h githubAt) CommentOnPullRequest(context.Context, string, string, int, string) error {
	return nil
}

func (h githubAt) AddPullRequestLabels(context.Context, string, string, int, []string) error {
	return nil
}

func (h githubAt) PullRequestState(_ context.Context, token, repository string, number int) (githubapp.PullRequestObservation, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return githubapp.PullRequestObservation{}, err
	}
	pr := r.trainPRs[number]
	if pr == nil {
		return githubapp.PullRequestObservation{}, &githubapp.StatusError{Status: 404, Endpoint: "/pulls"}
	}
	state := "closed"
	if pr.open {
		state = "open"
	}
	return githubapp.PullRequestObservation{Number: number, State: state, HeadRef: pr.head, Merged: pr.merged, MergeCommitSHA: pr.landedAs}, nil
}

func (h githubAt) ComparePatches(context.Context, string, string, string, string) ([]githubapp.ComparedFile, bool, error) {
	return nil, true, nil
}

func (h githubAt) MarkPullRequestReady(context.Context, string, string, int) error { return nil }

func (h githubAt) OwnerLogin(context.Context, string, string, string) (string, error) {
	return "the-owner", nil
}

// trainOpened is the pull_request delivery of a train's own pull request.
func (s *source) trainOpened(t *testing.T, number int, head, branch string) []byte {
	body := s.envelope()
	body["action"] = "opened"
	body["number"] = number
	body["pull_request"] = map[string]any{
		"number": number,
		"title":  "Train " + branch,
		"head":   map[string]any{"ref": branch, "sha": head, "repo": map[string]any{"full_name": s.name}},
		"base":   map[string]any{"ref": "main", "sha": s.base, "repo": map[string]any{"full_name": s.name}},
	}
	return mustJSON(t, body)
}

func TestATrainFromMainRunsInFullAndLandsAcrossReplicas(t *testing.T) {
	c := newCluster(t, dbtest.DSN())
	if c == nil {
		return
	}
	s := c.connect(t, "train", pipelinerun.DeliveryWebhook)
	departure := time.Now().UTC().Add(-time.Minute).Format("15:04")
	settings := fmt.Sprintf(`{"repositories":{%q:{"enabled":true,"departures":[%q],"timeZone":"UTC","channel":"releases","source":"main","landing":"automatic"}}}`,
		strings.ToLower(s.name), departure)
	for _, r := range []*replica{c.a, c.b} {
		r.integ.Configure(func(d *pipelinerun.Deps) {
			previous := d.SystemVariable
			d.SystemVariable = func(ctx context.Context, name string) (string, error) {
				if name == pipelines.TrainSettingsVariable {
					return settings, nil
				}
				if previous == nil {
					return "", nil
				}
				return previous(ctx, name)
			}
		})
	}
	internal := auth.ContextWithInternalOrigin(context.Background())
	pipelineID := s.pipeline.ID

	// The agent replica's tick builds it, against the real store.
	if res, err := c.b.integ.TrainTick(internal); err != nil || res.Departed != 1 {
		t.Fatalf("the departure: %+v, %v", res, err)
	}
	train, err := c.b.store.LatestTrain(fresh(), pipelineID)
	if err != nil || train == nil {
		t.Fatalf("the train: %+v, %v", train, err)
	}
	if train.State != pipelinerun.TrainRunning || len(train.OnBoard) != 0 || train.BaseSHA != s.base || train.Version != "0.1.1" {
		t.Fatalf("the built train: %+v", train)
	}

	// GitHub tells the bff about its pull request. The run opens there, in full
	// whatever the policy says of a pull request, and the agent drives it.
	c.deliverAndFire(t, "pull_request", "train-opened-"+c.run, s.trainOpened(t, train.PullRequest, train.HeadSHA, train.Branch))
	key := pipelines.RunKey(s.name, train.HeadSHA, pipelines.ModeFull, pipelines.EventPullRequest)
	run := c.awaitConcluded(t, s, c.onlyRun(t, key).ID)
	if run.Mode != pipelines.ModeFull || run.Conclusion != pipelinerun.ConclusionSuccess || run.DriverNodeID != nodeB {
		t.Fatalf("the train's run: mode %s, conclusion %s, driver %s\n%s", run.Mode, run.Conclusion, run.DriverNodeID, c.diagnose(s, run.ID))
	}

	// main moves on under it, as a direct push does, and it lands anyway, on
	// the other replica, reading the row the first one left.
	moved := c.commit(s, "pushed-during-the-run")
	c.github.update(s.name, func(r *fakeRepo) { r.heads["main"] = moved })
	if _, err := c.a.integ.TrainTick(internal); err != nil {
		t.Fatalf("the landing tick: %v", err)
	}
	landed, err := c.a.store.LatestTrain(fresh(), pipelineID)
	if err != nil || landed == nil {
		t.Fatalf("the train: %+v, %v", landed, err)
	}
	if landed.State != pipelinerun.TrainLanded || landed.LandedSHA == "" || landed.ApprovedBy != pipelines.TrainAutomaticApprover || landed.RunID != run.ID {
		t.Fatalf("after its run passed: %+v", landed)
	}
	if landed.Rebuilds != 0 || landed.HeadSHA != train.HeadSHA {
		t.Errorf("it was rebuilt: %+v", landed)
	}
}

// installManifest is testManifest with the stage a deploy verifies with, which nothing
// but a manual run selects.
const installManifest = testManifest + `    - name: deployment
      on: [manual]
      steps:
        - name: verify-rollout
          run: ./verify
`

// hopReleaser publishes at once. It sits behind the seam because the journey itself
// (integrations/release) has its own database tests, and what this file proves is the
// train's half on real rows.
type hopReleaser struct{ version string }

func (r hopReleaser) AdvanceTrainRelease(ctx context.Context, q pipelines.TrainReleaseRequest) (pipelines.TrainReleaseStatus, error) {
	if !auth.OriginFromContext(ctx).IsInternal() || len(q.Authority) == 0 {
		return pipelines.TrainReleaseStatus{}, errors.New("a train's release takes internal origin and the owner's authority")
	}
	return pipelines.TrainReleaseStatus{IntentID: "intent-" + q.Version, Phase: "published", Tag: "v" + q.Version, CandidateID: "sha256:c",
		ApprovalID: "approval-1", ApprovedBy: "policy:standing-release:owner", CatalogDigest: "sha256:d", Published: true}, nil
}

// hopCluster plays the installation's ArgoCD and the pin, shared by both replicas as the
// real ones are. Every sync succeeds at once.
type hopCluster struct {
	mu      sync.Mutex
	syncs   []string
	done    map[string]string
	release pipelines.DeployRelease
	pin     func(pipelines.DeployPinRequest) pipelines.DeployPin
	was     string
}

func (c *hopCluster) NewestRelease(context.Context, string, map[string]any, string, string) (pipelines.DeployRelease, error) {
	return c.release, nil
}

func (c *hopCluster) CheckDeployOwner(context.Context, string, map[string]any) error { return nil }

func (c *hopCluster) ProposeDeployPin(_ context.Context, r pipelines.DeployPinRequest) (pipelines.DeployPin, error) {
	return c.pin(r), nil
}

func (c *hopCluster) ReadApplication(_ context.Context, _, name string) (pipelines.DeployApplicationFacts, error) {
	return pipelines.DeployApplicationFacts{UID: "uid-" + name, Revision: c.was, Phase: "Succeeded", Healthy: true, Settled: true}, nil
}

func (c *hopCluster) StartSync(_ context.Context, r pipelines.DeployRequest) (pipelines.DeployApplicationFacts, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done == nil {
		c.done = map[string]string{}
	}
	if c.done[r.RequestID] == "" {
		c.done[r.RequestID] = r.Revision
		c.syncs = append(c.syncs, r.Name+"@"+r.Revision)
	}
	return pipelines.DeployApplicationFacts{UID: r.UID, Revision: r.Revision, Phase: "Succeeded", Mine: true, Succeeded: true, Healthy: true, Settled: true}, nil
}

func (c *hopCluster) TerminateSync(context.Context, pipelines.DeployRequest) error { return nil }

// A release and its deploy, on two replicas over one Postgres: every row through the
// real store and its DSL, the verification through the real opening, policy and driver.
// verifyPasses says whether the rollout passes, and what follows differs accordingly.
func runDeployAcrossReplicas(t *testing.T, verifyPasses bool) {
	c := newCluster(t, dbtest.DSN())
	if c == nil {
		return
	}
	s := c.connectWith(t, "train", pipelinerun.DeliveryWebhook, "", repositoryWith(testManifest))
	install := c.connectWith(t, "install", pipelinerun.DeliveryWebhook, "", repositoryWith(installManifest))
	departure := time.Now().UTC().Add(-time.Minute).Format("15:04")
	settings := fmt.Sprintf(`{"repositories":{%q:{"enabled":true,"departures":[%q],"timeZone":"UTC","channel":"releases","source":"main","landing":"automatic","releaseProfile":"engine",`+
		`"deployment":{"repository":%q,"applications":["memql","znas-cloud"],"verifyStages":["deployment"],"window":{"start":"00:00","end":"23:59"}}}}}`,
		strings.ToLower(s.name), departure, strings.ToLower(install.name))
	pin := commitFor(c.run, "pin")
	cluster := &hopCluster{was: commitFor(c.run, "ran-before")}
	for _, r := range []*replica{c.a, c.b} {
		r.integ.Configure(func(d *pipelinerun.Deps) {
			previous := d.SystemVariable
			d.SystemVariable = func(ctx context.Context, name string) (string, error) {
				if name == pipelines.TrainSettingsVariable {
					return settings, nil
				}
				if previous == nil {
					return "", nil
				}
				return previous(ctx, name)
			}
			d.TrainReleaser = hopReleaser{}
			d.DeployEffects = cluster
		})
	}
	internal := auth.ContextWithInternalOrigin(context.Background())
	pipelineID := s.pipeline.ID
	c.github.update(install.name, func(r *fakeRepo) {
		r.commitFiles = map[string]map[string][]byte{install.base: {"product.env": []byte("ENGINE_REF=v0.1.0\n")}}
	})

	// Refused by the owner's standing row being absent, then allowed through the real write.
	if err := c.a.store.SetStanding(fresh(), s.pipeline.OwnerUserID, pipelinerun.TrainStanding{
		ID: pipelinerun.StandingIDFor(pipelineID), AccountID: s.pipeline.AccountID, PipelineID: pipelineID, Repository: s.name,
		Release: true, Deploy: true, Authority: map[string]any{"credentialClass": "user", "roleCeiling": "owner"}, SetAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SetStanding: %v", err)
	}
	standing, err := c.b.store.Standing(fresh(), pipelineID)
	if err != nil || standing == nil || !standing.Release || !standing.Deploy || standing.Authority["roleCeiling"] != "owner" {
		t.Fatalf("the standing row, read on the other replica: %+v, %v", standing, err)
	}

	// The train leaves, runs and lands, as in the test above.
	if res, err := c.b.integ.TrainTick(internal); err != nil || res.Departed != 1 {
		t.Fatalf("the departure: %+v, %v", res, err)
	}
	train, err := c.b.store.LatestTrain(fresh(), pipelineID)
	if err != nil || train == nil {
		t.Fatalf("the train: %+v, %v", train, err)
	}
	c.deliverAndFire(t, "pull_request", "train-opened-"+c.run, s.trainOpened(t, train.PullRequest, train.HeadSHA, train.Branch))
	c.awaitConcluded(t, s, c.onlyRun(t, pipelines.RunKey(s.name, train.HeadSHA, pipelines.ModeFull, pipelines.EventPullRequest)).ID)
	if _, err := c.a.integ.TrainTick(internal); err != nil {
		t.Fatalf("the landing tick: %v", err)
	}
	landed, err := c.a.store.LatestTrain(fresh(), pipelineID)
	if err != nil || landed == nil || landed.State != pipelinerun.TrainReleasing || landed.LandedSHA == "" {
		t.Fatalf("after landing: %+v, %v", landed, err)
	}

	// The other replica publishes it, and the row keeps the journey.
	if _, err := c.b.integ.TrainTick(internal); err != nil {
		t.Fatalf("the release tick: %v", err)
	}
	published, err := c.b.store.LatestTrain(fresh(), pipelineID)
	if err != nil || published == nil || published.State != pipelinerun.TrainPublished || published.ReleaseIntentID == "" ||
		published.ReleaseApprovedBy != "policy:standing-release:owner" || published.ReleaseCatalogDigest != "sha256:d" {
		t.Fatalf("after publishing: %+v, %v", published, err)
	}
	last, err := c.a.store.LastLandedTrain(fresh(), pipelineID)
	if err != nil || last == nil || last.ID != published.ID {
		t.Fatalf("the last landed train, through its own query: %+v, %v", last, err)
	}

	// The deploy, a step at a time on either replica.
	cluster.release = pipelines.DeployRelease{Version: published.Version, Tag: "v" + published.Version, SHA: published.HeadSHA, CandidateID: "sha256:c", CatalogDigest: "sha256:d"}
	cluster.pin = func(r pipelines.DeployPinRequest) pipelines.DeployPin {
		c.github.update(install.name, func(repo *fakeRepo) {
			repo.heads["memql/update-1"] = pin
			repo.commits[pin] = true
			if repo.trainPRs == nil {
				repo.trainPRs = map[int]*fakeTrainPR{}
			}
			repo.trainPRs[77] = &fakeTrainPR{head: "memql/update-1", base: "main", open: true}
		})
		return pipelines.DeployPin{ProposalID: "proposal-1", PullRequest: 77, PullRequestURL: "https://github.com/x/pull/77", Commit: pin}
	}
	tick := func(n int) *pipelinerun.Deployment {
		t.Helper()
		replica := c.a
		if n%2 == 1 {
			replica = c.b
		}
		res, err := replica.integ.TrainTick(internal)
		if err != nil {
			t.Fatalf("tick %d: %v", n, err)
		}
		if len(res.Notes) > 0 {
			t.Logf("tick %d: %v", n, res.Notes)
		}
		dep, err := replica.store.LatestDeployment(fresh(), pipelineID)
		if err != nil {
			t.Fatalf("tick %d, the deployment: %v", n, err)
		}
		return dep
	}
	until := func(state string) pipelinerun.Deployment {
		t.Helper()
		for n := range 30 {
			dep := tick(n)
			if dep != nil && dep.State == state {
				return *dep
			}
			if dep != nil && dep.State != state && dep.Finished() {
				t.Fatalf("it finished %s before %s: %s", dep.State, state, dep.Reason)
			}
		}
		t.Fatalf("it never reached %s", state)
		return pipelinerun.Deployment{}
	}
	dep := until(pipelinerun.DeploymentPinning)
	if dep.Guards == nil && len(dep.Guards) != 0 || dep.Trigger != pipelinerun.DeploymentTriggerAutomatic || dep.Authority["roleCeiling"] != "owner" {
		t.Fatalf("the deployment row: %+v", dep)
	}
	for n := range 3 {
		if d := tick(n); d.PinSHA != "" {
			break
		}
	}
	// GitHub tells the bff about the pin's pull request, and its run decides.
	c.deliverAndFire(t, "pull_request", "pin-opened-"+c.run, install.trainOpened(t, 77, pin, "memql/update-1"))
	c.awaitConcluded(t, install, c.onlyRun(t, pipelines.RunKey(install.name, pin, pipelines.ModeAffected, pipelines.EventPullRequest)).ID)
	if verifyPasses {
		c.runner.answerWith(nil)
	} else {
		c.runner.answerWith(func(req pipelines.StepRequest) (pipelines.StepResult, bool) {
			if req.Step.Stage == "deployment" && req.SHA == pin {
				return pipelines.StepResult{Status: pipelines.OutcomeFailed, ExitCode: 1, Failure: &pipelines.Failure{Code: "rollout_unhealthy", Message: "the rollout is not healthy"}}, true
			}
			return pipelines.StepResult{}, false
		})
	}
	dep = until(pipelinerun.DeploymentVerifying)
	if len(dep.Applications) != 2 || dep.Applications[0].Previous != cluster.was || dep.Applications[1].UID != "uid-znas-cloud" {
		t.Fatalf("the applications, through the real row: %+v", dep.Applications)
	}
	if want := []string{"memql@" + pin, "znas-cloud@" + pin}; !slices.Equal(cluster.syncs, want) {
		t.Fatalf("syncs = %v, want %v", cluster.syncs, want)
	}
	for n := range 3 {
		if d := tick(n); d.RunID != "" {
			dep = *d
			break
		}
	}
	run := c.awaitConcluded(t, install, dep.RunID)
	if !slices.Equal(run.OnlyStages, []string{"deployment"}) || run.Event != pipelines.EventManual || run.SHA != pin {
		t.Fatalf("the verification run: %+v", run)
	}
	steps, err := c.b.store.WorkSteps(fresh(), run.WorkRunID)
	if err != nil {
		t.Fatalf("its steps: %v", err)
	}
	if len(steps) != 1 || steps[0].Key != "deployment.verify-rollout" {
		t.Fatalf("a verification ran %+v, want the one stage it names", steps)
	}

	if verifyPasses {
		dep = until(pipelinerun.DeploymentDeployed)
		if dep.LandedSHA == "" {
			t.Fatalf("deployed with no landing: %+v", dep)
		}
		return
	}
	dep = until(pipelinerun.DeploymentVerifyingRollback)
	if want := []string{"znas-cloud@" + cluster.was, "memql@" + cluster.was}; !slices.Equal(cluster.syncs[2:], want) {
		t.Fatalf("syncs back = %v, want %v", cluster.syncs[2:], want)
	}
	for n := range 3 {
		if d := tick(n); d.RunID != "" {
			dep = *d
			break
		}
	}
	back := c.awaitConcluded(t, install, dep.RunID)
	if back.SHA != install.base || back.Conclusion != pipelinerun.ConclusionSuccess {
		t.Fatalf("the verification going back: %s at %s", back.Conclusion, back.SHA)
	}
	dep = until(pipelinerun.DeploymentRolledBack)
	if !dep.PausesAutomatic() || dep.ResumedAt.IsZero() != true {
		t.Fatalf("a rollback did not pause: %+v", dep)
	}
	c.github.update(install.name, func(r *fakeRepo) {
		if pr := r.trainPRs[77]; pr.open || pr.merged {
			t.Errorf("the failed pin: %+v", pr)
		}
	})
}

func TestAReleaseAndItsDeployCrossReplicas(t *testing.T) { runDeployAcrossReplicas(t, true) }

func TestAFailedDeployGoesBackAcrossReplicas(t *testing.T) { runDeployAcrossReplicas(t, false) }

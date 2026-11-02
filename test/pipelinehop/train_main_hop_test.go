package pipelinehop

import (
	"context"
	"fmt"
	"strings"
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

func (h githubAt) CommentOnPullRequest(context.Context, string, string, int, string) error { return nil }

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
	return githubapp.PullRequestObservation{Number: number, State: state, Merged: pr.merged, MergeCommitSHA: pr.landedAs}, nil
}

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

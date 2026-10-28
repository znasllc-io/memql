package pipelinerun

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// baseSHA is main's head in the train tests.
const baseSHA = "3333333333333333333333333333333333333333"

// trainRemote is GitHub for a train: the run driver's fake (embedded) plus
// the train calls, each held to the gate discipline like every other fake
// GitHub call.
type trainRemote struct {
	*fakeGitHub
	mu         sync.Mutex
	open       map[int]*githubapp.TrainPullRequest
	checks     map[string]string
	files      map[string]map[string][]byte
	branchFile map[string]map[string][]byte
	branches   map[string]string
	branchBase map[string]string
	conflicts  map[string]bool
	merged     []string
	trainPRs   map[int]*trainPR
	nextPR     int
	reviews    map[int][]githubapp.PullRequestReview
	prFiles    map[int][]string
	comments   map[int][]string
	login      string
	seq        int
	// checkName is the only check name the fake answers for, as GitHub
	// answers only for the name asked; empty answers every name.
	checkName string
}

type trainPR struct {
	head, base, title, body string
	open, merged            bool
	landedAs                string
}

func newTrainRemote(g *fakeGitHub) *trainRemote {
	return &trainRemote{
		fakeGitHub: g, open: map[int]*githubapp.TrainPullRequest{}, checks: map[string]string{},
		files: map[string]map[string][]byte{}, branchFile: map[string]map[string][]byte{}, branches: map[string]string{},
		branchBase: map[string]string{}, conflicts: map[string]bool{}, trainPRs: map[int]*trainPR{}, nextPR: 100,
		reviews: map[int][]githubapp.PullRequestReview{}, prFiles: map[int][]string{}, comments: map[int][]string{}, login: "ada-gh",
	}
}

func (r *trainRemote) nextSHA(prefix string) string {
	r.seq++
	return fmt.Sprintf("%s%036d", prefix, r.seq)
}

func (r *trainRemote) addPR(n int, head string, check string, opts ...func(*githubapp.TrainPullRequest)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pr := &githubapp.TrainPullRequest{Number: n, Title: fmt.Sprintf("Change %d", n), HeadSHA: head, HeadRef: fmt.Sprintf("claude/change-%d", n),
		HeadRepository: repoName, BaseRef: "main", Author: "znas-io", URL: fmt.Sprintf("https://github.com/acme/shop/pull/%d", n)}
	for _, o := range opts {
		o(pr)
	}
	r.open[n] = pr
	if check != "" {
		r.checks[head] = check
	}
}

func (r *trainRemote) RepositoryWriterToken(ctx context.Context, credential, user, repository string) (string, int64, error) {
	watchGitHubCall(r.t, ctx, "RepositoryWriterToken")
	return "writer-token", 7, nil
}

func (r *trainRemote) TrainPullRequests(ctx context.Context, token, repository, base string) ([]githubapp.TrainPullRequest, error) {
	watchGitHubCall(r.t, ctx, "TrainPullRequests")
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []githubapp.TrainPullRequest
	for _, pr := range r.open {
		if pr.BaseRef == base {
			copy := *pr
			copy.Labels = append([]string(nil), pr.Labels...)
			out = append(out, copy)
		}
	}
	return out, nil
}

func (r *trainRemote) CheckRunConclusion(ctx context.Context, token, repository, sha, name string, appID int64) (string, string, error) {
	watchGitHubCall(r.t, ctx, "CheckRunConclusion")
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.checks[sha]
	if !ok || (r.checkName != "" && name != r.checkName) {
		return "", "", nil
	}
	if c == "" {
		return "in_progress", "", nil
	}
	return "completed", c, nil
}

func (r *trainRemote) CreateBranchRef(ctx context.Context, token, repository, branch, sha string) error {
	watchGitHubCall(r.t, ctx, "CreateBranchRef")
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.branches[branch]; ok {
		return &githubapp.StatusError{Status: http.StatusUnprocessableEntity, Endpoint: "/git/refs"}
	}
	r.branches[branch], r.branchBase[branch] = sha, sha
	return nil
}

func (r *trainRemote) ResetBranchRef(ctx context.Context, token, repository, branch, sha string) error {
	watchGitHubCall(r.t, ctx, "ResetBranchRef")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.branches[branch], r.branchBase[branch] = sha, sha
	delete(r.branchFile, branch)
	return nil
}

func (r *trainRemote) DeleteBranchRef(ctx context.Context, token, repository, branch string) error {
	watchGitHubCall(r.t, ctx, "DeleteBranchRef")
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.branches, branch)
	return nil
}

func (r *trainRemote) MergeIntoBranch(ctx context.Context, token, repository, base, headSHA, message string) (githubapp.MergeOutcome, error) {
	watchGitHubCall(r.t, ctx, "MergeIntoBranch")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conflicts[headSHA] {
		return githubapp.MergeOutcome{Conflict: true}, nil
	}
	sha := r.nextSHA("m")
	r.branches[base] = sha
	r.merged = append(r.merged, base+" <- "+headSHA)
	return githubapp.MergeOutcome{SHA: sha}, nil
}

func (r *trainRemote) FileAtRef(ctx context.Context, token, repository, ref, path string) ([]byte, error) {
	watchGitHubCall(r.t, ctx, "FileAtRef")
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.branchFile[ref][path]; ok {
		return b, nil
	}
	if b, ok := r.files[ref][path]; ok {
		return b, nil
	}
	if base, ok := r.branchBase[ref]; ok {
		if b, ok := r.files[base][path]; ok {
			return b, nil
		}
	}
	return nil, &githubapp.StatusError{Status: http.StatusNotFound, Endpoint: "/contents/" + path}
}

func (r *trainRemote) PutBranchFile(ctx context.Context, token, repository, branch, path, message string, content []byte) (string, error) {
	watchGitHubCall(r.t, ctx, "PutBranchFile")
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.branchFile[branch] == nil {
		r.branchFile[branch] = map[string][]byte{}
	}
	r.branchFile[branch][path] = content
	sha := r.nextSHA("c")
	r.branches[branch] = sha
	return sha, nil
}

func (r *trainRemote) CreateTrainPullRequest(ctx context.Context, token, repository, head, base, title, body string) (int, string, error) {
	watchGitHubCall(r.t, ctx, "CreateTrainPullRequest")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextPR++
	r.trainPRs[r.nextPR] = &trainPR{head: head, base: base, title: title, body: body, open: true}
	return r.nextPR, fmt.Sprintf("https://github.com/acme/shop/pull/%d", r.nextPR), nil
}

func (r *trainRemote) EditPullRequestBody(ctx context.Context, token, repository string, number int, body string) error {
	watchGitHubCall(r.t, ctx, "EditPullRequestBody")
	r.mu.Lock()
	defer r.mu.Unlock()
	if pr := r.trainPRs[number]; pr != nil {
		pr.body = body
	}
	return nil
}

func (r *trainRemote) PullRequestReviews(ctx context.Context, token, repository string, number int) ([]githubapp.PullRequestReview, error) {
	watchGitHubCall(r.t, ctx, "PullRequestReviews")
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]githubapp.PullRequestReview(nil), r.reviews[number]...), nil
}

func (r *trainRemote) PullRequestFiles(ctx context.Context, token, repository string, number int) ([]string, bool, error) {
	watchGitHubCall(r.t, ctx, "PullRequestFiles")
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prFiles[number]...), true, nil
}

func (r *trainRemote) MergePullRequest(ctx context.Context, token, repository string, number int, headSHA string) (string, error) {
	watchGitHubCall(r.t, ctx, "MergePullRequest")
	r.mu.Lock()
	defer r.mu.Unlock()
	pr := r.trainPRs[number]
	if pr == nil || !pr.open {
		return "", &githubapp.StatusError{Status: http.StatusMethodNotAllowed, Endpoint: "/merge"}
	}
	if r.branches[pr.head] != headSHA {
		return "", &githubapp.StatusError{Status: http.StatusConflict, Endpoint: "/merge"}
	}
	pr.open, pr.merged, pr.landedAs = false, true, r.nextSHA("l")
	return pr.landedAs, nil
}

func (r *trainRemote) CommentOnPullRequest(ctx context.Context, token, repository string, number int, body string) error {
	watchGitHubCall(r.t, ctx, "CommentOnPullRequest")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.comments[number] = append(r.comments[number], body)
	return nil
}

func (r *trainRemote) AddPullRequestLabels(ctx context.Context, token, repository string, number int, labels []string) error {
	watchGitHubCall(r.t, ctx, "AddPullRequestLabels")
	r.mu.Lock()
	defer r.mu.Unlock()
	if pr := r.open[number]; pr != nil {
		pr.Labels = append(pr.Labels, labels...)
	}
	return nil
}

func (r *trainRemote) PullRequestOpen(ctx context.Context, token, repository string, number int) (bool, bool, error) {
	watchGitHubCall(r.t, ctx, "PullRequestOpen")
	r.mu.Lock()
	defer r.mu.Unlock()
	pr := r.trainPRs[number]
	if pr == nil {
		return false, false, &githubapp.StatusError{Status: http.StatusNotFound, Endpoint: "/pulls"}
	}
	return pr.open, pr.merged, nil
}

func (r *trainRemote) OwnerLogin(ctx context.Context, credential, user, repository string) (string, error) {
	watchGitHubCall(r.t, ctx, "OwnerLogin")
	return r.login, nil
}

// trainHarness is a pipeline with a Discord channel, a release train set to
// depart at 09:00 and 15:00 UTC, and a clock the test moves.
type trainHarness struct {
	*harness
	remote   *trainRemote
	clock    time.Time
	settings string
	p        Pipeline
}

const trainSettingsJSON = `{"repositories":{"acme/shop":{"enabled":true,"departures":["09:00","15:00"],"timeZone":"UTC","channel":"releases","ownerDiscordUserId":"123456789012345678"}}}`

func newTrainHarness(t *testing.T) *trainHarness {
	t.Helper()
	h := newHarness(t)
	th := &trainHarness{harness: h, remote: newTrainRemote(h.github), clock: time.Date(2026, 10, 28, 9, 0, 30, 0, time.UTC), settings: trainSettingsJSON}
	h.store.work = newFakeWork()
	th.p = testPipeline(DeliveryWebhook)
	th.p.ChannelIDs = []string{"ch1"}
	th.remote.checkName = pipelines.CheckRunName(th.p.Name)
	h.store.addPipeline(th.p)
	h.store.mu.Lock()
	h.store.putChannel(Channel{ID: "ch1", OwnerUserID: th.p.OwnerUserID, Name: "releases", Kind: channelDiscord, SecretRef: "RELEASES_WEBHOOK", Status: channelActive})
	h.store.mu.Unlock()
	h.github.mu.Lock()
	h.github.heads[repoName+"@main"] = headAnswer{SHA: baseSHA}
	h.github.mu.Unlock()
	th.remote.files[baseSHA] = map[string][]byte{"VERSION": []byte("0.31.4\n"), "go.mod": []byte("module github.com/acme/shop\n\ngo 1.26\n")}
	h.integ = New(Deps{
		Store:    h.store,
		GitHub:   th.remote,
		Gate:     h.gate.run,
		OSOrigin: func() string { return testOSOrigin },
		NodeID:   "agent-a",
		Now:      func() time.Time { return th.clock },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		SystemVariable: func(_ context.Context, name string) (string, error) {
			if name == pipelines.TrainSettingsVariable {
				return th.settings, nil
			}
			return "", nil
		},
	})
	return th
}

func (th *trainHarness) tick(t *testing.T) TrainTickResult {
	t.Helper()
	res, err := th.integ.TrainTick(automationCtx())
	if err != nil {
		t.Fatalf("TrainTick: %v", err)
	}
	return res
}

func (th *trainHarness) train(t *testing.T) Train {
	t.Helper()
	tr, err := th.store.LatestTrain(context.Background(), th.p.ID)
	if err != nil || tr == nil {
		t.Fatalf("no train: %v", err)
	}
	return *tr
}

func (th *trainHarness) messages() []string {
	th.store.mu.Lock()
	defer th.store.mu.Unlock()
	var out []string
	for _, n := range th.store.staged {
		if strings.HasPrefix(n.RequestedBy, trainRequestedByPrefix) {
			out = append(out, n.Body)
		}
	}
	return out
}

// runFor puts the train pull request's run at sha, concluded, with failed
// steps when the conclusion is a failure.
func (th *trainHarness) runFor(t *testing.T, tr Train, conclusion string, failed ...map[string]any) Run {
	t.Helper()
	r := Run{ID: "run-" + tr.HeadSHA[:8], OwnerUserID: th.p.OwnerUserID, PipelineID: th.p.ID, Repository: repoName, SHA: tr.HeadSHA,
		Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest, RunKey: "k-" + tr.HeadSHA, Attempt: 1, Trigger: TriggerWebhook,
		PullRequest: tr.PullRequest, HeadBranch: tr.Branch, Status: StatusCompleted, Conclusion: conclusion, WorkRunID: "work-" + tr.HeadSHA[:8],
		QueuedAt: th.clock}
	th.store.addRun(r)
	for i, step := range failed {
		step["runId"] = r.WorkRunID
		th.store.work.mu.Lock()
		th.store.work.steps[fmt.Sprintf("%s-%d", r.WorkRunID, i)] = step
		th.store.work.mu.Unlock()
	}
	return r
}

func TestTrainDepartsAtTheScheduledTime(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.remote.addPR(9, shaB, "success", func(pr *githubapp.TrainPullRequest) { pr.Draft = true })

	th.tick(t)
	tr := th.train(t)
	if tr.State != TrainRunning || tr.Departure != TrainDepartureScheduled || tr.Branch != "train/2026-10-28-1" {
		t.Fatalf("train = %+v", tr)
	}
	if len(tr.OnBoard) != 1 || tr.OnBoard[0].Number != 7 || tr.OnBoard[0].SHA != shaA {
		t.Errorf("on board %+v, want #7 at its head", tr.OnBoard)
	}
	if len(tr.LeftOff) != 1 || tr.LeftOff[0].Number != 9 || !strings.Contains(tr.LeftOff[0].Reason, "draft") {
		t.Errorf("left off %+v, want #9 as a draft", tr.LeftOff)
	}
	if tr.BaseSHA != baseSHA || tr.Version != "0.31.5" || tr.PullRequest == 0 || tr.HeadSHA == "" {
		t.Errorf("base %s version %s pr %d head %s", tr.BaseSHA, tr.Version, tr.PullRequest, tr.HeadSHA)
	}
	if got := string(th.remote.branchFile[tr.Branch]["VERSION"]); got != "0.31.5\n" {
		t.Errorf("VERSION on the train = %q, want the patch bump", got)
	}
	pr := th.remote.trainPRs[tr.PullRequest]
	if pr == nil || pr.head != tr.Branch || pr.base != "main" || !strings.Contains(pr.body, "#7") {
		t.Errorf("train pull request %+v", pr)
	}
	if msgs := th.messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "left with 1 pull request") {
		t.Errorf("messages %v, want one departure", msgs)
	}
}

func TestNoDepartureWithoutReadyPullRequests(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(9, shaB, "")
	th.tick(t)
	tr := th.train(t)
	if tr.State != TrainCancelled || !strings.Contains(tr.HeldReason, "No pull request was ready") || tr.PullRequest != 0 {
		t.Errorf("an empty departure: %+v", tr)
	}
	if len(th.remote.trainPRs) != 0 || len(th.messages()) != 0 {
		t.Errorf("an empty departure opened %d pull requests and sent %d messages", len(th.remote.trainPRs), len(th.messages()))
	}
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if again := th.train(t); again.ID != tr.ID {
		t.Errorf("a second tick in the same departure built another train")
	}
}

func TestConflictingPullRequestIsLeftOffWithAComment(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.remote.addPR(8, shaB, "success")
	th.remote.conflicts[shaA] = true
	th.tick(t)
	tr := th.train(t)
	if len(tr.OnBoard) != 1 || tr.OnBoard[0].Number != 8 {
		t.Errorf("on board %+v, want only #8", tr.OnBoard)
	}
	if len(tr.LeftOff) != 1 || tr.LeftOff[0].Number != 7 || !strings.Contains(tr.LeftOff[0].Reason, "conflict") {
		t.Errorf("left off %+v, want #7 for its conflict", tr.LeftOff)
	}
	if c := th.remote.comments[7]; len(c) != 1 || !strings.Contains(c[0], tr.Branch) {
		t.Errorf("comments on #7: %v", c)
	}
}

func TestTrainMergesTheBoardedCommitNotTheNewHead(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.tick(t)
	tr := th.train(t)
	// A session pushes to #7 while the train runs.
	th.remote.mu.Lock()
	th.remote.open[7].HeadSHA = shaB
	th.remote.mu.Unlock()
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	again := th.train(t)
	if again.HeadSHA != tr.HeadSHA || again.OnBoard[0].SHA != shaA || again.State != TrainRunning {
		t.Errorf("a new push to a boarded pull request changed the running train: %+v", again)
	}
	if len(th.remote.merged) != 1 || th.remote.merged[0] != tr.Branch+" <- "+shaA {
		t.Errorf("merged %v, want only the boarded commit", th.remote.merged)
	}
}

func TestTwoTicksBuildOneTrain(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = th.integ.TrainTick(automationCtx())
		}()
	}
	wg.Wait()
	if n := len(th.remote.trainPRs); n != 1 {
		t.Errorf("two ticks opened %d train pull requests, want 1", n)
	}
	th.store.mu.Lock()
	creates := len(th.store.trainCreates)
	th.store.mu.Unlock()
	if creates != 1 {
		t.Errorf("two ticks created %d train rows, want 1", creates)
	}
}

func TestFailedTrainRetriesATimeoutOnce(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.tick(t)
	tr := th.train(t)
	original := th.runFor(t, tr, ConclusionFailure, map[string]any{"key": "tests.go-unit#3", "status": WorkStepFailed, "errorCode": pipelines.CodeStepTimeout,
		"call": map[string]any{"packages": []any{"github.com/acme/shop/component/memql"}}})
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	got := th.train(t)
	if !got.Retried || got.State != TrainRunning {
		t.Fatalf("after a timeout: %+v", got)
	}
	var rerun *Run
	th.store.mu.Lock()
	for _, r := range th.store.runs {
		if r.RerunOf == original.ID {
			copy := r
			rerun = &copy
		}
	}
	th.store.mu.Unlock()
	if rerun == nil || !rerun.RerunFailedOnly {
		t.Errorf("no failed-only re-run of %s: %+v", original.ID, rerun)
	}
}

func TestFailedTrainDropsTheSingleSuspect(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.remote.addPR(8, shaB, "success")
	th.remote.prFiles[7] = []string{"component/memql/x.go"}
	th.remote.prFiles[8] = []string{"docs/a.md"}
	th.tick(t)
	tr := th.train(t)
	th.runFor(t, tr, ConclusionFailure, map[string]any{"key": "tests.go-unit#2", "status": WorkStepFailed,
		"call": map[string]any{"packages": []any{"github.com/acme/shop/component/memql"}}})
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	got := th.train(t)
	if len(got.Dropped) != 1 || got.Dropped[0].Number != 7 || got.Rebuilds != 1 {
		t.Fatalf("dropped %+v rebuilds %d, want #7 and one rebuild", got.Dropped, got.Rebuilds)
	}
	if got.State != TrainRunning || len(got.OnBoard) != 1 || got.OnBoard[0].Number != 8 || got.HeadSHA == tr.HeadSHA {
		t.Errorf("the rebuilt train: %+v", got)
	}
	if c := th.remote.comments[7]; len(c) == 0 || !strings.Contains(c[len(c)-1], "tests.go-unit#2") {
		t.Errorf("comments on #7: %v", c)
	}
	th.remote.mu.Lock()
	labels := th.remote.open[7].Labels
	th.remote.mu.Unlock()
	if !strings.Contains(strings.Join(labels, ","), pipelines.TrainDroppedLabel) {
		t.Errorf("#7 labels %v, want %s", labels, pipelines.TrainDroppedLabel)
	}
	var dropped bool
	for _, m := range th.messages() {
		dropped = dropped || strings.Contains(m, "Dropped #7")
	}
	if !dropped {
		t.Errorf("no Dropped message in %v", th.messages())
	}
}

func TestMainFailingTheSameStepHoldsTheTrain(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.tick(t)
	tr := th.train(t)
	th.runFor(t, tr, ConclusionFailure, map[string]any{"key": "vulnerabilities.go-vulnerabilities", "status": WorkStepFailed})
	push := Run{ID: "push-1", OwnerUserID: th.p.OwnerUserID, PipelineID: th.p.ID, Repository: repoName, SHA: baseSHA, Mode: pipelines.ModeFull,
		Event: pipelines.EventPush, RunKey: "push-k", Attempt: 1, Trigger: TriggerWebhook, Status: StatusCompleted, Conclusion: ConclusionFailure,
		WorkRunID: "work-push", QueuedAt: th.clock.Add(-time.Hour)}
	th.store.addRun(push)
	th.store.work.mu.Lock()
	th.store.work.steps["work-push-0"] = map[string]any{"runId": "work-push", "key": "vulnerabilities.go-vulnerabilities", "status": WorkStepFailed}
	th.store.work.mu.Unlock()
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	got := th.train(t)
	if got.State != TrainHeld || !strings.Contains(got.HeldReason, "main fails vulnerabilities.go-vulnerabilities too") {
		t.Fatalf("train = %+v", got)
	}
	var held bool
	for _, m := range th.messages() {
		held = held || (strings.Contains(m, "is held") && strings.Contains(m, "<@123456789012345678>"))
	}
	if !held {
		t.Errorf("no ACTION NEEDED held message mentioning the owner in %v", th.messages())
	}
}

func TestClosingAHeldTrainCancelsIt(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.tick(t)
	tr := th.train(t)
	th.runFor(t, tr, ConclusionFailure, map[string]any{"key": "checks.lint", "status": WorkStepFailed})
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if got := th.train(t); got.State != TrainHeld {
		t.Fatalf("a failure nobody can be blamed for holds the train: %+v", got)
	}
	th.remote.mu.Lock()
	th.remote.trainPRs[tr.PullRequest].open = false
	th.remote.mu.Unlock()
	th.clock = th.clock.Add(time.Minute)
	th.tick(t)
	if got := th.train(t); got.State != TrainCancelled {
		t.Errorf("closing the train pull request left the train %q", got.State)
	}
}

func TestTrainMessagesAreSentOnce(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.tick(t)
	for range 3 {
		th.clock = th.clock.Add(time.Minute)
		th.tick(t)
	}
	if n := len(th.messages()); n != 1 {
		t.Errorf("%d messages after four ticks of one running train, want the one departure", n)
	}
}

func TestTickRefusesAClientCaller(t *testing.T) {
	th := newTrainHarness(t)
	if _, err := th.integ.TrainTick(personCtx(ownerID)); err == nil {
		t.Fatal("a person's call ran the train tick")
	}
}

// TestBoardingReadsThePipelinesOwnCheck: a pull request boards on its
// pipeline's own check, MemQL / <pipeline name> -- the harness pipeline is
// "shop" -- and not on a check name written into the train.
func TestBoardingReadsThePipelinesOwnCheck(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.tick(t)
	if tr := th.train(t); len(tr.OnBoard) != 1 || tr.OnBoard[0].Number != 7 {
		t.Fatalf("a pull request whose MemQL / shop check passed did not board: state %s, left off %+v", tr.State, tr.LeftOff)
	}
}

// TestAForkNeverBoards: a fork's pull request is refused a pipeline run of its
// own, so a train must never carry it into one. A check of the right name on
// a fork's head can come from anything that writes checks -- another App, or
// a workflow the fork itself adds -- and boarding it would run the fork's code
// in this repository's train run, with the pipeline's secrets.
func TestAForkNeverBoards(t *testing.T) {
	th := newTrainHarness(t)
	th.remote.addPR(7, shaA, "success")
	th.remote.addPR(9, "4444444444444444444444444444444444444444", "success", func(pr *githubapp.TrainPullRequest) {
		pr.HeadRepository = "mallory/shop"
	})
	th.remote.addPR(11, "5555555555555555555555555555555555555555", "success", func(pr *githubapp.TrainPullRequest) {
		pr.HeadRepository = "" // a deleted fork
	})
	th.tick(t)
	tr := th.train(t)
	if len(tr.OnBoard) != 1 || tr.OnBoard[0].Number != 7 {
		t.Fatalf("on board %+v, want only #7", tr.OnBoard)
	}
	reasons := map[int]string{}
	for _, sk := range tr.LeftOff {
		reasons[sk.Number] = sk.Reason
	}
	for _, n := range []int{9, 11} {
		if !strings.Contains(reasons[n], "fork") {
			t.Errorf("#%d left off for %q, want a reason naming the fork", n, reasons[n])
		}
	}
}

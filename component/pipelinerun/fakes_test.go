package pipelinerun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// fakes_test.go -- the fakes BOTH halves of this package test against (the
// opening half here, the driver in Task 10b): an in-memory Store that keeps
// the DSL's read-merge and @createOnly rules, a GitHub that records every
// call, and a gate that really serializes per key.

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// memStore is the Store over maps. Its person-facing reads apply the same
// owner rule the DSL filters do, reading the caller from the context; its
// server-only reads see everything; its writes keep updatePipeline's and
// updatePipelineRun's read-merge and createPipelineRun's @createOnly fields.
type memStore struct {
	mu         sync.Mutex
	packages   map[string]PackageSource
	pipelines  map[string]Pipeline
	runs       map[string]Run
	deliveries map[string]InboundDelivery

	// Writes, in order, for assertions.
	pipelineCreates []Pipeline
	pipelineUpdates []pipelineUpdate
	runCreates      []Run
	runUpdates      []runUpdate

	// Injected failures.
	failCreateRun error
	failUpdateRun error
	// failRunsForPullRequest is every RunsUnfinishedForPullRequest call's
	// answer when set.
	failRunsForPullRequest error

	// afterCreatePipeline runs after each CreatePipeline lands, outside the
	// store's lock: what another writer does right after a connect.
	afterCreatePipeline func()

	// work is the work spine the journal writes through, whose step rows
	// WorkSteps reads back (nil: no work rows at all).
	work *fakeWork
}

type pipelineUpdate struct {
	Owner, ID string
	Patch     PipelinePatch
}

type runUpdate struct {
	Owner, ID string
	Patch     RunPatch
}

func newMemStore() *memStore {
	return &memStore{
		packages: map[string]PackageSource{}, pipelines: map[string]Pipeline{}, runs: map[string]Run{},
		deliveries: map[string]InboundDelivery{},
	}
}

// stageDelivery puts a row on the inbound seam, as the receiver stages one.
func (s *memStore) stageDelivery(d InboundDelivery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries[bareID(d.ID)] = d
}

// InboundDelivery is inboundRequestById: any staged row, whoever's.
func (s *memStore) InboundDelivery(_ context.Context, requestID string) (*InboundDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deliveries[bareID(requestID)]
	if !ok {
		return nil, nil
	}
	return &d, nil
}

func (s *memStore) addPackage(p PackageSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.packages[bareID(p.ID)] = p
}

func (s *memStore) addPipeline(p Pipeline) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pipelines[bareID(p.ID)] = p
}

func (s *memStore) addRun(r Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[bareID(r.ID)] = r
}

func (s *memStore) pipeline(id string) (Pipeline, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pipelines[bareID(id)]
	return p, ok
}

func (s *memStore) run(id string) (Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[bareID(id)]
	return r, ok
}

// allRuns is every run, oldest attempt first.
func (s *memStore) allRuns() []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Collect(maps.Values(s.runs))
	sort.Slice(out, func(i, j int) bool {
		if out[i].RunKey != out[j].RunKey {
			return out[i].RunKey < out[j].RunKey
		}
		return out[i].Attempt < out[j].Attempt
	})
	return out
}

// callerOf is the person the context carries, and whether they are a
// cluster owner -- what the DSL's owner conjunct and the composite tier read.
func callerOf(ctx context.Context) (string, bool) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return "", false
	}
	return ac.UserId, ac.IsClusterOwner()
}

func (s *memStore) PackageForCaller(ctx context.Context, packageID string) (*PackageSource, error) {
	who, clusterOwner := callerOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.packages[bareID(packageID)]
	// packageById reads under the composite tier: the owner, or a cluster
	// owner.
	if !ok || (!sameID(p.OwnerUserID, who) && !clusterOwner) {
		return nil, nil
	}
	return &p, nil
}

func (s *memStore) PipelineForOwner(ctx context.Context, pipelineID string) (*Pipeline, error) {
	who, _ := callerOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pipelines[bareID(pipelineID)]
	if !ok || !sameID(p.OwnerUserID, who) {
		return nil, nil
	}
	return &p, nil
}

func (s *memStore) PipelineForPackage(ctx context.Context, packageID string) (*Pipeline, error) {
	who, _ := callerOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pipelines {
		if sameID(p.PackageID, packageID) && sameID(p.OwnerUserID, who) {
			return &p, nil
		}
	}
	return nil, nil
}

func (s *memStore) PipelinesForOwner(ctx context.Context) ([]Pipeline, error) {
	who, _ := callerOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Pipeline
	for _, p := range s.pipelines {
		if sameID(p.OwnerUserID, who) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *memStore) RunForOwner(ctx context.Context, runID string) (*Run, error) {
	who, _ := callerOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[bareID(runID)]
	if !ok || !sameID(r.OwnerUserID, who) {
		return nil, nil
	}
	return &r, nil
}

func (s *memStore) RunsForOwner(ctx context.Context, pipelineID string) ([]Run, error) {
	who, _ := callerOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Run
	for _, r := range s.runs {
		if sameID(r.OwnerUserID, who) && (pipelineID == "" || sameID(r.PipelineID, pipelineID)) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *memStore) PipelinesForRepository(_ context.Context, repository string) ([]Pipeline, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Pipeline
	for _, p := range s.pipelines {
		if p.Repository == normalizeRepository(repository) && p.Active() {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectedAt.Before(out[j].ConnectedAt) })
	return out, nil
}

func (s *memStore) PipelinesPolled(context.Context) ([]Pipeline, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Pipeline
	for _, p := range s.pipelines {
		if p.Delivery == DeliveryPoll && p.Active() {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *memStore) PipelineByID(_ context.Context, pipelineID string) (*Pipeline, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pipelines[bareID(pipelineID)]
	if !ok {
		return nil, nil
	}
	return &p, nil
}

func (s *memStore) RunsForKey(_ context.Context, runKey string) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Run
	for _, r := range s.runs {
		if r.RunKey == runKey {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *memStore) RunsForPipelineSHA(_ context.Context, pipelineID, sha string) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Run
	for _, r := range s.runs {
		if sameID(r.PipelineID, pipelineID) && r.SHA == strings.ToLower(sha) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QueuedAt.After(out[j].QueuedAt) })
	return out, nil
}

// RunsUnfinishedForPullRequest is pipelineRunsUnfinishedForPullRequest: one
// pipeline's runs of one pull request that are queued or in progress, in
// EVERY mode and in no order. The mode is the caller's rule, not the read's,
// so a full run carrying the pull request comes back here as it does from the
// DSL, and only the caller's rule keeps it running.
func (s *memStore) RunsUnfinishedForPullRequest(_ context.Context, pipelineID string, pullRequest int) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failRunsForPullRequest != nil {
		return nil, s.failRunsForPullRequest
	}
	var out []Run
	for _, r := range s.runs {
		if pullRequest > 0 && sameID(r.PipelineID, pipelineID) && r.PullRequest == pullRequest &&
			(r.Status == StatusQueued || r.Status == StatusInProgress) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *memStore) RunByCheckRun(_ context.Context, repository string, checkRunID int64) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.Repository == normalizeRepository(repository) && r.CheckRunID == checkRunID && checkRunID > 0 {
			return &r, nil
		}
	}
	return nil, nil
}

func (s *memStore) RunsUnfinished(context.Context) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Run
	for _, r := range s.runs {
		if !r.Finished() {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].QueuedAt.Before(out[j].QueuedAt) })
	return out, nil
}

// RunsFinalCheckRunUnavailable is pipelineRunsFinalCheckRunUnavailable:
// completed, the final report not landed, finished at or after since; newest
// first.
func (s *memStore) RunsFinalCheckRunUnavailable(_ context.Context, since time.Time) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Run
	for _, r := range s.runs {
		if r.Status == StatusCompleted && r.CheckRunState == CheckRunUnavailable && !r.FinishedAt.Before(since) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FinishedAt.After(out[j].FinishedAt) })
	return out, nil
}

func (s *memStore) RunByID(_ context.Context, runID string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[bareID(runID)]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

// PipelinesActive is pipelinesActive: one active pipeline, whoever's.
func (s *memStore) PipelinesActive(context.Context) ([]Pipeline, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pipelines {
		if p.Active() {
			return []Pipeline{p}, nil
		}
	}
	return nil, nil
}

// WorkSteps is workStepsForRun over the rows the journal wrote through the
// fake work spine, read back through the production row reader.
func (s *memStore) WorkSteps(_ context.Context, workRunID string) ([]WorkStep, error) {
	if s.work == nil {
		return nil, nil
	}
	var out []WorkStep
	for _, row := range s.work.stepRows(bareID(workRunID)) {
		out = append(out, workStepFromRow(row))
	}
	return out, nil
}

// CreatePipeline is createPipeline: a read-merge insert that restates the
// configuration, re-activates the row and moves connectedAt, keeping heads
// and timings.
func (s *memStore) CreatePipeline(_ context.Context, p Pipeline) error {
	if strings.TrimSpace(p.OwnerUserID) == "" {
		return errors.New("memStore: a pipeline is written only under its owner")
	}
	s.mu.Lock()
	hook := s.afterCreatePipeline
	defer func() {
		if hook != nil {
			hook()
		}
	}()
	defer s.mu.Unlock()
	s.pipelineCreates = append(s.pipelineCreates, p)
	id := bareID(p.ID)
	if prev, ok := s.pipelines[id]; ok {
		p.Heads, p.Timings, p.TimingsRunID, p.TimingsUpdatedAt = prev.Heads, prev.Timings, prev.TimingsRunID, prev.TimingsUpdatedAt
		if len(p.ChannelIDs) == 0 {
			p.ChannelIDs = prev.ChannelIDs
		}
	}
	p.ID = id
	p.Status = PipelineActive
	if p.Compute == "" {
		p.Compute = pipelines.ComputeCluster
	}
	p.ConnectedAt = time.Now().UTC()
	s.pipelines[id] = p
	return nil
}

func (s *memStore) UpdatePipeline(_ context.Context, owner, pipelineID string, patch PipelinePatch) error {
	if strings.TrimSpace(owner) == "" {
		return errors.New("memStore: a pipeline is written only under its owner")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pipelineUpdates = append(s.pipelineUpdates, pipelineUpdate{Owner: owner, ID: pipelineID, Patch: patch})
	id := bareID(pipelineID)
	p, ok := s.pipelines[id]
	if !ok {
		return fmt.Errorf("memStore: no pipeline %q", pipelineID)
	}
	if !sameID(p.OwnerUserID, owner) {
		return fmt.Errorf("memStore: %q may not write %q's pipeline", owner, p.OwnerUserID)
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&p.Name, patch.Name)
	set(&p.DefaultBranch, patch.DefaultBranch)
	set(&p.Delivery, patch.Delivery)
	set(&p.Status, patch.Status)
	set(&p.TimingsRunID, patch.TimingsRunID)
	if patch.Compute != nil {
		p.Compute = *patch.Compute
	}
	if patch.SecretNames != nil {
		p.SecretNames = slices.Clone(*patch.SecretNames)
	}
	if patch.ChannelIDs != nil {
		p.ChannelIDs = slices.Clone(*patch.ChannelIDs)
	}
	if patch.Heads != nil {
		p.Heads = maps.Clone(*patch.Heads)
	}
	if patch.Timings != nil {
		p.Timings = maps.Clone(*patch.Timings)
	}
	if patch.TimingsUpdatedAt != nil {
		p.TimingsUpdatedAt = *patch.TimingsUpdatedAt
	}
	s.pipelines[id] = p
	return nil
}

// CreateRun is createPipelineRun, @createOnly fields included: a second
// create at an existing id cannot move status, conclusion, checkRunState or
// cancelRequested.
func (s *memStore) CreateRun(_ context.Context, r Run) error {
	if strings.TrimSpace(r.OwnerUserID) == "" {
		return errors.New("memStore: a run is written only under its owner")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failCreateRun != nil {
		return s.failCreateRun
	}
	s.runCreates = append(s.runCreates, r)
	id := bareID(r.ID)
	r.ID = id
	if prev, ok := s.runs[id]; ok {
		r.Status, r.Conclusion, r.CheckRunState, r.CancelRequested = prev.Status, prev.Conclusion, prev.CheckRunState, prev.CancelRequested
	}
	s.runs[id] = r
	return nil
}

func (s *memStore) UpdateRun(_ context.Context, owner, runID string, patch RunPatch) error {
	if strings.TrimSpace(owner) == "" {
		return errors.New("memStore: a run is written only under its owner")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failUpdateRun != nil {
		return s.failUpdateRun
	}
	s.runUpdates = append(s.runUpdates, runUpdate{Owner: owner, ID: runID, Patch: patch})
	id := bareID(runID)
	r, ok := s.runs[id]
	if !ok {
		return fmt.Errorf("memStore: no run %q", runID)
	}
	if !sameID(r.OwnerUserID, owner) {
		return fmt.Errorf("memStore: %q may not write %q's run", owner, r.OwnerUserID)
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	at := func(dst *time.Time, v *time.Time) {
		if v != nil {
			*dst = *v
		}
	}
	set(&r.Status, patch.Status)
	set(&r.Conclusion, patch.Conclusion)
	set(&r.RefusalCode, patch.RefusalCode)
	set(&r.RefusalMessage, patch.RefusalMessage)
	set(&r.RefusalScope, patch.RefusalScope)
	set(&r.CheckRunState, patch.CheckRunState)
	set(&r.WorkRunID, patch.WorkRunID)
	set(&r.WorkGoalID, patch.WorkGoalID)
	set(&r.DriverNodeID, patch.DriverNodeID)
	set(&r.CancelledBy, patch.CancelledBy)
	at(&r.DriverHeartbeatAt, patch.DriverHeartbeatAt)
	at(&r.StartedAt, patch.StartedAt)
	at(&r.FinishedAt, patch.FinishedAt)
	if patch.CheckRunID != nil {
		r.CheckRunID = *patch.CheckRunID
	}
	if patch.Notes != nil {
		r.Notes = slices.Clone(*patch.Notes)
	}
	if patch.CancelRequested != nil {
		r.CancelRequested = *patch.CancelRequested
	}
	if patch.Stages != nil {
		r.Stages = slices.Clone(*patch.Stages)
	}
	if patch.DurationMs != nil {
		r.DurationMs = *patch.DurationMs
	}
	s.runs[id] = r
	return nil
}

// ---------------------------------------------------------------------------
// GitHub
// ---------------------------------------------------------------------------

// fakeGitHub answers every GitHub call from maps and records the writes.
// Repositories are keyed "owner/name"; heads "owner/name@branch"; commits
// "owner/name@ref"; trees "owner/name@sha".
//
// It keeps the GATE DISCIPLINE (watchGitHubCall): every call fails the test
// when it is made under a held gate, except open()'s check-run create.
type fakeGitHub struct {
	mu sync.Mutex
	t  testing.TB

	unconfigured bool
	// installationID is what a token mint answers; 0 means 7.
	installationID int64
	tokenErr       error
	tokenMints     []tokenMint

	createErr error
	updateErr error
	created   []checkWrite
	updated   []checkWrite
	nextID    int64

	repos   map[string]githubapp.RepositoryInfo
	heads   map[string]headAnswer
	pulls   map[string][]githubapp.PullRequestHead
	commits map[string]headAnswer
	trees   map[string]fstest.MapFS
	compare map[string]compareAnswer

	// pullsErr is every OpenPullRequests call's answer when set.
	pullsErr error
	// pullHeadErr is every PullRequestHead call's answer when set;
	// pullHeadCalls is each one asked, "owner/name#<number>", in order.
	pullHeadErr   error
	pullHeadCalls []string

	// treeKeeps records each Tree call's keep decision for the files it
	// held, so a test can assert what was asked for.
	treeCalls []string
	// treeErr is every Tree call's answer when set; treeKept is every path
	// keep admitted, across calls.
	treeErr  error
	treeKept []string

	// onBranchHead runs inside each BranchHead call: what another writer
	// does while GitHub is being asked.
	onBranchHead func()

	// installations is what Installations answers, or installationsErr.
	installations    []githubapp.AppInstallation
	installationsErr error
}

type tokenMint struct{ CredentialID, Owner, Repository string }

type checkWrite struct {
	Token      string
	Repository string
	ID         int64
	Run        githubapp.CheckRun
}

type headAnswer struct{ SHA, Message string }

type compareAnswer struct {
	Files    []string
	Complete bool
}

func newFakeGitHub(t testing.TB) *fakeGitHub {
	return &fakeGitHub{
		t:       t,
		nextID:  1000,
		repos:   map[string]githubapp.RepositoryInfo{},
		heads:   map[string]headAnswer{},
		pulls:   map[string][]githubapp.PullRequestHead{},
		commits: map[string]headAnswer{},
		trees:   map[string]fstest.MapFS{},
		compare: map[string]compareAnswer{},
	}
}

func (g *fakeGitHub) Configured() bool { return !g.unconfigured }

func (g *fakeGitHub) InstallationToken(ctx context.Context, credentialID, ownerUserID, repository string) (string, int64, error) {
	watchGitHubCall(g.t, ctx, "InstallationToken")
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tokenMints = append(g.tokenMints, tokenMint{CredentialID: credentialID, Owner: ownerUserID, Repository: repository})
	if g.unconfigured {
		return "", 0, githubapp.ErrNotConfigured
	}
	if g.tokenErr != nil {
		return "", 0, g.tokenErr
	}
	inst := g.installationID
	if inst == 0 {
		inst = 7
	}
	return "ghs_" + credentialID, inst, nil
}

func (g *fakeGitHub) CreateCheckRun(ctx context.Context, token, repository string, run githubapp.CheckRun) (int64, error) {
	watchGitHubCall(g.t, ctx, "CreateCheckRun")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.createErr != nil {
		return 0, g.createErr
	}
	g.nextID++
	g.created = append(g.created, checkWrite{Token: token, Repository: repository, ID: g.nextID, Run: run})
	return g.nextID, nil
}

func (g *fakeGitHub) UpdateCheckRun(ctx context.Context, token, repository string, id int64, run githubapp.CheckRun) error {
	watchGitHubCall(g.t, ctx, "UpdateCheckRun")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.updateErr != nil {
		return g.updateErr
	}
	g.updated = append(g.updated, checkWrite{Token: token, Repository: repository, ID: id, Run: run})
	return nil
}

func (g *fakeGitHub) Repository(ctx context.Context, _, repository string) (githubapp.RepositoryInfo, error) {
	watchGitHubCall(g.t, ctx, "Repository")
	g.mu.Lock()
	defer g.mu.Unlock()
	info, ok := g.repos[repository]
	if !ok {
		return githubapp.RepositoryInfo{}, &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository}
	}
	return info, nil
}

func (g *fakeGitHub) BranchHead(ctx context.Context, _, repository, branch string) (string, string, error) {
	watchGitHubCall(g.t, ctx, "BranchHead")
	g.mu.Lock()
	h, ok := g.heads[repository+"@"+branch]
	hook := g.onBranchHead
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	if !ok {
		return "", "", &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository + "/branches/" + branch}
	}
	return h.SHA, h.Message, nil
}

func (g *fakeGitHub) OpenPullRequests(ctx context.Context, _, repository string) ([]githubapp.PullRequestHead, error) {
	watchGitHubCall(g.t, ctx, "OpenPullRequests")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pullsErr != nil {
		return nil, g.pullsErr
	}
	return slices.Clone(g.pulls[repository]), nil
}

// PullRequestHead answers one pull request from the SAME list OpenPullRequests
// answers -- GitHub's single read agrees with its list -- and a number the list
// does not hold is GitHub's 404.
func (g *fakeGitHub) PullRequestHead(ctx context.Context, _, repository string, number int) (githubapp.PullRequestHead, error) {
	watchGitHubCall(g.t, ctx, "PullRequestHead")
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pullHeadCalls = append(g.pullHeadCalls, fmt.Sprintf("%s#%d", repository, number))
	if g.pullHeadErr != nil {
		return githubapp.PullRequestHead{}, g.pullHeadErr
	}
	for _, pr := range g.pulls[repository] {
		if pr.Number == number {
			return pr, nil
		}
	}
	return githubapp.PullRequestHead{}, &githubapp.StatusError{Status: 404, Endpoint: fmt.Sprintf("/repos/%s/pulls/%d", repository, number)}
}

// setPullHead is a push to pull request number on GitHub: its head moves to
// sha, and a pull request the list does not hold yet is opened from this
// repository.
func (g *fakeGitHub) setPullHead(repository string, number int, sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, pr := range g.pulls[repository] {
		if pr.Number == number {
			g.pulls[repository][i].HeadSHA = sha
			return
		}
	}
	g.pulls[repository] = append(g.pulls[repository], githubapp.PullRequestHead{
		Number: number, Title: "Show the cart count", HeadSHA: sha, HeadRef: "cart-badge", HeadRepository: repository, BaseSHA: shaBase,
	})
}

// headReads is every PullRequestHead call, in order.
func (g *fakeGitHub) headReads() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.pullHeadCalls)
}

func (g *fakeGitHub) Compare(ctx context.Context, _, repository, base, head string) ([]string, bool, error) {
	watchGitHubCall(g.t, ctx, "Compare")
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.compare[repository+"@"+base+"..."+head]
	if !ok {
		return nil, false, &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository + "/compare"}
	}
	return slices.Clone(c.Files), c.Complete, nil
}

func (g *fakeGitHub) CommitForRef(ctx context.Context, _, repository, ref string) (string, string, error) {
	watchGitHubCall(g.t, ctx, "CommitForRef")
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.commits[repository+"@"+ref]
	if !ok {
		return "", "", &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository + "/commits/" + ref}
	}
	return c.SHA, c.Message, nil
}

func (g *fakeGitHub) Tree(ctx context.Context, _, repository, sha string, keep func(string) bool, maxBytes int64) (fs.FS, error) {
	watchGitHubCall(g.t, ctx, "Tree")
	g.mu.Lock()
	defer g.mu.Unlock()
	g.treeCalls = append(g.treeCalls, repository+"@"+sha)
	if g.treeErr != nil {
		return nil, g.treeErr
	}
	src, ok := g.trees[repository+"@"+sha]
	if !ok {
		return nil, &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository + "/tarball/" + sha}
	}
	out := fstest.MapFS{}
	var total int64
	for name, f := range src {
		if keep == nil || !keep(name) {
			continue
		}
		g.treeKept = append(g.treeKept, name)
		total += int64(len(f.Data))
		if total > maxBytes {
			return nil, ErrTreeTooLarge
		}
		out[name] = &fstest.MapFile{Data: slices.Clone(f.Data)}
	}
	return out, nil
}

func (g *fakeGitHub) Installations(ctx context.Context) ([]githubapp.AppInstallation, error) {
	watchGitHubCall(g.t, ctx, "Installations")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unconfigured {
		return nil, githubapp.ErrNotConfigured
	}
	return slices.Clone(g.installations), g.installationsErr
}

func (g *fakeGitHub) createdRuns() []checkWrite {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.created)
}

func (g *fakeGitHub) updatedRuns() []checkWrite {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.updated)
}

// ---------------------------------------------------------------------------
// Gate
// ---------------------------------------------------------------------------

// keyedGate is a Gate that really serializes per key, as the advisory lock
// does across replicas, records every key it was asked for, and keeps the
// GATE DISCIPLINE (watchedGate).
type keyedGate struct {
	t     testing.TB
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	keys  []string
	// refuse makes every acquisition fail, as WithGate does with no
	// database.
	refuse error
}

func newKeyedGate(t testing.TB) *keyedGate { return &keyedGate{t: t, locks: map[string]*sync.Mutex{}} }

func (g *keyedGate) run(ctx context.Context, key string, fn func(context.Context) error) error {
	return watchedGate(g.t, g.acquire)(ctx, key, fn)
}

func (g *keyedGate) acquire(ctx context.Context, key string, fn func(context.Context) error) error {
	g.mu.Lock()
	if g.refuse != nil {
		g.mu.Unlock()
		return g.refuse
	}
	l, ok := g.locks[key]
	if !ok {
		l = &sync.Mutex{}
		g.locks[key] = l
	}
	g.keys = append(g.keys, key)
	g.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	return fn(ctx)
}

func (g *keyedGate) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.keys)
}

// ---------------------------------------------------------------------------
// The gate discipline
// ---------------------------------------------------------------------------
//
// The production gate holds a connection of the DIRECT database pool for its
// whole section -- a pool of four, of which an agent's cron leaders already
// hold one or two, whose waiters give up after five seconds
// (githubconnect.WithGate). Two rules follow, and every test in this package
// is held to them through the two shared fakes:
//
//   - NO NESTING. A gate taken while another is held holds two of the four
//     connections, and two replicas taking two keys in opposite orders wait
//     on each other until both time out.
//   - NO NETWORK UNDER A GATE. A call to GitHub takes as long as GitHub
//     takes; under a gate it holds the connection, and every other opener
//     of the key, for that long. The ONE exception is open()'s check-run
//     CREATE, under the run key's gate: a duplicate check run on GitHub is
//     visible and permanent, so the dedup read and the create are one
//     critical section (open.go says why), and the token it is created with
//     is minted before the gate.
//
// The fake gate marks the context it hands its section with the key it
// holds; a gate asked for on a marked context, and a GitHub call made on one,
// fail the test.

// gateHeldKey marks a context with the gate key its holder holds.
type gateHeldKey struct{}

func gateHeldOn(ctx context.Context) (string, bool) {
	key, ok := ctx.Value(gateHeldKey{}).(string)
	return key, ok
}

// watchedGate is inner under the discipline: it fails t when key is asked
// for while a gate is held, and marks the context fn runs on.
func watchedGate(t testing.TB, inner Gate) Gate {
	return func(ctx context.Context, key string, fn func(context.Context) error) error {
		if held, ok := gateHeldOn(ctx); ok {
			t.Errorf("GATE DISCIPLINE: the gate %q was asked for while %q is held -- a nested gate holds two of the direct pool's four connections, and two replicas nesting two keys in opposite orders deadlock", key, held)
		}
		return inner(ctx, key, func(gctx context.Context) error {
			return fn(context.WithValue(gctx, gateHeldKey{}, key))
		})
	}
}

// watchGitHubCall fails t when a GitHub call is made under a held gate --
// unless it is the check-run create under a run key's open gate, the one
// network call open() makes there.
func watchGitHubCall(t testing.TB, ctx context.Context, call string) {
	held, ok := gateHeldOn(ctx)
	if !ok || t == nil {
		return
	}
	if call == "CreateCheckRun" && strings.HasPrefix(held, OpenGateKey("")) {
		return
	}
	t.Errorf("GATE DISCIPLINE: GitHub's %s was called while the gate %q is held -- a call to GitHub holds the gate's connection, and every other opener of the key, for as long as GitHub takes", call, held)
}

// ---------------------------------------------------------------------------
// The integration under test
// ---------------------------------------------------------------------------

// testOSOrigin is the OS origin every test integration links its check runs
// to.
const testOSOrigin = "https://os.example.test"

// testNow is the clock every test integration reads.
var testNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

const tenMinutes = 10 * time.Minute

// errStatus is GitHub answering status for a request.
func errStatus(status int) error {
	return &githubapp.StatusError{Status: status, Endpoint: "/repos/acme/shop/check-runs"}
}

// decodeNode reads a capability's one answer node.
func decodeNode(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("a capability answers one node, got %d", len(nodes))
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		t.Fatalf("answer is not a JSON object: %v", err)
	}
	return payload
}

type harness struct {
	integ  *Integration
	store  *memStore
	github *fakeGitHub
	gate   *keyedGate

	staged atomic.Int64
}

// stage puts a GitHub delivery on the inbound seam as the receiver stages
// one -- signature verified, the allowlisted headers beside it -- and answers
// the row's id, which is all the trigger is handed.
func (h *harness) stage(t *testing.T, event, deliveryID, body string) string {
	t.Helper()
	return h.stageFrom(t, githubSource, true, body, deliveryHeadersJSON(t, event, deliveryID))
}

// stageFrom stages a delivery on any source, verified or not.
func (h *harness) stageFrom(t *testing.T, source string, verified bool, body, headersJSON string) string {
	t.Helper()
	id := fmt.Sprintf("inbound-%d", h.staged.Add(1))
	h.store.stageDelivery(InboundDelivery{ID: id, Source: source, Body: body, HeadersJSON: headersJSON, SignatureVerified: verified})
	return id
}

// automationCtx is the context a shipped automation reaches a builtin on:
// internal origin, which the automation executor stamps on a tree-loaded
// body's step context (component/automations, originForSource) and on
// nothing a client sends.
func automationCtx() context.Context { return auth.ContextWithInternalOrigin(context.Background()) }

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{store: newMemStore(), github: newFakeGitHub(t), gate: newKeyedGate(t)}
	h.integ = New(Deps{
		Store:    h.store,
		GitHub:   h.github,
		Gate:     h.gate.run,
		OSOrigin: func() string { return testOSOrigin },
		NodeID:   "agent-a",
		Now:      func() time.Time { return testNow },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h
}

// The cast every test shares.
const (
	ownerID   = "user-ada"
	otherID   = "user-bob"
	repoName  = "acme/shop"
	packageID = "pkg-shop"
	credID    = "cred-ada"
	shaA      = "1111111111111111111111111111111111111111"
	shaB      = "2222222222222222222222222222222222222222"
	shaC      = "3333333333333333333333333333333333333333"
	shaBase   = "0000000000000000000000000000000000000abc"
)

// testPipeline is an active pipeline on acme/shop, owned by ownerID, as the
// store hands it back (owner canonical, as a relationship field is stored).
func testPipeline(delivery string) Pipeline {
	return Pipeline{
		ID:             PipelineIDFor(packageID),
		OwnerUserID:    "v1:identity:user:" + ownerID,
		AccountID:      "acct-1",
		PackageID:      packageID,
		Name:           "shop",
		Repository:     repoName,
		DefaultBranch:  "main",
		InstallationID: 7,
		CredentialID:   credID,
		Delivery:       delivery,
		Compute:        pipelines.ComputeCluster,
		Status:         PipelineActive,
		ConnectedAt:    testNow.Add(-time.Hour),
	}
}

// personCtx is a signed-in person as the stream interceptor presents one.
func personCtx(userID string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: auth.RoleDeveloper})
}

// clusterOwnerCtx is a cluster owner -- who reads every source.
func clusterOwnerCtx(userID string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: auth.RoleOwner})
}

// ---------------------------------------------------------------------------
// The work spine under the journal (the driver, Task 10b)
// ---------------------------------------------------------------------------

// fakeWork is the engine the REAL workjournal writes through. Every call is
// handed to the real parser -- the string is what the engine receives -- and
// recorded; the goal, run and step rows it describes are kept as the
// read-merge of every version, so a resumed driver reads back exactly what
// the journal wrote (memStore.WorkSteps).
type fakeWork struct {
	mu    sync.Mutex
	calls []journalCall
	goals map[string]map[string]any
	runs  map[string]map[string]any
	steps map[string]map[string]any
	// refuse fails every call, as an engine that refuses the writes would;
	// refuseCall fails only the calls to one mutation, with refuseErr.
	refuse     error
	refuseCall string
	refuseErr  error
}

// journalCall is one write the journal made.
type journalCall struct {
	Name     string
	Args     map[string]any
	Actor    string
	Internal bool
	Raw      string
}

func newFakeWork() *fakeWork {
	return &fakeWork{goals: map[string]map[string]any{}, runs: map[string]map[string]any{}, steps: map[string]map[string]any{}}
}

func (w *fakeWork) Execute(ctx context.Context, query string) (any, error) {
	parsed, err := langparser.ParseExpression(strings.TrimPrefix(strings.TrimSpace(query), "mutation "))
	if err != nil {
		return nil, fmt.Errorf("fakeWork: the parser refuses the journal's call %s: %w", query, err)
	}
	fn, ok := parsed.(*langparser.FunctionCallExpr)
	if !ok {
		return nil, fmt.Errorf("fakeWork: %s parsed as %T", query, parsed)
	}
	ac, _ := auth.AccessFromContext(ctx)
	actor := ""
	if ac != nil {
		actor = ac.UserId
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.refuse != nil {
		return nil, w.refuse
	}
	if w.refuseErr != nil && fn.Name == w.refuseCall {
		return nil, w.refuseErr
	}
	w.calls = append(w.calls, journalCall{
		Name: fn.Name, Args: fn.Args, Actor: actor, Internal: auth.OriginFromContext(ctx).IsInternal(), Raw: query,
	})
	merge := func(table map[string]map[string]any, idArg string) {
		id, _ := fn.Args[idArg].(string)
		row := table[id]
		if row == nil {
			row = map[string]any{}
			table[id] = row
		}
		for k, v := range fn.Args {
			row[k] = v
		}
	}
	switch fn.Name {
	case "createWorkGoal", "updateWorkGoal":
		merge(w.goals, "goalId")
	case "createWorkRun", "updateWorkRun":
		merge(w.runs, "runId")
	case "createWorkStep", "updateWorkStep":
		merge(w.steps, "stepId")
	}
	return nil, nil
}

// stepRows is every step row of one work run, as merged so far.
func (w *fakeWork) stepRows(runID string) []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []map[string]any
	for _, row := range w.steps {
		if id, _ := row["runId"].(string); bareID(id) == runID {
			out = append(out, maps.Clone(row))
		}
	}
	return out
}

// stepRow is the merged row of the step keyed key, or nil.
func (w *fakeWork) stepRow(key string) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, row := range w.steps {
		if row["key"] == key {
			return maps.Clone(row)
		}
	}
	return nil
}

// run is the merged work run row, or nil.
func (w *fakeWork) run(runID string) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return maps.Clone(w.runs[bareID(runID)])
}

// goal is the merged goal row, or nil.
func (w *fakeWork) goal(goalID string) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return maps.Clone(w.goals[bareID(goalID)])
}

func (w *fakeWork) recorded() []journalCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.calls)
}

// callsNamed is every call to one mutation, in order.
func (w *fakeWork) callsNamed(name string) []journalCall {
	var out []journalCall
	for _, c := range w.recorded() {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// receiptsOf is every receipt (updateWorkStep) of the step keyed key.
func (w *fakeWork) receiptsOf(key string) []journalCall {
	row := w.stepRow(key)
	if row == nil {
		return nil
	}
	id, _ := row["stepId"].(string)
	var out []journalCall
	for _, c := range w.callsNamed("updateWorkStep") {
		if c.Args["stepId"] == id {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The executor
// ---------------------------------------------------------------------------

// fakeExecutor is a runner: it records every request in the order Execute was
// entered and every Cancel, and answers through answer (a pass when nil).
type fakeExecutor struct {
	mu       sync.Mutex
	requests []pipelines.StepRequest
	cancels  []string
	answer   func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error)
	// entered is told each step key as its Execute begins.
	entered chan string
}

func (e *fakeExecutor) Execute(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
	e.mu.Lock()
	e.requests = append(e.requests, req)
	answer, entered := e.answer, e.entered
	e.mu.Unlock()
	if entered != nil {
		entered <- req.StepKey
	}
	if answer == nil {
		return passed(req), nil
	}
	return answer(ctx, req)
}

func (e *fakeExecutor) Cancel(_ context.Context, runID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancels = append(e.cancels, runID)
	return nil
}

func (e *fakeExecutor) sent() []pipelines.StepRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.requests)
}

func (e *fakeExecutor) sentKeys() []string {
	var out []string
	for _, r := range e.sent() {
		out = append(out, r.StepKey)
	}
	return out
}

func (e *fakeExecutor) cancelled() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.cancels)
}

// passed is a step that ran on the cluster and passed, with everything a
// runner reports: where, for how long, its log and its artifacts.
func passed(req pipelines.StepRequest) pipelines.StepResult {
	return pipelines.StepResult{
		Status:          pipelines.OutcomeSucceeded,
		ExitCode:        0,
		StartedAt:       "2026-10-03T12:00:00Z",
		FinishedAt:      "2026-10-03T12:00:42Z",
		Where:           pipelines.Where{Surface: "cluster", NodeID: "workbench-0", JobName: "job-" + req.StepKey},
		LogFileID:       "file-log-" + req.StepKey,
		ArtifactFileIDs: []string{"file-art-" + req.StepKey},
		LogTail:         "ok\n",
		LogLines:        12,
	}
}

// installExecutor registers e as the node's executor for the test's length.
func installExecutor(t *testing.T, e pipelines.Executor) {
	t.Helper()
	prev := pipelines.RegisterExecutor(e)
	t.Cleanup(func() { pipelines.RegisterExecutor(prev) })
}

// ---------------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------------

// lockedBuffer is a log sink many goroutines write to at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

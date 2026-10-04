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
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
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
	mu        sync.Mutex
	packages  map[string]PackageSource
	pipelines map[string]Pipeline
	runs      map[string]Run

	// Writes, in order, for assertions.
	pipelineCreates []Pipeline
	pipelineUpdates []pipelineUpdate
	runCreates      []Run
	runUpdates      []runUpdate

	// Injected failures.
	failCreateRun error
	failUpdateRun error
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
	return &memStore{packages: map[string]PackageSource{}, pipelines: map[string]Pipeline{}, runs: map[string]Run{}}
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

func (s *memStore) RunByID(_ context.Context, runID string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[bareID(runID)]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

// CreatePipeline is createPipeline: a read-merge insert that restates the
// configuration, re-activates the row and moves connectedAt, keeping heads
// and timings.
func (s *memStore) CreatePipeline(_ context.Context, p Pipeline) error {
	if strings.TrimSpace(p.OwnerUserID) == "" {
		return errors.New("memStore: a pipeline is written only under its owner")
	}
	s.mu.Lock()
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
type fakeGitHub struct {
	mu sync.Mutex

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

	// treeKeeps records each Tree call's keep decision for the files it
	// held, so a test can assert what was asked for.
	treeCalls []string
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

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
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

func (g *fakeGitHub) InstallationToken(_ context.Context, credentialID, ownerUserID, repository string) (string, int64, error) {
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

func (g *fakeGitHub) CreateCheckRun(_ context.Context, token, repository string, run githubapp.CheckRun) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.createErr != nil {
		return 0, g.createErr
	}
	g.nextID++
	g.created = append(g.created, checkWrite{Token: token, Repository: repository, ID: g.nextID, Run: run})
	return g.nextID, nil
}

func (g *fakeGitHub) UpdateCheckRun(_ context.Context, token, repository string, id int64, run githubapp.CheckRun) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.updateErr != nil {
		return g.updateErr
	}
	g.updated = append(g.updated, checkWrite{Token: token, Repository: repository, ID: id, Run: run})
	return nil
}

func (g *fakeGitHub) Repository(_ context.Context, _, repository string) (githubapp.RepositoryInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	info, ok := g.repos[repository]
	if !ok {
		return githubapp.RepositoryInfo{}, &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository}
	}
	return info, nil
}

func (g *fakeGitHub) BranchHead(_ context.Context, _, repository, branch string) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h, ok := g.heads[repository+"@"+branch]
	if !ok {
		return "", "", &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository + "/branches/" + branch}
	}
	return h.SHA, h.Message, nil
}

func (g *fakeGitHub) OpenPullRequests(_ context.Context, _, repository string) ([]githubapp.PullRequestHead, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.pulls[repository]), nil
}

func (g *fakeGitHub) Compare(_ context.Context, _, repository, base, head string) ([]string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.compare[repository+"@"+base+"..."+head]
	if !ok {
		return nil, false, &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository + "/compare"}
	}
	return slices.Clone(c.Files), c.Complete, nil
}

func (g *fakeGitHub) CommitForRef(_ context.Context, _, repository, ref string) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.commits[repository+"@"+ref]
	if !ok {
		return "", "", &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository + "/commits/" + ref}
	}
	return c.SHA, c.Message, nil
}

func (g *fakeGitHub) Tree(_ context.Context, _, repository, sha string, keep func(string) bool, maxBytes int64) (fs.FS, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.treeCalls = append(g.treeCalls, repository+"@"+sha)
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
		total += int64(len(f.Data))
		if total > maxBytes {
			return nil, ErrTreeTooLarge
		}
		out[name] = &fstest.MapFile{Data: slices.Clone(f.Data)}
	}
	return out, nil
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
// does across replicas, and records every key it was asked for.
type keyedGate struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	keys  []string
	// refuse makes every acquisition fail, as WithGate does with no
	// database.
	refuse error
}

func newKeyedGate() *keyedGate { return &keyedGate{locks: map[string]*sync.Mutex{}} }

func (g *keyedGate) run(ctx context.Context, key string, fn func(context.Context) error) error {
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
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{store: newMemStore(), github: newFakeGitHub(), gate: newKeyedGate()}
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

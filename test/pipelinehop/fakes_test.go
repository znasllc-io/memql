package pipelinehop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/pipelinerun"
	"github.com/znasllc-io/memql/component/pipelines"
)

// fakes_test.go -- the two things in the hop that are not this cluster's:
// GitHub, and the step runner (the substrate's, epic memql#5478).
//
// Everything else is real: two engines, the inbound receiver, the shipped
// trigger automation, the pipelines plug-in as its own factory builds it, the
// DSL store, the work journal and the Postgres advisory gate.

// testManifest is the tiny repository's memql-package.yaml: two stages, the
// second waiting on the first, so stage order is observable in what the
// runner is handed.
const testManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:5491
  stages:
    - name: checks
      steps:
        - name: vet
          run: go vet ./...
    - name: tests
      needs: [checks]
      steps:
        - name: unit
          run: go test ./...
        - name: race
          run: go test -race ./...
`

// testStages is the stage order testManifest declares.
var testStages = []string{"checks", "tests"}

// testStepKeys is every step key testManifest compiles to, sorted.
var testStepKeys = []string{"checks.vet", "tests.race", "tests.unit"}

// tinyRepository is the repository at every commit the fake serves: the
// manifest and a Go module small enough to read whole.
func tinyRepository() fstest.MapFS {
	return fstest.MapFS{
		pipelines.ManifestPath: {Data: []byte(testManifest)},
		"go.mod":               {Data: []byte("module acme.test/shop\n\ngo 1.26\n")},
		"main.go":              {Data: []byte("package main\n\nfunc main() {}\n")},
		// A file no run keeps: the tree read's filter is the driver's.
		"README.md": {Data: []byte("# shop\n")},
	}
}

// ---------------------------------------------------------------------------
// GitHub
// ---------------------------------------------------------------------------

// fakeGitHub is ONE GitHub both nodes talk to, as two replicas of a cluster
// talk to the same github.com. Each node holds its own view of it (as), so
// every call is recorded with the node that made it: which node created a
// check run and which moved it is the hop's evidence.
type fakeGitHub struct {
	mu     sync.Mutex
	repos  map[string]*fakeRepo
	nextID int64

	writes   []checkWrite
	trees    []ghCall
	refs     []ghCall
	compares []ghCall
}

// fakeRepo is one repository, and the grant that reaches it.
type fakeRepo struct {
	// owner and credential are the grant: a token is minted only for the
	// source owner's own credential (bare ids).
	owner, credential string
	installation      int64
	defaultBranch     string
	heads             map[string]string // branch -> sha
	pulls             []githubapp.PullRequestHead
	tags              map[string]string // "tags/<name>" -> sha
	commits           map[string]bool   // the commits a tarball exists for
	changed           []string          // every compare's answer
	files             fstest.MapFS
}

// checkWrite is one check-run write, as GitHub received it.
type checkWrite struct {
	Node       string
	Create     bool
	Repository string
	ID         int64
	Run        githubapp.CheckRun
}

// ghCall is one read a node made of a repository: Arg is the commit, the ref
// or the compared range.
type ghCall struct {
	Node, Repository, Arg string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{repos: map[string]*fakeRepo{}, nextID: 54910000}
}

// as is the GitHub port one node holds.
func (g *fakeGitHub) as(node string) pipelinerun.GitHub { return githubAt{g: g, node: node} }

func (g *fakeGitHub) add(repository string, r *fakeRepo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repos[strings.ToLower(repository)] = r
}

// update changes a repository under the fake's lock: a push, a pull request
// opened, a tag cut.
func (g *fakeGitHub) update(repository string, change func(r *fakeRepo)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g.repos[strings.ToLower(repository)])
}

// writesFor is every check-run write to repository, in the order GitHub
// received them.
func (g *fakeGitHub) writesFor(repository string) []checkWrite {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []checkWrite
	for _, w := range g.writes {
		if w.Repository == repository {
			out = append(out, w)
		}
	}
	return out
}

// checkRun is every write to one check run, in order.
func (g *fakeGitHub) checkRun(id int64) []checkWrite {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []checkWrite
	for _, w := range g.writes {
		if w.ID == id {
			out = append(out, w)
		}
	}
	return out
}

// callsOf is every read of repository one of the fake's read logs holds, in
// order.
func (g *fakeGitHub) callsOf(calls *[]ghCall, repository string) []ghCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []ghCall
	for _, c := range *calls {
		if c.Repository == repository {
			out = append(out, c)
		}
	}
	return out
}

// repo answers the repository a token-taking call names, refusing a token
// minted for any other: a call that crossed two repositories' grants is a
// defect no answer should hide.
func (g *fakeGitHub) repo(token, repository string) (*fakeRepo, error) {
	r, ok := g.repos[strings.ToLower(repository)]
	if !ok {
		return nil, &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository}
	}
	if token != tokenFor(repository) {
		return nil, &githubapp.StatusError{Status: 401, Endpoint: "/repos/" + repository}
	}
	return r, nil
}

// tokenFor is the installation token the fake mints for a repository. Not a
// GitHub token's shape, so nothing mistakes it for one.
func tokenFor(repository string) string {
	return "hop-installation-token:" + strings.ToLower(repository)
}

// githubAt is one node's view of the shared fake.
type githubAt struct {
	g    *fakeGitHub
	node string
}

func (h githubAt) Configured() bool { return true }

// Installations answers none: no hop reads the app's installations, which
// only the Settings item asks for, on the node that answers the builtin.
func (h githubAt) Installations(context.Context) ([]githubapp.AppInstallation, error) { return nil, nil }

func (h githubAt) InstallationToken(_ context.Context, credentialID, ownerUserID, repository string) (string, int64, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.repos[strings.ToLower(repository)]
	if !ok {
		// A repository this GitHub was never told about: in the shared
		// database CI runs this lane against, another package's pipeline
		// row, which this test's poll reads too. Refused here, before the
		// poll writes anything (pollOne mints first), so no other package's
		// row is ever touched.
		return "", 0, githubapp.ErrNotInstalled
	}
	if memql.BareShortId(credentialID) != r.credential || memql.BareShortId(ownerUserID) != r.owner {
		return "", 0, fmt.Errorf("fake GitHub: credential %q of %q is not the grant that reaches %s", credentialID, ownerUserID, repository)
	}
	return tokenFor(repository), r.installation, nil
}

func (h githubAt) CreateCheckRun(_ context.Context, token, repository string, run githubapp.CheckRun) (int64, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.repo(token, repository); err != nil {
		return 0, err
	}
	g.nextID++
	g.writes = append(g.writes, checkWrite{Node: h.node, Create: true, Repository: repository, ID: g.nextID, Run: run})
	return g.nextID, nil
}

func (h githubAt) UpdateCheckRun(_ context.Context, token, repository string, id int64, run githubapp.CheckRun) error {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.repo(token, repository); err != nil {
		return err
	}
	known := false
	for _, w := range g.writes {
		known = known || (w.Create && w.ID == id && w.Repository == repository)
	}
	if !known {
		return &githubapp.StatusError{Status: 404, Endpoint: fmt.Sprintf("/repos/%s/check-runs/%d", repository, id)}
	}
	g.writes = append(g.writes, checkWrite{Node: h.node, Repository: repository, ID: id, Run: run})
	return nil
}

func (h githubAt) Repository(_ context.Context, token, repository string) (githubapp.RepositoryInfo, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return githubapp.RepositoryInfo{}, err
	}
	return githubapp.RepositoryInfo{FullName: repository, DefaultBranch: r.defaultBranch}, nil
}

func (h githubAt) BranchHead(_ context.Context, token, repository, branch string) (string, string, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return "", "", err
	}
	sha, ok := r.heads[branch]
	if !ok {
		return "", "", &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository + "/branches/" + branch}
	}
	return sha, "Head of " + branch, nil
}

func (h githubAt) OpenPullRequests(_ context.Context, token, repository string) ([]githubapp.PullRequestHead, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return nil, err
	}
	return slices.Clone(r.pulls), nil
}

func (h githubAt) Compare(_ context.Context, token, repository, base, head string) ([]string, bool, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return nil, false, err
	}
	g.compares = append(g.compares, ghCall{Node: h.node, Repository: repository, Arg: base + "..." + head})
	if !r.commits[base] || !r.commits[head] {
		return nil, false, &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository + "/compare/" + base + "..." + head}
	}
	return slices.Clone(r.changed), true, nil
}

func (h githubAt) CommitForRef(_ context.Context, token, repository, ref string) (string, string, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return "", "", err
	}
	g.refs = append(g.refs, ghCall{Node: h.node, Repository: repository, Arg: ref})
	sha, ok := r.tags[ref]
	if !ok {
		return "", "", &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository + "/commits/" + ref}
	}
	return sha, "Release " + ref, nil
}

func (h githubAt) Tree(_ context.Context, token, repository, sha string, keep func(string) bool, maxBytes int64) (fs.FS, error) {
	g := h.g
	g.mu.Lock()
	defer g.mu.Unlock()
	r, err := g.repo(token, repository)
	if err != nil {
		return nil, err
	}
	g.trees = append(g.trees, ghCall{Node: h.node, Repository: repository, Arg: sha})
	if !r.commits[sha] {
		// Only a commit the repository has: a driver reading the wrong
		// commit fails its run instead of passing on somebody else's tree.
		return nil, &githubapp.StatusError{Status: 404, Endpoint: "/repos/" + repository + "/tarball/" + sha}
	}
	out := fstest.MapFS{}
	var total int64
	for name, f := range r.files {
		if keep == nil || !keep(name) {
			continue
		}
		total += int64(len(f.Data))
		if total > maxBytes {
			return nil, pipelinerun.ErrTreeTooLarge
		}
		out[name] = &fstest.MapFile{Data: bytes.Clone(f.Data)}
	}
	return out, nil
}

// commitFor is a deterministic 40-hex commit id, unique to this run of the
// test and to what it names.
func commitFor(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:20])
}

// ---------------------------------------------------------------------------
// The step runner
// ---------------------------------------------------------------------------

// fakeExecutor is a runner that passes every step: it records each request
// in the order Execute was entered, and every Cancel.
type fakeExecutor struct {
	mu       sync.Mutex
	requests []pipelines.StepRequest
	cancels  []string
}

func (e *fakeExecutor) Execute(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
	e.mu.Lock()
	e.requests = append(e.requests, req)
	e.mu.Unlock()
	started := time.Now().UTC().Truncate(time.Second)
	return pipelines.StepResult{
		Status:     pipelines.OutcomeSucceeded,
		StartedAt:  started.Format(time.RFC3339),
		FinishedAt: started.Add(2 * time.Second).Format(time.RFC3339),
		Where:      pipelines.Where{Surface: "cluster", NodeID: "workbench-0", JobName: "job-" + req.StepKey},
		LogTail:    "ok\n",
		LogLines:   1,
	}, nil
}

func (e *fakeExecutor) Cancel(_ context.Context, runID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cancels = append(e.cancels, runID)
	return nil
}

// forRun is every request the runner was handed for one run, in order.
func (e *fakeExecutor) forRun(runID string) []pipelines.StepRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []pipelines.StepRequest
	for _, r := range e.requests {
		if r.RunID == memql.BareShortId(runID) {
			out = append(out, r)
		}
	}
	return out
}

// forRepository is every request for one repository's runs.
func (e *fakeExecutor) forRepository(repository string) []pipelines.StepRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []pipelines.StepRequest
	for _, r := range e.requests {
		if strings.EqualFold(r.Repository.Owner+"/"+r.Repository.Name, repository) {
			out = append(out, r)
		}
	}
	return out
}

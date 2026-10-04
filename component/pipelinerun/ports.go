package pipelinerun

import (
	"context"
	"io/fs"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/packages/githubapp"
)

// ports.go -- the narrow seams every decision in this package is made
// behind, so each one is unit-tested against a fake: rows (Store), GitHub
// (GitHub), cross-replica serialization (Gate) and secret values (Secrets).

// Engine is the one engine method the DSL store calls.
type Engine interface {
	Execute(ctx context.Context, query string) (*memql.ExecuteResult, error)
}

// Gate serializes a read-modify-write across replicas: fn runs while no other
// holder of key is running, on any node. Production is githubconnect.WithGate
// over the direct (non-pooled) database, a Postgres advisory lock held on one
// session for fn's duration; it fails CLOSED -- with no database it refuses
// before fn runs -- and so must every implementation, because a gate that
// lets fn through unserialized is how one head becomes two runs.
//
// A read inside fn that decides the write must be fresh
// (memql.ContextWithFreshRead): this node's result cache can hold a row
// another replica has since rewritten. And fn is SHORT -- that read and its
// writes: it never takes another gate and never calls GitHub, save open()'s
// check-run create (ids.go; every test holds every path to it).
type Gate func(ctx context.Context, key string, fn func(context.Context) error) error

// Secrets resolves a v1:platform:globalSecret value by name. Production is
// PluginContext.ResolveSystemSecret. Only the driver calls it, and only for a
// name the pipeline's secretNames allowlist holds.
type Secrets func(ctx context.Context, name string) (string, error)

// Store is every row this package reads and writes, over the DSL constructs
// of dsl/pipelines (and Deployables' packageById). Each method applies its
// own authority, so a caller cannot pick the wrong one:
//
//   - the person-facing reads run under the CALLER's actor, unstamped: the
//     owner-scoped constructs decide which rows come back, and a caller who
//     does not own a row reads none;
//   - the server-only reads run under this package's own system actor with
//     internal origin: the trigger, the poll and recovery have no person
//     behind them and must see every owner's rows;
//   - every write runs under the row OWNER's borrowed authority with
//     internal origin: the @serverOnly mutations stamp ownerUserId from the
//     actor, and an empty owner is refused rather than written as nobody's.
//
// A nil row with a nil error is "no such row (that this authority reads)".
type Store interface {
	// Person-facing, under the caller's actor.
	PackageForCaller(ctx context.Context, packageID string) (*PackageSource, error)
	PipelineForOwner(ctx context.Context, pipelineID string) (*Pipeline, error)
	PipelineForPackage(ctx context.Context, packageID string) (*Pipeline, error)
	PipelinesForOwner(ctx context.Context) ([]Pipeline, error)
	RunForOwner(ctx context.Context, runID string) (*Run, error)
	RunsForOwner(ctx context.Context, pipelineID string) ([]Run, error)

	// Server-only, under the pipelines system actor.
	//
	// InboundDelivery is one staged delivery by its row id (the inbound
	// seam's inboundRequestById): the trigger's only source of a delivery's
	// body, headers and signature verdict.
	InboundDelivery(ctx context.Context, requestID string) (*InboundDelivery, error)
	PipelinesForRepository(ctx context.Context, repository string) ([]Pipeline, error)
	PipelinesPolled(ctx context.Context) ([]Pipeline, error)
	PipelineByID(ctx context.Context, pipelineID string) (*Pipeline, error)
	RunsForKey(ctx context.Context, runKey string) ([]Run, error)
	// RunsForPipelineSHA answers newest first.
	RunsForPipelineSHA(ctx context.Context, pipelineID, sha string) ([]Run, error)
	RunByCheckRun(ctx context.Context, repository string, checkRunID int64) (*Run, error)
	// RunsUnfinished answers oldest first: recovery's read (Task 10b).
	RunsUnfinished(ctx context.Context) ([]Run, error)
	// RunsUnfinishedForPullRequest is every queued or in-progress run of one
	// pipeline's pull request, in EVERY mode and in no order: what a new push
	// to the pull request supersedes (supersede.go), whose caller applies
	// the mode rule. A pull request numbered 0 or less is no pull request,
	// and reads nothing.
	RunsUnfinishedForPullRequest(ctx context.Context, pipelineID string, pullRequest int) ([]Run, error)
	// RunsFinalCheckRunUnavailable is every run concluded at or after since
	// whose final check run did not land (checkRunState unavailable), newest
	// first: what recovery republishes.
	RunsFinalCheckRunUnavailable(ctx context.Context, since time.Time) ([]Run, error)
	RunByID(ctx context.Context, runID string) (*Run, error)
	// PipelinesActive answers at most one active pipeline, whoever owns it:
	// the readiness report asks only whether ANY repository is connected.
	PipelinesActive(ctx context.Context) ([]Pipeline, error)
	// WorkSteps is every v1:work:step of one work run at its latest version
	// (dsl/work's workStepsForRun): what a resumed driver keeps and re-sends.
	WorkSteps(ctx context.Context, workRunID string) ([]WorkStep, error)

	// Writes, under the owner's borrowed authority. Create* borrow the
	// value's own OwnerUserID; Update* take the owner explicitly.
	CreatePipeline(ctx context.Context, p Pipeline) error
	UpdatePipeline(ctx context.Context, owner, pipelineID string, patch PipelinePatch) error
	CreateRun(ctx context.Context, r Run) error
	UpdateRun(ctx context.Context, owner, runID string, patch RunPatch) error
}

// GitHub is every GitHub call a pipeline makes. Repository is always
// "owner/name". Token-taking calls run under an installation token from
// InstallationToken, which never leaves memory.
//
// Errors keep githubapp's shapes -- a *githubapp.StatusError, ErrNotConfigured,
// ErrNotInstalled, or a component/packages *Refusal from the grant path -- so
// a caller classifies with githubapp.StatusOf and errors.As.
type GitHub interface {
	// Configured reports whether this cluster has a GitHub App at all.
	Configured() bool
	// InstallationToken mints the token a source owner's grant reaches for
	// repository, verifying the grant still reaches it on every call, and
	// answers the installation it was minted for.
	InstallationToken(ctx context.Context, credentialID, ownerUserID, repository string) (token string, installationID int64, err error)
	CreateCheckRun(ctx context.Context, token, repository string, run githubapp.CheckRun) (int64, error)
	UpdateCheckRun(ctx context.Context, token, repository string, id int64, run githubapp.CheckRun) error
	Repository(ctx context.Context, token, repository string) (githubapp.RepositoryInfo, error)
	BranchHead(ctx context.Context, token, repository, branch string) (sha, message string, err error)
	OpenPullRequests(ctx context.Context, token, repository string) ([]githubapp.PullRequestHead, error)
	// PullRequestHead reads one pull request's head as GitHub reports it now,
	// by its number: what decides whether an opening is the pull request's
	// current head (supersede.go). Not OpenPullRequests, which is one page of
	// a hundred, newest first, and misses an older pull request in a busy
	// repository.
	PullRequestHead(ctx context.Context, token, repository string, number int) (githubapp.PullRequestHead, error)
	// Compare answers complete=false when GitHub stopped listing (300+).
	Compare(ctx context.Context, token, repository, base, head string) (files []string, complete bool, err error)
	CommitForRef(ctx context.Context, token, repository, ref string) (sha, message string, err error)
	// Tree streams the repository's tarball at sha and keeps only the
	// regular files keep admits, by repository-relative path (GitHub's
	// synthesized top-level directory stripped). More than maxBytes of kept
	// content is ErrTreeTooLarge.
	Tree(ctx context.Context, token, repository, sha string, keep func(path string) bool, maxBytes int64) (fs.FS, error)
	// Installations is every installation of the cluster's app with what it
	// has ACCEPTED, asked as the app itself: the Settings item's read of
	// installations whose permissions lag the app's (epic memql#5479, D15).
	Installations(ctx context.Context) ([]githubapp.AppInstallation, error)
}

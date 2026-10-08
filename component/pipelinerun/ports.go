package pipelinerun

import (
	"context"
	"io/fs"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
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
// of dsl/pipelines (and Deployables' packageById, the outbound seam's staging
// and the Library's file read). Each method applies its own authority, so a
// caller cannot pick the wrong one. Two things are decided for it, and they are
// independent: WHO the call runs as, and whether it is stamped internal origin
// (what a @serverOnly construct requires).
//
//   - a person-facing read runs as the CALLER, unstamped: the owner-scoped
//     constructs decide which rows come back, and a caller who does not own a
//     row reads none;
//   - an owner-scoped read made for a person who is not on the line runs as
//     that OWNER, borrowed, likewise unstamped: the construct's owner conjunct
//     decides the rows, so the borrowed actor reads what that person could and
//     nothing more, and an empty owner is refused rather than read as nobody;
//   - a server-only read runs as this package's own SYSTEM actor, stamped: the
//     trigger, the poll and recovery have no person behind them and must see
//     every owner's rows;
//   - a write runs as the row's OWNER, borrowed, stamped: the @serverOnly
//     mutations stamp ownerUserId from the actor, and an empty owner is
//     refused rather than written as nobody's. The one exception is
//     StageNotification: an outbound row is nobody's, so it is staged as the
//     SYSTEM actor, stamped.
//
// OWNERSHIP OF A ROW THAT EXISTS IS NOT CHECKED BY A WRITE. A stamped write
// escapes the engine's owner write guard (rowauthz_write_guard.go: internal
// origin is trusted server-side Go), so the owner an Update* names is
// attribution and not an authorization, and a Create* gives the row to whoever
// it names. What keeps a person off another's rows is the owner-scoped READ a
// caller makes first, which finds nothing of theirs; every person-facing act
// is held to making it by TestNoPersonReachesAnotherOwnersRows
// (internal_origin_test.go).
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
	// ChannelsForOwner is the caller's notification channels, by name
	// (pipelineChannelsForOwner).
	ChannelsForOwner(ctx context.Context) ([]Channel, error)

	// Owner-scoped, under the OWNER's borrowed authority and not the caller's.
	//
	// ChannelForOwnerByName is the owner's channel of one name
	// (pipelineChannelForOwnerByName) -- what a notify stage names -- and nil
	// for a name no channel of theirs carries. An archived channel is returned
	// as it is: telling it from an active one is the caller's. A caller that
	// decides on the answer reads it FRESH (memql.ContextWithFreshRead): a
	// channel archived or renamed on another replica a moment ago must not be
	// delivered to from this node's result cache.
	ChannelForOwnerByName(ctx context.Context, owner, name string) (*Channel, error)
	// LibraryFileNames is the name of each of the owner's Library files
	// (libraryFileById), keyed by each id as it was passed, trimmed of
	// surrounding space; a blank id asks nothing. A file the owner cannot read,
	// or that is not there, is simply absent from the map.
	LibraryFileNames(ctx context.Context, owner string, ids []string) (map[string]string, error)

	// Server-only, under the pipelines system actor.
	//
	// InboundDelivery is one staged delivery by its row id (the inbound
	// seam's inboundRequestById): the trigger's only source of a delivery's
	// body, headers and signature verdict.
	InboundDelivery(ctx context.Context, requestID string) (*InboundDelivery, error)
	PipelinesForRepository(ctx context.Context, repository string) ([]Pipeline, error)
	PipelinesPolled(ctx context.Context) ([]Pipeline, error)
	PipelinesForScheduledScan(ctx context.Context) ([]Pipeline, error)
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
	// PreviousRuns is one pipeline's runs of one event, newest queued first and
	// at most twenty, in every status (pipelineRunsForPipelineEvent): what a
	// notification reads to learn whether the run before this one failed. The
	// caller picks the newest COMPLETED run other than its own, and reads FRESH
	// (memql.ContextWithFreshRead) when it decides on the answer: this node's
	// result cache can hold a run another replica has since concluded.
	PreviousRuns(ctx context.Context, pipelineID string, event pipelines.Event) ([]Run, error)
	// OutboundStatuses is the delivery state of each outbound row asked for
	// (outboundRequestById), one entry per id, in order; a blank id is an empty
	// entry and asks nothing. A row that is not there is an entry with an empty
	// Status, never an error and never `sent`; a read that FAILS is an error,
	// never an absent row. EVERY read is FRESH, whatever the caller's ctx
	// carries: the outbound worker moves a row on whichever replica claimed it,
	// so a poll must never take this node's cached `pending` after another
	// replica stamped `sent`, or the reverse. The method marks its own reads;
	// no caller has to remember.
	OutboundStatuses(ctx context.Context, ids []string) ([]OutboundStatus, error)

	// Writes, under the owner's borrowed authority. Create* borrow the
	// value's own OwnerUserID; Update* take the owner explicitly.
	CreatePipeline(ctx context.Context, p Pipeline) error
	UpdatePipeline(ctx context.Context, owner, pipelineID string, patch PipelinePatch) error
	CreateRun(ctx context.Context, r Run) error
	UpdateRun(ctx context.Context, owner, runID string, patch RunPatch) error
	CreateChannel(ctx context.Context, c Channel) error
	UpdateChannel(ctx context.Context, owner, channelID string, patch ChannelPatch) error

	// StageNotification stages one outbound row for the outbound worker to
	// deliver: a webhook to the globalSecret n.TargetSecret names
	// (stageOutboundRequestToSecret), else a row for n.Medium and n.Target
	// (stageServerOutboundRequest). Both are server-only, called as the system
	// actor with internal origin -- the row is nobody's -- and both mark the row
	// serverStaged, so the engine refuses every later write to it without
	// internal origin: the delivery state a caller reads back is the worker's.
	// The stage is idempotent by RequestID, and the worker owns the row's
	// delivery state: a second stage at an id refreshes what the row says and
	// leaves where the worker has taken it (@createOnly).
	//
	// THAT IS WHY A REQUEST ID MUST BE UNGUESSABLE. A client can still stage a
	// plain row of its own (stageOutboundRequest) at a guessable id and leave it
	// `sent` or `failed`, and the stage the server makes there later inherits
	// that state. Callers use a random id, one per row.
	//
	// A notification that cannot be sent as written -- no id, nothing to say, a
	// secret with a medium other than webhook or with a target beside it, a
	// plain row with nowhere to go -- is refused before the engine is asked.
	StageNotification(ctx context.Context, n NotificationRequest) error
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

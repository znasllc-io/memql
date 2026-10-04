package pipelinerun

import (
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// The concepts this package reads and writes, for a log line's subject
// (logger.Subject takes the concept and a BARE id).
const (
	PipelineConcept = "v1:pipelines:pipeline"
	RunConcept      = "v1:pipelines:run"
)

// The values v1:pipelines:run and v1:pipelines:pipeline spell their
// lifecycle in (dsl/pipelines/concepts.memql). One spelling, here, so the
// opening half and the driver never write two words for one state.
const (
	// Run status.
	StatusQueued     = "queued"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"

	// Run conclusion. Empty until the run is completed. A refused run never
	// executed a step: a fork, a manifest that does not compile, a secret
	// the owner did not allow -- with the code in refusalCode.
	ConclusionNone      = ""
	ConclusionSuccess   = "success"
	ConclusionFailure   = "failure"
	ConclusionCancelled = "cancelled"
	ConclusionRefused   = "refused"

	// What opened a run row.
	TriggerWebhook = "webhook"
	TriggerPoll    = "poll"
	TriggerRerun   = "rerun"

	// Whether the run's GitHub check run exists. Pending is the value a run
	// is opened with before any write was attempted; refused is GitHub's
	// 403 (an installation predating `checks: write`), which never stops the
	// run; unavailable is no app, no token, or a write that failed for
	// another reason.
	CheckRunPending     = ""
	CheckRunWritten     = "written"
	CheckRunRefused     = "refused"
	CheckRunUnavailable = "unavailable"

	// How changes reach a pipeline.
	DeliveryWebhook = "webhook"
	DeliveryPoll    = "poll"

	// Pipeline status.
	PipelineActive       = "active"
	PipelineDisconnected = "disconnected"
)

// Step statuses, in the vocabulary pipelines.StepReport (and so the check
// run's stage table) reads.
const (
	StepPending   = "pending"
	StepRunning   = "running"
	StepSucceeded = "succeeded"
	StepFailed    = "failed"
	StepCancelled = "cancelled"
	StepSkipped   = "skipped"
	StepRefused   = "refused"
)

// A stage's status as the run row's `stages` records it: the words a machine
// reads (the OS renders its own). The check run's table spells the same
// states for a person (pipelines.CheckOutput).
const (
	StageWaiting   = "waiting"
	StageRunning   = "running"
	StagePassed    = "passed"
	StageFailed    = "failed"
	StageCancelled = "cancelled"
	StageSkipped   = "skipped"
	// StageBlocked is a stage an earlier stage's failure kept from running
	// (pipeline_stage_blocked).
	StageBlocked = "blocked"
)

// A v1:work:step's status, in the step concept's own vocabulary
// (dsl/work/concepts.memql) -- which is not StepState's: the work spine says
// `done` where the check run says succeeded.
const (
	WorkStepPending   = "pending"
	WorkStepRunning   = "running"
	WorkStepDone      = "done"
	WorkStepFailed    = "failed"
	WorkStepSkipped   = "skipped"
	WorkStepCancelled = "cancelled"
)

// WorkStep is one v1:work:step of a pipeline's work run, at its latest
// version: what a resumed driver reads to keep the steps that finished and to
// re-send the one that was running. The journal writes these rows; this
// package only reads them.
type WorkStep struct {
	Key string
	// Seq is the step's place in the plan the work run was opened with.
	Seq          int
	Status       string
	Attempt      int
	DurationMs   int64
	ErrorCode    string
	ErrorMessage string
	// Stage and Name are the step's call: which manifest step the row is.
	Stage, Name string
	// Packages is the slice the plan gave the step (call.packages); nil for a
	// step that selects none. A resumed step is re-sent with THIS slice, not
	// one recomputed from a timing table that may have moved since.
	Packages []string
	// Skip is the skip the plan gave the step (call.skip), nil for a step the
	// plan ran. A resumed driver keeps it whatever a compare read now says:
	// a step that was planned to run, and so may have been sent, is not
	// settled as skipped behind the runner's back.
	Skip *pipelines.Skip
	// Reason is a skipped or cancelled step's reason (result.reason).
	Reason string
}

// Finished reports whether the row carries the step's receipt.
func (s WorkStep) Finished() bool {
	switch s.Status {
	case WorkStepDone, WorkStepFailed, WorkStepSkipped, WorkStepCancelled:
		return true
	}
	return false
}

// Pipeline is one v1:pipelines:pipeline row as this package reads it.
//
// EVERY ID IS BARE except OwnerUserID. A relationship field is stored
// canonicalized ("v1:platform:package:<id>"), an id handed back by a read is
// canonical, and a request carries the bare id; the store bare-ifies every
// one of them on the way in, so a comparison here is between two spellings
// of nothing. OwnerUserID is kept EXACTLY AS STORED, because it is the value
// this package borrows authority under (auth.ContextWithUserActor) -- the
// shape component/packages' openDeployment borrows its package row's owner
// in -- and it is compared through sameID, never with ==.
type Pipeline struct {
	ID          string
	OwnerUserID string
	AccountID   string
	PackageID   string
	Name        string
	// Repository is "owner/name", lower-cased: the trigger's match key.
	Repository    string
	DefaultBranch string
	// InstallationID is the GitHub App installation that reaches the
	// repository. Stored as text; 0 when it could not be read.
	InstallationID int64
	CredentialID   string
	Delivery       string
	// Compute is never empty here: an absent value reads as cluster.
	Compute     pipelines.Compute
	Status      string
	SecretNames []string
	ChannelIDs  []string
	// Heads is what the poll has seen: "branch:<name>" and "pr:<number>" to
	// a lower-cased SHA. Empty before the first poll.
	Heads map[string]string
	// Timings is seconds per Go import path, merged from successful full
	// runs.
	Timings          map[string]float64
	TimingsRunID     string
	TimingsUpdatedAt time.Time
	ConnectedAt      time.Time
}

// Active reports whether the pipeline opens runs.
func (p Pipeline) Active() bool { return p.Status == PipelineActive }

// Run is one v1:pipelines:run row as this package reads it. Ids are bare,
// OwnerUserID is as stored -- see Pipeline.
type Run struct {
	ID          string
	OwnerUserID string
	AccountID   string
	PipelineID  string
	Repository  string
	SHA         string
	Mode        pipelines.Mode
	Event       pipelines.Event
	RunKey      string
	Attempt     int
	Trigger     string
	RerunOf     string
	// RerunFailedOnly: this attempt runs only what RerunOf did not pass
	// (epic memql#5479); the driver carries the rest over as skipped
	// pipeline_passed_earlier.
	RerunFailedOnly bool
	DeliveryID      string
	PullRequest     int
	HeadBranch      string
	BaseSHA         string
	Title           string
	Version         string

	Status         string
	Conclusion     string
	RefusalCode    string
	RefusalMessage string
	RefusalScope   string

	// CheckRunID is GitHub's id for the run's check run, stored as text; 0
	// when none was written, in which case CheckRunState says why.
	CheckRunID    int64
	CheckRunState string
	Notes         []Note

	// The driver's fields (Task 10b): the work run the plan compiled into,
	// the lease, and the cancel request.
	WorkRunID         string
	WorkGoalID        string
	DriverNodeID      string
	DriverHeartbeatAt time.Time
	CancelRequested   bool
	CancelledBy       string
	Stages            []StageSummary

	QueuedAt   time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	DurationMs int64
	CreatedAt  time.Time
}

// Finished reports whether the run has a conclusion. A finished run is never
// reopened: a re-run is a new row.
func (r Run) Finished() bool { return r.Status == StatusCompleted }

// Refusal is the refusal a completed run ended on, or nil. Two conclusions
// carry one (D9): a fork's run is `refused`, never queued; and a run the
// driver ended before any step ran -- a manifest that does not compile, a
// grant that no longer reaches the repository, a disconnected pipeline -- is
// a `failure` WITH its refusal, so its check run says what is wrong and what
// to do rather than "Failed" over an empty table. The refusal fields are
// written only then: a run failed by its steps carries the codes on its steps
// and none here, and its check run reports the stage table.
func (r Run) Refusal() *pipelines.Refusal {
	if r.Status != StatusCompleted || strings.TrimSpace(r.RefusalCode) == "" {
		return nil
	}
	return &pipelines.Refusal{Code: r.RefusalCode, Detail: r.RefusalMessage, Scope: r.RefusalScope}
}

// Note is an informational fact on a run that changes no outcome -- a
// note-class code (pipelines.ClassNote) and its sentence.
type Note struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StageSummary is one stage as the run row records it, in manifest order:
// what the check run's stage table and the OS run page render.
type StageSummary struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMs int64  `json:"durationMs"`
	// Steps is how many steps the stage compiled to; Failed how many of
	// them failed or were refused.
	Steps  int `json:"steps"`
	Failed int `json:"failed"`
}

// StepState is one compiled step's progress as the driver tracks it in
// memory. It is never a row: a step's row is its v1:work:step. LogTail is
// UNMASKED until the check run is composed -- the caller masks it with the
// step's secret values (pipelines.MaskSecrets) before Report reaches GitHub.
type StepState struct {
	Key        string
	Stage      string
	Name       string
	Status     string
	DurationMs int64
	Code       string
	Message    string
	LogTail    string
}

// Report is the step as the check run reports it.
func (s StepState) Report() pipelines.StepReport {
	return pipelines.StepReport{
		Key: s.Key, Stage: s.Stage, Name: s.Name, Status: s.Status,
		DurationMs: s.DurationMs, Code: s.Code, Message: s.Message, LogTail: s.LogTail,
	}
}

// InboundDelivery is one staged v1:platform:inboundRequest row as the trigger
// reads it: what a GitHub webhook left on the inbound seam. The trigger acts on
// THIS row -- the body the receiver verified and the headers it staged beside
// it -- and never on a body or headers handed to it as arguments, because a
// builtin's arguments are whatever its caller chose.
type InboundDelivery struct {
	ID     string
	Source string
	// Body is exactly as staged: the bytes the signature covered, untrimmed.
	Body        string
	HeadersJSON string
	// SignatureVerified is the receiver's verdict. False on a source an
	// operator configured with no signature scheme, which signs nothing.
	SignatureVerified bool
}

// PackageSource is the v1:platform:package row a pipeline hangs off -- the
// fields connect reads, under the CALLER's actor.
type PackageSource struct {
	ID           string
	OwnerUserID  string
	AccountID    string
	Name         string
	SourceKind   string
	RepoURL      string
	CredentialID string
	Status       string
}

// PipelinePatch is a read-merge write to a pipeline. A nil field is not
// written and keeps its value; a non-nil one is written, its zero value
// included -- that is how a writer CLEARS a field, because updatePipeline
// carries no defaults (a `??` default there would erase a co-writer's value).
// The merge is shallow: Heads and Timings are always sent whole.
type PipelinePatch struct {
	Name             *string
	DefaultBranch    *string
	Delivery         *string
	Compute          *pipelines.Compute
	Status           *string
	SecretNames      *[]string
	ChannelIDs       *[]string
	Heads            *map[string]string
	Timings          *map[string]float64
	TimingsRunID     *string
	TimingsUpdatedAt *time.Time
}

// RunPatch is a read-merge write to a run, on PipelinePatch's rule: nil is
// not written, non-nil is written as given. Notes and Stages are sent whole.
type RunPatch struct {
	Status            *string
	Conclusion        *string
	RefusalCode       *string
	RefusalMessage    *string
	RefusalScope      *string
	CheckRunID        *int64
	CheckRunState     *string
	Notes             *[]Note
	WorkRunID         *string
	WorkGoalID        *string
	DriverNodeID      *string
	DriverHeartbeatAt *time.Time
	CancelRequested   *bool
	CancelledBy       *string
	Stages            *[]StageSummary
	StartedAt         *time.Time
	FinishedAt        *time.Time
	DurationMs        *int64
}

// ptr is a pointer to v, for a patch field.
func ptr[T any](v T) *T { return &v }

// sameID reports whether two spellings name the same row: bare or
// canonical, each side reduced to its short id. Blank matches nothing.
func sameID(a, b string) bool {
	a, b = bareID(a), bareID(b)
	return a != "" && a == b
}

// bareID is an id with its concept prefix removed; an id already bare is
// returned as itself.
func bareID(id string) string { return memql.BareShortId(strings.TrimSpace(id)) }

// withNote adds note to notes unless a note of the same code is already
// there: a run that met a 403 on every check-run write carries the fact
// once.
func withNote(notes []Note, note Note) []Note {
	for _, n := range notes {
		if n.Code == note.Code {
			return notes
		}
	}
	out := make([]Note, 0, len(notes)+1)
	out = append(out, notes...)
	return append(out, note)
}

package pipelinerun

import (
	"errors"
	"fmt"
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
	TriggerWebhook  = "webhook"
	TriggerPoll     = "poll"
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerRerun    = "rerun"

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
	// DefinitionFingerprint binds the complete execution definition recorded
	// before admission. Older receipts without it cannot authorize recovery.
	DefinitionFingerprint string
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
	// ArtifactFileIDs are the Library files the step's receipt names as its
	// artifacts, as the driver wrote them (already masked): what a resumed
	// run's notification lists for a step its predecessor ran.
	ArtifactFileIDs []string
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
	DriverLeaseID     string
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
	// ArtifactFileIDs are the Library files the step's receipt names as its
	// artifacts (masked, as the receipt is): what a notify stage later in
	// the run lists in its message.
	ArtifactFileIDs []string
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
	DriverLeaseID     *string
	DriverHeartbeatAt *time.Time
	CancelRequested   *bool
	CancelledBy       *string
	Stages            *[]StageSummary
	StartedAt         *time.Time
	FinishedAt        *time.Time
	DurationMs        *int64
}

// Channel is one v1:pipelines:channel row as this package reads it: where a
// notify stage delivers (D16). Ids are bare, OwnerUserID is as stored -- see
// Pipeline.
//
// A channel carries a REFERENCE to what it needs, never the value: SecretRef
// is the NAME of the globalSecret holding a Discord webhook's URL, and the URL
// is resolved at send time by the outbound worker.
type Channel struct {
	ID          string
	OwnerUserID string
	AccountID   string
	// Name is what a manifest's notify stage says; unique among one owner's
	// channels, which the channel builtin checks before it writes.
	Name string
	// Kind is `discord` or `email`.
	Kind string
	// SecretRef is a Discord channel's globalSecret name. Empty for email.
	SecretRef string
	// Status is `active`, or `archived`: kept for the runs that name it, and
	// delivering nothing.
	Status string
	// Recipients are an email channel's addresses. Empty for Discord.
	Recipients []string
}

// ChannelPatch is a read-merge write to a channel, on PipelinePatch's rule: nil
// is not written and keeps its value, non-nil is written as given -- an empty
// Recipients clears the list.
type ChannelPatch struct {
	Name       *string
	Kind       *string
	SecretRef  *string
	Status     *string
	Recipients *[]string
}

// OutboundStatus is the delivery state of one v1:platform:outboundRequest, as
// the outbound worker leaves it: what the notify stage polls to learn whether a
// notification it staged went. Status is the row's own word -- pending,
// sending, sent, retrying or failed -- and EMPTY when no such row exists, which
// is an answer rather than an error: the row is the only proof of a delivery,
// so absent is never read as sent.
type OutboundStatus struct {
	ID        string
	Status    string
	LastError string
	Attempts  int
	// SentAt is when the transport accepted the delivery; zero until it did.
	SentAt time.Time
}

// NotificationRequest is one outbound row the notify stage stages: a Discord
// webhook post or one email. A webhook to a channel's Discord URL names the
// globalSecret holding it (TargetSecret) and carries no Target of its own, the
// URL being a credential that must never sit on a row; every other row names
// its Target, an address, and a Medium.
type NotificationRequest struct {
	// RequestID is the row's id, chosen by the caller, and it must be
	// UNGUESSABLE: a row keeps its delivery state at its id (@createOnly), and a
	// client can pre-stage a plain row at a guessable id and leave it `sent`, so
	// the stage the server makes there later inherits it. A caller uses a random
	// id, one per row, never one derived from what it is about to say.
	RequestID string
	// Medium is `webhook` or `email`.
	Medium string
	// Target is where a plain row goes, an email address for the notify stage.
	// A row naming TargetSecret carries none, and staging both is refused: its
	// target is the descriptor the mutation stamps from the secret's name, and a
	// Target beside it would be dropped without a word.
	Target string
	// TargetSecret is the globalSecret NAME whose value is a webhook's URL. A
	// row naming one is a webhook, and Medium must say so.
	TargetSecret string
	Subject      string
	Body         string
	// DedupeKey is passed to the receiver as an idempotency key.
	DedupeKey string
	// RequestedBy is provenance: which run staged the row.
	RequestedBy string
}

// validate refuses a notification that cannot be sent as written, before any
// engine is asked. The DSL declares the arguments; these are the rules it
// cannot: a webhook to a secret is a webhook and nothing else and names no
// target beside the secret, and a plain row says where it goes.
func (n NotificationRequest) validate() error {
	if bareID(n.RequestID) == "" {
		return errors.New("pipelines: a notification needs a request id")
	}
	if strings.TrimSpace(n.Body) == "" {
		return errors.New("pipelines: a notification needs something to say")
	}
	medium := strings.TrimSpace(n.Medium)
	if strings.TrimSpace(n.TargetSecret) != "" {
		if medium != secretTargetMedium {
			return fmt.Errorf("pipelines: a notification to a secret is a webhook, not %q: only a webhook's URL is a secret", n.Medium)
		}
		if strings.TrimSpace(n.Target) != "" {
			return errors.New("pipelines: a notification names the secret holding its URL or a target, not both: the row's target is the descriptor stamped from the secret's name")
		}
		return nil
	}
	if medium == "" {
		return errors.New("pipelines: a notification needs a medium")
	}
	if strings.TrimSpace(n.Target) == "" {
		return errors.New("pipelines: a notification needs somewhere to go: a target, or the secret holding it")
	}
	return nil
}

// secretTargetMedium is the one outbound medium a row naming a globalSecret can
// have: stageOutboundRequestToSecret stamps it, so a caller asking for another
// is asking for something the row cannot be.
const secretTargetMedium = "webhook"

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

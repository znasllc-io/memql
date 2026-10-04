package pipelines

// Mode is how much of the suite a run executes (design record D1, D5).
type Mode string

const (
	// ModeAffected runs only what a change affects: a pull request.
	ModeAffected Mode = "affected"
	// ModeFull runs everything, once, on the exact tree that lands: the merge
	// queue, a push to the default branch and a published release.
	ModeFull Mode = "full"
)

// Event is the GitHub event a run was opened for (D5, plus `release` from the
// documentation program's D15). A re-requested check run is not an event of
// its own: it re-runs the original run's event and mode.
type Event string

const (
	EventPullRequest Event = "pull_request"
	EventMergeGroup  Event = "merge_group"
	EventPush        Event = "push"
	EventRelease     Event = "release"
)

// Compute is where a pipeline's steps may run (D10, D14). Absent means
// cluster: a step naming a need the cluster cannot meet is refused unless the
// owner consented to the fleet, because nothing about a laptop is a default.
type Compute string

const (
	ComputeCluster         Compute = "cluster"
	ComputeClusterAndFleet Compute = "cluster_and_fleet"
)

// StepKind says what executes a compiled step.
type StepKind string

const (
	// StepCommand is a shell command in the image's working copy -- the same
	// contract as a deployable's build.command (D7).
	StepCommand StepKind = "command"
	// StepNotify is a stage that names a channel instead of steps (D16).
	StepNotify StepKind = "notify"
)

// WorkTriggerPrefix marks a v1:work:run the pipelines runner opened: its
// triggeredBy is "pipeline:<mode>". Such a run is the runner's own -- the
// work dispatcher never executes it and the spine's sweep never judges it --
// in the way integrations/work treats a procedure replay's run.
const WorkTriggerPrefix = "pipeline:"

// Repository names a GitHub repository.
type Repository struct {
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	CloneURL string `json:"cloneUrl"`
}

// FullName is "owner/name".
func (r Repository) FullName() string { return r.Owner + "/" + r.Name }

// Service is a sidecar a step declares by name (D7, D10).
//
// Env is plain configuration -- a database's bootstrap password, a port --
// never a secret: a secret is a reference on the step, resolved under the
// owner's allowlist. Ready is an optional shell probe the runner waits on
// before the step's own command starts ("pg_isready -U memql").
type Service struct {
	Image string            `json:"image" yaml:"image"`
	Env   map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Ready string            `json:"ready,omitempty" yaml:"ready,omitempty"`
}

// ShardRef places a step in its shard set. Index is 1-based; Count is 0 for a
// step that was not sharded.
type ShardRef struct {
	Index int `json:"index"`
	Count int `json:"count"`
}

// Skip is a step the plan decided not to run, with its reason in words (D13).
// A skipped step is never a refusal in disguise: a manifest that cannot
// compile is a failed run (D9), and a skip is reserved for work that had
// nothing to do.
type Skip struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// Step is one compiled step: one v1:work:step row and one Execute call.
type Step struct {
	// Key is the v1:work:step key: "stage/step", or "stage/step#i" for the
	// i-th shard.
	Key   string   `json:"key"`
	Stage string   `json:"stage"`
	Name  string   `json:"name"`
	Kind  StepKind `json:"kind"`
	// Run is the shell command, for a command step.
	Run      string             `json:"run,omitempty"`
	Image    string             `json:"image,omitempty"`
	Services map[string]Service `json:"services,omitempty"`
	Caches   []string           `json:"caches,omitempty"`
	// Needs is the step's environment hint from the closed set Needs()
	// returns, sorted. Empty means the cluster can run it.
	Needs          []string `json:"needs,omitempty"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
	Artifacts      []string `json:"artifacts,omitempty"`
	// Secrets are globalSecret NAMES. A value never appears on a Step.
	Secrets []string `json:"secrets,omitempty"`
	// Packages is the step's package slice, as Go import paths, rendered to
	// MEMQL_PACKAGES. Nil for a step that selects no packages.
	Packages []string `json:"packages,omitempty"`
	Shard    ShardRef `json:"shard"`
	// Channel names the v1:pipelines:channel a notify step delivers to.
	Channel string `json:"channel,omitempty"`
	// DependsOn are the keys of the steps this one waits for: the steps of
	// the stage before it.
	DependsOn []string `json:"dependsOn,omitempty"`
	// Skip is non-nil when the plan decided not to run the step.
	Skip *Skip `json:"skip,omitempty"`
}

// StepRequest is what the driver hands an Executor for one step. It crosses
// NodeService as JSON, so it carries plain data only.
type StepRequest struct {
	// RunID is the v1:pipelines:run id (bare): what log lines bind to.
	RunID string `json:"runId"`
	// WorkRunID is the v1:work:run id (bare): what Library files bind to.
	WorkRunID string `json:"workRunId"`
	// StepKey equals the v1:work:step key, so producedByStepKey lines up.
	StepKey string `json:"stepKey"`
	// Attempt is the step's attempt, 1-based. A resumed driver re-sends the
	// SAME attempt for a step that has no receipt, so a runner can re-attach
	// to work already in flight rather than start it twice.
	Attempt int `json:"attempt"`
	// RunAttempt is the pipeline run's attempt (a re-run is attempt + 1).
	RunAttempt int `json:"runAttempt"`
	// RunStartedAt is RFC3339: a step's deadline is the lesser of its own
	// timeout and the run's ceiling, measured from here.
	RunStartedAt   string     `json:"runStartedAt"`
	PipelineID     string     `json:"pipelineId"`
	OwnerUserID    string     `json:"ownerUserId"`
	Repository     Repository `json:"repository"`
	SHA            string     `json:"sha"`
	Mode           Mode       `json:"mode"`
	Event          Event      `json:"event"`
	Version        string     `json:"version"`
	InstallationID int64      `json:"installationId"`
	Compute        Compute    `json:"compute"`
	Step           Step       `json:"step"`
	// Secrets are the resolved values of Step.Secrets, by name, resolved
	// under the owner's allowlist on the driver's side. They are never
	// journaled: a step row records only the names.
	Secrets map[string]string `json:"secrets,omitempty"`
}

// Outcome is how a step ended.
type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeRefused is a step the runner would not start: a need nobody can
	// meet, an image it may not pull. It counts as a failure of the run.
	OutcomeRefused Outcome = "refused"
)

// Failure is a typed failure from the refusal catalogue.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Where says where a step ran.
type Where struct {
	// Surface is "cluster" or "fleet".
	Surface       string            `json:"surface"`
	NodeID        string            `json:"nodeId,omitempty"`
	JobName       string            `json:"jobName,omitempty"`
	WorkerID      string            `json:"workerId,omitempty"`
	MachineLabels map[string]string `json:"machineLabels,omitempty"`
}

// StepResult is what an Executor answers for one step.
type StepResult struct {
	Status Outcome `json:"status"`
	// ExitCode is the command's exit status, or -1 when it never ran.
	ExitCode int      `json:"exitCode"`
	Failure  *Failure `json:"failure,omitempty"`
	// StartedAt and FinishedAt are RFC3339.
	StartedAt       string   `json:"startedAt,omitempty"`
	FinishedAt      string   `json:"finishedAt,omitempty"`
	Where           Where    `json:"where"`
	LogFileID       string   `json:"logFileId,omitempty"`
	ArtifactFileIDs []string `json:"artifactFileIds,omitempty"`
	// LogTail is the last lines of output (at most 40), for a failed step's
	// inline excerpt (D13).
	LogTail  string `json:"logTail,omitempty"`
	LogLines int    `json:"logLines,omitempty"`
	// LogCapped says the live log hit its cap; the Library copy is complete.
	LogCapped bool `json:"logCapped,omitempty"`
	// Timings is each passing Go package's wall time in seconds, by import
	// path, read from the step's output with ParseGoTestOutput. The driver
	// writes them back into the pipeline's timing table after a full run.
	Timings map[string]float64 `json:"timings,omitempty"`
}

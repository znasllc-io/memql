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

// Event is the cause a run was opened for (GitHub events, plus MemQL's
// manifest-opted-in daily schedule and owner-requested manual rehearsal). A
// re-requested check run is not an event of its own: it re-runs the original
// run's event and mode.
type Event string

const (
	EventPullRequest Event = "pull_request"
	EventMergeGroup  Event = "merge_group"
	EventPush        Event = "push"
	EventRelease     Event = "release"
	EventSchedule    Event = "schedule"
	EventManual      Event = "manual"
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
	// StepSARIF publishes an immutable scanner artifact through the driver.
	StepSARIF            StepKind = "sarif"
	StepRepositoryChange StepKind = "repositoryChange"
)

// WorkTriggerPrefix marks a v1:work:run the pipelines runner opened: its
// triggeredBy is "pipeline:<mode>". Such a run is the runner's own -- the
// work dispatcher never executes it and the spine's sweep never judges it --
// in the way integrations/work treats a procedure replay's run.
const WorkTriggerPrefix = "pipeline:"

// StepKey is a step's v1:work:step key: "<stage>.<step>".
//
// NOT "<stage>/<step>": the work spine reads a "/" in a step key as a NESTED
// step -- a row of a logic a statement called -- and its loaders, the head
// rules and the Nexus drawing all treat one that way. A dot is outside the
// stage and step name grammar, so a key still splits back unambiguously, and
// it means nothing to the spine. A refusal's Scope stays "stage/step": it is
// a path into the manifest, like "select/buckets/os", not a key.
func StepKey(stage, step string) string { return stage + "." + step }

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
	Verification     bool              `json:"verification,omitempty"`
	GitHubReadToken  bool              `json:"githubReadToken,omitempty"`
	CheckoutHistory  string            `json:"checkoutHistory,omitempty"`
	RepositoryChange *RepositoryChange `json:"repositoryChange,omitempty"`
	SARIF            *SARIFReport      `json:"sarif,omitempty"`
	CPUMilli         int               `json:"cpuMilli,omitempty"`
	ImageBuild       *ImageBuild       `json:"imageBuild,omitempty"`
	MemoryMiB        int               `json:"memoryMiB,omitempty"`
	// RunAfterFailure is inherited from the stage and bound into the durable
	// execution fingerprint, so recovery cannot change failure behavior.
	RunAfterFailure bool   `json:"runAfterFailure,omitempty"`
	Placement       string `json:"placement,omitempty"`
	Execution       string `json:"execution,omitempty"`
	Platform        string `json:"platform,omitempty"`
	// Key is the v1:work:step key, StepKey(stage, step): "stage.step", or
	// "stage.step#i" for the i-th shard.
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
	Secrets         []string `json:"secrets,omitempty"`
	ImagePullSecret string   `json:"imagePullSecret,omitempty"`
	// Packages is the step's package slice, as Go import paths, rendered to
	// MEMQL_PACKAGES. Nil for a step that selects no packages.
	Packages []string `json:"packages,omitempty"`
	Shard    ShardRef `json:"shard"`
	// Channel names the v1:pipelines:channel a notify step delivers to.
	Channel string `json:"channel,omitempty"`
	// Links are the notify stage's own links (a Docs link), rendered into its
	// message beside the run page. A command step carries none.
	Links []Link `json:"links,omitempty"`
	// DependsOn are the keys of the steps this one waits for: the steps of
	// the stage before it.
	DependsOn []string `json:"dependsOn,omitempty"`
	// Skip is non-nil when the plan decided not to run the step.
	Skip *Skip `json:"skip,omitempty"`
}

// StepRequest is what the driver hands an Executor for one step. It crosses
// NodeService as JSON, so it carries plain data only.
type StepRequest struct {
	// ComputePlacement comes from the installed delivery-policy snapshot, never
	// the tested repository. It selects capacity only for a pool-placed step;
	// machine consent, resource limits and cloud admission remain mandatory.
	ComputePlacement *ComputePlacementPolicy `json:"computePlacement,omitempty"`
	// Placement is where the driver sends THIS attempt (AttemptPlacement). Nil is
	// the step's own placement: how every step's first attempt goes. Unlike
	// ComputePlacement it is per attempt, set only by the engine's driver from
	// the attempts it has already journaled, and it never widens what the
	// manifest placed or the installation's policy allowed.
	Placement *AttemptPlacement `json:"placement,omitempty"`
	// RecoverOnly means a previous driver recorded an intent without a receipt.
	// The executor may adopt existing work, but cannot start a replacement effect.
	RecoverOnly bool `json:"recoverOnly,omitempty"`
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
	RunStartedAt string     `json:"runStartedAt"`
	PipelineID   string     `json:"pipelineId"`
	OwnerUserID  string     `json:"ownerUserId"`
	Repository   Repository `json:"repository"`
	SHA          string     `json:"sha"`
	BaseSHA      string     `json:"baseSha,omitempty"`
	Mode         Mode       `json:"mode"`
	Event        Event      `json:"event"`
	// HeadBranch is the immutable run's head, used with Event for train admission.
	// It is trusted driver metadata, never derived from a step's environment.
	HeadBranch string `json:"headBranch,omitempty"`
	Version    string `json:"version"`
	// Domain is the front-door domain of the cluster running the pipeline
	// (its MEMQL_DOMAIN), "" when none is configured. A step reads it as
	// MEMQL_DOMAIN to reach this cluster's public hosts -- verify-rollout's
	// api.<domain>, identity.<domain> and os.<domain> -- from outside, the
	// way any client does; a step reaches nothing inside the cluster.
	Domain         string  `json:"domain,omitempty"`
	InstallationID int64   `json:"installationId"`
	Compute        Compute `json:"compute"`
	Step           Step    `json:"step"`
	// Secrets are the resolved values of Step.SecretNames(), by name, resolved
	// under the owner's allowlist on the driver's side. They are never
	// journaled: a step row records only the names.
	Secrets map[string]string `json:"secrets,omitempty"`
}

// ComputePlacementPolicy is the installation's choice of how long to prefer
// compatible Fleet capacity before considering a budget-admitted cloud job.
// A false CloudFallback keeps waiting for Fleet within the existing run bound.
type ComputePlacementPolicy struct {
	FleetWaitSeconds int  `json:"fleetWaitSeconds"`
	CloudFallback    bool `json:"cloudFallback"`
}

// AttemptPlacement is where the driver sends one attempt of a step, and the
// only way it says so (spec section 5 of the 2026-10-28 fleet-first retries
// design). A step is retried by the driver, not by the executor, because only
// the driver has the journal of what the earlier attempts proved; so each
// attempt arrives with its place already chosen, and the executor takes it
// literally: an attempt placed on the fleet never spills to the cluster inside
// that attempt, and one placed on the cluster never asks a machine.
//
// What it cannot do is widen. The executor still refuses a place the step's own
// manifest placement does not allow (a step that needs a machine is never given
// to the cluster; a plain cluster step is never given to a machine) and, for a
// pool step, a cluster attempt the installed policy did not allow.
type AttemptPlacement struct {
	// Surface is "" (the step's own placement), "fleet" or "cluster".
	Surface string `json:"surface,omitempty"`
	// PinWorker, when set, is the one machine the attempt may run on: a
	// same-machine retry. It is a registration id an earlier attempt reported as
	// Where.WorkerID, never a name from a model or a request. It narrows the
	// machines the owner's own routing already offered and admits none: a pinned
	// machine that stopped allowing pipelines refuses, and the attempt is
	// nothing ran.
	PinWorker string `json:"pinWorker,omitempty"`
	// ExcludeWorkers are machines this step has already been tried on, in the
	// order they were tried: the attempt may run on any other eligible machine.
	ExcludeWorkers []string `json:"excludeWorkers,omitempty"`
	// MaxWaitSeconds bounds how long a fleet attempt waits for a busy or
	// disconnected machine before it counts as nothing ran there. Zero is no
	// bound beyond the run's ceiling, which is a fleet step's first attempt. It
	// is a wait for capacity: it never shortens a command that has started.
	MaxWaitSeconds int `json:"maxWaitSeconds,omitempty"`
}

// AttemptClass says what a step's attempt that did not succeed proves about
// whether its command could have run (spec section 4). The executor sets it
// because only the executor knows whether a dispatch reached a machine and what
// the machine or the Job answered; the driver retries from it and from nothing
// else. An empty class reads as AttemptFinal, so an older or unexpected answer
// is never retried (spec D3).
type AttemptClass string

const (
	// AttemptNothingRan: the command did not start, so another attempt cannot
	// run it twice. Any step may be retried after this.
	AttemptNothingRan AttemptClass = "nothing_ran"
	// AttemptRetryRequested: the command ran and exited ExitRetryRequested,
	// which is the command saying another run is safe and useful. Any step may
	// be retried after this; a command cannot ask for more than the policy gives.
	AttemptRetryRequested AttemptClass = "retry_requested"
	// AttemptOutcomeUnknown: the command may have run, and nothing says how it
	// ended (the connection dropped after the dispatch left, the node under the
	// Job went away, the command exited 0 and its artifacts did not arrive). Only
	// a step that declares repeating it harmless (spec D5) is retried.
	AttemptOutcomeUnknown AttemptClass = "outcome_unknown"
	// AttemptFinal: nothing about this ending is worth another try -- an
	// ordinary failing exit, a timeout, a cancel, a refusal, or a code nobody
	// classified. The step fails as it did before attempts existed.
	AttemptFinal AttemptClass = "final"
)

// ExitRetryRequested is the exit status with which a command says "transient,
// retry me": EX_TEMPFAIL, from sysexits.h. Exiting it needs no declaration in
// the manifest, because the command itself is saying another run is safe, and
// it is read as AttemptRetryRequested only when it is the whole answer (no
// typed failure beside it).
const ExitRetryRequested = 75

// MaxStepAttempts is the engine's cap on one step's attempts, whatever an
// installation's policy says (spec D6): a policy can only lower it.
const MaxStepAttempts = 8

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
	// ArtifactIntentIDs name durable, immutable upload records. Unlike
	// editable Library links, these IDs are never trimmed from an outcome.
	// Candidate consumers resolve them under the owning run/step/attempt.
	ArtifactIntentIDs []string `json:"artifactIntentIds,omitempty"`
	// LogTail is the last lines of output -- at most 40 lines and at most 16
	// KiB, because the runner's outcome rides a Job annotation and all of a
	// Job's annotations share 256 KiB -- for a failed step's inline excerpt
	// (D13).
	LogTail  string `json:"logTail,omitempty"`
	LogLines int    `json:"logLines,omitempty"`
	// LogCapped says the live log hit its cap; the Library copy is complete.
	LogCapped bool `json:"logCapped,omitempty"`
	// Timings is each passing Go package's wall time in seconds, by import
	// path, read from the step's output with ParseGoTestOutput. The driver
	// writes them back into the pipeline's timing table after a full run.
	Timings map[string]float64 `json:"timings,omitempty"`
	// Notes are note-class codes (ClassNote): facts a person should see
	// beside the step that change no outcome -- a declared artifact path that
	// matched nothing, beside a green step.
	Notes []Failure `json:"notes,omitempty"`
	// AttemptClass is the executor's reading of an attempt that did not succeed
	// (AttemptClass). The executor sets it on every such result and on no
	// other; it is never taken from a Job runner's reply, which may be another
	// version's. Empty reads as AttemptFinal.
	AttemptClass AttemptClass `json:"attemptClass,omitempty"`
	// NotRetriedHere says the machine this attempt named (Where.WorkerID) should
	// not be tried again for this step: it refused for a reason that outlasts a
	// retry's delay (its Host commands are off, it no longer allows pipelines,
	// it needs recovering), or the wait the driver allowed for it ran out. The
	// driver moves to another machine at once. It is a statement about the
	// machine, so it is false on the cluster, where the place is the cluster
	// as a whole.
	NotRetriedHere bool `json:"notRetriedHere,omitempty"`
}

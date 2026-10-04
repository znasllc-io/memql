package pipelinesteps

import (
	"fmt"
	"log/slog"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// The runner's own wire: what the agent node forwards to a workbench replica
// over NodeService, as JSON, and what comes back.
//
// The four forward action NAMES are integrations/workbench's
// (PipelineStepAction "pipelineStep", PipelineStatusAction "pipelineStatus",
// PipelineAckAction "pipelineAck", PipelineCancelAction "pipelineCancel"). The
// transport owns its vocabulary, and integrations/workbench cannot import this
// package, so a second spelling here would be a literal that drifts silently.
//
// The step's identity and contract come from the seam (component/pipelines):
// pl.Repository, pl.Service, pl.Failure, pl.Where and pl.StepResult. The JSON
// field names are the wire, so renaming a tag is a protocol change on both
// sides of a rolling deploy at once.

// StepRun is what the AGENT computes from a pl.StepRequest and the runner
// needs, and nothing more: the pipelineStep forward's payload. The runner
// answers it with a pl.StepResult, whose LogTail is one string of at most the
// last 40 lines and whose Notes carry the note-class codes.
type StepRun struct {
	// RunID is the v1:pipelines:run id, bare: the subject log lines bind to.
	RunID string `json:"runId"`
	// WorkRunID is the v1:work:run id, bare: what Library files record as
	// producedByRunId.
	WorkRunID string `json:"workRunId"`
	// StepKey equals the v1:work:step key, so producedByStepKey lines up.
	StepKey string `json:"stepKey"`
	// Attempt is the step's attempt, 1-based. With RunID and StepKey it names
	// the Job (JobName): a retried step gets a fresh Job, a resumed driver
	// re-attaches to the same one.
	Attempt     int           `json:"attempt"`
	OwnerUserID string        `json:"ownerUserId"`
	Repository  pl.Repository `json:"repository"`
	SHA         string        `json:"sha"`
	// InstallationID is the GitHub App installation the clone token is minted
	// under; 0 is an anonymous clone of a public repository.
	InstallationID int64  `json:"installationId,omitempty"`
	Image          string `json:"image"`
	Command        string `json:"command"`
	// Env is StepRequest.Environment() minus the secrets: the contract every
	// runner exports, as plain values.
	Env map[string]string `json:"env"`
	// Secrets are the resolved values, by env name. They reach the step only
	// through the step's Secret and are never logged.
	Secrets  map[string]string     `json:"secrets,omitempty"`
	Services map[string]pl.Service `json:"services,omitempty"`
	// Caches name the caches to mount; the known ones are go and npm.
	Caches []string `json:"caches,omitempty"`
	// Artifacts are relative paths or globs in the working copy.
	Artifacts []string `json:"artifacts,omitempty"`
	// TimeoutSeconds is the EFFECTIVE timeout: the lesser of the step's own
	// timeout and what is left of the run's ceiling.
	TimeoutSeconds int `json:"timeoutSeconds"`
	// DeadlineCode names whichever bound TimeoutSeconds is:
	// pl.CodeStepTimeout or pl.CodeRunCeiling.
	DeadlineCode string `json:"deadlineCode"`
	// GoTimings asks the runner to read Go test timings out of the output.
	GoTimings bool `json:"goTimings,omitempty"`
}

// A StepRun carries resolved secret values because the runner needs them, so
// printing one must not print them (ruling R17). Format and LogValue show each
// secret's NAME, so a reader can tell it was there, and never its value. JSON
// keeps the values: it is the forward's wire, the one place they travel.

// redactedStepRun has StepRun's fields and none of its methods, so formatting
// one cannot come back into Format.
type redactedStepRun StepRun

func (r StepRun) redacted() redactedStepRun {
	out := redactedStepRun(r)
	if r.Secrets != nil {
		out.Secrets = make(map[string]string, len(r.Secrets))
		for name := range r.Secrets {
			out.Secrets[name] = "[redacted]"
		}
	}
	return out
}

// Format prints a StepRun with every secret value replaced, whatever the verb
// and flags.
func (r StepRun) Format(f fmt.State, verb rune) {
	fmt.Fprintf(f, fmt.FormatString(f, verb), r.redacted())
}

// LogValue is what slog records for a StepRun: who and what it is, and its
// secrets by name only.
func (r StepRun) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("runId", r.RunID),
		slog.String("workRunId", r.WorkRunID),
		slog.String("stepKey", r.StepKey),
		slog.Int("attempt", r.Attempt),
		slog.String("ownerUserId", r.OwnerUserID),
		slog.String("repository", r.Repository.FullName()),
		slog.String("sha", r.SHA),
		slog.String("image", r.Image),
		slog.Any("secretNames", sortedKeys(r.Secrets)),
		slog.Int("timeoutSeconds", r.TimeoutSeconds),
		slog.String("deadlineCode", r.DeadlineCode),
	)
}

// The states a pipelineStatus reply reports for a step's Job.
const (
	// StateRunning: a runner holds the Job and its heartbeat is fresh.
	StateRunning = "running"
	// StateFinished: the outcome is persisted on the Job; Result carries it.
	StateFinished = "finished"
	// StateAbsent: there is no such Job (never created, acked, or collected).
	StateAbsent = "absent"
	// StateStale: the Job exists but its runner's heartbeat went stale, so
	// another replica may adopt it.
	StateStale = "stale"
)

// StatusRequest asks a workbench replica about one step's Job.
type StatusRequest struct {
	JobName string `json:"jobName"`
}

// StatusReply is the answer: one of the State* values, and the outcome when
// the state is finished.
type StatusReply struct {
	State  string         `json:"state"`
	Result *pl.StepResult `json:"result,omitempty"`
}

// AckRequest tells the runner the agent holds the outcome, so the Job and its
// Secret can go now rather than at the TTL.
type AckRequest struct {
	JobName string `json:"jobName"`
}

// CancelRequest stops every step of a run, on whichever replica holds it.
type CancelRequest struct {
	RunID string `json:"runId"`
}

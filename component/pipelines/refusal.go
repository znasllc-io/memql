package pipelines

import (
	"fmt"
	"sort"
)

// Refusal is a typed refusal in Deployables' vocabulary (D9): a code from the
// catalogue below, the sentence a person reads, and the scope it is about --
// "stage/step", a stage name, or "" for the whole pipeline.
//
// Every code is ALSO spelled as a literal in component/packages/refusal.go,
// the one catalogue MemQL OS reads its copy from; a parity test there holds
// the two lists together, and the OS copy test fails on a code with no copy.
type Refusal struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Scope  string `json:"scope,omitempty"`
}

// Refuse builds a refusal with a formatted detail.
func Refuse(code, scope, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Detail: fmt.Sprintf(format, args...), Scope: scope}
}

// Error makes a refusal usable as an error.
func (r *Refusal) Error() string {
	if r == nil {
		return ""
	}
	if r.Scope != "" {
		return r.Code + " (" + r.Scope + "): " + r.Detail
	}
	return r.Code + ": " + r.Detail
}

// Class says what a code means for a run.
type Class string

const (
	// ClassRefusal ends the run as failed before any step executes: a
	// manifest that cannot compile, a fork, a secret the owner did not allow.
	ClassRefusal Class = "refusal"
	// ClassFailure is a step that ran, or tried to, and did not succeed.
	ClassFailure Class = "failure"
	// ClassSkip is a step the plan or the run decided not to execute.
	ClassSkip Class = "skip"
	// ClassNote is informational and changes no outcome.
	ClassNote Class = "note"
)

// The engine's codes (epic memql#5477).
const (
	// The manifest and the plan.
	CodeNotDeclared       = "pipeline_not_declared"
	CodeStageInvalid      = "pipeline_stage_invalid"
	CodeStepInvalid       = "pipeline_step_invalid"
	CodeSelectInvalid     = "pipeline_select_invalid"
	CodeSelectMissing     = "pipeline_select_missing"
	CodeEventUnknown      = "pipeline_event_unknown"
	CodeBucketUnknown     = "pipeline_bucket_unknown"
	CodeServiceUnknown    = "pipeline_service_unknown"
	CodeNeedUnknown       = "pipeline_need_unknown"
	CodeSecretInvalid     = "pipeline_secret_invalid"
	CodeSecretNotAllowed  = "pipeline_secret_not_allowed"
	CodeFleetNotConsented = "pipeline_fleet_not_consented"

	// The trigger (D6).
	CodeForkRefused = "pipeline_fork_refused"

	// The run.
	CodeRunnerUnavailable = "pipeline_runner_unavailable"
	CodeExecutorError     = "pipeline_executor_error"
	CodeSecretMissing     = "pipeline_secret_missing"
	CodeDisconnected      = "pipeline_disconnected"
	CodeCheckPermission   = "pipeline_check_permission_missing"

	// Skips.
	CodeStageBlocked      = "pipeline_stage_blocked"
	CodeNotAffected       = "pipeline_not_affected"
	CodeNotifyUnavailable = "pipeline_notify_unavailable"
)

// The substrate's codes (epic memql#5478), declared here so the catalogue,
// its parity test and the OS copy land once.
const (
	CodeStepTimeout      = "pipeline_step_timeout"
	CodeRunCeiling       = "pipeline_run_ceiling"
	CodeNoMachineForNeed = "pipeline_no_machine_for_need"
	CodeFleetDisabled    = "pipeline_fleet_disabled"
	CodeJobRejected      = "pipeline_job_rejected"
	CodeJobUnschedulable = "pipeline_job_unschedulable"
	CodeImagePullFailed  = "pipeline_image_pull_failed"
	CodeCloneFailed      = "pipeline_clone_failed"
	CodeServiceFailed    = "pipeline_service_failed"
	CodeStepCancelled    = "pipeline_step_cancelled"
	CodeNodeLost         = "pipeline_node_lost"
	CodeArtifactTooLarge = "pipeline_artifact_too_large"
	CodeArtifactMissing  = "pipeline_artifact_missing"
	CodeLogCapped        = "pipeline_log_capped"
)

var codeClasses = map[string]Class{
	CodeNotDeclared:       ClassRefusal,
	CodeStageInvalid:      ClassRefusal,
	CodeStepInvalid:       ClassRefusal,
	CodeSelectInvalid:     ClassRefusal,
	CodeSelectMissing:     ClassRefusal,
	CodeEventUnknown:      ClassRefusal,
	CodeBucketUnknown:     ClassRefusal,
	CodeServiceUnknown:    ClassRefusal,
	CodeNeedUnknown:       ClassRefusal,
	CodeSecretInvalid:     ClassRefusal,
	CodeSecretNotAllowed:  ClassRefusal,
	CodeFleetNotConsented: ClassRefusal,
	CodeForkRefused:       ClassRefusal,
	CodeDisconnected:      ClassRefusal,

	CodeRunnerUnavailable: ClassFailure,
	CodeExecutorError:     ClassFailure,
	CodeSecretMissing:     ClassFailure,
	CodeStepTimeout:       ClassFailure,
	CodeRunCeiling:        ClassFailure,
	CodeNoMachineForNeed:  ClassFailure,
	CodeFleetDisabled:     ClassFailure,
	CodeJobRejected:       ClassFailure,
	CodeJobUnschedulable:  ClassFailure,
	CodeImagePullFailed:   ClassFailure,
	CodeCloneFailed:       ClassFailure,
	CodeServiceFailed:     ClassFailure,
	CodeStepCancelled:     ClassFailure,
	CodeNodeLost:          ClassFailure,
	CodeArtifactTooLarge:  ClassFailure,

	CodeStageBlocked:      ClassSkip,
	CodeNotAffected:       ClassSkip,
	CodeNotifyUnavailable: ClassSkip,

	CodeCheckPermission: ClassNote,
	CodeArtifactMissing: ClassNote,
	CodeLogCapped:       ClassNote,
}

// Codes is every code this package can produce, sorted.
func Codes() []string {
	out := make([]string, 0, len(codeClasses))
	for code := range codeClasses {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// ClassOf is a code's class, and false for a code this package does not own.
func ClassOf(code string) (Class, bool) {
	c, ok := codeClasses[code]
	return c, ok
}

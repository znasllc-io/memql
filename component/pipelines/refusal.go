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
	// One pipeline per repository: a second source connecting a repository
	// another source already runs would write a second, same-named check
	// run on every commit.
	CodeAlreadyConnected = "pipeline_already_connected"
	CodeCheckPermission  = "pipeline_check_permission_missing"

	// Skips.
	CodeStageBlocked      = "pipeline_stage_blocked"
	CodeNotAffected       = "pipeline_not_affected"
	CodeNotifyUnavailable = "pipeline_notify_unavailable"
)

// Re-running only what failed (epic memql#5479, D13's "Re-run failed").
const (
	// CodePassedEarlier is a step a failed-only re-run carries over rather
	// than runs: the attempt it re-runs passed it, with the same package
	// slice. A skip, because it fails nothing -- and not a pass, because this
	// attempt executed nothing and the journal does not say otherwise.
	CodePassedEarlier = "pipeline_passed_earlier"
	// CodeNothingToRerun refuses a failed-only re-run of a run that has no
	// failed or cancelled step to run again: one that passed, or one refused
	// before any step began.
	CodeNothingToRerun = "pipeline_nothing_to_rerun"
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
	// The runner proves the step network is isolated before its first step
	// on a replica; a cluster whose policy engine does not enforce the
	// pipelines namespace's NetworkPolicy starts no step at all.
	CodeIsolationUnenforced = "pipeline_isolation_unenforced"
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
	CodeAlreadyConnected:  ClassRefusal,
	CodeNothingToRerun:    ClassRefusal,

	CodeRunnerUnavailable:   ClassFailure,
	CodeExecutorError:       ClassFailure,
	CodeSecretMissing:       ClassFailure,
	CodeStepTimeout:         ClassFailure,
	CodeRunCeiling:          ClassFailure,
	CodeNoMachineForNeed:    ClassFailure,
	CodeFleetDisabled:       ClassFailure,
	CodeJobRejected:         ClassFailure,
	CodeJobUnschedulable:    ClassFailure,
	CodeImagePullFailed:     ClassFailure,
	CodeCloneFailed:         ClassFailure,
	CodeServiceFailed:       ClassFailure,
	CodeStepCancelled:       ClassFailure,
	CodeNodeLost:            ClassFailure,
	CodeArtifactTooLarge:    ClassFailure,
	CodeIsolationUnenforced: ClassFailure,

	CodeStageBlocked:      ClassSkip,
	CodeNotAffected:       ClassSkip,
	CodeNotifyUnavailable: ClassSkip,
	CodePassedEarlier:     ClassSkip,

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

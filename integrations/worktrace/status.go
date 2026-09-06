package worktrace

// status.go names the run and step statuses the timeline reads, so the
// assembler and the metrics do not compare against bare literals.
//
// These are the WORK JOURNAL's vocabulary (dsl/work/concepts.memql), not
// the harness one this package's assembler came from. The mapping is why
// the port needed reading rather than renaming:
//
//	harness plan  open / running / done / failed  ->  run  running / succeeded / failed / cancelled
//	harness step  pending / ready / running / done / failed / blocked
//	                                              ->  step pending / ready / running / waiting /
//	                                                       done / failed / skipped / cancelled
//
// `done` and `failed` survived on the step, which is what let the metrics
// rollup port unchanged; `succeeded` is the run's terminal, where the
// harness plan said `done`.
const (
	// RunStatusRunning is a run in flight.
	RunStatusRunning = "running"
	// RunStatusSucceeded is the run's successful terminal status. The
	// harness plan spelled this `done`; ComputeMetrics keys Success on it.
	RunStatusSucceeded = "succeeded"
	// RunStatusFailed is a run that ended on a step failure.
	RunStatusFailed = "failed"
	// RunStatusCancelled is a run stopped by a person or by context cancel.
	RunStatusCancelled = "cancelled"
)

const (
	// StepStatusPending is a step not yet reached.
	StepStatusPending = "pending"
	// StepStatusReady is a step whose dependencies are met.
	StepStatusReady = "ready"
	// StepStatusRunning is the INTENT version, written before the body runs.
	// ReplaySequence keys on it: the order steps first reached running IS
	// the dispatch order a replay reproduces.
	StepStatusRunning = "running"
	// StepStatusDone is the receipt of a step that succeeded.
	StepStatusDone = "done"
	// StepStatusFailed is the receipt of a step that failed.
	StepStatusFailed = "failed"
	// StepStatusSkipped is a step whose condition decided it would not run.
	// It has no intent version, because nothing was intended.
	StepStatusSkipped = "skipped"
)

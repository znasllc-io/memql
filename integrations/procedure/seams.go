package procedure

import (
	"context"
	"sync"

	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
)

// seams.go -- the four things a replay needs that this package cannot do by
// itself, as interfaces the node that CAN installs (epic memql#5408, D4,
// D16). Each is optional in the sense that its absence is a named refusal,
// never a silent success:
//
//	Dispatcher   runs one materialized step on a target (workbench, machine)
//	Prober       measures the target's environment against learned preconditions
//	AppFallback  hands a diverged goal back to the app with the partial trace
//
// WHY INTERFACES AND NOT CALLS. The workbench dispatch lives in
// integrations/workbench, the machine dispatch and the app session behind
// the `agent` build tag, and Gate 1 needs the concrete *memql.MemQLEngine.
// This package is untagged and registers on every node, so it cannot import
// any of them; app wiring installs what the node it runs on actually has.
// A node that has no machine dispatcher answers `no_machine_dispatcher` for a
// machine-local procedure -- the replay falls back to the app rather than
// pretending the workbench could have run it (D4's failure mode).

// Dispatcher runs one materialized step on its target and reports what it
// did in the executor-independent terms component/work.Compare reads. The
// cockpit's result digest is NOT one of them: it digests the app's own tool
// output, which no other executor reproduces byte for byte.
type Dispatcher interface {
	Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error)
}

// DispatchRequest is one step, fully bound.
type DispatchRequest struct {
	Target      work.ReplayTarget
	OwnerUserId string
	// RunId is the REPLAY run. On the workbench it keys the workspace, so a
	// shadow replay never sees the app's files and two replays never share
	// a directory.
	RunId          string
	StepKey        string
	IdempotencyKey string
	// Tool is the recorded action: exec, fs_write, fs_read, fetch, mcp.
	Tool string
	// Args are the step's arguments in the APP's own spelling (Claude
	// Code's {command} / {file_path, content} / {file_path, old_string,
	// new_string}; Codex's argv vectors), materialized from the template.
	// Translating them to an executor's actions is the dispatcher's job.
	Args map[string]any
	// Sandbox marks a shadow replay: never the person's machine, never a
	// write to a MemQL row, never a delivery outside the workbench.
	Sandbox bool
	// AgentId is the agent the machine dispatch runs under (its standing
	// computer-use scope is the consent). Empty on the workbench.
	AgentId string
}

// DispatchResult is what the step did.
type DispatchResult struct {
	Observation work.StepObservation
	// Output is the executor's own result, kept for the step's receipt and
	// for a later step's data-flow hole.
	Output any
	// Delivered reports that a side effect reached something outside the
	// replay's own workspace (a machine file, a mail, an HTTP write).
	Delivered bool
}

// Prober measures, on the target, the predicates a procedure learned.
// It answers only what it measured: a learned predicate it could not measure
// is ABSENT from the observation, and CheckPreconditions turns absent into
// "unmeasured, does not hold" -- never into a match.
type Prober interface {
	Probe(ctx context.Context, target work.ReplayTarget, ownerUserId, runId string, learned proc.Preconditions) (proc.Preconditions, error)
}

// AppFallback hands the goal back to the app when a replay cannot serve it:
// the preconditions did not hold, a parameter could not be bound, or a step
// diverged. The guidance is the partial trace and the diagnosis (D16):
// repair, not resample.
type AppFallback interface {
	Handover(ctx context.Context, req FallbackRequest) (FallbackOutcome, error)
}

// FallbackRequest is one goal handed back.
type FallbackRequest struct {
	OwnerUserId string
	GoalId      string
	// RunId is the run the goal is being served in; StepId is the CANONICAL
	// journal step id of the replay statement (<bareRunId>-<key>), which is
	// what the app session's recording stamps childRunId onto.
	RunId  string
	StepId string
	// App is the app the procedure was recorded from ("claude-code"), and
	// Level the level the fallback asks for.
	App       string
	Level     string
	AgentId   string
	Statement string
	Guidance  Guidance
}

// Guidance is what the app is told before it starts.
type Guidance struct {
	Procedure string
	// Diagnosis is one paragraph: what diverged, where, and why.
	Diagnosis string
	// Completed are the steps that already ran, with their idempotency
	// keys. The app must not redo them -- this is what keeps a divergence
	// from delivering a side effect twice.
	Completed []CompletedStep
	Alignment []proc.Move
	// Prompt is the rendered guidance appended to the goal statement.
	Prompt string
}

// CompletedStep is one step a replay ran before it stopped.
type CompletedStep struct {
	Index          int
	Tool           string
	IdempotencyKey string
	Summary        string
	SideEffect     bool
}

// FallbackOutcome is what the app's session produced.
type FallbackOutcome struct {
	// ChildRunId is the session's recording run -- the repaired run, which
	// the learner reads as a new recording.
	ChildRunId string
	Content    string
	SessionId  string
}

// seams holds what app wiring installed. Guarded: a node installs them once
// at boot, and a capability can run on any goroutine after that.
type seams struct {
	mu          sync.RWMutex
	dispatchers map[work.ReplayTarget]Dispatcher
	prober      Prober
	fallback    AppFallback
}

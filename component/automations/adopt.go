package automations

// adopt.go -- executing an automation onto a run row that ALREADY EXISTS
// (memql#5054).
//
// # The gap this closes
//
// The work spine had exactly one direction: component/automations executes an
// automation from a TRIGGER, and the journal opens a v1:work:run row as its
// record. Automation to run.
//
// The compile path needs the reverse. `createGoal` opens a run in `compiling`
// BEFORE a template is known -- that ordering is deliberate, so the model
// calls compilation itself makes have a home from the first one -- and the
// compile pass then records the automation it chose and flips the run to
// `running`. At that point a run row exists, names an automation, and says it
// is running. Nothing executed it.
//
// Execute() cannot serve, because it would MINT A SECOND RUN: NewExecution
// generates a fresh id and journal.openRun writes a new row under it. The
// goal's run would stay untouched at `running` with no heartbeat until the
// abandoned sweep closed it, while a second, unrelated run row carried the
// actual work. Two rows, one job, and the one the user is watching is the
// one that reports failure.
//
// So the run is ADOPTED: the execution takes the existing run's id, and the
// journal updates that row instead of inserting one.
//
// # Why this is not ResumeFrom
//
// ResumeFrom already runs on an existing run id, and reusing it was the first
// thing tried. It refuses, and its refusals are right:
//
//   - ValidateRunJournal requires a non-empty FailedStep -- "no failed or
//     unfinished step to resume from". A compiled run has no steps at all.
//     Synthesizing one to get past the check would make the validator say yes
//     to a journal it was written to reject.
//   - It stamps `triggeredBy: resumed:<runId>` and publishes
//     `automation.resumed`. Every first execution would then be recorded, and
//     drawn in Nexus, as a resume of a run that had never run.
//   - Its first step would need AllowSideEffects, which means "retry a step
//     that may already have had its effect" -- a claim nobody can make about a
//     step that has not executed once.
//
// Resume is still what runs a run that HAS steps; RunDispatch below branches
// on exactly that. This is its missing sibling, not its replacement.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// RunAdoption names the existing run an execution should take over.
type RunAdoption struct {
	// RunId is the v1:work:run row this execution IS. The execution's id
	// becomes this, so every step row, heartbeat and terminal status the
	// journal writes lands on the run the caller is already watching.
	RunId string

	// TriggeredBy is recorded on the run. Compile-dispatched runs pass
	// "compiled"; the sweep's backstop passes "swept". Empty defaults to
	// "adopted" rather than being left blank, because an empty triggeredBy
	// is indistinguishable from a row written before the field existed.
	TriggeredBy string

	// Variables are the bound args the compiled template runs with. They
	// are seeded as the execution's input so step args resolve against
	// them, and are NOT re-written to the row -- compile already recorded
	// them, and a second writer of one field is a field with two versions
	// of the truth.
	Variables map[string]any
}

// ExecuteAdopted runs an automation as an EXISTING run.
//
// It is the whole of what memql#5054 found missing: something that takes a
// run in `running` carrying an automationName and actually executes it.
//
// The execution's id IS adopt.RunId, so the journal's step rows, its
// heartbeats and its terminal close all land on that run. The run row is
// advanced rather than inserted -- see workJournal.adoptRun.
//
// The caller is responsible for the CLAIM. This function will happily run the
// same run twice if called twice; refusing here would need a database read
// this package deliberately does not do, and the claim the work integration
// already holds (ClusterExecutionGuard, Postgres-backed) is both stronger and
// the one every other cross-replica dispatch in the tree uses.
func (e *Executor) ExecuteAdopted(ctx context.Context, automation *Automation, adopt RunAdoption) (*AutomationExecution, error) {
	if automation == nil {
		return nil, fmt.Errorf("automation is nil")
	}
	if strings.TrimSpace(adopt.RunId) == "" {
		// Refused rather than defaulted to a fresh id. A caller that meant
		// to adopt and passed nothing would otherwise get exactly the
		// second-run bug this function exists to prevent, silently.
		return nil, fmt.Errorf("ExecuteAdopted: a run id is required (adopting no run is the bug memql#5054 describes)")
	}
	if adopt.TriggeredBy == "" {
		adopt.TriggeredBy = "adopted"
	}

	// The variables ride in as a synthetic trigger payload, which is what
	// puts them through bindEventArgs -- the SAME fire-time gate every other
	// entry point passes. A compiled template declares `args { ... }`, and
	// binding this way means the goal's bound args are checked against that
	// contract (@required, type, @enum, @pattern) before a step runs, and are
	// readable as `args.X` in step bodies exactly as an authored automation
	// expects. Handing them over as exec.Input instead would skip the
	// contract and leave `args.X` unresolved.
	trigger := &events.Event{
		Topic:     "work.run.dispatched",
		Kind:      events.KindTelemetry,
		Timestamp: time.Now(),
		Payload:   adopt.Variables,
	}

	// callerSuppliedPayload is TRUE, and it is the security-relevant line in
	// this file.
	//
	// exec.SourceTrusted is `automation.Trusted && !callerSuppliedPayload`,
	// and these variables came from `createGoal`'s caller -- a person's goal
	// statement and input. The template is a trusted authored construct; its
	// ARGUMENTS are not. That is exactly the pairing memql#2888 and #2890 are
	// about: a trusted source carrying a payload somebody else chose must not
	// promote its steps to internal origin, or a caller reaches @serverOnly
	// constructs through a template that was trusted for other reasons.
	//
	// The cost is real and accepted: a compiled template whose steps write
	// @serverOnly rows is refused rather than silently privileged. That
	// refusal is visible (the step fails and the run says so), which is the
	// side of this trade that can be debugged.
	return e.executeWithEvent(ctx, automation, adopt.TriggeredBy, trigger, true, &adopt)
}

// adoptRun advances an EXISTING run row to running, in place of openRun's
// insert.
//
// It names only what execution knows and compile did not: that this node has
// it, when it last spoke, and the fingerprint of the automation actually
// being run. Everything compile wrote -- automationName, templateConstructId,
// variables, goalId -- is left alone, because updateWorkRun is a read-merge
// and a field this write does not name keeps its value.
//
// nodeId IS rewritten here, and that is a deliberate correction rather than
// an oversight (memql#5054). The field is described as "the replica running
// this run", but the three sites that stamped it -- createGoal, deriveRun and
// OpenResponsibilityGoal -- all run on the node that OPENS the run, which
// under design-record section H is the planner, while steps run on an agent.
// So the field held the opener and claimed to hold the runner, and the
// abandoned sweep's message ("the node running this run stopped answering")
// named the wrong machine. Now the runner writes it when it starts running.
func (j *workJournal) adoptRun(ctx context.Context, automation *Automation, exec *AutomationExecution) {
	if j == nil || automation == nil || exec == nil {
		return
	}
	j.call(ctx, "updateWorkRun", map[string]any{
		"runId":               exec.ID,
		"status":              "running",
		"templateFingerprint": automation.DefinitionFingerprint(fingerprintEngine),
		"triggeredBy":         exec.TriggeredBy,
		"nodeId":              j.nodeId,
		"heartbeatAt":         rfc3339(exec.StartedAt),
		"initialChainHead":    exec.InitialChainHead,
	})
}

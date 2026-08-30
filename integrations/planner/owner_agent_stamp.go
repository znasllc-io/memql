package planner

import (
	"fmt"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// stampOwnerAgentQuery builds the mutation that attaches an owning agent to a
// plan that is STILL IN ITS PLANNING PHASE.
//
// The status it writes is "planning", and that is the memql#4691 fix.
//
// Both call sites used to write status:"routing" here. Nothing consumes
// "routing" to advance a plan, and the only thing that flipped it back was a
// tail in insertDispatchedTask that fires only when the model happens to emit a
// non-empty task.agentId -- which no schema requires it to. When it did not,
// the plan sat in "routing" until the planner emitted markPlanSucceeded, whose
// planning-complete branch tested `status == "planning"` exactly. A plan in
// "routing" therefore fell through to the TERMINAL succeeded write, with empty
// output: the goal read as done and had produced nothing. That is the worst
// shape a failure can take, because no error is raised and no retry is offered.
//
// Writing "planning" is not a workaround for that gate. The plan genuinely IS
// still planning -- it is being decomposed, and picking its agent is a step
// inside that, not a phase after it. The enum's own lifecycle note puts routing
// AFTER queued (`queued -> routing -> running`), so stamping it mid-planning
// was describing the plan as somewhere it was not.
//
// The status must be passed explicitly because updatePlanStatus declares
// `status string!`. There is no "leave it alone" form, so a caller that only
// wants to attach an agent must name the status the plan is already in.
func stampOwnerAgentQuery(planId, agentId string) string {
	return fmt.Sprintf(
		`mutation updatePlanStatus(planId:%s, status:"planning", ownerAgentId:%s)`,
		langparser.QuoteString(planId), langparser.QuoteString(agentId),
	)
}

// stillPlanning reports whether a plan in this status is mid-planning, i.e.
// markPlanSucceeded from the planner means "I have emitted every task" rather
// than "the work is done".
//
// "routing" is accepted alongside "planning" deliberately, and it is NOT
// redundant with the fix above. Rows written before it exist in live databases
// right now, stranded in "routing"; without this arm the next markPlanSucceeded
// on one of them still writes terminal succeeded with empty output. Reading the
// status as planning-ish converges them to queued instead, which is where they
// should have been all along.
func stillPlanning(status string) bool {
	return status == "planning" || status == "routing"
}

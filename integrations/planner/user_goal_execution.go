package planner

import (
	"context"
	"fmt"
	"sort"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// userGoalPlanKind is the kind a person's typed goal is created under -- the
// "New goal" surface's plan. It reaches running when they click Run.
//
// It had no executor at all (memql#4688). See executableViaApprovedPath.
const userGoalPlanKind = "userGoal"

// userGoalDirective is the trusted instruction that accompanies a userGoal
// dispatch. It says the thing the agent cannot infer from the goal text alone:
// that it is the EXECUTOR for this goal rather than a participant discussing
// it, and that the turn is expected to end with the work done.
//
// The "do not re-delegate" clause is the produceArtifact lesson (memql#1133):
// an agent handed a goal and holding a delegation tool will delegate, and the
// delegate does the same, and the plan tree grows while nothing is produced.
const userGoalDirective = "EXECUTE THIS GOAL NOW. You are the executor for it -- the user has " +
	"already reviewed the plan and clicked Run, so do not restate the plan, ask to begin, or " +
	"delegate it to another agent (no produceArtifact -- that re-delegates and produces nothing). " +
	"Work through the steps listed in plan_steps if they are present, using your tools; prefer " +
	"the workbench for files and commands. End the turn with a short report of what you actually " +
	"did and where the result is, not with a description of what you would do."

// planTaskSummary is the compact per-task projection the execution turn
// carries. Deliberately narrow: this is prompt context, and the full task row
// carries fields (working memory, per-attempt bookkeeping, delegation
// reasoning) that would spend the agent's context on the planner's internals.
type planTaskSummary struct {
	Order       int
	Title       string
	Kind        string
	Phase       string
	Status      string
	Description string
}

// renderTaskSteps flattens the approved task list into the numbered plain-text
// block that rides as hints["plan_steps"].
//
// Already-terminal tasks are marked rather than dropped. A resumed or retried
// plan has some steps done, and an agent shown only what is left cannot tell
// whether a step it needs the output of already ran.
func renderTaskSteps(tasks []planTaskSummary) string {
	if len(tasks) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range tasks {
		fmt.Fprintf(&b, "%d. %s", t.Order, t.Title)
		if t.Phase != "" {
			fmt.Fprintf(&b, " [phase: %s]", t.Phase)
		}
		switch t.Status {
		case "succeeded", "failed", "cancelled":
			fmt.Fprintf(&b, " [ALREADY %s -- do not redo]", strings.ToUpper(t.Status))
		}
		if d := strings.TrimSpace(t.Description); d != "" {
			fmt.Fprintf(&b, "\n   %s", d)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// loadPlanTaskSummaries reads the plan's semantic task rows for the execution
// turn.
//
// Best-effort BY DESIGN: a read failure returns nil and the dispatch proceeds
// with the goal alone. The steps are context that makes the turn better, and a
// plan that executes without them is far better than a plan that does not
// execute -- which is the state memql#4688 describes. Only userGoal reads them;
// every other approved kind is a single self-contained turn with no task list
// to carry, so this stays a no-op on those paths and costs them no query.
func (p *PlannerIntegration) loadPlanTaskSummaries(ctx context.Context, planId, kind string) []planTaskSummary {
	if p == nil || p.engine == nil || kind != userGoalPlanKind || planId == "" {
		return nil
	}
	q := fmt.Sprintf(`query tasksForPlan(planId:%s)`, langparser.QuoteString(planId))
	res, err := p.engine.Execute(ctx, q)
	if err != nil {
		p.logger.Warn("plan execution: task list read failed; dispatching with the goal alone",
			"plan_id", planId, "error", err)
		return nil
	}
	return taskSummariesFromRows(memql.MaterializeRows(res))
}

// taskSummariesFromRows projects and orders raw task rows. Pure, so the
// ordering rule below is directly testable.
//
// Order is (sequenceNumber, createdAt, id) and the sort is STABLE. A prompt
// that lists the same steps in a different order on two runs is a different
// prompt: it defeats the identical-request circuit breaker in
// component/memql/ai_guard.go and makes two identical failures look unrelated.
func taskSummariesFromRows(rows []map[string]any) []planTaskSummary {
	type keyed struct {
		summary planTaskSummary
		seq     float64
		created string
		id      string
	}
	keys := make([]keyed, 0, len(rows))
	for _, row := range rows {
		payload := row
		if p, ok := row["payload"].(map[string]any); ok && p != nil {
			payload = p
		}
		if getString(payload, "category") == "toolInvocation" {
			// Engine-stamped records of an agent's own tool calls, not steps a
			// person planned. Listing them back to the agent as instructions
			// would tell it to redo its own past tool calls.
			continue
		}
		title := firstNonEmpty(
			getString(payload, "title"),
			getString(payload, "goal"),
			getString(payload, "kind"),
		)
		if strings.TrimSpace(title) == "" {
			continue
		}
		seq, _ := payload["sequenceNumber"].(float64)
		keys = append(keys, keyed{
			summary: planTaskSummary{
				Title:       title,
				Kind:        getString(payload, "kind"),
				Phase:       getString(payload, "phase"),
				Status:      getString(payload, "status"),
				Description: getString(payload, "description"),
			},
			seq:     seq,
			created: getString(payload, "createdAt") + getString(row, "createdAt"),
			id:      firstNonEmpty(getString(row, "id"), getString(payload, "id")),
		})
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].seq != keys[j].seq {
			return keys[i].seq < keys[j].seq
		}
		if keys[i].created != keys[j].created {
			return keys[i].created < keys[j].created
		}
		return keys[i].id < keys[j].id
	})
	out := make([]planTaskSummary, 0, len(keys))
	for i, k := range keys {
		k.summary.Order = i + 1
		out = append(out, k.summary)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ownedByAnotherDispatcher reports whether HandlePlanUpdated must stay out of
// this kind's running transition because a different handler already owns it.
//
// The set is NOT the same as executableViaApprovedPath's, and conflating them
// is the mistake to avoid: adHocAction is owned elsewhere without being
// dispatched through the approved path at all.
//
// userGoal was added in memql#4688, and it is REQUIRED rather than tidy. Once
// handlePlanApprovedForExecution dispatches a userGoal, leaving it here means
// one running transition drives two handlers: the dispatch starts the agent
// turn, while maybeCheckpointAtPhaseBoundary concurrently parks the SAME plan
// to awaitingFeedback -- a plan being worked on and parked at the same time,
// whose terminal status is decided by whichever finishes last.
//
// Feedback resumes are not lost by leaving: executeApprovedPlan threads
// Plan.feedbackResponse into the dispatched turn, which is exactly how
// produceArtifact and scopeElevation have always resumed.
func ownedByAnotherDispatcher(kind string) bool {
	switch kind {
	case "adHocAction", "scopeElevation", produceArtifactPlanKind, userGoalPlanKind:
		return true
	}
	return false
}

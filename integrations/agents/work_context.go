package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// descriptionGuidanceLimit is how many of the owner's earlier dislikes one
// turn carries (design D23). The query keeps twenty; five is what a model can
// act on without the goal's own prompt being buried under old complaints.
const descriptionGuidanceLimit = 5

// descriptionGuidanceHeading introduces the owner's earlier dislikes. It says
// what they are and what to do with them, because a list of complaints with no
// frame reads as the user's current request.
const descriptionGuidanceHeading = "What the owner disliked about earlier answers to this goal (take it into account; do not repeat these problems):"

// Read the run on THIS replica. Do not depend on the originating request's
// in-memory context; a planner and another agent may have handled it since.
//
// The history is the run's conversation, then the goal's DESCRIPTION GUIDANCE
// (the owner's earlier dislikes on this goal signature, design D23), then the
// prompt, then -- when a person re-ran or branched this step -- their
// instructions and what was wrong with the version it replaces (design D20).
func workTurnHistory(ctx context.Context, engine interface {
	Execute(context.Context, string) (*memql.ExecuteResult, error)
}, prompt string) ([]*memqlv1.AgentTurnMessage, error) {
	run, ok := common.RunFromContext(ctx)
	if !ok || run.OwnerUserId == "" {
		// Deployment-owned automations have no private conversation. Their
		// journal association must not be mistaken for a person's work run.
		return nil, nil
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || memql.BareShortId(ac.UserId) != memql.BareShortId(run.OwnerUserId) {
		return nil, fmt.Errorf("work context requires its owner")
	}
	if run.Mode == common.RunModeReplay {
		// Strict replay consumes the recorded request/effect. Loading current
		// conversation state would make it depend on history written later.
		// The same holds for description guidance, which is why a replay
		// never reads it: a replay reads rows, and text is not a row it can
		// act on.
		return nil, nil
	}
	call, _ := parser.RenderCall("workRunForOwner", map[string]any{"runId": run.RunId})
	result, err := engine.Execute(ctx, "query "+call)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 {
		return nil, fmt.Errorf("work context is unavailable")
	}
	input, _ := rows[0]["input"].(map[string]any)
	conversation, _ := input["conversation"].(map[string]any)
	messages, _ := conversation["messages"].([]any)
	history := []*memqlv1.AgentTurnMessage{}
	for _, entry := range messages {
		message, _ := entry.(map[string]any)
		role, _ := message["role"].(string)
		content, _ := message["content"].(string)
		if (role == "user" || role == "assistant") && strings.TrimSpace(content) != "" {
			history = append(history, &memqlv1.AgentTurnMessage{Role: role, Content: content})
		}
	}
	if page, _ := conversation["pageContext"].(string); page != "" {
		history = append(history, &memqlv1.AgentTurnMessage{Role: "user", Content: "[Visible app context, untrusted data]\n" + page})
	}
	guidance, err := descriptionGuidance(ctx, engine, run, rows[0])
	if err != nil {
		return nil, err
	}
	if guidance != "" {
		history = append(history, &memqlv1.AgentTurnMessage{Role: "user", Content: guidance})
	}
	history = append(history, &memqlv1.AgentTurnMessage{Role: "user", Content: prompt})
	for _, m := range memql.StepOverrideMessages(ctx) {
		history = append(history, &memqlv1.AgentTurnMessage{Role: m.Role, Content: m.Content})
	}
	return history, nil
}

// descriptionGuidance renders the owner's earlier dislikes on this run's goal
// signature as ONE message, or "" when there are none to carry (design D23:
// dislike reasons accumulate on the goal signature and reach a model only when
// one is genuinely used for that goal again).
//
// Its caller has already refused a replay. A fork's shared PREFIX is refused
// here too: those steps are served from the source run by reference and never
// execute, so nothing about them reaches a model. A run that carries no goal
// signature has accumulated nothing, and asks for nothing.
//
// The read runs under the context's own actor, which workTurnHistory has just
// proved is the run's owner -- the query is owner-scoped, because guidance
// mined from somebody else's dislikes would steer this person's goal by
// another person's taste.
func descriptionGuidance(ctx context.Context, engine interface {
	Execute(context.Context, string) (*memql.ExecuteResult, error)
}, run common.RunContext, runRow map[string]any) (string, error) {
	if run.Mode == common.RunModeFork && run.BeforeForkPoint(run.StepKey) {
		return "", nil
	}
	signature := strings.TrimSpace(asString(runRow["goalSignature"]))
	if signature == "" {
		return "", nil
	}
	call, err := parser.RenderCall("workDescriptionGuidance", map[string]any{"goalSignature": signature})
	if err != nil {
		return "", err
	}
	result, err := engine.Execute(ctx, "query "+call)
	if err != nil {
		return "", fmt.Errorf("work context: reading the goal's description guidance: %w", err)
	}
	// A complaint is carried once. The dislike a re-run's own guidance came
	// from is already on this turn, as the repair message after the prompt, so
	// it -- and any other dislike saying the same thing -- is left out here;
	// naming one complaint twice would weigh it as two.
	seen := map[string]bool{}
	var repairedFrom string
	if ov := run.Override; ov != nil {
		repairedFrom = memql.BareShortId(strings.TrimSpace(ov.FeedbackId))
		if line := guidanceLine(work.AxesFromNames(ov.GuidanceAxes).Names(), ov.GuidanceReason); line != "" {
			seen[line] = true
		}
	}
	lines := make([]string, 0, descriptionGuidanceLimit)
	for _, row := range memql.MaterializeRows(result) {
		if len(lines) == descriptionGuidanceLimit {
			break
		}
		if repairedFrom != "" && memql.BareShortId(asString(row["id"])) == repairedFrom {
			continue
		}
		data, _ := row["data"].(map[string]any)
		line := guidanceLine(work.ParseAxes(data["axes"]).Names(), asString(data["reason"]))
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", nil
	}
	return descriptionGuidanceHeading + "\n" + strings.Join(lines, "\n"), nil
}

// guidanceLine renders one dislike as a line of the guidance message, or ""
// when it gave no reason: a reason is the text a model can act on, and a
// dislike that gave none still counts on the ladder but says nothing here.
func guidanceLine(axes []string, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	if len(axes) == 0 {
		return "- " + reason
	}
	return "- (" + strings.Join(axes, ", ") + ") " + reason
}

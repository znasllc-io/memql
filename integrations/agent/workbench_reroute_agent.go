//go:build agent

package agent

// WORKBENCH FIRST, THE FLEET WHEN IT CANNOT (memql#4353, design D7).
//
// The workbench is the default surface for headless work and stays that way.
// What changes here is what happens when it answers `environment_mismatch`:
// the action needs a display, a GPU, macOS tooling or files that live on the
// user's own machine, and the workbench said so BEFORE running anything
// instead of failing three layers down.
//
// THE PROHIBITION THIS IMPLEMENTS RATHER THAN REPLACES. The workbench
// knowledge domain (integrations/knowledge/seed.go, the
// `workbench:failureFallback` chunk) rules:
//
//	"Never silently switch to the user's own machine. If the workbench cannot
//	do the job, say so and request computer-use scope through
//	requestComputerUseScope -- the user approves on the canvas card before any
//	tool touches their machine."
//
// That ruling stands, and the automatic path does not weaken it. The reroute
// runs the call on the user's machine ONLY where the user has already
// consented to exactly that: an approved task, and standing scope at or above
// the tier the unmet needs imply. Where either is missing, the card is raised
// exactly as before. So this is not "switch silently when convenient" -- it is
// "stop making the user re-approve something they already approved".
//
// HOW THE DECISION IS MADE, and why it is not a second copy of the rule. The
// reroute does not re-derive whether the user consented. It ATTEMPTS the fleet
// dispatch, and the dispatcher's existing gate -- per-task approval, the kill
// switch, standing scope, the classifier -- answers. Those gates run entirely
// BEFORE any wire traffic, so an attempt that is refused touches nothing.
// Asking the gate is what keeps one authority for the answer; re-implementing
// its ladder here is how the tool loop and the dispatcher come to disagree
// about what the user approved.
//
// A KILL SWITCH IS NOT A MISSING CARD. `kill_switch_engaged` means the user
// deliberately turned computer use off, so it is surfaced rather than answered
// with a card asking them to turn it back on.

import (
	"context"
	"encoding/json"
	"strings"

	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// rerouteToolNames are the two the reroute drives.
const (
	rerouteFleetTool = "workerHost"
	rerouteScopeTool = "requestComputerUseScope"
)

// rerouteWorkbenchMismatch inspects a workbenchHost result and, when it is an
// environment mismatch, either runs the call on the user's fleet or raises the
// consent card.
//
// It returns the content the model should see and whether it replaced the
// original. A result that is not a mismatch -- a success, any other failure,
// or a tool that is not workbenchHost -- returns ("", false) and the caller
// keeps what it had.
func (r *Replier) rerouteWorkbenchMismatch(
	ctx context.Context,
	turnCtx turnContext,
	toolName string,
	result string,
	args map[string]any,
) (string, bool) {
	if toolName != "workbenchHost" || r == nil || r.stamper == nil {
		return "", false
	}
	plan, ok := planWorkbenchReroute(result, args, turnCtx)
	if !ok {
		return "", false
	}
	mismatch, action, requireLabels := plan.Mismatch, plan.Action, plan.RequireLabels

	r.logger.Info("agent: workbench reported an environment mismatch; trying the fleet",
		"action", action,
		"unmet_needs", mismatch.UnmetNeeds,
		"require_labels", requireLabels,
		"plan_id", turnCtx.PlanId,
	)

	fleetArgs := plan.FleetArgs
	fleetResult, err := r.stamper.ExecuteToolByName(
		agentToolCallContext(ctx, rerouteFleetTool, turnCtx), rerouteFleetTool, fleetArgs)
	if err != nil {
		// The fleet tool itself failed to execute -- not a refusal, an error.
		// Leave the workbench's own mismatch as the answer: it is the more
		// useful one, and it already tells the model what the action needs.
		r.logger.Warn("agent: the fleet dispatch failed to execute after a workbench mismatch",
			"error", err)
		return "", false
	}

	code := rerouteErrorCode(fleetResult)
	if code != "denied_no_per_task_approval" && code != "denied_by_scope" {
		// Ran, or failed for a reason a card cannot fix. Either way this is
		// the answer.
		return fleetResult, true
	}

	// NO CONSENT YET. Raise the card, exactly as the corpus prescribes, and
	// tell the model to end its turn -- the user's Allow dispatches a fresh
	// one.
	scope := plan.RequestedScope
	cardArgs := plan.CardArgs
	cardResult, cardErr := r.stamper.ExecuteToolByName(
		agentToolCallContext(ctx, rerouteScopeTool, turnCtx), rerouteScopeTool, cardArgs)
	if cardErr != nil {
		r.logger.Warn("agent: could not raise the consent card after a workbench mismatch",
			"error", cardErr)
		return "", false
	}
	r.logger.Info("agent: raised the computer-use consent card after a workbench mismatch",
		"requested_scope", scope, "unmet_needs", mismatch.UnmetNeeds)
	return cardResult, true
}

// rerouteCardSummary is the human sentence on the canvas card. It says what the
// workbench could not do, in the user's terms, because "environment_mismatch"
// is not a thing anyone should have to read.
func rerouteCardSummary(m workbench.EnvironmentMismatch, action string) string {
	var b strings.Builder
	b.WriteString("The sandboxed workbench cannot run this ")
	if action != "" {
		b.WriteString(action + " ")
	}
	b.WriteString("step: it needs ")
	b.WriteString(describeNeeds(m.UnmetNeeds))
	b.WriteString(".")
	return b.String()
}

// describeNeeds renders the closed need set as prose.
func describeNeeds(needs []string) string {
	if len(needs) == 0 {
		return "something the workbench does not provide"
	}
	words := make([]string, 0, len(needs))
	for _, need := range needs {
		switch need {
		case workbench.NeedDisplay:
			words = append(words, "a graphical display")
		case workbench.NeedGPU:
			words = append(words, "a GPU")
		case workbench.NeedMacOSTooling:
			words = append(words, "macOS-only tooling")
		case workbench.NeedUserFiles:
			words = append(words, "files that are on your own machine")
		case workbench.UnmetNeedOS:
			words = append(words, "a different operating system")
		default:
			words = append(words, need)
		}
	}
	switch len(words) {
	case 1:
		return words[0]
	case 2:
		return words[0] + " and " + words[1]
	default:
		return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
	}
}

// rerouteErrorCode pulls errorCode out of a worker tool result. An unparseable
// result yields "", which the caller reads as "not a refusal" -- the safe
// direction, because the alternative is raising a consent card for a call that
// already ran.
func rerouteErrorCode(result string) string {
	var envelope struct {
		OK        bool   `json:"ok"`
		ErrorCode string `json:"errorCode"`
	}
	if err := json.Unmarshal([]byte(result), &envelope); err != nil {
		return ""
	}
	if envelope.OK {
		return ""
	}
	return envelope.ErrorCode
}

// stringFromArgs reads a string argument, tolerating absence.
func stringFromArgs(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	s, _ := args[key].(string)
	return s
}

// workbenchReroute is the decision, separated from its execution so it can be
// tested without a tool executor. Everything that reads the mismatch, derives
// the routing requirement and builds both argument maps is here; the method
// above only runs it and reads the answers.
type workbenchReroute struct {
	Mismatch       workbench.EnvironmentMismatch
	Action         string
	RequireLabels  map[string]string
	RequestedScope string
	FleetArgs      map[string]any
	CardArgs       map[string]any
}

// planWorkbenchReroute reads a workbenchHost result. The second return is
// false for anything that is not an environment mismatch -- a success, any
// other failure, or an unparseable body -- which is the caller's signal to
// leave the model's answer alone.
func planWorkbenchReroute(result string, args map[string]any, turnCtx turnContext) (workbenchReroute, bool) {
	mismatch, ok := workbench.EnvironmentMismatchFromPayload([]byte(result))
	if !ok {
		return workbenchReroute{}, false
	}
	action := strings.TrimSpace(stringFromArgs(args, "action"))
	inner, _ := args["args"].(map[string]any)
	requireLabels := agentworker.EnvironmentNeedsLabels(mismatch.UnmetNeeds, mismatch.RequestedOS)
	scope := agentworker.EnvironmentNeedsScope(mismatch.UnmetNeeds)

	return workbenchReroute{
		Mismatch:       mismatch,
		Action:         action,
		RequireLabels:  requireLabels,
		RequestedScope: scope,
		FleetArgs: map[string]any{
			// The SAME action and the SAME arguments. workerHost and
			// workbenchHost carry the identical six verbs, so a reroute is a
			// change of destination and nothing else -- rewriting the call on
			// the way across would make the machine run something other than
			// what the workbench refused.
			"action":        action,
			"args":          inner,
			"agentId":       turnCtx.AgentId,
			"ownerUserId":   turnCtx.OwnerUserId,
			"planId":        turnCtx.PlanId,
			"requireLabels": requireLabels,
			// The routing record's answer to "why did this run on the laptop".
			"reroutedFrom": agentworker.ReroutedFromWorkbench,
		},
		CardArgs: map[string]any{
			"intent":         action,
			"requestedScope": scope,
			"summary":        rerouteCardSummary(mismatch, action),
			"agentId":        turnCtx.AgentId,
			"ownerUserId":    turnCtx.OwnerUserId,
			"partitionId":    turnCtx.PartitionId,
			// The card names the requirement, so the user's Allow visibly
			// covers a SET of machines rather than appearing to name one.
			"requireLabels": requireLabels,
		},
	}, true
}

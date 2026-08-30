package agents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// maxFactoryAnalyzeAttempts bounds how many times the factory will ask
// agentFactoryAnalyze for a decision it can actually apply (memql#4690).
//
// Three, not more: the retry exists to correct a MISTAKE (a slug that is not in
// the catalog, an action that is not one of the three, a target that does not
// exist), and a model that has been handed the catalog and the exact reason its
// last two answers were rejected is not going to be talked round by a third
// nudge. Every attempt is a real LLM call against the plan's budget, so the cap
// is part of the cost-control structure, not a tuning knob
// (docs/public/ai/llm-cost-control.md).
const maxFactoryAnalyzeAttempts = 3

// correctableDecisionError marks a factory failure the MODEL can fix if it is
// told about it -- as opposed to one no amount of re-prompting will help.
//
// The distinction is the whole design. Before memql#4690 a single bad guess was
// terminal: createAgent returned a plain error, ensureAgentForGoal propagated
// it, and ensureSpecialistForPlan called markPlanFailed -- the user's goal dead
// because a model picked `content-creator` when the catalog said
// `creative-companion`. But retrying is only right for that class. A provider
// outage, a missing engine handle or a refused write are NOT correctable: the
// model's answer was fine, so re-asking burns tokens and budget to arrive at
// the same wall. Those stay one-shot terminal, exactly as they were.
type correctableDecisionError struct{ msg string }

func (e *correctableDecisionError) Error() string { return e.msg }

// correctable reports whether err is worth re-prompting over.
func correctable(err error) bool {
	var c *correctableDecisionError
	return errors.As(err, &c)
}

// validateFactoryDecision checks a decision against the catalogs it was
// supposed to choose from, and phrases every rejection AS FEEDBACK -- naming
// the offending value and listing what was actually available.
//
// This runs BEFORE any dispatch, so an invalid decision costs nothing but the
// call that produced it. The per-branch errors in handleEnsureForGoal stay as a
// backstop: they are unreachable through this path, and they should remain, so
// that a future caller reaching the switch some other way still fails closed.
func validateFactoryDecision(d factoryDecision, existing []agentSnapshot, roles []roleSnapshot) error {
	switch d.Action {
	case "create":
		if _, ok := findRoleBySlug(roles, d.RoleSlug); !ok {
			return &correctableDecisionError{msg: fmt.Sprintf(
				"roleSlug %q is not in the role catalog. Choose one of: %s",
				d.RoleSlug, slugList(roles))}
		}
	case "match", "extend":
		if strings.TrimSpace(d.TargetAgentId) == "" {
			return &correctableDecisionError{msg: fmt.Sprintf(
				"action %q requires a targetAgentId naming one of the user's existing agents, and it was empty. Existing agents: %s",
				d.Action, agentIdList(existing))}
		}
		if _, ok := findById(existing, d.TargetAgentId); !ok {
			return &correctableDecisionError{msg: fmt.Sprintf(
				"targetAgentId %q is not one of the user's agents. Existing agents: %s. If none of them fit, use action \"create\" instead.",
				d.TargetAgentId, agentIdList(existing))}
		}
	default:
		return &correctableDecisionError{msg: fmt.Sprintf(
			"action %q is not valid. It must be exactly one of: match, extend, create.", d.Action)}
	}
	return nil
}

// decideForGoal runs analyze -> validate, re-prompting with the rejection
// reason until a decision passes or the attempt cap is reached.
//
// The returned error is the LAST rejection, so a caller that gives up reports
// what the model kept getting wrong rather than a generic "analysis failed".
func (i *Integration) decideForGoal(
	ctx context.Context,
	goal string,
	existing []agentSnapshot,
	roles []roleSnapshot,
	skills []skillSnapshot,
) (factoryDecision, error) {
	var priorError string
	var lastErr error
	for attempt := 1; attempt <= maxFactoryAnalyzeAttempts; attempt++ {
		decision, err := i.analyzeGoal(ctx, goal, existing, roles, skills, priorError)
		if err != nil {
			if !correctable(err) {
				// Provider down, engine handle missing, prompt not registered:
				// re-asking cannot help. Fail now rather than three times.
				return factoryDecision{}, err
			}
			lastErr, priorError = err, err.Error()
			continue
		}
		if verr := validateFactoryDecision(decision, existing, roles); verr != nil {
			lastErr, priorError = verr, verr.Error()
			continue
		}
		return decision, nil
	}
	return factoryDecision{}, fmt.Errorf(
		"agentFactoryAnalyze did not produce an applicable decision in %d attempts; last rejection: %w",
		maxFactoryAnalyzeAttempts, lastErr)
}

// slugList renders the catalog's slugs for a feedback message. Sorted, because
// the message goes into a prompt and a set that reorders run-to-run makes two
// identical failures look like two different ones -- and defeats the
// identical-request circuit breaker in component/memql/ai_guard.go.
func slugList(roles []roleSnapshot) string {
	if len(roles) == 0 {
		return "(the role catalog is EMPTY -- this is a server-side fault, not something you can fix by choosing differently)"
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.Slug)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func agentIdList(existing []agentSnapshot) string {
	if len(existing) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(existing))
	for _, a := range existing {
		out = append(out, fmt.Sprintf("%s (%s)", a.Id, a.RoleSlug))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

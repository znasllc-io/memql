// agent_loop_triage.go
//
// Goal complexity triage (epic #836 / memql#837). A CHEAP classifier
// runs FIRST on every goal-bearing Plan, before the expensive
// plannerAgent decompose loop. It answers one question -- is this goal
// TRIVIAL (a single self-contained deliverable like "a list of 10
// birds"), MODERATE, or COMPLEX -- and routes accordingly:
//
//   - trivial  -> ONE direct production turn (startPlanDirect): the
//                 owning agent writes the deliverable in a single turn.
//                 NO decompose recursion, NO specialist creation, NO
//                 training. This is the cost fix -- a one-line
//                 deliverable must never trigger the Opus+thinking
//                 decompose loop that cost ~$250 (memql#816/#832).
//   - moderate / complex -> fall through to the (bounded, guarded)
//                 decompose loop below.
//
// The classifier itself is a single chat54Mini call (cheap tier). A
// classify failure never blocks the Plan -- it falls through to the
// decompose loop, which carries its own hard caps.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// goalComplexity is the classifier's verdict.
type goalComplexity string

const (
	complexityTrivial  goalComplexity = "trivial"
	complexityModerate goalComplexity = "moderate"
	complexityComplex  goalComplexity = "complex"
	// complexityUnknown is the zero value -- treated as "not trivial"
	// (route to decompose) so an unparseable / missing classification is
	// never mistaken for a trivial shortcut.
	complexityUnknown goalComplexity = ""
)

// triageRoute is the routing decision derived from a complexity class.
type triageRoute string

const (
	// routeDirect: produce the deliverable in a single agent turn, no
	// decompose loop.
	routeDirect triageRoute = "direct"
	// routeDecompose: enter the bounded plannerAgent decompose loop.
	routeDecompose triageRoute = "decompose"
)

// goalTriageEnabled gates the whole triage step. Defaults on; an
// operator can disable it (MEMQL_PLANNER_GOAL_TRIAGE_ENABLED=0) to fall
// every goal back to the decompose loop.
func goalTriageEnabled() bool {
	v := strings.TrimSpace(os.Getenv("MEMQL_PLANNER_GOAL_TRIAGE_ENABLED"))
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// routeForComplexity is the PURE mapping from a complexity class to a
// routing decision. ONLY trivial routes direct; moderate / complex /
// unknown all route to the decompose loop. Kept pure + deterministic so
// the routing contract is unit-testable without an LLM.
func routeForComplexity(c goalComplexity) triageRoute {
	if c == complexityTrivial {
		return routeDirect
	}
	return routeDecompose
}

// normalizeComplexity coerces a raw model string to a known class.
// Anything unrecognized maps to complexityUnknown (-> decompose), so a
// junk classification never shortcuts a goal into the trivial path.
func normalizeComplexity(raw string) goalComplexity {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "trivial":
		return complexityTrivial
	case "moderate":
		return complexityModerate
	case "complex":
		return complexityComplex
	default:
		return complexityUnknown
	}
}

// goalTriageDecision is the structured-output envelope the
// goalComplexityTriage prompt emits.
type goalTriageDecision struct {
	Complexity string `json:"complexity"`
	Reasoning  string `json:"reasoning"`
}

// parseGoalComplexity normalizes the goalComplexityTriage response into
// a (class, reasoning) pair. Mirrors parsePlannerDecision's tolerance:
// accepts a string or a map, strips a ```json fence, and never panics.
// An unparseable response yields complexityUnknown + an error so the
// caller can log and fall through to decompose.
func parseGoalComplexity(resp any) (goalComplexity, string, error) {
	if resp == nil {
		return complexityUnknown, "", fmt.Errorf("triage returned nil")
	}
	var raw []byte
	switch r := resp.(type) {
	case string:
		raw = []byte(r)
	case map[string]any:
		b, err := json.Marshal(r)
		if err != nil {
			return complexityUnknown, "", err
		}
		raw = b
	default:
		b, err := json.Marshal(resp)
		if err != nil {
			return complexityUnknown, "", err
		}
		raw = b
	}
	raw = stripJSONFence(raw)
	var d goalTriageDecision
	if err := json.Unmarshal(raw, &d); err != nil {
		return complexityUnknown, "", fmt.Errorf("parse triage JSON: %w (raw=%s)", err, truncate(string(raw), 160))
	}
	c := normalizeComplexity(d.Complexity)
	if c == complexityUnknown {
		return complexityUnknown, d.Reasoning, fmt.Errorf("triage emitted unknown complexity %q", d.Complexity)
	}
	return c, d.Reasoning, nil
}

// classifyGoalComplexity runs the cheap goalComplexityTriage prompt over
// a goal and returns the normalized class + reasoning.
func (l *PlannerAgentLoop) classifyGoalComplexity(ctx context.Context, goal, nowRFC3339 string) (goalComplexity, string, error) {
	resp, err := l.engine.InvokeSI(systemActorContext(ctx), "goalComplexityTriage", map[string]any{
		"goal": truncate(goal, maxGoalChars),
		"now":  nowRFC3339,
	})
	if err != nil {
		return complexityUnknown, "", err
	}
	return parseGoalComplexity(resp)
}

// triageAndMaybeShortcut runs the complexity classifier for a Plan and,
// when the goal is TRIVIAL and the Plan already has an owning agent to
// run the production turn, shortcuts to a single direct turn
// (startPlanDirect) -- bypassing the decompose loop entirely. Returns
// handled=true when it took the shortcut; the caller then returns
// without entering the decompose loop.
//
// It falls through (handled=false) -- letting the normal decompose loop
// run -- whenever:
//   - triage is disabled,
//   - the Plan can't be loaded or carries no goal,
//   - the classifier errors or returns a non-trivial class,
//   - the goal is trivial but the Plan has no ownerAgentId (no agent to
//     run the single turn; the decompose loop will provision/assign one).
//
// A classify failure must NEVER strand the Plan: every non-shortcut
// path returns (false, nil) so the caller proceeds to the bounded loop.
func (l *PlannerAgentLoop) triageAndMaybeShortcut(ctx context.Context, planId, nowRFC3339 string) (handled bool, err error) {
	if !goalTriageEnabled() {
		return false, nil
	}
	plan, lerr := l.loadPlan(ctx, planId)
	if lerr != nil {
		// Can't classify what we can't read -- let the normal loop
		// load/handle/fail it.
		return false, nil
	}
	goal := getString(plan, "goal")
	if strings.TrimSpace(goal) == "" {
		return false, nil
	}

	// Known-trivial kinds skip the classifier entirely (memql#835): a
	// produceArtifact plan is a single self-contained deliverable by
	// construction -- the Assistant's produceArtifact tool only mints it
	// for "make me a <file>" requests, and it carries the requesting
	// assistant as ownerAgentId. Treat it as trivial directly (zero
	// classifier LLM call) and route to the single direct production turn.
	// This is what lets produceArtifact flow through the unified loop (its
	// old hardcoded HandlePlanCreated bypass was removed) while still
	// making ZERO planner LLM calls.
	if getString(plan, "kind") == produceArtifactPlanKind {
		if strings.TrimSpace(getString(plan, "ownerAgentId")) == "" {
			return false, nil // no owner to run the turn -> let the loop assign one
		}
		l.logger.Info("planner triage: produceArtifact is known-trivial -> single direct production turn (no classifier, no decompose)",
			"planId", planId)
		l.startPlanDirect(ctx, planId)
		return true, nil
	}

	complexity, reasoning, cerr := l.classifyGoalComplexity(ctx, goal, nowRFC3339)
	if cerr != nil {
		l.logger.Warn("planner triage: classify failed; falling through to decompose loop",
			"planId", planId, "error", cerr)
		return false, nil
	}
	if routeForComplexity(complexity) != routeDirect {
		l.logger.Info("planner triage: goal not trivial; entering decompose loop",
			"planId", planId, "complexity", complexity, "reasoning", reasoning)
		return false, nil
	}

	// Trivial. We need an owning agent to run the single production turn.
	// Without one, fall through so the decompose loop can provision/assign
	// an agent (it can't be produced in a single turn with nobody to run it).
	if strings.TrimSpace(getString(plan, "ownerAgentId")) == "" {
		l.logger.Info("planner triage: trivial goal but no owning agent; decompose loop will assign one",
			"planId", planId, "reasoning", reasoning)
		return false, nil
	}

	l.logger.Info("planner triage: trivial goal -> single direct production turn (no decompose loop)",
		"planId", planId, "reasoning", reasoning)
	// startPlanDirect flips the Plan to running; handlePlanApprovedForExecution
	// then dispatches the owning agent's single turn. ZERO plannerAgent
	// (decompose) LLM calls are made on this path.
	l.startPlanDirect(ctx, planId)
	return true, nil
}

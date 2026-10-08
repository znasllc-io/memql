package planner

// goal_complexity.go -- the goal-complexity classifier's vocabulary
// (memql#5052).
//
// This lived in agent_loop_triage.go, whose job was to route a PLAN: classify
// the goal, and shortcut a trivial one to a single direct turn instead of the
// decompose loop. That routing is gone with the loop.
//
// The CLASSIFIER is not. `work_compile.go` calls `classifySectionable`, which
// runs the same `goalComplexityTriage` prompt and reads both answers off the
// one response -- it is the compile order's third tier, the single triage call
// that runs when the catalog misses. So the types it returns need a home that
// is not a deleted file.
//
// What is NOT here is `triageRoute` / `routeForComplexity` / the shortcut:
// those mapped a complexity onto "direct turn or decompose loop", and there is
// no decompose loop to route to. the installed Spine owns routing now.

import (
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
	// complexityUnknown is the zero value -- treated as "not trivial" so an
	// unparseable or missing classification is never mistaken for a cheap
	// answer.
	complexityUnknown goalComplexity = ""
)

// maxGoalChars bounds the goal echoed into the triage prompt. It lived in
// agent_loop_prompt.go, which held the plan loop's prompt projection.
const maxGoalChars = 1200

// goalTriageEnabled gates the classifier. Defaults on; an operator can
// disable it (MEMQL_PLANNER_GOAL_TRIAGE_ENABLED=0).
func goalTriageEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_PLANNER_GOAL_TRIAGE_ENABLED"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// normalizeComplexity coerces a raw model string to a known class. Anything
// unrecognized maps to complexityUnknown, so a junk classification never
// shortcuts a goal into the cheap path.
func normalizeComplexity(raw string) goalComplexity {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "trivial":
		return complexityTrivial
	case "moderate":
		return complexityModerate
	case "complex":
		return complexityComplex
	}
	return complexityUnknown
}

// goalTriageDecision is the goalComplexityTriage prompt's shape.
type goalTriageDecision struct {
	Complexity string `json:"complexity"`
	Reasoning  string `json:"reasoning"`
}

// parseGoalComplexity normalizes the triage response into a (class,
// reasoning) pair. Accepts a string or a map, strips a ```json fence, and
// never panics. An unparseable response yields complexityUnknown plus an
// error, so the caller falls through rather than acting on a guess.
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

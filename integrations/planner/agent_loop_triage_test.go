package planner

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

// --- pure routing mapping (acceptance: classifier mapping) ---------------

func TestRouteForComplexity(t *testing.T) {
	cases := []struct {
		in   goalComplexity
		want triageRoute
	}{
		{complexityTrivial, routeDirect},
		{complexityModerate, routeDecompose},
		{complexityComplex, routeDecompose},
		{complexityUnknown, routeDecompose},
		{goalComplexity("garbage"), routeDecompose},
	}
	for _, c := range cases {
		if got := routeForComplexity(c.in); got != c.want {
			t.Fatalf("routeForComplexity(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeComplexity(t *testing.T) {
	cases := map[string]goalComplexity{
		"trivial":   complexityTrivial,
		"TRIVIAL":   complexityTrivial,
		" moderate": complexityModerate,
		"Complex":   complexityComplex,
		"":          complexityUnknown,
		"nonsense":  complexityUnknown,
	}
	for in, want := range cases {
		if got := normalizeComplexity(in); got != want {
			t.Fatalf("normalizeComplexity(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- parser tolerance ------------------------------------------------------

func TestParseGoalComplexity(t *testing.T) {
	// Plain JSON string.
	if c, _, err := parseGoalComplexity(`{"complexity":"trivial","reasoning":"one list"}`); err != nil || c != complexityTrivial {
		t.Fatalf("plain string: got %q err %v", c, err)
	}
	// Fenced JSON.
	fenced := "```json\n{\"complexity\":\"complex\",\"reasoning\":\"multi-phase\"}\n```"
	if c, _, err := parseGoalComplexity(fenced); err != nil || c != complexityComplex {
		t.Fatalf("fenced: got %q err %v", c, err)
	}
	// Map input (structured-output path).
	if c, _, err := parseGoalComplexity(map[string]any{"complexity": "moderate"}); err != nil || c != complexityModerate {
		t.Fatalf("map: got %q err %v", c, err)
	}
	// Unknown class -> error + unknown (so caller falls through to decompose).
	if c, _, err := parseGoalComplexity(`{"complexity":"epic"}`); err == nil || c != complexityUnknown {
		t.Fatalf("unknown class should error: got %q err %v", c, err)
	}
	// Nil / junk -> error, never panics.
	if _, _, err := parseGoalComplexity(nil); err == nil {
		t.Fatalf("nil should error")
	}
	if _, _, err := parseGoalComplexity("not json at all"); err == nil {
		t.Fatalf("non-json should error")
	}
}

// --- loop routing (acceptance: trivial never reaches the decompose path) ---

// triageFakeEngine extends the shared fakeEngine pattern: it answers the
// goalComplexityTriage InvokeAI with a canned complexity and the
// queryPlanById Execute with a canned plan row, so we can assert which
// prompts the loop calls.
func newTriageLoop(t *testing.T, complexity, ownerAgentId string) (*PlannerAgentLoop, *fakeEngine) {
	t.Helper()
	planRow := map[string]any{
		"output": []any{
			map[string]any{
				"id":           "plan-1",
				"kind":         "userGoal",
				"status":       "planning",
				"goal":         "a list of 10 birds",
				"requestedBy":  "user-1",
				"ownerAgentId": ownerAgentId,
			},
		},
	}
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			if containsAll(query, "queryPlanById") {
				return planRow, nil
			}
			return nil, nil
		},
		aiResponder: func(templateId string, _ map[string]any) (any, error) {
			if templateId == "goalComplexityTriage" {
				return map[string]any{"complexity": complexity, "reasoning": "test"}, nil
			}
			return nil, nil
		},
	}
	return &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}, fe
}

func containsAll(s string, sub string) bool { return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// Trivial goal WITH an owning agent: triage must shortcut to a single
// direct production turn (mutationStartPlan) and make ZERO plannerAgent
// (decompose) calls.
func TestTriage_TrivialWithOwner_ShortcutsNoDecompose(t *testing.T) {
	l, fe := newTriageLoop(t, "trivial", "agent-1")
	handled, err := l.triageAndMaybeShortcut(context.Background(), "plan-1", "2026-06-03T00:00:00Z")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !handled {
		t.Fatalf("trivial goal with owner must be handled (shortcut), got handled=false")
	}
	_, si, _ := fe.snapshot()
	if countContains(si, "plannerAgent") != 0 {
		t.Fatalf("trivial shortcut must make ZERO plannerAgent (decompose) calls, got %d", countContains(si, "plannerAgent"))
	}
	if countContains(si, "goalComplexityTriage") != 1 {
		t.Fatalf("expected exactly 1 cheap triage call, got %d", countContains(si, "goalComplexityTriage"))
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "mutationStartPlan") != 1 {
		t.Fatalf("trivial shortcut must call mutationStartPlan once, got %d", countContains(exec, "mutationStartPlan"))
	}
}

// Trivial goal WITHOUT an owning agent: can't run a single turn with no
// agent, so it falls through (handled=false) to the decompose loop.
func TestTriage_TrivialNoOwner_FallsThrough(t *testing.T) {
	l, fe := newTriageLoop(t, "trivial", "")
	handled, err := l.triageAndMaybeShortcut(context.Background(), "plan-1", "")
	if err != nil || handled {
		t.Fatalf("trivial without owner must fall through: handled=%v err=%v", handled, err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "mutationStartPlan") != 0 {
		t.Fatalf("no-owner trivial must NOT shortcut-start the plan, got %d mutationStartPlan", countContains(exec, "mutationStartPlan"))
	}
}

// Complex goal: never shortcuts, always falls through to the decompose loop.
func TestTriage_Complex_FallsThrough(t *testing.T) {
	l, _ := newTriageLoop(t, "complex", "agent-1")
	handled, err := l.triageAndMaybeShortcut(context.Background(), "plan-1", "")
	if err != nil || handled {
		t.Fatalf("complex goal must fall through to decompose: handled=%v err=%v", handled, err)
	}
}

// A classifier failure must never strand the Plan -- fall through to the
// (bounded) decompose loop.
func TestTriage_ClassifyError_FallsThrough(t *testing.T) {
	l, _ := newTriageLoop(t, "", "agent-1") // empty complexity -> parse error
	handled, err := l.triageAndMaybeShortcut(context.Background(), "plan-1", "")
	if err != nil || handled {
		t.Fatalf("classify error must fall through, not strand: handled=%v err=%v", handled, err)
	}
}

// Sanity: the marshaled triage decision round-trips so the prompt schema
// and parser agree on field names.
func TestGoalTriageDecision_RoundTrip(t *testing.T) {
	b, _ := json.Marshal(goalTriageDecision{Complexity: "trivial", Reasoning: "x"})
	c, r, err := parseGoalComplexity(string(b))
	if err != nil || c != complexityTrivial || r != "x" {
		t.Fatalf("round-trip failed: c=%q r=%q err=%v", c, r, err)
	}
}

// --- produceArtifact routing (memql#1393: the known-trivial shortcut is
// gone; the classifier decides, with a direct-turn fallback on error) ---

// newProduceArtifactTriageLoop mirrors newTriageLoop but mints the plan as
// kind=produceArtifact. complexity "" makes the classifier emit an
// unparseable decision (the classify-error path).
func newProduceArtifactTriageLoop(t *testing.T, complexity, ownerAgentId string) (*PlannerAgentLoop, *fakeEngine) {
	t.Helper()
	planRow := map[string]any{
		"output": []any{
			map[string]any{
				"id":           "plan-1",
				"kind":         produceArtifactPlanKind,
				"status":       "planning",
				"goal":         "a markdown file with 10 folk tales, each told as a complete story",
				"requestedBy":  "user-1",
				"ownerAgentId": ownerAgentId,
			},
		},
	}
	fe := &fakeEngine{
		execResponder: func(query string) (any, error) {
			if containsAll(query, "queryPlanById") {
				return planRow, nil
			}
			return nil, nil
		},
		aiResponder: func(templateId string, _ map[string]any) (any, error) {
			if templateId == "goalComplexityTriage" {
				return map[string]any{"complexity": complexity, "reasoning": "test"}, nil
			}
			return nil, nil
		},
	}
	return &PlannerAgentLoop{engine: fe, logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}, fe
}

// Small artifact: classifier says trivial -> single direct production turn,
// exactly the pre-#1393 behavior, now with one cheap classifier call.
func TestTriage_ProduceArtifactTrivial_RoutesDirect(t *testing.T) {
	l, fe := newProduceArtifactTriageLoop(t, "trivial", "agent-1")
	handled, err := l.triageAndMaybeShortcut(context.Background(), "plan-1", "")
	if err != nil || !handled {
		t.Fatalf("trivial produceArtifact must shortcut direct: handled=%v err=%v", handled, err)
	}
	exec, si, _ := fe.snapshot()
	if countContains(si, "goalComplexityTriage") != 1 {
		t.Fatalf("expected exactly 1 classifier call, got %d", countContains(si, "goalComplexityTriage"))
	}
	if countContains(exec, "mutationStartPlan") != 1 {
		t.Fatalf("expected 1 mutationStartPlan, got %d", countContains(exec, "mutationStartPlan"))
	}
}

// Large artifact: classifier says moderate -> fall through to the decompose
// loop (the #1393 fix: '10 full stories' must NOT be forced into one turn).
func TestTriage_ProduceArtifactModerate_FallsThroughToDecompose(t *testing.T) {
	l, fe := newProduceArtifactTriageLoop(t, "moderate", "agent-1")
	handled, err := l.triageAndMaybeShortcut(context.Background(), "plan-1", "")
	if err != nil || handled {
		t.Fatalf("moderate produceArtifact must fall through to decompose: handled=%v err=%v", handled, err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "mutationStartPlan") != 0 {
		t.Fatalf("moderate artifact must NOT shortcut-start, got %d mutationStartPlan", countContains(exec, "mutationStartPlan"))
	}
}

// Classifier failure on a produceArtifact plan falls BACK to the direct
// turn (the pre-#1393 posture) instead of the decompose loop -- the plan
// always has an owner and a small first pass beats stranding it.
func TestTriage_ProduceArtifactClassifyError_FallsBackDirect(t *testing.T) {
	l, fe := newProduceArtifactTriageLoop(t, "", "agent-1") // "" -> unknown class -> classify error
	handled, err := l.triageAndMaybeShortcut(context.Background(), "plan-1", "")
	if err != nil || !handled {
		t.Fatalf("classify error on produceArtifact must fall back to direct: handled=%v err=%v", handled, err)
	}
	exec, _, _ := fe.snapshot()
	if countContains(exec, "mutationStartPlan") != 1 {
		t.Fatalf("expected the direct-turn fallback to start the plan once, got %d", countContains(exec, "mutationStartPlan"))
	}
}

// produceArtifact without an owning agent falls through before any
// classifier call -- the decompose loop assigns an agent.
func TestTriage_ProduceArtifactNoOwner_FallsThrough(t *testing.T) {
	l, fe := newProduceArtifactTriageLoop(t, "trivial", "")
	handled, err := l.triageAndMaybeShortcut(context.Background(), "plan-1", "")
	if err != nil || handled {
		t.Fatalf("ownerless produceArtifact must fall through: handled=%v err=%v", handled, err)
	}
	if _, si, _ := fe.snapshot(); countContains(si, "goalComplexityTriage") != 0 {
		t.Fatalf("ownerless produceArtifact must not call the classifier, got %d", countContains(si, "goalComplexityTriage"))
	}
}

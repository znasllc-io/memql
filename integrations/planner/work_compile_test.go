package planner

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// countingCompileEngine records every Execute and every provider call, so
// "made no model call" is an observation rather than an assumption.
type countingCompileEngine struct {
	queries   []string
	aiCalls   []string
	catalogue []map[string]any
}

func (e *countingCompileEngine) Execute(_ context.Context, q string) (any, error) {
	e.queries = append(e.queries, q)
	if strings.Contains(q, "cataloguedConstructsForGoalSignature") {
		// Model the query's own filter. A fake that returns its whole
		// fixture regardless of the argument tests the fake, not the code.
		// Parse the NAMED-ARGS form `goalSignature: "..."`. A fake that
		// matched the object-literal form would keep passing against a
		// renderer the parser rejects, which is how this defect survived
		// in the tree: the fake, not the engine, was the thing agreeing.
		want := ""
		if i := strings.Index(q, `goalSignature: "`); i >= 0 {
			rest := q[i+len(`goalSignature: "`):]
			if j := strings.Index(rest, `"`); j >= 0 {
				want = rest[:j]
			}
		}
		out := []map[string]any{}
		for _, r := range e.catalogue {
			if s, _ := r["goalSignature"].(string); s == want {
				out = append(out, r)
			}
		}
		return out, nil
	}
	return []map[string]any{}, nil
}

func (e *countingCompileEngine) InvokeAI(_ context.Context, templateId string, _ map[string]any) (any, error) {
	e.aiCalls = append(e.aiCalls, templateId)
	return map[string]any{}, nil
}

func (e *countingCompileEngine) InvokeAIChatWithFilteredTools(_ context.Context, templateId string, _ map[string]any, _ []string) (string, error) {
	e.aiCalls = append(e.aiCalls, templateId)
	return "", nil
}

// countingNearMatcher proves the near tier is not consulted on an exact hit.
type countingNearMatcher struct {
	calls   int
	matches []memql.CatalogNearMatch
}

func (m *countingNearMatcher) CatalogNearMatches(_ context.Context, _ string, _ int) ([]memql.CatalogNearMatch, error) {
	m.calls++
	return m.matches, nil
}

func compileReq() CompileRequest {
	return CompileRequest{
		GoalId:      "v1:work:goal:g1",
		RunId:       "v1:work:run:r1",
		OwnerUserId: "u1",
		Statement:   "Summarise yesterday's support tickets",
		Input:       map[string]any{"day": "2026-09-04"},
	}
}

// THE HEADLINE TEST (spec section J, issue #4967): a goal that fully
// matches the catalog makes ZERO provider calls.
func TestCompileGoalForRun_ExactCatalogHitMakesZeroModelCalls(t *testing.T) {
	sig := work.GoalSignature("Summarise yesterday's support tickets", []string{"day"})
	eng := &countingCompileEngine{catalogue: []map[string]any{
		{"id": "v1:authoring:construct:c1", "name": "summariseTickets", "goalSignature": sig, "reliability": 0.9},
	}}
	near := &countingNearMatcher{}
	l := &PlannerAgentLoop{engine: eng}

	out, err := l.CompileGoalForRun(context.Background(), compileReq(), near, nil)
	if err != nil {
		t.Fatalf("CompileGoalForRun: %v", err)
	}
	if out.Route != work.RouteCatalogExact {
		t.Fatalf("route = %q, want catalogExact", out.Route)
	}
	if out.ModelCalls != 0 {
		t.Errorf("ModelCalls = %d, want 0", out.ModelCalls)
	}
	if len(eng.aiCalls) != 0 {
		t.Fatalf("an exact catalog hit reached a provider %d time(s): %v -- this is the claim the whole catalog exists to make", len(eng.aiCalls), eng.aiCalls)
	}
	if near.calls != 0 {
		t.Errorf("the near tier was consulted %d time(s) on an exact hit; it costs a vector search", near.calls)
	}
	if out.ConstructId != "v1:authoring:construct:c1" || out.AutomationName != "summariseTickets" {
		t.Errorf("outcome = %+v", out)
	}
}

// The negative control. Without it the test above proves nothing: a
// counter that is never incremented on ANY path reads as zero forever.
func TestCompileGoalForRun_ACatalogMissDoesReachAProvider(t *testing.T) {
	eng := &countingCompileEngine{catalogue: []map[string]any{}}
	near := &countingNearMatcher{}
	l := &PlannerAgentLoop{engine: eng}

	out, err := l.CompileGoalForRun(context.Background(), compileReq(), near, nil)
	if err == nil && out.Route == work.RouteCatalogExact {
		t.Fatal("a catalog miss must not report an exact hit")
	}
	if out.ModelCalls == 0 {
		t.Fatal("a catalog miss reaches the triage classifier; a counter that stays zero here is measuring nothing, which would make the headline test vacuous")
	}
	if near.calls != 1 {
		t.Errorf("the near tier must be consulted exactly once on an exact miss, got %d", near.calls)
	}
}

// An exact hit must win even when a near match scores higher, or the free
// path becomes unreachable exactly when the catalog is richest.
func TestCompileGoalForRun_ExactBeatsAStrongNearMatch(t *testing.T) {
	sig := work.GoalSignature("Summarise yesterday's support tickets", []string{"day"})
	eng := &countingCompileEngine{catalogue: []map[string]any{
		{"id": "c-exact", "name": "exactOne", "goalSignature": sig, "reliability": 0.1},
	}}
	near := &countingNearMatcher{matches: []memql.CatalogNearMatch{
		{CatalogEntry: memql.CatalogEntry{Name: "nearOne"}, Similarity: 0.99},
	}}
	l := &PlannerAgentLoop{engine: eng}
	out, _ := l.CompileGoalForRun(context.Background(), compileReq(), near, nil)
	if out.ConstructId != "c-exact" {
		t.Fatalf("a 0.99 near match outranked an exact hit: %+v", out)
	}
}

// Reliability decides WHICH catalogued template answers a repeated goal.
func TestCompileGoalForRun_MostProvenTemplateWins(t *testing.T) {
	sig := work.GoalSignature("Summarise yesterday's support tickets", []string{"day"})
	eng := &countingCompileEngine{catalogue: []map[string]any{
		{"id": "c-weak", "name": "weak", "goalSignature": sig, "reliability": 0.2},
		{"id": "c-strong", "name": "strong", "goalSignature": sig, "reliability": 0.95},
	}}
	l := &PlannerAgentLoop{engine: eng}
	out, _ := l.CompileGoalForRun(context.Background(), compileReq(), nil, nil)
	if out.ConstructId != "c-strong" {
		t.Fatalf("the most-proven template must answer a repeated goal, got %+v", out)
	}
}

func TestCompileGoalForRun_RefusesAnEmptyStatement(t *testing.T) {
	l := &PlannerAgentLoop{engine: &countingCompileEngine{}}
	req := compileReq()
	req.Statement = "   "
	if _, err := l.CompileGoalForRun(context.Background(), req, nil, nil); err == nil {
		t.Fatal("an empty goal has nothing to compile and must be refused rather than authored")
	}
}

// The signature is computed over the goal AND its input shape, so the
// exact tier cannot serve a template that takes different arguments.
func TestCompileGoalForRun_DifferentInputShapeIsNotAnExactHit(t *testing.T) {
	sig := work.GoalSignature("Summarise yesterday's support tickets", []string{"day"})
	eng := &countingCompileEngine{catalogue: []map[string]any{
		{"id": "c1", "name": "summariseTickets", "goalSignature": sig, "reliability": 0.9},
	}}
	l := &PlannerAgentLoop{engine: eng}
	req := compileReq()
	req.Input = map[string]any{"day": "2026-09-04", "team": "support"}
	out, _ := l.CompileGoalForRun(context.Background(), req, nil, nil)
	if out.Route == work.RouteCatalogExact {
		t.Fatal("a goal taking an extra argument is a different signature and must not be served as an exact hit")
	}
}

// The exact tier's read must use the named-args form too. It is a READ,
// so the failure is worse than a silent no-op: the parser refuses it, the
// error is swallowed into "catalog read failed", and compile falls
// through to the PAID tiers on every single goal -- the catalog would
// simply never hit, and nothing would say so.
func TestCompileGoalForRun_ExactTierRendersTheNamedArgsForm(t *testing.T) {
	eng := &countingCompileEngine{}
	l := &PlannerAgentLoop{engine: eng}
	if _, err := l.CompileGoalForRun(context.Background(), compileReq(), nil, nil); err != nil {
		_ = err // the route is irrelevant here; the rendered query is the subject
	}
	var found string
	for _, q := range eng.queries {
		if strings.Contains(q, "cataloguedConstructsForGoalSignature") {
			found = q
		}
	}
	if found == "" {
		t.Fatal("the exact tier did not run")
	}
	if strings.Contains(found, "({") {
		t.Fatalf("the exact tier uses the REMOVED object-literal wrapper; the parser refuses it and every goal would fall through to the paid tiers with nothing saying so: %s", found)
	}
	if !strings.Contains(found, "goalSignature: ") {
		t.Fatalf("expected the named-args form `goalSignature: \"...\"`, got %s", found)
	}
}

// readSource reads a file in this package for the source-level assertions.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

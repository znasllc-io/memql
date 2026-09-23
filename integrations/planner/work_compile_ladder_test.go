package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

// work_compile_ladder_test.go -- the exact tier reads the certification
// ladder (epic memql#5408, D15). A learned procedure is served from the
// construct only on a rung DecideServe says to serve from -- trusted, or
// canary with the app standing by -- and always through the one embedded
// replay template, never as its own authored construct.

func ladderSig() string {
	return work.GoalSignature("Summarise yesterday's support tickets", []string{"day"})
}

func learnedRow(id, rung string, reliability float64) map[string]any {
	return map[string]any{
		"id": id, "name": "learnedProcedure_" + id, "goalSignature": ladderSig(),
		"ladder": rung, "targetNamespace": "procedure", "reliability": reliability,
	}
}

// TestATrustedProcedureOnAnExactHitIsServedByTheReplayAutomationWithNoModel
// is the headline of the ladder at compile: the goal is answered by the
// procedure, compile reaches no provider, and the run is pointed at the
// replay template with the construct id in its variables -- never at the
// procedure's own bundle, which the template loader would refuse.
func TestATrustedProcedureOnAnExactHitIsServedByTheReplayAutomationWithNoModel(t *testing.T) {
	for _, rung := range []string{"trusted", "canary"} {
		t.Run(rung, func(t *testing.T) {
			eng := &countingCompileEngine{procedures: []map[string]any{learnedRow("v1:authoring:construct:p1", rung, 0.5)}}
			near := &countingNearMatcher{}
			l := &PlannerAgentLoop{engine: eng}

			out, err := l.CompileGoalForRun(context.Background(), compileReq(), near, nil)
			if err != nil {
				t.Fatalf("CompileGoalForRun: %v", err)
			}
			if out.Route != work.RouteCatalogExact {
				t.Fatalf("route = %q, want catalogExact", out.Route)
			}
			if out.ModelCalls != 0 || len(eng.aiCalls) != 0 {
				t.Fatalf("a %s procedure reached a provider: ModelCalls=%d calls=%v", rung, out.ModelCalls, eng.aiCalls)
			}
			if out.AutomationName != replayProcedureAutomation {
				t.Errorf("AutomationName = %q, want %q", out.AutomationName, replayProcedureAutomation)
			}
			if out.ConstructId != "" {
				t.Errorf("ConstructId = %q: a procedure must not become the run's templateConstructId", out.ConstructId)
			}
			if got := out.Variables["procedureConstructId"]; got != "v1:authoring:construct:p1" {
				t.Errorf("Variables = %v, want procedureConstructId", out.Variables)
			}
			if near.calls != 0 {
				t.Errorf("the near tier was consulted on an exact hit (%d)", near.calls)
			}
		})
	}
}

// TestAShadowProcedureIsNotServedAndTheAppRuns: in shadow the app still
// answers every goal (D15) -- the procedure is compared beside it once the
// recording succeeds. So compile must fall through to the paid tiers exactly
// as if the ladder held nothing.
func TestAShadowProcedureIsNotServedAndTheAppRuns(t *testing.T) {
	eng := &countingCompileEngine{procedures: []map[string]any{learnedRow("v1:authoring:construct:p1", "shadow", 0.9)}}
	l := &PlannerAgentLoop{engine: eng}

	out, _ := l.CompileGoalForRun(context.Background(), compileReq(), &countingNearMatcher{}, nil)
	if out.Route == work.RouteCatalogExact {
		t.Fatalf("a shadow procedure was served: %+v", out)
	}
	if out.ModelCalls == 0 {
		t.Error("a goal whose only procedure is in shadow must reach the triage classifier; zero here means the ladder served it")
	}
}

// TestACandidateOrRetiredProcedureIsNeverServed -- including a rung this
// build does not know, which must read as "not servable" rather than as the
// nearest spelling.
func TestACandidateOrRetiredProcedureIsNeverServed(t *testing.T) {
	for _, rung := range []string{"candidate", "retired", "Trusted", ""} {
		t.Run("rung="+rung, func(t *testing.T) {
			eng := &countingCompileEngine{procedures: []map[string]any{learnedRow("v1:authoring:construct:p1", rung, 1)}}
			l := &PlannerAgentLoop{engine: eng}
			out, _ := l.CompileGoalForRun(context.Background(), compileReq(), &countingNearMatcher{}, nil)
			if out.AutomationName == replayProcedureAutomation {
				t.Fatalf("a %q procedure was served", rung)
			}
		})
	}
}

// TestATrustedProcedureOutranksACanaryAndTheAuthoredCatalog: when several
// things answer one goal exactly, the proven procedure serves. A canary
// still has the app on standby, and an authored template has not been
// compared against anything.
func TestATrustedProcedureOutranksACanaryAndTheAuthoredCatalog(t *testing.T) {
	eng := &countingCompileEngine{
		catalogue: []map[string]any{{"id": "v1:authoring:construct:authored", "name": "summariseTickets", "goalSignature": ladderSig(), "reliability": 1.0}},
		procedures: []map[string]any{
			learnedRow("v1:authoring:construct:canary", "canary", 0.99),
			learnedRow("v1:authoring:construct:trusted", "trusted", 0.10),
		},
	}
	l := &PlannerAgentLoop{engine: eng}
	out, err := l.CompileGoalForRun(context.Background(), compileReq(), &countingNearMatcher{}, nil)
	if err != nil {
		t.Fatalf("CompileGoalForRun: %v", err)
	}
	if got := out.Variables["procedureConstructId"]; got != "v1:authoring:construct:trusted" {
		t.Errorf("served %v, want the trusted procedure", got)
	}
}

// TestALearnedRowForAnotherSignatureIsDropped: the query filters on the
// signature, and the compile re-checks it, because a served procedure runs
// with no model reading it first.
func TestALearnedRowForAnotherSignatureIsDropped(t *testing.T) {
	row := learnedRow("v1:authoring:construct:p1", "trusted", 1)
	eng := &countingCompileEngine{}
	l := &PlannerAgentLoop{engine: &mislabelledProcedures{countingCompileEngine: eng, row: row}}
	out, _ := l.CompileGoalForRun(context.Background(), compileReq(), &countingNearMatcher{}, nil)
	if out.AutomationName == replayProcedureAutomation {
		t.Fatal("a procedure whose row carries a different goalSignature was served")
	}
}

// mislabelledProcedures answers the procedure query with a row whose
// signature does not match what was asked -- the query's filter failing,
// which the compile must not trust blindly.
type mislabelledProcedures struct {
	*countingCompileEngine
	row map[string]any
}

func (m *mislabelledProcedures) Execute(ctx context.Context, q string) (any, error) {
	if strings.Contains(q, "procedureConstructsForGoalSignature") {
		r := map[string]any{}
		for k, v := range m.row {
			r[k] = v
		}
		r["goalSignature"] = "some-other-signature"
		return []map[string]any{r}, nil
	}
	return m.countingCompileEngine.Execute(ctx, q)
}

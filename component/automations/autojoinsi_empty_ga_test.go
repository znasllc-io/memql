package automations

import (
	"encoding/json"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// Regression for memql#1044 (autoJoinSI errors when the space owner has no
// assistant agent).
//
// In logicAutoJoinSI the GA is resolved with
//
//	getGA := coalesce(getActiveGA, getFallbackGA)
//
// When the owner has NO assistant, getActiveGA is a skipped `if` and
// getFallbackGA returns 0 rows, so coalesce(...) yields a NON-NIL but
// EMPTY (0-node) ExecuteResult. The original guard `!getGA.Empty()`
// could never gate that case: the automation condition evaluator does
// not apply the leading `!` negation operator, so `!getGA.Empty()`
// evaluated truthy unconditionally, the join branch fired with no GA,
// and coalesce(getGA.First().id, "") resolved to nil -- which
// queryParticipantByAgentSpace rejected as a missing required
// `agentId`.
//
// The fix guards the join on a POSITIVE id-presence check,
// `first(getGA).id != ""`, which the compiler rewrites to a fully
// resolvable `$steps.getGA.result.Bundle.nodes.0.id` path. These tests
// pin both the runtime condition semantics and the compiled shape so a
// revert to the broken `!...Empty()` form is caught.

func autoJoinResult(ids ...string) *memqlengine.ExecuteResult {
	var nodes []*memqlv1.MemoryNode
	for _, id := range ids {
		payload, err := structpb.NewStruct(map[string]any{"name": "Sofia"})
		if err != nil {
			panic(err)
		}
		nodes = append(nodes, &memqlv1.MemoryNode{Id: id, Payload: payload})
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}
}

// The runtime guard `first(getGA).id != ""` (in its compiled,
// path-resolvable form) must be FALSE when the owner has no assistant
// (empty getGA) so the join branch cleanly no-ops, and TRUE when the
// owner has an assistant so the GA joins.
func TestAutoJoinSI_JoinGuard_NoOpsWithoutAssistant(t *testing.T) {
	// The guard as the compiler emits it (see
	// TestAutoJoinSI_JoinGuardCompilesToResolvablePath).
	const guard = `$steps.getGA.result.Bundle.nodes.0.id != ""`

	cases := []struct {
		name     string
		getGA    *memqlengine.ExecuteResult
		wantJoin bool
	}{
		{
			name:     "owner has no assistant -> clean no-op",
			getGA:    autoJoinResult(), // 0 nodes: skipped active + empty fallback
			wantJoin: false,
		},
		{
			name:     "owner has an assistant -> join",
			getGA:    autoJoinResult("v1:agents:agent:assistant-REAL"),
			wantJoin: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eval := NewEvaluator()
			eval.SetStepResult("getGA", &StepResult{
				StepId: "getGA",
				Status: "success",
				Result: tc.getGA,
			})

			got, err := eval.EvaluateCondition(guard)
			if err != nil {
				t.Fatalf("EvaluateCondition(%q): %v", guard, err)
			}
			if got != tc.wantJoin {
				t.Fatalf("join guard = %v, want %v (owner-no-assistant must no-op, owner-with-assistant must join)", got, tc.wantJoin)
			}
		})
	}
}

// Pins the now-FIXED negation semantics behind the original #1044 bug.
//
// memql#1096 taught the condition evaluator to honour a leading `!`, so the
// `! $steps.getGA.Empty` form the compiler emits for `!getGA.Empty()` now
// correctly evaluates to FALSE on an empty result (Empty=true, !Empty=false)
// and TRUE on a non-empty one. The #1044 DSL guard was rewritten to the
// positive `first(getGA).id != ""` form before the evaluator was fixed; that
// form stays as belt-and-suspenders (see
// TestAutoJoinSI_JoinGuard_NoOpsWithoutAssistant), but the `!`-negation form
// it replaced is no longer broken -- this test pins that fix.
func TestAutoJoinSI_BangEmptyGuard_NowHonoursNegation(t *testing.T) {
	// `! ...` is the form the compiler emits for `!getGA.Empty()`.
	const guard = "! $steps.getGA.Empty"

	cases := []struct {
		name  string
		getGA *memqlengine.ExecuteResult
		want  bool
	}{
		{
			name:  "empty getGA -> Empty=true -> !Empty is false (guard no-ops)",
			getGA: autoJoinResult(),
			want:  false,
		},
		{
			name:  "non-empty getGA -> Empty=false -> !Empty is true (guard fires)",
			getGA: autoJoinResult("v1:agents:agent:assistant-REAL"),
			want:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eval := NewEvaluator()
			eval.SetStepResult("getGA", &StepResult{
				StepId: "getGA",
				Status: "success",
				Result: tc.getGA,
			})

			got, err := eval.EvaluateCondition(guard)
			if err != nil {
				t.Fatalf("EvaluateCondition(%q): %v", guard, err)
			}
			if got != tc.want {
				t.Fatalf("`%s` = %v, want %v (#1096 fixed leading-! negation)", guard, got, tc.want)
			}
		})
	}
}

// The DSL guard `first(getGA).id != ""` must compile to a fully
// resolvable `$steps`-path comparison (not a raw `*.Empty()` method-call
// or `!`-negation, neither of which the condition evaluator resolves at
// runtime).
func TestAutoJoinSI_JoinGuardCompilesToResolvablePath(t *testing.T) {
	src := `use cognition.queries.{ queryParticipantByAgentSpace }
@description("autoJoinSI guard shape")
logic logicAutoJoinGuardShape {
  args { event object @required }
  body {
    getGA := queryParticipantByAgentSpace({ spaceId: args.event.payload.id, agentId: "seed" })
    joinGA := if first(getGA).id != "" {
      queryParticipantByAgentSpace({ spaceId: args.event.payload.id, agentId: "join" })
    }
    return joinGA
  }
}`
	body := parseLogicBody(t, src)
	r := NewLogicRunner(nil, nil, nil)
	auto, err := r.compileBodyToAutomation("logicAutoJoinGuardShape", body)
	if err != nil {
		t.Fatalf("compileBodyToAutomation: %v", err)
	}
	raw, err := json.Marshal(auto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	compiled := string(raw)

	if !strings.Contains(compiled, `$steps.getGA.result.Bundle.nodes.0.id`) {
		t.Fatalf("compiled guard missing resolvable first()-id path; got:\n%s", compiled)
	}
	// Guard against a regression to either runtime-unresolvable form.
	if strings.Contains(compiled, `getGA.Empty()`) {
		t.Fatalf("compiled guard still carries the runtime-unresolvable getGA.Empty() method-call form:\n%s", compiled)
	}
}

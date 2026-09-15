package automations

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// Regression for memql#1044 (autoJoinAI errors when the space owner has no
// assistant agent).
//
// The GA was resolved as `getGA := getActiveGA ?? getFallbackGA`. When the
// owner has NO assistant, getActiveGA is a skipped `if` and getFallbackGA
// returns 0 rows, so getGA is a NON-NIL but EMPTY (0-node) result. The
// original guard `!getGA.empty()` could never gate that case in the string
// evaluator, which did not apply a leading `!` (memql#1096 later taught it
// to), so the join branch fired with no GA and the write was refused for a
// missing `agentId`.
//
// Both guard spellings must decide correctly over a query statement's rows:
// the positive id-presence check and the `!`-negation.

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

// The id-presence guard must be FALSE when the owner has no assistant
// (empty getGA) so the join branch cleanly no-ops, and TRUE when the owner
// has an assistant so the GA joins.
func TestAutoJoinAI_JoinGuard_NoOpsWithoutAssistant(t *testing.T) {
	const guard = `getGA.first().id != nil`

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
			eval.enterStatements()
			eval.Bind("getGA", functionStatementValue("query", tc.getGA))

			got, err := evalV1Cond(t, eval, guard)
			if err != nil {
				t.Fatalf("%s: %v", guard, err)
			}
			if got != tc.wantJoin {
				t.Fatalf("join guard = %v, want %v (owner-no-assistant must no-op, owner-with-assistant must join)", got, tc.wantJoin)
			}
		})
	}
}

// The `!`-negation guard: `!getGA.empty()` is FALSE on an empty result and
// TRUE on a non-empty one.
func TestAutoJoinAI_BangEmptyGuard_HonoursNegation(t *testing.T) {
	const guard = "!getGA.empty()"

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
			eval.enterStatements()
			eval.Bind("getGA", functionStatementValue("query", tc.getGA))

			got, err := evalV1Cond(t, eval, guard)
			if err != nil {
				t.Fatalf("%s: %v", guard, err)
			}
			if got != tc.want {
				t.Fatalf("`%s` = %v, want %v", guard, got, tc.want)
			}
		})
	}
}

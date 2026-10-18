package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func TestLookupContractsAreAuthorizedBoundedAndReplicaIndependent(t *testing.T) {
	registry := memql.NewPromptRegistry()
	if _, err := memql.LoadUnifiedPrompts(nil, registry, template.New("partials")); err != nil {
		t.Fatal(err)
	}
	owner := "v1:identity:user:lookup-contract-owner"
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), owner), common.RunContext{RunId: "persisted-run", GoalId: "goal", OwnerUserId: owner})
	for _, tc := range []struct {
		name, workload, title, statement, search string
		fail, large, want                        bool
	}{
		{name: "fresh executing replica", workload: "lookup", title: "Count current records", want: true},
		{name: "owner request beats object title", workload: "lookup", title: "Read capability-check.md", statement: "Read the saved file and identify its first item.", want: true},
		{name: "owner request needs no generated title", workload: "lookup", statement: "Read the saved file.", want: true},
		{name: "long request falls back to title", workload: "lookup", title: "Read saved records", statement: strings.Repeat("saved records ", 30), want: true},
		{name: "semantic action survives replica hop", workload: "lookup", title: "Identify first item", statement: "What is the first item in the file you just created?", search: "read file content", want: true},
		{name: "invalid semantic action falls back", workload: "lookup", title: "Read file", statement: "Read the saved file", search: strings.Repeat("x", 301), want: true},
		{name: "quick", workload: "quick", title: "Greeting"},
		{name: "research", workload: "research", title: "Research records"},
		{name: "missing title", workload: "lookup"},
		{name: "unavailable discovery", workload: "lookup", title: "Records", fail: true},
		{name: "oversized discovery", workload: "lookup", title: "Records", large: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := []string{}
			engine := &workPromptEngine{registryEngine: registryEngine{registered: map[string]bool{"discoverCapabilities": true, "executeCapability": true}}, prompts: registry}
			engine.execute = func(callCtx context.Context, q string) (any, error) {
				ac, _ := auth.AccessFromContext(callCtx)
				if ac == nil || ac.UserId != owner {
					t.Fatalf("lost owner: %+v", ac)
				}
				calls = append(calls, q)
				if strings.HasPrefix(q, "query work.workRunForOwner(") {
					return []map[string]any{{"classification": map[string]any{"workload": tc.workload, "workTitle": tc.title, "capabilitySearch": tc.search}}}, nil
				}
				if strings.HasPrefix(q, "query work.workGoalForOwner(") {
					return []map[string]any{{"statement": tc.statement}}, nil
				}
				if strings.HasPrefix(q, "query work.workCapabilities(") {
					search := tc.title
					if tc.statement != "" && len(tc.statement) <= 300 {
						search = tc.statement
					}
					if tc.search != "" && len(tc.search) <= 300 {
						search = tc.search
					}
					if !strings.Contains(q, search) {
						t.Fatalf("discovery lost requested operation: %s", q)
					}
					if tc.fail {
						return nil, fmt.Errorf("unavailable")
					}
					contract := "records.currentRecords"
					if tc.large {
						contract = strings.Repeat("x", 25*1024)
					}
					return map[string]any{"data": map[string]any{"capabilities": map[string]any{"id": "capabilities", "payload": map[string]any{"capabilities": []any{map[string]any{"name": contract, "arguments": []any{}}}}}}}, nil
				}
				return nil, nil
			}
			// No originating classifier cache exists here. An untrusted lookup hint
			// cannot change the persisted classification used by this executor.
			msg := &memqlv1.AgentGenerateTurnMsg{AgentId: "planner", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "planner", Role: "specialist"}, Hints: map[string]string{HarnessRoleHintKey: "assistant", "workload": "lookup"}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "Check current records"}}}
			prepared, err := newTestReplier(engine).prepareTurn(ctx, msg, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(prepared.messages[0].Content, "Authorized capability contracts already discovered")
			if got != tc.want {
				t.Fatalf("contracts included=%v want=%v", got, tc.want)
			}
			if tc.want && !strings.Contains(prepared.messages[0].Content, "records.currentRecords") {
				t.Fatal("contract lost through adapter envelope")
			}
			if prepared.routerReq.Level != airoute.LevelStrong {
				t.Fatal("discovery downgraded reasoning")
			}
			for _, q := range calls {
				if strings.Contains(q, "query records.currentRecords") {
					t.Fatal("prefetch executed user data query")
				}
			}
			before := len(calls)
			if got := newTestReplier(engine).workLookupContracts(auth.ContextWithUserActor(ctx, "someone-else")); got != "" || len(calls) != before {
				t.Fatal("foreign owner reached discovery")
			}
		})
	}
}

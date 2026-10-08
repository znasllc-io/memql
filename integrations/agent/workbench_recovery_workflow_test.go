//go:build agent

package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/workbench"
)

func recoveryScope(t *testing.T, needs []string) *toolRecoveryScope {
	t.Helper()
	original := mismatchResult(t, needs, "darwin")
	plan, ok := planWorkbenchReroute(original, hostArgs(), testTurn())
	if !ok {
		t.Fatal("missing verified mismatch")
	}
	return &toolRecoveryScope{plan: plan, turn: testTurn(), original: original}
}

func TestRecoveryDSLChoosesHostOrComputerAndPreservesForwardedAuthority(t *testing.T) {
	for _, tc := range []struct{ need, tool string }{
		{workbench.NeedMacOSTooling, "workerHost"}, {workbench.NeedDisplay, "workerComputer"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			s := recoveryScope(t, []string{tc.need})
			run := common.RunContext{RunId: "plan-1", OwnerUserId: "user-1", StepKey: "remote-step"}
			calls := 0
			s.execute = func(ctx context.Context, tool string, args map[string]any) (string, error) {
				calls++
				got, _ := common.RunFromContext(ctx)
				if !reflect.DeepEqual(got, run) || tool != tc.tool || args["runId"] != run.RunId || args["ownerUserId"] != run.OwnerUserId {
					t.Fatalf("lost forwarded scope: tool=%s args=%v run=%+v", tool, args, got)
				}
				if tc.tool == "workerComputer" && args["action"] != "window_list" {
					t.Fatal("invented computer interaction")
				}
				if tc.tool == "workerHost" && !reflect.DeepEqual(args["args"], hostArgs()["args"]) {
					t.Fatal("rewrote original command")
				}
				return `{"ok":true,"payload":{"verified":"result"}}`, nil
			}
			out, err := s.run(common.ContextWithRun(context.Background(), run), nil)
			if err != nil || calls != 1 || !strings.Contains(out, "verified") {
				t.Fatalf("out=%s err=%v calls=%d", out, err, calls)
			}
		})
	}
}

func TestRecoveryDSLConsentKillSwitchAndUncertainOutcomes(t *testing.T) {
	for _, tc := range []struct {
		code string
		card bool
	}{
		{"denied_no_per_task_approval", true}, {"denied_by_scope", true}, {"kill_switch_engaged", false},
		{"no_worker_available", false}, {"timeout", false}, {"", false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			s := recoveryScope(t, []string{workbench.NeedMacOSTooling, workbench.NeedDisplay})
			calls := 0
			s.execute = func(_ context.Context, tool string, args map[string]any) (string, error) {
				calls++
				if calls == 1 {
					return `{"ok":false,"errorCode":"` + tc.code + `"}`, nil
				}
				if !tc.card || tool != "requestComputerUseScope" {
					t.Fatalf("unexpected dispatch: %s", tool)
				}
				summary, _ := args["summary"].(string)
				if !strings.Contains(summary, "macOS-only tooling") || !strings.Contains(summary, "graphical display") || args["requestedScope"] != "full" {
					t.Fatalf("lost authored explanation or scope: %v", args)
				}
				return `{"ok":true,"status":"pending_approval"}`, nil
			}
			_, err := s.run(context.Background(), nil)
			want := 1
			if tc.card {
				want++
			}
			if err != nil || calls != want {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
	s := recoveryScope(t, []string{workbench.NeedMacOSTooling})
	uncertain := errors.New("connection lost after send")
	calls := 0
	s.execute = func(context.Context, string, map[string]any) (string, error) { calls++; return "", uncertain }
	if _, err := s.run(context.Background(), nil); !errors.Is(err, uncertain) || calls != 1 {
		t.Fatalf("hidden unknown outcome: %v calls=%d", err, calls)
	}
}

func TestDeveloperRecoveryRecipeCanDeclineAndCannotRepeatOrForgeResults(t *testing.T) {
	for _, tc := range []struct {
		body      string
		wantCalls int
		wantError bool
	}{
		{"context := builtin agentRecoveryContext()\nreturn context.original", 0, false},
		{"action retryHost()\naction retryHost()", 1, true},
		{"action retryHost()\nreturn \"invented success\"", 1, true},
		{"action requestScope(summary: \"unearned consent request\")", 0, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			s := recoveryScope(t, []string{workbench.NeedMacOSTooling})
			calls := 0
			s.execute = func(context.Context, string, map[string]any) (string, error) { calls++; return `{"ok":true}`, nil }
			source := func(kind, name string) (string, error) {
				switch kind + ":" + name {
				case "automation:agentWorkbenchRecovery":
					return "@template\nautomation agentWorkbenchRecovery {\n" + tc.body + "\n}", nil
				case "action:retryHost":
					return "use capabilities.integration.agents.{ spineRetryHost }\naction retryHost { capability spineRetryHost() }", nil
				case "action:requestScope":
					return "use capabilities.integration.agents.{ spineRequestScope }\naction requestScope { args { summary string! } capability spineRequestScope(summary: args.summary) }", nil
				}
				return "", fmt.Errorf("not installed: %s %s", kind, name)
			}
			_, err := s.run(context.Background(), workflowhost.SourceLoader(source))
			if calls != tc.wantCalls || (err != nil) != tc.wantError {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestRecoveryUsesFrozenHookOnAnotherExecutionReplica(t *testing.T) {
	s := recoveryScope(t, []string{workbench.NeedDisplay})
	snapshot, err := workflowhost.CapturePhases("entry", "work.spine/1", map[string]string{"agentWorkbenchRecovery": "companyObserve"}, nil, func(kind, name string) (string, error) {
		switch kind + ":" + name {
		case "automation:entry":
			return "@template\nautomation entry { return true }", nil
		case "automation:companyObserve":
			return "@template\nautomation companyObserve { receipt := action companyObservation()\nreturn receipt.content }", nil
		case "action:companyObservation":
			return "use capabilities.integration.agents.{ spineObserveComputer }\naction companyObservation { capability spineObserveComputer(action: \"display_info\") }", nil
		}
		return "", fmt.Errorf("missing source")
	}, s.operations())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.execute = func(_ context.Context, tool string, args map[string]any) (string, error) {
		calls++
		if tool != "workerComputer" || args["action"] != "display_info" {
			t.Fatalf("substituted installed policy: %s %v", tool, args)
		}
		return `{"ok":true}`, nil
	}
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "plan-1", OwnerUserId: "user-1", Spine: snapshot.Map()})
	out, err := s.run(ctx, func(string, string) (string, error) { t.Fatal("receiver consulted installed source"); return "", nil })
	if err != nil || !strings.Contains(out, `"originalActionExecuted":false`) || calls != 1 {
		t.Fatalf("out=%s calls=%d err=%v", out, calls, err)
	}
}

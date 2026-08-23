//go:build agent

package agent

import (
	"encoding/json"
	"strings"
	"testing"

	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
	"github.com/znasllc-io/memql/integrations/workbench"
)

func mismatchResult(t *testing.T, needs []string, requestedOS string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"ok":        false,
		"errorCode": workbench.ErrCodeEnvironmentMismatch,
		"payload": map[string]any{
			"unmetNeeds":  needs,
			"requestedOs": requestedOS,
			"workbenchOs": "linux",
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(body)
}

func testTurn() turnContext {
	return turnContext{
		AgentId:     "agent-1",
		OwnerUserId: "user-1",
		PartitionId: "space-1",
		PlanId:      "plan-1",
	}
}

func hostArgs() map[string]any {
	return map[string]any{
		"action": "exec",
		"args":   map[string]any{"cmd": "xcodebuild"},
	}
}

func TestOnlyAnEnvironmentMismatchTriggersAReroute(t *testing.T) {
	// Anything else must leave the model's answer alone. A reroute on a
	// SUCCESS would run the same command twice, on two machines.
	for name, result := range map[string]string{
		"success":       `{"ok":true,"payload":{"stdout":"done"}}`,
		"other failure": `{"ok":false,"errorCode":"exec_failed","errorMessage":"exit 1"}`,
		"invalid hint":  `{"ok":false,"errorCode":"` + workbench.ErrCodeInvalidEnvironmentHint + `"}`,
		"not json":      `<html>nope</html>`,
		"empty":         ``,
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := planWorkbenchReroute(result, hostArgs(), testTurn()); ok {
				t.Fatalf("%s must not be treated as a mismatch", name)
			}
		})
	}
}

func TestAnInvalidHintIsNotAMismatch(t *testing.T) {
	// A typo in the agent's own hint ("macos-tooling") must never read as "the
	// workbench cannot do this" -- that would send a call to the user's
	// machine because the model misspelled a word.
	result := `{"ok":false,"errorCode":"` + workbench.ErrCodeInvalidEnvironmentHint + `","errorMessage":"unknown need"}`
	if _, ok := planWorkbenchReroute(result, hostArgs(), testTurn()); ok {
		t.Fatal("an invalid hint must be a caller error, not a reroute trigger")
	}
}

func TestNeedsBecomeRoutingLabelsAndAScope(t *testing.T) {
	cases := []struct {
		name       string
		needs      []string
		os         string
		wantLabels map[string]string
		wantScope  string
	}{
		{
			name: "macOS tooling implies darwin and full access",
			// A need for Xcode IS a need for macOS, stated as the os label so a
			// Mac the cockpit already tagged os=darwin matches without the
			// owner hand-tagging it.
			needs: []string{workbench.NeedMacOSTooling}, os: "darwin",
			wantLabels: map[string]string{"os": "darwin"}, wantScope: "full",
		},
		{
			name:  "a display implies full access",
			needs: []string{workbench.NeedDisplay},
			// No os in the hint, so no os label -- narrowing on an OS nobody
			// asked for would rule out machines that could do the work.
			wantLabels: map[string]string{"display": "true"}, wantScope: "full",
		},
		{
			name: "user files alone is a read",
			// The workbench could not see the file; the machine is being asked
			// to look at it. No label: the files are on the user's machines,
			// which is true of all of them.
			needs: []string{workbench.NeedUserFiles},
			// nil, not an empty map.
			wantLabels: nil, wantScope: "observe",
		},
		{
			name:  "gpu plus user files takes the wider tier",
			needs: []string{workbench.NeedGPU, workbench.NeedUserFiles},
			// The set is not "the narrowest need wins"; one need for full
			// access makes the whole call full.
			wantLabels: map[string]string{"gpu": "true"}, wantScope: "full",
		},
		{
			name: "an os mismatch alone",
			// UnmetNeedOS is output-only, and the router must still be told
			// which OS to look for.
			needs: []string{workbench.UnmetNeedOS}, os: "windows",
			wantLabels: map[string]string{"os": "windows"}, wantScope: "full",
		},
		{
			name:  "an empty set takes the conservative tier",
			needs: nil,
			// Unreachable in practice (a mismatch always names a need), but an
			// unknown set asking for the NARROWER tier is how a card gets
			// approved for less than what runs.
			wantLabels: nil, wantScope: "full",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLabels := agentworker.EnvironmentNeedsLabels(tc.needs, tc.os)
			if len(gotLabels) != len(tc.wantLabels) {
				t.Fatalf("labels = %v, want %v", gotLabels, tc.wantLabels)
			}
			for k, v := range tc.wantLabels {
				if gotLabels[k] != v {
					t.Errorf("label %s = %q, want %q", k, gotLabels[k], v)
				}
			}
			if got := agentworker.EnvironmentNeedsScope(tc.needs); got != tc.wantScope {
				t.Errorf("scope = %q, want %q", got, tc.wantScope)
			}
		})
	}
}

func TestTheRerouteCarriesTheSameCallToTheFleet(t *testing.T) {
	plan, ok := planWorkbenchReroute(
		mismatchResult(t, []string{workbench.NeedMacOSTooling}, "darwin"), hostArgs(), testTurn())
	if !ok {
		t.Fatal("want a reroute plan")
	}
	if plan.FleetArgs["action"] != "exec" {
		t.Fatalf("action = %v, want the workbench's own action -- rewriting the call on the way "+
			"across would run something other than what the workbench refused", plan.FleetArgs["action"])
	}
	inner, _ := plan.FleetArgs["args"].(map[string]any)
	if inner["cmd"] != "xcodebuild" {
		t.Fatalf("args = %v, want the original arguments verbatim", inner)
	}
	if plan.FleetArgs["reroutedFrom"] != agentworker.ReroutedFromWorkbench {
		t.Fatalf("reroutedFrom = %v, want %q -- the invocation's routing record is what makes "+
			"\"why did this run on the laptop\" answerable", plan.FleetArgs["reroutedFrom"], agentworker.ReroutedFromWorkbench)
	}
	if plan.FleetArgs["planId"] != "plan-1" {
		t.Fatalf("planId = %v -- the per-task approval gate keys on it, so dropping it turns "+
			"every reroute into a denial", plan.FleetArgs["planId"])
	}
	if _, named := plan.FleetArgs["workerId"]; named {
		t.Fatal("the reroute must not name a machine: there is no workerId argument by design (D4)")
	}
}

func TestTheConsentCardSaysWhatTheWorkbenchCouldNotDo(t *testing.T) {
	plan, ok := planWorkbenchReroute(
		mismatchResult(t, []string{workbench.NeedMacOSTooling, workbench.NeedDisplay}, "darwin"), hostArgs(), testTurn())
	if !ok {
		t.Fatal("want a reroute plan")
	}
	summary, _ := plan.CardArgs["summary"].(string)
	for _, want := range []string{"macOS-only tooling", "graphical display"} {
		if !strings.Contains(summary, want) {
			t.Errorf("card summary %q does not mention %q -- \"environment_mismatch\" is not a "+
				"phrase anyone should have to read on a canvas card", summary, want)
		}
	}
	if strings.Contains(summary, "environment_mismatch") {
		t.Errorf("card summary leaks the wire code: %q", summary)
	}
	if plan.CardArgs["requestedScope"] != "full" {
		t.Fatalf("requestedScope = %v, want full", plan.CardArgs["requestedScope"])
	}
	if got, _ := plan.CardArgs["requireLabels"].(map[string]string); got["os"] != "darwin" {
		t.Fatalf("requireLabels = %v, want the card to name the requirement so the user's Allow "+
			"visibly covers a set of machines rather than appearing to name one", got)
	}
}

func TestOnlyAMissingApprovalOrScopeRaisesTheCard(t *testing.T) {
	// A kill switch is the user's deliberate "no". Answering it with a card
	// asking them to turn computer use back on is nagging, not consent.
	cases := map[string]bool{
		"denied_no_per_task_approval": true,
		"denied_by_scope":             true,
		"kill_switch_engaged":         false,
		"no_worker_available":         false,
		"denied_by_classifier":        false,
		"":                            false,
	}
	for code, wantCard := range cases {
		body := `{"ok":false,"errorCode":"` + code + `"}`
		if code == "" {
			body = `{"ok":true}`
		}
		got := rerouteErrorCode(body)
		raises := got == "denied_no_per_task_approval" || got == "denied_by_scope"
		if raises != wantCard {
			t.Errorf("errorCode %q raises the card = %v, want %v", code, raises, wantCard)
		}
	}
}

func TestAnUnparseableFleetResultIsNotReadAsARefusal(t *testing.T) {
	// Reading junk as a refusal would raise a consent card for a call that may
	// already have run on the user's machine.
	if got := rerouteErrorCode(`not json at all`); got != "" {
		t.Fatalf("errorCode = %q, want empty", got)
	}
}

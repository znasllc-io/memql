package harness

import "testing"

func TestSelectRunnable(t *testing.T) {
	steps := []StepView{
		{ID: "a", Status: StepStatusReady},
		{ID: "b", Status: StepStatusPending},
		{ID: "c", Status: StepStatusRunning},
		{ID: "d", Status: StepStatusReady},
		{ID: "e", Status: StepStatusDone},
	}
	got := SelectRunnable(steps)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "d" {
		t.Fatalf("SelectRunnable: want [a d], got %+v", got)
	}
}

func TestPromotablePending_DepsSatisfied(t *testing.T) {
	steps := []StepView{
		{ID: "root", Status: StepStatusDone},
		{ID: "child", Status: StepStatusPending, DependsOn: []string{"root"}},
		{ID: "grandchild", Status: StepStatusPending, DependsOn: []string{"child"}},
		{ID: "blocked", Status: StepStatusBlocked, DependsOn: []string{"root"}},
	}
	got := PromotablePending(steps)
	// child (dep root done) and blocked (dep root done) promote;
	// grandchild does not (child not done yet).
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	if !ids["child"] || !ids["blocked"] {
		t.Fatalf("expected child+blocked promotable, got %+v", got)
	}
	if ids["grandchild"] {
		t.Fatalf("grandchild should not be promotable yet: %+v", got)
	}
}

func TestPromotablePending_DanglingDep(t *testing.T) {
	steps := []StepView{
		{ID: "child", Status: StepStatusPending, DependsOn: []string{"missing"}},
	}
	if got := PromotablePending(steps); len(got) != 0 {
		t.Fatalf("dangling dep must not promote, got %+v", got)
	}
}

func TestComputePlanTerminal(t *testing.T) {
	cases := []struct {
		name  string
		steps []StepView
		want  string
	}{
		{"empty not terminal", nil, ""},
		{"all done -> done", []StepView{{Status: StepStatusDone}, {Status: StepStatusDone}}, PlanStatusDone},
		{"any running -> in flight", []StepView{{Status: StepStatusDone}, {Status: StepStatusRunning}}, ""},
		{"any ready -> in flight", []StepView{{Status: StepStatusReady}}, ""},
		{"failed + done -> failed", []StepView{{Status: StepStatusDone}, {Status: StepStatusFailed}}, PlanStatusFailed},
		{"all blocked -> failed", []StepView{{Status: StepStatusBlocked}}, PlanStatusFailed},
		{"pending present -> in flight", []StepView{{Status: StepStatusFailed}, {Status: StepStatusPending}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputePlanTerminal(tc.steps).Status; got != tc.want {
				t.Fatalf("ComputePlanTerminal(%s)=%q want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestBudgetTripped(t *testing.T) {
	b := PlanBudget{MaxSteps: 3, MaxToolCalls: 10, MaxWallClockMillis: 1000}
	cases := []struct {
		name string
		u    BudgetUsage
		want string
	}{
		{"within", BudgetUsage{StepsDispatched: 2, ToolCalls: 5, ElapsedMillis: 500}, ""},
		{"steps", BudgetUsage{StepsDispatched: 3}, "max_steps"},
		{"tools", BudgetUsage{StepsDispatched: 1, ToolCalls: 10}, "max_tool_calls"},
		{"wall", BudgetUsage{StepsDispatched: 1, ToolCalls: 1, ElapsedMillis: 1000}, "max_wall_clock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := BudgetTripped(b, tc.u)
			if tc.want == "" {
				if v.Tripped {
					t.Fatalf("expected within budget, got tripped %q", v.Reason)
				}
				return
			}
			if !v.Tripped || v.Reason != tc.want {
				t.Fatalf("BudgetTripped=%+v want reason %q", v, tc.want)
			}
		})
	}
}

func TestBudgetTripped_DisabledLimits(t *testing.T) {
	// Zero limits disable that dimension.
	b := PlanBudget{MaxSteps: 0, MaxToolCalls: 0, MaxWallClockMillis: 0}
	if v := BudgetTripped(b, BudgetUsage{StepsDispatched: 1000, ToolCalls: 1000, ElapsedMillis: 1 << 30}); v.Tripped {
		t.Fatalf("disabled budget must never trip, got %+v", v)
	}
}

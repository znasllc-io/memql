package harness

import "testing"

func TestDecideRoute_Bands(t *testing.T) {
	th := DefaultPlannerThresholds()
	cases := []struct {
		name   string
		fits   []AgentFit
		want   RouteAction
		wantID string
	}{
		{
			name: "high fit routes to existing",
			fits: []AgentFit{{AgentID: "a", Score: 0.90}, {AgentID: "b", Score: 0.40}},
			want: ActionRoute, wantID: "a",
		},
		{
			name: "partial fit upgrades",
			fits: []AgentFit{{AgentID: "a", Score: 0.70}},
			want: ActionUpgrade, wantID: "a",
		},
		{
			name: "low fit provisions",
			fits: []AgentFit{{AgentID: "a", Score: 0.30}},
			want: ActionProvision,
		},
		{
			name: "empty roster provisions",
			fits: nil,
			want: ActionProvision,
		},
		{
			name: "exactly at route threshold routes",
			fits: []AgentFit{{AgentID: "a", Score: DefaultRouteThreshold}},
			want: ActionRoute, wantID: "a",
		},
		{
			name: "exactly at upgrade threshold upgrades",
			fits: []AgentFit{{AgentID: "a", Score: DefaultUpgradeThreshold}},
			want: ActionUpgrade, wantID: "a",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := DecideRoute(c.fits, th)
			if d.Action != c.want {
				t.Fatalf("action = %q, want %q (%s)", d.Action, c.want, d.Rationale)
			}
			if c.wantID != "" && d.AgentID != c.wantID {
				t.Fatalf("agentID = %q, want %q", d.AgentID, c.wantID)
			}
			if d.Rationale == "" {
				t.Fatalf("expected a non-empty rationale for the decision")
			}
		})
	}
}

func TestDecideDedup(t *testing.T) {
	th := DefaultPlannerThresholds()
	// Near-duplicate at/above dedup threshold -> merge.
	dd := DecideDedup([]AgentFit{{AgentID: "x", Score: 0.95}}, th)
	if !dd.Merge || dd.MergeInto != "x" {
		t.Fatalf("expected merge into x, got %+v", dd)
	}
	// Distinct -> keep.
	dd = DecideDedup([]AgentFit{{AgentID: "x", Score: 0.50}}, th)
	if dd.Merge {
		t.Fatalf("expected keep (no merge), got %+v", dd)
	}
	// Empty roster -> keep.
	dd = DecideDedup(nil, th)
	if dd.Merge {
		t.Fatalf("expected keep on empty roster, got %+v", dd)
	}
}

func TestThresholds_WithDefaults_ClampsAndFills(t *testing.T) {
	// Zero values fill from defaults.
	got := PlannerThresholds{}.withDefaults()
	if got.RouteThreshold != DefaultRouteThreshold ||
		got.UpgradeThreshold != DefaultUpgradeThreshold ||
		got.DedupThreshold != DefaultDedupThreshold {
		t.Fatalf("zero thresholds did not fill defaults: %+v", got)
	}
	// Upgrade above route gets clamped to route.
	got = PlannerThresholds{RouteThreshold: 0.5, UpgradeThreshold: 0.9, DedupThreshold: 0.9}.withDefaults()
	if got.UpgradeThreshold > got.RouteThreshold {
		t.Fatalf("upgrade not clamped below route: %+v", got)
	}
}

func TestPickSeedRole(t *testing.T) {
	cat := SeedRoleCatalog()
	// Exact role hint wins.
	if r := PickSeedRole("builder", "find some papers", cat); r.Slug != "builder" {
		t.Fatalf("role hint builder ignored, got %q", r.Slug)
	}
	// Keyword fallback: "research the market" -> researcher.
	if r := PickSeedRole("", "research the market and summarize findings", cat); r.Slug != "researcher" {
		t.Fatalf("keyword fallback expected researcher, got %q", r.Slug)
	}
	// Keyword fallback: "build a parser" -> builder.
	if r := PickSeedRole("", "build a parser and write tests", cat); r.Slug != "builder" {
		t.Fatalf("keyword fallback expected builder, got %q", r.Slug)
	}
	// Unknown hint falls back to keywords (not an error).
	if r := PickSeedRole("nonexistent", "deploy and configure the server", cat); r.Slug != "operator" {
		t.Fatalf("unknown hint should keyword-fall-back to operator, got %q", r.Slug)
	}
}

func TestComposeAgent_CoherentRole(t *testing.T) {
	role := SeedRoleCatalog()[0] // researcher
	got := ComposeAgent(role, "Survey vector DBs", "compare pgvector and pinecone")
	if got.RoleSlug != "researcher" {
		t.Fatalf("roleSlug = %q", got.RoleSlug)
	}
	if len(got.Tools) == 0 {
		t.Fatalf("composed agent has no scoped tools")
	}
	if len(got.KnowledgeDomains) == 0 {
		t.Fatalf("composed agent has no knowledge domains")
	}
	if got.SystemPrompt == "" || got.CapabilityText == "" {
		t.Fatalf("composed agent missing prompt/capability text")
	}
	// The goal must be substituted into the prompt template (no {{goal}}).
	if got.SystemPrompt == role.PromptTemplate {
		t.Fatalf("prompt template goal hook not substituted")
	}
	if contains(got.SystemPrompt, "{{goal}}") {
		t.Fatalf("prompt still has unfilled {{goal}} hook: %s", got.SystemPrompt)
	}
}

func TestComputeUpgradeGap_AndMerge(t *testing.T) {
	role := SeedRole{
		KnowledgeDomains: []string{"k1", "k2"},
		Tools:            []string{"t1", "t2"},
	}
	gap := ComputeUpgradeGap(role, []string{"k1"}, []string{"t1"})
	if len(gap.AddDomains) != 1 || gap.AddDomains[0] != "k2" {
		t.Fatalf("AddDomains = %v, want [k2]", gap.AddDomains)
	}
	if len(gap.AddTools) != 1 || gap.AddTools[0] != "t2" {
		t.Fatalf("AddTools = %v, want [t2]", gap.AddTools)
	}
	if gap.Empty() {
		t.Fatalf("gap should not be empty")
	}
	domains, tools := MergeCapabilities([]string{"k1"}, []string{"t1"}, gap)
	if len(domains) != 2 || len(tools) != 2 {
		t.Fatalf("merge produced domains=%v tools=%v", domains, tools)
	}

	// No gap when the agent already covers the role.
	full := ComputeUpgradeGap(role, []string{"k1", "k2"}, []string{"t1", "t2"})
	if !full.Empty() {
		t.Fatalf("expected empty gap when agent covers role, got %+v", full)
	}
}

func TestValidateDecompose_HappyDAG(t *testing.T) {
	steps := []PlanStep{
		{Key: "s1", Title: "gather"},
		{Key: "s2", Title: "build", DependsOn: []string{"s1"}},
		{Key: "s3", Title: "deploy", DependsOn: []string{"s2", "s1"}},
	}
	res, err := ValidateDecompose(steps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(res.Steps))
	}
}

func TestValidateDecompose_Rejects(t *testing.T) {
	cases := []struct {
		name  string
		steps []PlanStep
	}{
		{"empty", nil},
		{"empty key", []PlanStep{{Key: "", Title: "x"}}},
		{"dup key", []PlanStep{{Key: "s1", Title: "a"}, {Key: "s1", Title: "b"}}},
		{"empty title", []PlanStep{{Key: "s1", Title: ""}}},
		{"self dep", []PlanStep{{Key: "s1", Title: "a", DependsOn: []string{"s1"}}}},
		{"dangling dep", []PlanStep{{Key: "s1", Title: "a", DependsOn: []string{"ghost"}}}},
		{"cycle", []PlanStep{
			{Key: "s1", Title: "a", DependsOn: []string{"s2"}},
			{Key: "s2", Title: "b", DependsOn: []string{"s1"}},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ValidateDecompose(c.steps); err == nil {
				t.Fatalf("expected validation error for %s", c.name)
			}
		})
	}
}

func TestSingleStepFallback(t *testing.T) {
	res := SingleStepFallback("do the thing")
	if len(res.Steps) != 1 || res.Steps[0].Goal != "do the thing" {
		t.Fatalf("fallback = %+v", res)
	}
	// A validated fallback is always a legal DAG.
	if _, err := ValidateDecompose(res.Steps); err != nil {
		t.Fatalf("fallback should validate: %v", err)
	}
}

func TestBestFit(t *testing.T) {
	if _, ok := BestFit(nil); ok {
		t.Fatalf("empty fits should report not-found")
	}
	best, ok := BestFit([]AgentFit{{AgentID: "a", Score: 0.1}, {AgentID: "b", Score: 0.9}, {AgentID: "c", Score: 0.5}})
	if !ok || best.AgentID != "b" {
		t.Fatalf("best = %+v ok=%v, want b", best, ok)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

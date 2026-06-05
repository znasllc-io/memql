package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// fakeSandbox is the Gate-1 seam stand-in: it returns a scripted sequence of
// SandboxReports (one per CompileBundle call) and records the bundles it was
// asked to compile. When the script runs out, the LAST report repeats.
type fakeSandbox struct {
	reports []memql.SandboxReport
	calls   [][]memql.SandboxConstruct
	n       int
}

func (f *fakeSandbox) CompileBundle(constructs []memql.SandboxConstruct) memql.SandboxReport {
	f.calls = append(f.calls, constructs)
	idx := f.n
	if idx >= len(f.reports) {
		idx = len(f.reports) - 1
	}
	f.n++
	if idx < 0 {
		return memql.SandboxReport{OK: true}
	}
	return f.reports[idx]
}

// okReport builds a clean Gate-1 report for the given constructs.
func okReport(constructs ...memql.SandboxConstruct) memql.SandboxReport {
	r := memql.SandboxReport{OK: true}
	for _, c := range constructs {
		r.Diagnostics = append(r.Diagnostics, memql.SandboxDiagnostic{Name: c.Name, Kind: c.Kind, OK: true})
	}
	return r
}

// failReport builds a Gate-1 report where the named (kind/name) construct
// failed with the given error and the rest are OK.
func failReport(failKind, failName, errMsg string, constructs ...memql.SandboxConstruct) memql.SandboxReport {
	r := memql.SandboxReport{OK: true}
	for _, c := range constructs {
		d := memql.SandboxDiagnostic{Name: c.Name, Kind: c.Kind, OK: true}
		if c.Kind == failKind && c.Name == failName {
			d.OK = false
			d.Error = errMsg
			r.OK = false
		}
		r.Diagnostics = append(r.Diagnostics, d)
	}
	return r
}

// emitJSON builds an authoringEmit / authoringRepair structured-output
// envelope: a {constructs:[...]} list.
func emitJSON(t *testing.T, constructs []memql.SandboxConstruct) string {
	t.Helper()
	res := emitResult{}
	for _, c := range constructs {
		res.Constructs = append(res.Constructs, emittedConstruct{Kind: c.Kind, Name: c.Name, Source: c.Source})
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal emit json: %v", err)
	}
	return string(b)
}

// designPlanWith builds a one-author-dep designPlan the emit pass renders.
func designPlanWith(deps ...resolvedDependency) designPlan {
	return designPlan{
		AutomationName:    "dailyDigest",
		AutomationPurpose: "Send a daily digest.",
		Dependencies:      deps,
	}
}

var automationCon = memql.SandboxConstruct{
	Kind: "automation", Name: "dailyDigest", Source: "automation dailyDigest { }",
}
var specCon = memql.SandboxConstruct{
	Kind: "spec", Name: "specDigestItemActive", Source: "spec specDigestItemActive {\n  payload.active == true\n}",
}

// emitFakeEngine returns a fakeEngine whose authoringEmit / authoringRepair
// InvokeSI calls yield the scripted JSON (by template id), and whose
// queryPlanById Execute yields a plan row carrying the given metrics (for the
// budget-gate path).
func emitFakeEngine(emitOut string, repairOuts []string, planRow map[string]any) *fakeEngine {
	repairIdx := 0
	return &fakeEngine{
		siResponder: func(templateId string, _ map[string]any) (any, error) {
			switch templateId {
			case "authoringEmit":
				return emitOut, nil
			case "authoringRepair":
				if repairIdx < len(repairOuts) {
					out := repairOuts[repairIdx]
					repairIdx++
					return out, nil
				}
				return `{"constructs":[]}`, nil
			}
			return nil, nil
		},
		execResponder: func(query string) (any, error) {
			if strings.Contains(query, "queryPlanById") && planRow != nil {
				return map[string]any{"output": []any{planRow}}, nil
			}
			return nil, nil
		},
	}
}

func planRowWithCalls(planId string, llmCallCount int) map[string]any {
	return map[string]any{
		"id":      planId,
		"status":  "planning",
		"metrics": map[string]any{"llmCallCount": float64(llmCallCount)},
	}
}

// TestEmitAndRepair_CleanFirstPass: a bundle that compiles clean on the first
// Gate-1 pass needs ZERO repair calls. Core happy path.
func TestEmitAndRepair_CleanFirstPass(t *testing.T) {
	fe := emitFakeEngine(emitJSON(t, []memql.SandboxConstruct{automationCon, specCon}), nil, planRowWithCalls("p1", 0))
	l := newDesignLoop(fe)
	sb := &fakeSandbox{reports: []memql.SandboxReport{okReport(automationCon, specCon)}}

	plan := designPlanWith(resolvedDependency{
		designDependency: designDependency{Kind: "spec", Name: "specDigestItemActive", CandidateSource: specCon.Source},
		Disposition:      dispAuthor,
	})
	bundle, report, clean, err := l.emitAndRepairBundle(context.Background(), "p1", "digest", plan, sb)
	if err != nil {
		t.Fatalf("emitAndRepairBundle: %v", err)
	}
	if !clean || !report.OK {
		t.Fatalf("clean bundle expected; clean=%v reportOK=%v", clean, report.OK)
	}
	if len(bundle.Constructs) != 2 {
		t.Fatalf("want 2 constructs, got %d", len(bundle.Constructs))
	}
	if n := len(sb.calls); n != 1 {
		t.Fatalf("clean first pass must call Gate 1 exactly once, got %d", n)
	}
	_, si, _ := fe.snapshot()
	if countContains(si, "authoringRepair") != 0 {
		t.Fatalf("clean first pass must make ZERO repair calls, got %d", countContains(si, "authoringRepair"))
	}
}

// TestEmitAndRepair_RepairsThenClean: a bundle that fails Gate 1 once, then
// compiles after one repair re-emit. Acceptance: Responsibility -> Gate-1-clean
// bundle WITHIN the repair budget.
func TestEmitAndRepair_RepairsThenClean(t *testing.T) {
	brokenSpec := memql.SandboxConstruct{Kind: "spec", Name: "specDigestItemActive", Source: "@bogus(\"x\")\nspec specDigestItemActive {\n  payload.active == true\n}"}
	fixedSpec := specCon

	fe := emitFakeEngine(
		emitJSON(t, []memql.SandboxConstruct{automationCon, brokenSpec}),
		[]string{emitJSON(t, []memql.SandboxConstruct{fixedSpec})},
		planRowWithCalls("p1", 0),
	)
	l := newDesignLoop(fe)
	sb := &fakeSandbox{reports: []memql.SandboxReport{
		failReport("spec", "specDigestItemActive", "unknown spec annotation @bogus", automationCon, brokenSpec),
		okReport(automationCon, fixedSpec),
	}}

	plan := designPlanWith(resolvedDependency{
		designDependency: designDependency{Kind: "spec", Name: "specDigestItemActive", CandidateSource: brokenSpec.Source},
		Disposition:      dispAuthor,
	})
	bundle, report, clean, err := l.emitAndRepairBundle(context.Background(), "p1", "digest", plan, sb)
	if err != nil {
		t.Fatalf("emitAndRepairBundle: %v", err)
	}
	if !clean || !report.OK {
		t.Fatalf("bundle should be clean after one repair; clean=%v reportOK=%v", clean, report.OK)
	}
	// The repaired spec replaced the broken one in place (same kind/name).
	for _, c := range bundle.Constructs {
		if c.Kind == "spec" && c.Name == "specDigestItemActive" && strings.Contains(c.Source, "@bogus") {
			t.Fatalf("repaired bundle still carries the broken @bogus spec source")
		}
	}
	_, si, _ := fe.snapshot()
	if countContains(si, "authoringRepair") != 1 {
		t.Fatalf("want exactly one repair call, got %d", countContains(si, "authoringRepair"))
	}
	if n := len(sb.calls); n != 2 {
		t.Fatalf("want 2 Gate-1 passes (emit + 1 repair), got %d", n)
	}
}

// TestEmitAndRepair_ExhaustsAttemptCap: a bundle that never compiles must stop
// at the repair-attempt cap and return clean=false with the last report --
// never spin forever.
func TestEmitAndRepair_ExhaustsAttemptCap(t *testing.T) {
	t.Setenv("MEMQL_AUTHORING_MAX_REPAIRS", "2")
	brokenSpec := memql.SandboxConstruct{Kind: "spec", Name: "specDigestItemActive", Source: "spec specDigestItemActive {\n  broken\n}"}

	fe := emitFakeEngine(
		emitJSON(t, []memql.SandboxConstruct{automationCon, brokenSpec}),
		[]string{
			emitJSON(t, []memql.SandboxConstruct{brokenSpec}),
			emitJSON(t, []memql.SandboxConstruct{brokenSpec}),
			emitJSON(t, []memql.SandboxConstruct{brokenSpec}),
		},
		planRowWithCalls("p1", 0),
	)
	l := newDesignLoop(fe)
	// Always-fail report.
	alwaysFail := failReport("spec", "specDigestItemActive", "syntax error", automationCon, brokenSpec)
	sb := &fakeSandbox{reports: []memql.SandboxReport{alwaysFail}}

	plan := designPlanWith(resolvedDependency{
		designDependency: designDependency{Kind: "spec", Name: "specDigestItemActive", CandidateSource: brokenSpec.Source},
		Disposition:      dispAuthor,
	})
	_, report, clean, err := l.emitAndRepairBundle(context.Background(), "p1", "digest", plan, sb)
	if err != nil {
		t.Fatalf("exhausting the cap must not error: %v", err)
	}
	if clean || report.OK {
		t.Fatalf("never-compiling bundle must return clean=false; clean=%v reportOK=%v", clean, report.OK)
	}
	_, si, _ := fe.snapshot()
	// 1 emit + exactly cap (2) repair calls.
	if n := countContains(si, "authoringRepair"); n != 2 {
		t.Fatalf("must stop at the repair cap (2 repairs), got %d", n)
	}
}

// TestEmitAndRepair_BudgetExhaustedStopsLoop: when the planner's cumulative
// per-plan LLM ceiling is already reached, the repair loop must NOT make
// another emit call -- it parks with the current diagnostics.
func TestEmitAndRepair_BudgetExhaustedStopsLoop(t *testing.T) {
	brokenSpec := memql.SandboxConstruct{Kind: "spec", Name: "specDigestItemActive", Source: "spec x {\n  broken\n}"}
	// Plan already at the hard invocation ceiling -> budget gate blocks.
	fe := emitFakeEngine(
		emitJSON(t, []memql.SandboxConstruct{automationCon, brokenSpec}),
		[]string{emitJSON(t, []memql.SandboxConstruct{brokenSpec})},
		planRowWithCalls("p1", maxPlannerInvocationsPerPlan()),
	)
	l := newDesignLoop(fe)
	sb := &fakeSandbox{reports: []memql.SandboxReport{
		failReport("spec", "specDigestItemActive", "syntax error", automationCon, brokenSpec),
	}}

	plan := designPlanWith(resolvedDependency{
		designDependency: designDependency{Kind: "spec", Name: "specDigestItemActive", CandidateSource: brokenSpec.Source},
		Disposition:      dispAuthor,
	})
	_, _, clean, err := l.emitAndRepairBundle(context.Background(), "p1", "digest", plan, sb)
	if err != nil {
		t.Fatalf("budget-exhausted stop must not error: %v", err)
	}
	if clean {
		t.Fatalf("budget-exhausted bundle must not be reported clean")
	}
	_, si, _ := fe.snapshot()
	if n := countContains(si, "authoringRepair"); n != 0 {
		t.Fatalf("budget-exhausted loop must make ZERO repair calls, got %d", n)
	}
}

// TestEmitBundle_ReuseEdgesNotAuthored: REUSE dependencies are recorded as
// reuse edges and NOT sent to the emit prompt as authored constructs.
func TestEmitBundle_ReuseEdgesNotAuthored(t *testing.T) {
	fe := emitFakeEngine(emitJSON(t, []memql.SandboxConstruct{automationCon}), nil, nil)
	l := newDesignLoop(fe)

	plan := designPlanWith(
		resolvedDependency{
			designDependency: designDependency{Kind: "spec", Name: "specReused"},
			Disposition:      dispReuseExact, ReuseName: "specReused", ReuseKind: "spec", ReuseNamespace: "v1:digest",
		},
	)
	bundle, err := l.emitBundle(context.Background(), "digest", plan)
	if err != nil {
		t.Fatalf("emitBundle: %v", err)
	}
	if len(bundle.ReuseEdges) != 1 || bundle.ReuseEdges[0].Name != "specReused" {
		t.Fatalf("reuse edge not recorded: %+v", bundle.ReuseEdges)
	}
}

// TestParseEmittedConstructs_DropsIncomplete: entries missing kind/name/source
// are dropped; an all-empty result errors.
func TestParseEmittedConstructs_DropsIncomplete(t *testing.T) {
	good := `{"constructs":[{"kind":"spec","name":"s1","source":"spec s1 { payload.x==1 }"},{"kind":"spec","name":"","source":"x"}]}`
	out, err := parseEmittedConstructs(good)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out) != 1 || out[0].Name != "s1" {
		t.Fatalf("incomplete entry must be dropped, got %+v", out)
	}
	if _, err := parseEmittedConstructs(`{"constructs":[]}`); err == nil {
		t.Fatalf("empty constructs must error")
	}
}

// realSandbox bridges the REAL registry-only Gate-1 compiler
// (memql.SandboxCompileBundle -- no engine, no DB) to the authoringSandbox
// seam, so an end-to-end acceptance test exercises the genuine compile pass.
type realSandbox struct{}

func (realSandbox) CompileBundle(constructs []memql.SandboxConstruct) memql.SandboxReport {
	return memql.SandboxCompileBundle(constructs)
}

// TestEmitAndRepair_RealGate1_RepairsToClean is the acceptance demonstration:
// a representative Responsibility flows design -> emit -> REAL Gate 1, where the
// first emit is a genuinely broken spec (carries an unknown annotation @bogus,
// which the real sandbox rejects) and the single repair re-emits a clean spec
// the real sandbox accepts. Proves the loop drives a Responsibility to a
// Gate-1-clean bundle within the repair budget against the actual compiler --
// no live DB needed (the registry-only pass binds against the embedded core
// concept registry).
func TestEmitAndRepair_RealGate1_RepairsToClean(t *testing.T) {
	auto := memql.SandboxConstruct{Kind: "logic", Name: "logicDigest",
		Source: "logic logicDigest {\n  args { userId string @required }\n  body { return args.userId }\n}"}
	broken := memql.SandboxConstruct{Kind: "spec", Name: "specDigestActive",
		Source: "@bogus(\"x\")\nspec specDigestActive {\n  payload.active == true\n}"}
	fixed := memql.SandboxConstruct{Kind: "spec", Name: "specDigestActive",
		Source: "spec specDigestActive {\n  payload.active == true\n}"}

	fe := emitFakeEngine(
		emitJSON(t, []memql.SandboxConstruct{auto, broken}),
		[]string{emitJSON(t, []memql.SandboxConstruct{fixed})},
		planRowWithCalls("p1", 0),
	)
	l := newDesignLoop(fe)

	plan := designPlanWith(resolvedDependency{
		designDependency: designDependency{Kind: "spec", Name: "specDigestActive", CandidateSource: broken.Source},
		Disposition:      dispAuthor,
	})
	bundle, report, clean, err := l.emitAndRepairBundle(context.Background(), "p1", "Send me a daily digest of active items.", plan, realSandbox{})
	if err != nil {
		t.Fatalf("emitAndRepairBundle: %v", err)
	}
	if !clean || !report.OK {
		var errs []string
		for _, d := range report.Diagnostics {
			if !d.OK && !d.Skipped {
				errs = append(errs, d.Error)
			}
		}
		t.Fatalf("real Gate-1 bundle should be clean after one repair; clean=%v errs=%v", clean, errs)
	}
	// Final bundle's spec must be the clean (repaired) source.
	for _, c := range bundle.Constructs {
		if c.Kind == "spec" && strings.Contains(c.Source, "@bogus") {
			t.Fatalf("final bundle still carries the broken @bogus spec the real sandbox rejects")
		}
	}
	_, si, _ := fe.snapshot()
	if countContains(si, "authoringRepair") != 1 {
		t.Fatalf("real-Gate-1 acceptance: want exactly one repair, got %d", countContains(si, "authoringRepair"))
	}
}

// TestMergeRepaired_ReplacesInPlaceAndAppends: a repaired construct replaces
// its same-keyed original; a brand-new one is appended.
func TestMergeRepaired_ReplacesInPlaceAndAppends(t *testing.T) {
	orig := []memql.SandboxConstruct{
		{Kind: "spec", Name: "a", Source: "old-a"},
		{Kind: "query", Name: "q", Source: "q-src"},
	}
	repaired := []memql.SandboxConstruct{
		{Kind: "spec", Name: "a", Source: "new-a"},
		{Kind: "shape", Name: "newdep", Source: "shape-src"},
	}
	out := mergeRepaired(orig, repaired)
	if len(out) != 3 {
		t.Fatalf("want 3 constructs after merge, got %d", len(out))
	}
	for _, c := range out {
		if c.Kind == "spec" && c.Name == "a" && c.Source != "new-a" {
			t.Fatalf("spec a should be replaced in place, got %q", c.Source)
		}
	}
}

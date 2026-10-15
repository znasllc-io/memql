package goaleval

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func catalog(t *testing.T) Catalog {
	t.Helper()
	c, err := Load(os.DirFS("../../.."), "test/proving/goals/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVersionedGoalCorpus(t *testing.T) {
	c := catalog(t)
	if len(c.Scenarios) < 14 {
		t.Fatal("developer duties or recovery cases disappeared")
	}
	for _, s := range c.Scenarios {
		if s.Fingerprint() == "" {
			t.Fatal("no fingerprint")
		}
	}
}

func TestFixtureTamperingAndUnknownFieldsAreRefused(t *testing.T) {
	c := catalog(t)
	if _, err := Decode[Catalog]([]byte(`{"schemaVersion":1,"typo":true}`)); err == nil {
		t.Fatal("unknown field ignored")
	}
	if _, err := Decode[Catalog]([]byte(`{} {}`)); err == nil {
		t.Fatal("trailing document ignored")
	}
	raw, _ := os.ReadFile("../../../test/proving/goals/catalog.json")
	f := fstest.MapFS{"catalog.json": {Data: raw}}
	snapshot, err := ReadFixture(os.DirFS("../../.."), "test/proving/goals", c.Scenarios[0].Fixture)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for name, content := range snapshot {
		files[name] = string(content)
	}
	files["totals.go"] = "tampered"
	changed, _ := json.Marshal(map[string]any{"schemaVersion": 1, "files": files})
	f["fixtures/go-totals.json"] = &fstest.MapFile{Data: changed}
	if _, err := Load(f, "catalog.json"); err == nil {
		t.Fatal("modified fixture accepted")
	}
	c.Scenarios[0].Fixture.Files["../escape"] = strings.Repeat("a", 64)
	if err := c.Validate(); err == nil {
		t.Fatal("escaping fixture path accepted")
	}
}

func validResult(s Scenario) Result {
	r := Result{SchemaVersion: 1, ID: "run-1", ScenarioID: s.ID, ScenarioVersion: s.Version, ScenarioFingerprint: s.Fingerprint(), FixtureVersion: s.Fixture.Version,
		EngineCommit: strings.Repeat("e", 40), HarnessCommit: strings.Repeat("f", 40), Setup: Setup{Name: "Local baseline", RoutingDigest: strings.Repeat("a", 64), Routes: []Route{{Role: "reasoning", Level: "standard", Rule: "fixture-rule", Doors: []Door{{Source: "fleet", Model: "fixture-model", Configuration: map[string]any{"temperature": 0}}}}}}, GoalID: "goal-1", RunID: "work-1", Outcome: "autonomous",
		StartedAt: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 10, 13, 0, 1, 0, 0, time.UTC),
		InputTokens: Measure{Basis: "unavailable", Source: "provider did not report usage"}, OutputTokens: Measure{Basis: "unavailable", Source: "provider did not report usage"}, CostUSD: Measure{Basis: "unavailable", Source: "provider did not report cost"}}
	r.ModelsUsed = []ModelUse{{Role: "reasoning", Level: "standard", Source: "fleet", Model: "fixture-model", Calls: 1, Evidence: []string{"modelCall:one"}, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CostUSD: r.CostUSD}}
	r.Setup.RoutingDigest = r.Setup.Fingerprint()
	for _, a := range s.Criteria {
		r.Assertions = append(r.Assertions, Assertion{CriterionID: a.ID, Verdict: "pass", Evidence: []string{"artifact:oracle-" + a.ID}})
	}
	return r
}

func TestSuccessRequiresIndependentEvidenceAndDisclosesIntervention(t *testing.T) {
	s := catalog(t).Scenarios[0]
	r := validResult(s)
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*Result){
		"missing oracle":              func(r *Result) { r.Assertions = nil },
		"failed oracle":               func(r *Result) { r.Assertions[0].Verdict = "fail" },
		"unverified oracle":           func(r *Result) { r.Assertions[0].Verdict = "unverified" },
		"no evidence":                 func(r *Result) { r.Assertions[0].Evidence = nil },
		"blank evidence":              func(r *Result) { r.Assertions[0].Evidence = []string{"   "} },
		"blank call receipt":          func(r *Result) { r.ModelsUsed[0].Evidence = []string{""} },
		"paid call in no-spend setup": func(r *Result) { r.ModelsUsed[0].Source = "federation" },
		"duplicate oracle":            func(r *Result) { r.Assertions = append(r.Assertions, r.Assertions[0]) },
		"hidden intervention": func(r *Result) {
			r.Interventions = []Intervention{{At: r.StartedAt, Kind: "guidance", Description: "clarified task", Evidence: "observation:feedback"}}
		},
		"missing live execution":  func(r *Result) { r.RunID = "" },
		"zero pretending absent":  func(r *Result) { z := 0.0; r.CostUSD.Value = &z },
		"wrong fixture":           func(r *Result) { r.FixtureVersion = "2.0.0" },
		"not-run pretending live": func(r *Result) { r.Outcome = "not-run" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			r := validResult(s)
			change(&r)
			if err := r.Validate(s); err == nil {
				t.Fatal("invalid success accepted")
			}
		})
	}
	r.Interventions = []Intervention{{At: r.StartedAt, Kind: "approval", Description: "approved exact evaluation PR", Evidence: "observation:approval"}}
	r.Outcome = "assisted"
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
	// A violation remains inspectable as a failure; validation must not erase
	// the call that explains why the no-spend run failed.
	r.ModelsUsed[0].Source = "federation"
	r.Outcome = "failed"
	r.Artifacts = []string{"observation:paid-call-violation"}
	if err := r.Validate(s); err != nil {
		t.Fatal(err)
	}
}

func TestChangingTheGoalOrOracleBreaksComparability(t *testing.T) {
	s := catalog(t).Scenarios[0]
	a := validResult(s)
	b := a
	b.Setup.Name = "Another routing setup"
	if !Comparable(a, b) {
		t.Fatal("a model matrix must compare explicit different models")
	}
	s.Criteria[0].Oracle += " with a different acceptance threshold"
	b.ScenarioFingerprint = s.Fingerprint()
	if Comparable(a, b) {
		t.Fatal("different acceptance contract compared as a trend")
	}
}

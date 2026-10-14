package goaleval

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

var commitSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)

// Setup describes the whole configured MemQL system. A goal can call several
// models; its identity is never reduced to whichever model answered last.
type Setup struct {
	Name                 string  `json:"name"`
	RoutingDigest        string  `json:"routingDigest"`
	Routes               []Route `json:"routes"`
	PaidProvidersAllowed bool    `json:"paidProvidersAllowed"`
}

type Route struct {
	Role  string `json:"role"`
	Level string `json:"level"`
	Rule  string `json:"rule"`
	Doors []Door `json:"doors"`
}

type Door struct {
	Source        string         `json:"source"`
	Model         string         `json:"model"`
	Configuration map[string]any `json:"configuration"`
}

// ModelUse comes from call receipts. Configured doors alone do not prove use.
type ModelUse struct {
	Role         string   `json:"role"`
	Level        string   `json:"level"`
	Source       string   `json:"source"`
	Model        string   `json:"model"`
	Calls        int      `json:"calls"`
	Fallback     bool     `json:"fallback"`
	Evidence     []string `json:"evidence"`
	InputTokens  Measure  `json:"inputTokens"`
	OutputTokens Measure  `json:"outputTokens"`
	CostUSD      Measure  `json:"costUsd"`
}

type Binding struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Source  string `json:"source"`
}

// Measure never promotes a visible-text estimate to actual provider usage.
type Measure struct {
	Value  *float64 `json:"value,omitempty"`
	Basis  string   `json:"basis"`
	Source string   `json:"source"`
}

type Assertion struct {
	CriterionID string   `json:"criterionId"`
	Verdict     string   `json:"verdict"`
	Evidence    []string `json:"evidence"`
}

type Intervention struct {
	At          time.Time `json:"at"`
	Kind        string    `json:"kind"`
	Description string    `json:"description"`
	Evidence    string    `json:"evidence"`
}

type Result struct {
	SchemaVersion       int                `json:"schemaVersion"`
	ID                  string             `json:"id"`
	ScenarioID          string             `json:"scenarioId"`
	ScenarioVersion     string             `json:"scenarioVersion"`
	ScenarioFingerprint string             `json:"scenarioFingerprint"`
	FixtureVersion      string             `json:"fixtureVersion"`
	EngineCommit        string             `json:"engineCommit"`
	HarnessCommit       string             `json:"harnessCommit"`
	Setup               Setup              `json:"setup"`
	ModelsUsed          []ModelUse         `json:"modelsUsed"`
	Bindings            []Binding          `json:"bindings"`
	GoalID              string             `json:"goalId"`
	RunID               string             `json:"runId"`
	Outcome             string             `json:"outcome"`
	Assertions          []Assertion        `json:"assertions"`
	Interventions       []Intervention     `json:"interventions"`
	Artifacts           []string           `json:"artifacts"`
	StartedAt           time.Time          `json:"startedAt"`
	FinishedAt          time.Time          `json:"finishedAt"`
	InputTokens         Measure            `json:"inputTokens"`
	OutputTokens        Measure            `json:"outputTokens"`
	CostUSD             Measure            `json:"costUsd"`
	Resources           map[string]Measure `json:"resources"`
}

func (m Measure) Validate() error {
	if strings.TrimSpace(m.Source) == "" {
		return fmt.Errorf("a measurement needs its source or absence reason")
	}
	switch m.Basis {
	case "actual", "estimated":
		if m.Value == nil || math.IsNaN(*m.Value) || math.IsInf(*m.Value, 0) || *m.Value < 0 {
			return fmt.Errorf("invalid %s measurement", m.Basis)
		}
	case "unavailable":
		if m.Value != nil {
			return fmt.Errorf("unavailable measurement cannot carry a number")
		}
	default:
		return fmt.Errorf("unknown measurement basis %q", m.Basis)
	}
	return nil
}

func (r Result) Validate(s Scenario) error {
	if r.SchemaVersion != SchemaVersion || r.ID == "" || r.ScenarioID != s.ID || r.ScenarioVersion != s.Version || r.ScenarioFingerprint != s.Fingerprint() || r.FixtureVersion != s.Fixture.Version {
		return fmt.Errorf("result does not bind the exact scenario and fixture")
	}
	if !commitSHA.MatchString(r.EngineCommit) || !commitSHA.MatchString(r.HarnessCommit) || r.GoalID == "" || r.RunID == "" {
		return fmt.Errorf("a live result needs engine, harness and MemQL goal/run provenance")
	}
	if err := r.Setup.Validate(); err != nil {
		return err
	}
	for _, use := range r.ModelsUsed {
		if use.Role == "" || use.Level == "" || use.Source == "" || use.Model == "" || use.Calls < 1 || len(use.Evidence) == 0 {
			return fmt.Errorf("actual model use requires role, source, model, calls and receipt evidence")
		}
		if !nonblankEvidence(use.Evidence) {
			return fmt.Errorf("model call evidence must identify actual receipts")
		}
		for _, m := range []Measure{use.InputTokens, use.OutputTokens, use.CostUSD} {
			if err := m.Validate(); err != nil {
				return err
			}
		}
	}
	if r.StartedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) || r.FinishedAt.IsZero() {
		return fmt.Errorf("invalid run times")
	}
	for _, m := range []Measure{r.InputTokens, r.OutputTokens, r.CostUSD} {
		if err := m.Validate(); err != nil {
			return err
		}
	}
	for _, m := range r.Resources {
		if err := m.Validate(); err != nil {
			return err
		}
	}
	for _, b := range r.Bindings {
		if b.ID == "" || b.Version == "" || !digest.MatchString(b.Digest) || b.Source == "" {
			return fmt.Errorf("incomplete skill/lesson binding")
		}
	}
	for _, i := range r.Interventions {
		if i.At.Before(r.StartedAt) || i.At.After(r.FinishedAt) || i.Kind == "" || i.Description == "" || i.Evidence == "" {
			return fmt.Errorf("incomplete intervention")
		}
	}
	criteria := map[string]Criterion{}
	for _, a := range s.Criteria {
		criteria[a.ID] = a
	}
	seen := map[string]bool{}
	allPass := true
	for _, a := range r.Assertions {
		c, ok := criteria[a.CriterionID]
		if !ok || seen[a.CriterionID] {
			return fmt.Errorf("unknown or duplicate criterion %q", a.CriterionID)
		}
		seen[a.CriterionID] = true
		if a.Verdict != "pass" && a.Verdict != "fail" && a.Verdict != "unverified" {
			return fmt.Errorf("unknown assertion verdict")
		}
		if a.Verdict != "unverified" && !nonblankEvidence(a.Evidence) {
			return fmt.Errorf("criterion %s has no evidence", a.CriterionID)
		}
		if c.Required && a.Verdict != "pass" {
			allPass = false
		}
	}
	for _, c := range s.Criteria {
		if c.Required && !seen[c.ID] {
			allPass = false
		}
	}
	switch r.Outcome {
	case "autonomous", "assisted":
		if len(r.ModelsUsed) == 0 {
			return fmt.Errorf("a successful live model evaluation needs actual call receipts")
		}
		for _, use := range r.ModelsUsed {
			if !r.Setup.PaidProvidersAllowed && use.Source == "federation" {
				return fmt.Errorf("a no-spend evaluation cannot succeed after a metered call")
			}
		}
		if !allPass {
			return fmt.Errorf("success requires every required oracle to pass with evidence")
		}
		if (r.Outcome == "autonomous") != (len(r.Interventions) == 0) {
			return fmt.Errorf("success must disclose human intervention")
		}
	case "failed", "blocked", "cancelled":
		if !nonblankEvidence(r.Artifacts) {
			return fmt.Errorf("incomplete run needs diagnostic evidence")
		}
	default:
		return fmt.Errorf("unknown outcome %q; not-run is catalog state, not a fabricated live result", r.Outcome)
	}
	return nil
}

func nonblankEvidence(items []string) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if strings.TrimSpace(item) == "" {
			return false
		}
	}
	return true
}

func (s Setup) Validate() error {
	if s.Name == "" || !digest.MatchString(s.RoutingDigest) || s.RoutingDigest != s.Fingerprint() || len(s.Routes) == 0 {
		return fmt.Errorf("evaluation setup needs a named, version-bound routing configuration")
	}
	for _, route := range s.Routes {
		if route.Role == "" || route.Level == "" || route.Rule == "" || len(route.Doors) == 0 {
			return fmt.Errorf("each route needs a role, level, rule and ordered doors")
		}
		for _, door := range route.Doors {
			if door.Source == "" || door.Model == "" || door.Configuration == nil {
				return fmt.Errorf("each door needs source, model and configuration")
			}
			if door.Source != "fleet" && door.Source != "app" && door.Source != "session" && door.Source != "federation" {
				return fmt.Errorf("unknown configured model source %q", door.Source)
			}
			if !s.PaidProvidersAllowed && door.Source == "federation" {
				return fmt.Errorf("a no-spend setup cannot configure a metered door")
			}
		}
	}
	return nil
}

func (s Setup) Fingerprint() string {
	return fingerprint(map[string]any{"routes": s.Routes, "paidProvidersAllowed": s.PaidProvidersAllowed})
}

// Comparable deliberately ignores setup identity: the matrix compares complete
// routing setups, displaying configured routes and actual models beside results.
// Callers must compare actual and estimated usage separately.
func Comparable(a, b Result) bool {
	return a.ScenarioFingerprint == b.ScenarioFingerprint && a.ScenarioFingerprint != "" && a.FixtureVersion == b.FixtureVersion
}

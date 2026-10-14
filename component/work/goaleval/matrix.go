package goaleval

import (
	"fmt"
	"strings"
	"time"
)

// Matrix is an explicit run allowance. Zero dollars means no paid door, not
// the engine's legacy zero-as-unset cost ceiling. Enforcement uses a strict
// fleet pin on the owned goal; the monetary ceiling is not the spend gate.
type Matrix struct {
	SchemaVersion         int            `json:"schemaVersion"`
	Repetitions           int            `json:"repetitions"`
	ScenarioIDs           []string       `json:"scenarioIds"`
	MaxModelCalls         int            `json:"maxModelCalls"`
	WallClockMinutes      int            `json:"wallClockMinutes"`
	PaidProviderBudgetUSD float64        `json:"paidProviderBudgetUsd"`
	GitHubRepositories    []string       `json:"githubRepositories"`
	Schedule              Schedule       `json:"schedule"`
	Status                string         `json:"status"`
	Setups                []SetupRequest `json:"setups"`
}

type Schedule struct {
	Enabled  bool   `json:"enabled"`
	Cadence  string `json:"cadence"`
	Timezone string `json:"timezone"`
}

type SetupRequest struct {
	Name                 string `json:"name"`
	ReasoningModel       string `json:"reasoningModel"`
	PaidProvidersAllowed bool   `json:"paidProvidersAllowed"`
	RoutingSnapshot      string `json:"routingSnapshot"`
	OtherRoles           string `json:"otherRoles"`
}

func (m Matrix) Validate(c Catalog) error {
	if m.SchemaVersion != SchemaVersion || m.Repetitions < 1 || m.Repetitions > 10 || len(m.ScenarioIDs) == 0 || len(m.Setups) == 0 {
		return fmt.Errorf("matrix requires a version, scenarios, setups and 1–10 repetitions")
	}
	if m.MaxModelCalls < 1 || m.MaxModelCalls > 200 || m.WallClockMinutes < 1 || m.WallClockMinutes > 120 {
		return fmt.Errorf("matrix needs bounded model calls (1–200) and wall clock minutes (1–120)")
	}
	if m.PaidProviderBudgetUSD != 0 {
		return fmt.Errorf("this runner admits no paid-provider spend")
	}
	if m.Schedule.Enabled || m.Schedule.Cadence != "monthly" {
		return fmt.Errorf("manual runner requires the monthly schedule to remain disabled")
	}
	if _, err := time.LoadLocation(m.Schedule.Timezone); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, s := range c.Scenarios {
		known[s.ID] = true
	}
	seen := map[string]bool{}
	for _, id := range m.ScenarioIDs {
		if !known[id] || seen[id] {
			return fmt.Errorf("unknown or repeated scenario %q", id)
		}
		seen[id] = true
	}
	seen = map[string]bool{}
	for _, s := range m.Setups {
		model := strings.TrimPrefix(s.ReasoningModel, "fleet:")
		if s.Name == "" || seen[s.Name] || s.PaidProvidersAllowed || !strings.HasPrefix(s.ReasoningModel, "fleet:") || model == "" || model == "*" || model == "strongest" || model == "fastest" || strings.ContainsAny(model, " \t\n\r") {
			return fmt.Errorf("setup %q needs an exact fleet model and paid providers disabled", s.Name)
		}
		seen[s.Name] = true
	}
	for _, repo := range m.GitHubRepositories {
		parts := strings.Split(repo, "/")
		if len(parts) != 2 || !strings.HasPrefix(parts[1], "memql-evaluation-") || !identifier.MatchString(parts[0]) || !identifier.MatchString(parts[1]) {
			return fmt.Errorf("repository %q is not a dedicated evaluation target", repo)
		}
	}
	return nil
}

// Availability is a preflight answer, not a measured result. Missing
// injection authority must never quietly reduce a scenario to an easier one.
func Availability(s Scenario) string {
	if s.Fixture.Kind == "github" {
		return "requires a provisioned, allowlisted GitHub fixture and exact review/merge authority"
	}
	for _, injection := range s.Injections {
		if injection.Kind != "test-failure" {
			return "requires an execution adapter for the declared " + injection.Kind + " injection at " + injection.Boundary
		}
	}
	return ""
}

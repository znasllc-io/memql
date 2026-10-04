package readiness

import (
	"encoding/json"
	"errors"
)

// integrationReport is the slice of integration.<name>.status's payload the
// evaluator reads. The report's own fields, nothing invented.
type integrationReport struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Settings []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"settings"`
	Credentials []struct {
		Present bool   `json:"present"`
		Source  string `json:"source"`
	} `json:"credentials"`
}

// IntegrationStatus reads the named self-report from integration status's
// registry envelope. It returns only state and configuration presence; raw
// settings and credential values never leave this boundary, including errors.
func IntegrationStatus(payload []byte, name string) (state string, touched bool, err error) {
	var envelope struct {
		Integrations []integrationReport `json:"integrations"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return "", false, errors.New("readiness: malformed integration status envelope")
	}
	for _, rep := range envelope.Integrations {
		if rep.Name != name {
			continue
		}
		switch rep.State {
		case "configured", "unhealthy", "needs_configuration":
		default:
			return "", false, errors.New("readiness: integration status has no recognized state")
		}
		for _, s := range rep.Settings {
			if s.Source != "" && s.Source != "unset" {
				touched = true
			}
		}
		for _, c := range rep.Credentials {
			if c.Present {
				touched = true
			}
		}
		return rep.State, touched, nil
	}
	return "", false, errors.New("readiness: integration status has no matching report")
}

// IntegrationLaneName is the lane an integration-evaluated module's row
// carries: its report's settings, one slot each.
const IntegrationLaneName = "report"

// IntegrationLane is the named report's SETTINGS as one lane of slots --
// presence and source, never a value -- or nil when the report names none or
// cannot be read. It is what lets a surface say WHICH of an integration's
// facts hold rather than only whether all of them do: the pipelines item's
// three sub-steps (GitHub App, a repository connected, a runner) are its
// report's three settings (pipelines program design record, D15).
//
// Credentials are left out on purpose: a credential's presence is already
// the setup surface's own read (integrationStatus), and a row is broadcast to
// every signed-in reader.
func IntegrationLane(payload []byte, name string) *LaneReport {
	var envelope struct {
		Integrations []integrationReport `json:"integrations"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return nil
	}
	for _, rep := range envelope.Integrations {
		if rep.Name != name {
			continue
		}
		lane := LaneReport{
			Name:             IntegrationLaneName,
			ConfigurableFrom: "os",
			Complete:         rep.State == "configured" || rep.State == "unhealthy",
		}
		for _, s := range rep.Settings {
			if s.Name == "" {
				continue
			}
			present := s.Source != "" && s.Source != "unset"
			source := ""
			if present {
				source = s.Source
			}
			lane.Slots = append(lane.Slots, SlotReport{Name: s.Name, Present: present, Source: source})
		}
		if len(lane.Slots) == 0 {
			return nil
		}
		return &lane
	}
	return nil
}

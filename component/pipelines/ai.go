package pipelines

import (
	"bytes"
	"encoding/json"
	"strings"
)

// AISettingsVariable is the global variable holding the owner's pipeline AI
// switches, written from MemQL OS's Pipelines > Settings > AI.
//
// Both features are OFF unless the owner turns them on (owner decision,
// 2026-10-29): a pipeline runs, reports and gates on its own checks, and a
// model is asked only when its switch is on. The engine reads the variable on
// every use rather than when a run opens, so turning a switch never changes a
// run's captured policy, and a review already in flight is never refused for a
// policy change it did not make (the fence in independent_review.go).
const AISettingsVariable = "MEMQL_PIPELINES_AI"

// AISettings is what AISettingsVariable holds, as JSON.
type AISettings struct {
	// Review asks a model to review a pull request after all of its checks
	// pass; a rejection fails the run's check.
	Review bool `json:"review"`
	// Diagnosis lets the installed maintenance policy open an AI analysis goal
	// for a run's first failure.
	Diagnosis bool `json:"diagnosis"`
}

// ParseAISettings reads the variable's value. An absent, empty or unreadable
// value, or one carrying a field this engine does not know, reads as all off:
// a typo must never switch a model on.
func ParseAISettings(raw string) AISettings {
	if strings.TrimSpace(raw) == "" {
		return AISettings{}
	}
	d := json.NewDecoder(bytes.NewReader([]byte(raw)))
	d.DisallowUnknownFields()
	var s AISettings
	if err := d.Decode(&s); err != nil || d.More() {
		return AISettings{}
	}
	return s
}

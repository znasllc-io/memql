package agent

// grounding.go is the Go port of the graph-grounding half of
// the Python voice-agent's realtime_instructions.py: it renders topic-relevant
// graph facts / retrieved knowledge chunks into the labeled context block and
// the conversation.item payloads the realtime executor (#457) injects before
// triggering a response, so grounded answers do not depend solely on a tool
// round-trip.
//
// Everything here is pure / deterministic -- no stream, no provider client --
// so it is unit-tested without a real realtime session. The executor that
// PUSHES these items onto the session is #457; this file only PRODUCES them.

import (
	"fmt"
	"strings"
)

// GroundingFact is a single graph fact or retrieved knowledge chunk to inject.
// Text is the body; DomainName and Source are optional attribution used to
// label the chunk so the model can ground (and, downstream in #458, cite) the
// right domain. Mirrors realtime_instructions.py::GroundingFact.
type GroundingFact struct {
	Text       string
	DomainName string
	Source     string
}

// GroundingContext is the topic-relevant grounding for the active turn: a set
// of facts / chunks scoped to the active topic. Empty is a first-class state --
// a turn with no grounding injects nothing and the model answers from the
// persona + conversation alone. Mirrors
// realtime_instructions.py::GroundingContext.
type GroundingContext struct {
	Facts []GroundingFact
}

// IsEmpty reports whether the context has no non-blank fact text. An all-blank
// context is treated as empty so the caller skips injection entirely.
func (g GroundingContext) IsEmpty() bool {
	for _, f := range g.Facts {
		if strings.TrimSpace(f.Text) != "" {
			return false
		}
	}
	return true
}

// formatGroundingFact renders one numbered, attributed fact line, matching
// realtime_instructions.py::_format_grounding_fact: "[i][ domain / source] text".
func formatGroundingFact(index int, fact GroundingFact) string {
	text := strings.TrimSpace(fact.Text)
	var labelParts []string
	if domain := strings.TrimSpace(fact.DomainName); domain != "" {
		labelParts = append(labelParts, domain)
	}
	if source := strings.TrimSpace(fact.Source); source != "" {
		labelParts = append(labelParts, source)
	}
	label := ""
	if len(labelParts) > 0 {
		label = " [" + strings.Join(labelParts, " / ") + "]"
	}
	return fmt.Sprintf("[%d]%s %s", index, label, text)
}

// BuildGroundingMessage renders the grounding facts into a single labeled
// context block. Returns "" when there is nothing to inject so the caller can
// skip the injection. Each fact is numbered and attributed to its domain /
// source so the model can ground (and later cite, #458) the right material.
// Mirrors realtime_instructions.py::build_grounding_message.
func BuildGroundingMessage(grounding GroundingContext) string {
	if grounding.IsEmpty() {
		return ""
	}

	header := "Relevant context for the current topic (from the space's " +
		"knowledge and graph). Use it to ground your answer; cite the domain " +
		"it came from when you rely on it:"

	lines := []string{header}
	index := 1
	for _, fact := range grounding.Facts {
		if strings.TrimSpace(fact.Text) == "" {
			continue
		}
		lines = append(lines, formatGroundingFact(index, fact))
		index++
	}
	return strings.Join(lines, "\n")
}

// GroundingItem is one realtime conversation.item.create payload. It is a
// neutral, provider-shaped value (not tied to any SDK type) the executor
// (#457) marshals onto the realtime session before triggering a response.
// Mirrors the dict realtime_instructions.py::build_grounding_items returns.
type GroundingItem struct {
	// Type is always "message" for the grounding injection.
	Type string
	// Role is "system" -- out-of-band context, not something the user said.
	Role string
	// Text is the rendered grounding block (BuildGroundingMessage output).
	Text string
}

// BuildGroundingItems builds the conversation.item payloads that inject the
// grounding context as an out-of-band system message. Returns nil when there
// is no grounding to inject. Mirrors
// realtime_instructions.py::build_grounding_items; the executor (#457) turns
// each GroundingItem into a provider conversation.item.create call.
func BuildGroundingItems(grounding GroundingContext) []GroundingItem {
	message := BuildGroundingMessage(grounding)
	if message == "" {
		return nil
	}
	return []GroundingItem{
		{
			Type: "message",
			Role: "system",
			Text: message,
		},
	}
}

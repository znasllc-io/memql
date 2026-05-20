package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// AgentSuggestSchemaJSON is the JSON Schema handed to the structured
// chat provider for agent configuration suggestion. Mirrors the fields
// listed in agentSuggestSystemPrompt, gives the model provider-enforced
// shape (OpenAI's strict mode uses constrained decoding), and keeps
// PostProcessAgentSuggestion as a sanity pass for values we can't
// express in JSON Schema cleanly (e.g. coercing 'assistant'
// out of role suggestions and ensuring core tools end up in the list).
//
// Kept as a literal so the runtime cost is a single parse. Exported so
// the gRPC SISuggestMsg handler has one canonical schema for agent
// suggestions across the cluster.
var AgentSuggestSchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["suggestion", "existingMatches"],
  "properties": {
    "suggestion": {
      "type": "object",
      "additionalProperties": false,
      "required": ["name", "role", "gender", "personalityStyles", "knowledgeDomains", "tools"],
      "properties": {
        "name": {"type": "string", "description": "Single word agent name (e.g. Aria, Atlas, Nova)."},
        "role": {
          "type": "string",
          "enum": [
            "accounting-finance","human-resources","customer-service",
            "quality-assurance","sales-marketing","it-support","legal-compliance",
            "operations","project-management","research-development","training-education"
          ]
        },
        "gender": {"type": "string", "enum": ["male", "female"]},
        "personalityStyles": {
          "type": "array",
          "minItems": 2,
          "maxItems": 4,
          "items": {
            "type": "string",
            "enum": ["friendly","professional","assertive","empathetic","analytical","creative","patient","concise"]
          }
        },
        "knowledgeDomains": {
          "type": "array",
          "minItems": 3,
          "maxItems": 8,
          "items": {"type": "string"}
        },
        "tools": {"type": "array", "items": {"type": "string"}}
      }
    },
    "existingMatches": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["agentId", "name", "similarity", "recommendation", "reasoning"],
        "properties": {
          "agentId": {"type": "string"},
          "name": {"type": "string"},
          "similarity": {"type": "string", "enum": ["exact", "high", "moderate"]},
          "recommendation": {"type": "string", "enum": ["use_existing", "modify_existing", "create_new"]},
          "reasoning": {"type": "string"}
        }
      }
    }
  }
}`)

// ExistingAgent describes an already-configured agent that the suggester
// can consider for overlap / modify-existing decisions.
type ExistingAgent struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Description  string             `json:"description"`
	Capabilities *AgentCapabilities `json:"capabilities,omitempty"`
}

// AgentCapabilities describes the capabilities of an existing agent.
type AgentCapabilities struct {
	Domains []string `json:"domains"`
	Tools   []string `json:"tools"`
}

// agentSuggestSystemPrompt is the agent-configuration system prompt.
// Lives here (not as a .memql template) because it's tightly coupled
// to the structured JSON shape the handler parses back out.
const agentSuggestSystemPrompt = `Configure an AI agent from a user description. Return ONLY valid JSON (no markdown fences).

Required fields:
- name: single word (e.g. Aria, Atlas, Nova, Felix, Jade, Penny)
- role: one of: accounting-finance, human-resources, customer-service, quality-assurance, sales-marketing, it-support, legal-compliance, operations, project-management, research-development, training-education. DO NOT suggest assistant -- every user is auto-provisioned exactly one Assistant already (a per-user singleton), so the create-agent flow rejects that role.
- gender: "male" or "female" (default: female)
- personalityStyles: 2-4 from: friendly, professional, assertive, empathetic, analytical, creative, patient, concise
- knowledgeDomains: 3-6 relevant domains. Pick from the catalog the user implied; "business-administration" is the org-wide baseline domain a specialist can opt into when general business literacy is part of the role.
- tools: role-relevant tools (core tools data-query, document-search, calendar-access, notifications, email-compose, task-management are auto-added)
- existingMatches: IMPORTANT -- compare the user description against the existing agents listed below. For each existing agent with 50%+ functional overlap, add an entry: {agentId: "<id>", name: "<name>", similarity: "exact"|"high"|"moderate", recommendation: "use_existing"|"modify_existing"|"create_new", reasoning: "<why>"}. Return empty array [] if no agents overlap.

Rules: default to female gender unless specified. Return valid JSON only.`

// BuildAgentSuggestMessages builds system + user prompts for agent
// suggestion. The existing-agents list is folded into the system prompt
// so the model can judge overlap without an extra turn.
func BuildAgentSuggestMessages(description string, existingAgents []ExistingAgent) []common.ChatMessage {
	var lines []string
	for _, a := range existingAgents {
		domains := "none"
		tools := "none"
		if a.Capabilities != nil {
			if d := strings.Join(a.Capabilities.Domains, ", "); d != "" {
				domains = d
			}
			if t := strings.Join(a.Capabilities.Tools, ", "); t != "" {
				tools = t
			}
		}
		lines = append(lines, fmt.Sprintf("- ID: %s | Name: %s | Role: %s | Domains: %s | Tools: %s",
			a.ID, a.Name, a.Description, domains, tools))
	}
	existingList := "No existing agents."
	if len(lines) > 0 {
		existingList = strings.Join(lines, "\n")
	}

	systemPrompt := agentSuggestSystemPrompt + "\n\n## Existing Agents\n" + existingList
	userPrompt := fmt.Sprintf(`User wants to create an agent for: "%s"`, strings.TrimSpace(description))

	return []common.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
}

// PostProcessAgentSuggestion validates agent IDs and enforces required
// defaults (core tools, name without spaces, valid role). The
// business-administration knowledge domain is no longer force-added
// here -- it's a regular catalog domain specialists can opt into and
// it's auto-attached + locked only on the Assistant via
// provisionAssistant. Forcing it on every suggested specialist
// (the previous behaviour) shipped extra training-time embedding work
// the user never asked for.
func PostProcessAgentSuggestion(suggestion map[string]any, existingAgents []ExistingAgent) {
	// Validate existingMatches agent IDs
	if matches, ok := suggestion["existingMatches"].([]any); ok && len(existingAgents) > 0 {
		validIds := make(map[string]bool, len(existingAgents))
		for _, a := range existingAgents {
			validIds[a.ID] = true
		}
		filtered := make([]any, 0, len(matches))
		for _, m := range matches {
			if match, ok := m.(map[string]any); ok {
				if id, ok := match["agentId"].(string); ok && validIds[id] {
					filtered = append(filtered, m)
				}
			}
		}
		suggestion["existingMatches"] = filtered
	} else if len(existingAgents) == 0 {
		suggestion["existingMatches"] = []any{}
	}

	s, ok := suggestion["suggestion"].(map[string]any)
	if !ok {
		return
	}

	// Coerce assistant away. The JSON Schema enum excludes it,
	// so OpenAI's strict structured-output path cannot emit it; this
	// is a defense-in-depth fallback for non-strict providers (and
	// future schema drift). GA is reserved for the auto-provisioned
	// per-user Sofia created by provisionAssistantOnUserCreate;
	// the frontend picker also excludes it and would render an
	// undefined value if the suggestion slipped through.
	if role, ok := s["role"].(string); ok && role == "assistant" {
		s["role"] = "customer-service"
	}

	// Ensure core tools
	if tools, ok := s["tools"].([]any); ok {
		coreTools := []string{"data-query", "document-search", "calendar-access", "notifications", "email-compose", "task-management"}
		have := make(map[string]bool, len(tools))
		for _, t := range tools {
			if ts, ok := t.(string); ok {
				have[ts] = true
			}
		}
		for _, ct := range coreTools {
			if !have[ct] {
				tools = append([]any{ct}, tools...)
			}
		}
		s["tools"] = tools
	}

	// Strip spaces from name
	if name, ok := s["name"].(string); ok {
		s["name"] = strings.ReplaceAll(name, " ", "")
	}
}

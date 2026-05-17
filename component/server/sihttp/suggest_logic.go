package sihttp

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// BuildSpaceSuggestMessages builds the system and user prompts for space suggestion.
func BuildSpaceSuggestMessages(description string, agents []SpaceAgent) []common.ChatMessage {
	var agentLines []string
	for _, a := range agents {
		role := a.Description
		if role == "" {
			role = "General"
		}
		agentLines = append(agentLines, fmt.Sprintf("- ID: %s | Name: %s | Role: %s", a.ID, a.Name, role))
	}
	agentList := strings.Join(agentLines, "\n")

	systemPrompt := fmt.Sprintf(`You are a smart assistant that helps users set up spaces. Based on the user's description of what they want to work on, you must:

1. Generate a concise, descriptive title for the space (3-7 words, no quotes)
2. Select the best-matching agent(s) from the available list
3. Determine the architecture: "standard" for 1-on-1 work, "polyphon" for group/multi-agent sessions

Available agents:
%s

Rules:
- Select exactly 1 agent for standard architecture
- Select 1-3 agents for polyphon architecture
- Only use polyphon if the description clearly implies multi-perspective, group, or cross-functional work
- The title should be professional and specific to the described work
- Return ONLY valid JSON with fields: title (string), agentIds (string[]), architecture ("standard" or "polyphon")`, agentList)

	userPrompt := fmt.Sprintf(`User wants to create a space for: "%s"`, strings.TrimSpace(description))

	return []common.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
}

// PostProcessSpaceSuggestion validates agent IDs and enforces architecture constraints.
func PostProcessSpaceSuggestion(suggestion map[string]any, agents []SpaceAgent) {
	validIds := make(map[string]bool, len(agents))
	for _, a := range agents {
		validIds[a.ID] = true
	}

	if agentIds, ok := suggestion["agentIds"].([]any); ok {
		var valid []any
		for _, id := range agentIds {
			if idStr, ok := id.(string); ok && validIds[idStr] {
				valid = append(valid, id)
			}
		}
		if len(valid) == 0 && len(agents) > 0 {
			valid = []any{agents[0].ID}
		}
		suggestion["agentIds"] = valid

		// Enforce architecture constraints
		arch, _ := suggestion["architecture"].(string)
		if arch == "standard" && len(valid) > 1 {
			suggestion["agentIds"] = valid[:1]
		}
	}
}

// BuildGroupSuggestMessages builds the system and user prompts for group suggestion.
func BuildGroupSuggestMessages(description string, users []GroupUser) []common.ChatMessage {
	var userLines []string
	for _, u := range users {
		role := u.Role
		if role == "" {
			role = "user"
		}
		userLines = append(userLines, fmt.Sprintf("- ID: %s | Name: %s | Email: %s | Role: %s",
			u.ID, u.DisplayName, u.Email, role))
	}
	userList := "No users available."
	if len(userLines) > 0 {
		userList = strings.Join(userLines, "\n")
	}

	systemPrompt := fmt.Sprintf(`Configure an organizational group from a user description. Return ONLY flat JSON (no nesting, no markdown).

Fields:
- name: short team/department name (e.g. "Sales Team", "Customer Support", "Engineering"). Do NOT append "Workspace", "Group", "Space", or "Hub".
- description: 1-2 sentences explaining the group's purpose and what members collaborate on
- suggestedMemberIds: array of user IDs from the available list whose roles align with the group's purpose. Empty array if none match.

Available users:
%s

Rules: keep name under 4 words, no suffixes like "Workspace" or "Group". Return flat JSON only.`, userList)

	userPrompt := fmt.Sprintf(`User wants to create a group for: "%s"`, strings.TrimSpace(description))

	return []common.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
}

// PostProcessGroupSuggestion validates suggested member IDs against the user list.
func PostProcessGroupSuggestion(suggestion map[string]any, users []GroupUser) {
	if len(users) > 0 {
		validIds := make(map[string]bool, len(users))
		for _, u := range users {
			validIds[u.ID] = true
		}

		if memberIds, ok := suggestion["suggestedMemberIds"].([]any); ok {
			var valid []any
			for _, id := range memberIds {
				if idStr, ok := id.(string); ok && validIds[idStr] {
					valid = append(valid, id)
				}
			}
			if valid == nil {
				valid = []any{}
			}
			suggestion["suggestedMemberIds"] = valid
		}
	} else {
		suggestion["suggestedMemberIds"] = []any{}
	}
}

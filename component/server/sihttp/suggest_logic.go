package sihttp

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// BuildSpaceSuggestMessages builds the system and user prompts for the
// space title suggestion. Under the one-assistant space model
// (copresent #124) a space has 1+ humans plus exactly one assistant
// (auto-joined by the backend); there is no agent picker and no
// architecture choice, so the suggestion is title-only.
func BuildSpaceSuggestMessages(description string) []common.ChatMessage {
	systemPrompt := `You are a smart assistant that helps users set up spaces. Based on the user's description of what they want to work on, generate a concise, descriptive title for the space (3-7 words, no quotes). The title should be professional and specific to the described work. Return ONLY valid JSON with a single field: title (string).`

	userPrompt := fmt.Sprintf(`User wants to create a space for: "%s"`, strings.TrimSpace(description))

	return []common.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
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

package cognition

import (
	"fmt"
	"strings"
)

// buildAgentBoundaries generates a boundaries string for the prompt.
// It tells the agent what it should NOT do and when to defer to peers.
func buildAgentBoundaries(agent *agentPayload, allAgents []*agentPayload) string {
	if agent == nil {
		return ""
	}

	var sb strings.Builder

	// Domain boundaries.
	var myDomains []string
	if agent.Capabilities != nil {
		if domains, ok := agent.Capabilities["domains"].([]any); ok {
			for _, d := range domains {
				if s, ok := d.(string); ok {
					myDomains = append(myDomains, s)
				}
			}
		}
	}

	if len(myDomains) > 0 {
		sb.WriteString("- You specialize in: ")
		sb.WriteString(strings.Join(myDomains, ", "))
		sb.WriteString("\n")
		sb.WriteString("- If asked about topics clearly outside your domains, suggest the user ask another agent.\n")
	}

	// Tool boundaries.
	if !agent.ClawCapable() {
		sb.WriteString("- You do NOT have coding/automation capabilities.\n")
	}

	return strings.TrimSpace(sb.String())
}

// buildPeerRoster generates a roster of other agents in the space.
// This helps agents know who else is available and what they specialize in.
func buildPeerRoster(currentAgent *agentPayload, allAgentPayloads []*agentPayload) string {
	if currentAgent == nil || len(allAgentPayloads) <= 1 {
		return ""
	}

	var sb strings.Builder
	for _, peer := range allAgentPayloads {
		if peer == nil || peer.Name == currentAgent.Name {
			continue
		}

		sb.WriteString(fmt.Sprintf("- %s", peer.Name))
		if desc := strings.TrimSpace(peer.Description); desc != "" {
			sb.WriteString(fmt.Sprintf(": %s", desc))
		}

		// Peer domains.
		if peer.Capabilities != nil {
			if domains, ok := peer.Capabilities["domains"].([]any); ok && len(domains) > 0 {
				domainStrs := make([]string, 0, len(domains))
				for _, d := range domains {
					if s, ok := d.(string); ok {
						domainStrs = append(domainStrs, s)
					}
				}
				if len(domainStrs) > 0 {
					sb.WriteString(fmt.Sprintf(" [domains: %s]", strings.Join(domainStrs, ", ")))
				}
			}
		}

		// Peer capabilities summary.
		if peer.ClawCapable() {
			sb.WriteString(" [capabilities: coding]")
		}

		sb.WriteString("\n")
	}

	return strings.TrimSpace(sb.String())
}

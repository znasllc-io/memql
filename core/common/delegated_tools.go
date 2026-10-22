package common

import "context"

// DelegatedTools is the server-created scope of an app session. It travels
// through the session row so another MCP replica can enforce the same tool
// contract. It must never be accepted from a client or reconstructed from prose.
const DelegatedResponseTool = "respondToUser"

type DelegatedTools struct {
	Tools     []ToolDefinition          `json:"tools"`
	AgentID   string                    `json:"agentId"`
	AgentRole string                    `json:"agentRole"`
	Run       RunContext                `json:"run"`
	Defaults  map[string]map[string]any `json:"defaults,omitempty"`
	// Messages are the native turn at handover, never the app's private
	// reasoning. They anchor a durable continuation before a delegated question.
	Messages []ChatMessage `json:"messages,omitempty"`
}

type delegatedDefaultsKey struct{}

// ContextWithDelegatedToolDefaults carries the same per-tool argument stamps
// that the agent's native loop applies, including nested provenance fields.
func ContextWithDelegatedToolDefaults(ctx context.Context, defaults map[string]map[string]any) context.Context {
	return context.WithValue(ctx, delegatedDefaultsKey{}, defaults)
}

func DelegatedToolDefaultsFromContext(ctx context.Context) map[string]map[string]any {
	v, _ := ctx.Value(delegatedDefaultsKey{}).(map[string]map[string]any)
	return v
}

package common

import (
	"context"
	"encoding/json"
)

// ChatMessage represents a message in a chat conversation with proper roles.
// This enables proper multi-turn conversation support with SI models.
type ChatMessage struct {
	Role    string // "system", "user", or "assistant"
	Content string
	// Name is optional and may be used by some providers (e.g., tool messages).
	Name string
	// ToolCallId is used for Role="tool" to associate a tool result to a prior tool call.
	ToolCallId string
	// ToolCalls is used for Role="assistant" when the model requests tool calls.
	// It must be preserved in the message history so providers can validate subsequent tool results.
	ToolCalls []ToolCall
}

// ChatSIProvider provides chat-based SI completion with proper message roles.
// Providers that support multi-turn conversations should implement this interface.
type ChatSIProvider interface {
	// CallChat sends a conversation with proper message roles to the model.
	// This is the preferred method for conversations as it preserves turn-taking context.
	CallChat(ctx context.Context, messages []ChatMessage) (string, error)
}

// StructuredSchema describes a JSON schema that the provider enforces on
// the model's output. Used by ChatStructuredProvider for all "logic"
// prompts (routing, classification, prediction, config suggestion) --
// anywhere the caller parses the reply as JSON and wants provider-level
// guarantees on shape. Prose prompts (agent replies) stay on CallChat.
type StructuredSchema struct {
	// Name is a short identifier the provider may surface in errors
	// and traces, e.g. "cognitionRouting", "agentConfigSuggest".
	Name string
	// Description is optional human-readable context for the schema.
	Description string
	// Schema is the JSON Schema document. Must unmarshal to a valid
	// JSON schema object: top-level `{"type":"object", ...}`.
	Schema json.RawMessage
	// Strict asks the provider to refuse any output that doesn't
	// match the schema. OpenAI's json_schema mode with strict=true
	// uses constrained decoding and guarantees the output parses.
	Strict bool
}

// ChatStructuredProvider provides chat completion with provider-enforced
// structured output. The returned string is a JSON document guaranteed
// to validate against the supplied schema (when strict=true and the
// provider supports constrained decoding) or a best-effort JSON-looking
// response (fallback providers).
//
// Implementations should prefer the provider's native structured-output
// mode where available (OpenAI json_schema, Anthropic tool-use,
// Gemini response_schema). Providers without native support should
// document the fallback behaviour.
type ChatStructuredProvider interface {
	CallChatStructured(ctx context.Context, messages []ChatMessage, schema StructuredSchema) (string, error)
}

// ToolDefinition is a minimal tool schema used for tool-calling providers.
// It intentionally mirrors the MCP surface: name + description + JSON Schema input.
type ToolDefinition struct {
	Name        string
	Description string
	// InputSchema must be a JSON-schema-compatible value (typically map[string]any).
	InputSchema any
}

// ToolCall represents a model-requested tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON object string
}

// ToolCallingChatResult is the outcome of a single tool-calling model step.
// If ToolCalls is non-empty, the caller should execute them and continue the loop.
type ToolCallingChatResult struct {
	AssistantText string
	ToolCalls     []ToolCall
}

// ToolCallingChatSIProvider provides chat completion with tool calling support.
type ToolCallingChatSIProvider interface {
	CallChatWithTools(ctx context.Context, messages []ChatMessage, tools []ToolDefinition) (*ToolCallingChatResult, error)
}

// StreamChunk represents a single chunk from a streaming SI response.
type StreamChunk struct {
	Content string
	Done    bool
	Error   error
}

// ChatStreamProvider provides streaming chat completion.
type ChatStreamProvider interface {
	CallChatStream(ctx context.Context, messages []ChatMessage) (<-chan StreamChunk, error)
}

// ToolCallDelta represents an incremental fragment of a tool call from a streaming response.
type ToolCallDelta struct {
	Index     int
	ID        string // present only in the first delta for each tool call
	Name      string // present only in the first delta for each tool call
	Arguments string // incremental JSON fragment
}

// StreamToolChunk carries both text content and tool call deltas from a streaming response.
type StreamToolChunk struct {
	Content   string
	ToolCalls []ToolCallDelta
	Done      bool
	Error     error
}

// ChatStreamWithToolsProvider provides streaming chat completion with tool support.
// Text content and tool call arguments arrive incrementally in the same stream.
type ChatStreamWithToolsProvider interface {
	CallChatStreamWithTools(ctx context.Context, messages []ChatMessage, tools []ToolDefinition) (<-chan StreamToolChunk, error)
}

// VisionContent represents an image or document for vision models.
type VisionContent struct {
	MimeType string // e.g., "image/png", "image/jpeg"
	Data     []byte // Raw image bytes
}

// VisionSIProvider provides image understanding capabilities for SI models.
type VisionSIProvider interface {
	CallVision(ctx context.Context, prompt string, images []VisionContent) (string, error)
}

// ---------------------------------------------------------------------------
// Tool defaults injection via context
// ---------------------------------------------------------------------------

// toolDefaultsCtxKey is the context key for injecting default argument values
// into tool calls. When set, these defaults are merged into every tool call's
// arguments UNLESS the SI model already provided a value for that key.
// This allows callers (e.g., cognition) to inject spaceId, participantId,
// etc. without relying on the SI model to include them.
type toolDefaultsCtxKey struct{}

// ContextWithToolDefaults returns a new context carrying default tool argument values.
// These values are automatically injected into tool calls during the tool loop.
func ContextWithToolDefaults(ctx context.Context, defaults map[string]any) context.Context {
	if len(defaults) == 0 {
		return ctx
	}
	return context.WithValue(ctx, toolDefaultsCtxKey{}, defaults)
}

// ToolDefaultsFromContext extracts tool default values from the context, if any.
func ToolDefaultsFromContext(ctx context.Context) map[string]any {
	if defaults, ok := ctx.Value(toolDefaultsCtxKey{}).(map[string]any); ok {
		return defaults
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tool activity callback via context
// ---------------------------------------------------------------------------

// ToolActivityEvent describes a tool execution lifecycle event.
type ToolActivityEvent struct {
	ToolName string // Name of the tool being called
	Phase    string // "start" or "end"
	IsError  bool   // True if the tool call failed (Phase=="end" only)
	Label    string // Human-readable label (e.g., "Reading main.go"), set by caller
	TaskId   string // Unique task ID for tracking concurrent operations
}

// ToolActivityCallback is called during the tool loop to report tool execution.
type ToolActivityCallback func(event ToolActivityEvent)

type toolActivityCtxKey struct{}

// ContextWithToolActivityCallback returns a new context carrying a tool activity callback.
func ContextWithToolActivityCallback(ctx context.Context, cb ToolActivityCallback) context.Context {
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, toolActivityCtxKey{}, cb)
}

// ToolActivityCallbackFromContext extracts the tool activity callback, if any.
func ToolActivityCallbackFromContext(ctx context.Context) ToolActivityCallback {
	if cb, ok := ctx.Value(toolActivityCtxKey{}).(ToolActivityCallback); ok {
		return cb
	}
	return nil
}

// ---------------------------------------------------------------------------
// Early text callback via context
// ---------------------------------------------------------------------------

// EarlyTextCallback is called during the tool loop when the model produces text
// alongside tool calls on the first iteration. This allows the caller to emit
// an acknowledgment ("Let me work on that...") before blocking on tool execution.
type EarlyTextCallback func(text string)

type earlyTextCtxKey struct{}

// ContextWithEarlyTextCallback returns a new context carrying an early text callback.
func ContextWithEarlyTextCallback(ctx context.Context, cb EarlyTextCallback) context.Context {
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, earlyTextCtxKey{}, cb)
}

// EarlyTextCallbackFromContext extracts the early text callback, if any.
func EarlyTextCallbackFromContext(ctx context.Context) EarlyTextCallback {
	if cb, ok := ctx.Value(earlyTextCtxKey{}).(EarlyTextCallback); ok {
		return cb
	}
	return nil
}

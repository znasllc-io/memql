// Package metadata provides contextual metadata collection for memQL nodes.
// It reads identity, request, geographic, server, and source context from
// context.Context and assembles a flat map[string]string stored in the
// MemoryNodes.metadata JSONB column.
package metadata

import "context"

// --- Request metadata ---

type requestMetaKey struct{}

// RequestMeta carries transport-level request context.
type RequestMeta struct {
	RequestId     string // gRPC message ID or generated UUID
	CorrelationId string // Distributed trace ID
	Protocol      string // "grpc", "ws", "http"
	Method        string // gRPC message type (e.g., "ExecuteQuery")
	UserAgent     string // Client user-agent string
	ClientIP      string // Client IP address (from peer or X-Forwarded-For)
	Platform      string // Client-reported platform type (e.g., "web", "mobile", "voice")
	AppVersion    string // Client application version
}

// ContextWithRequestMeta injects RequestMeta into the context.
func ContextWithRequestMeta(ctx context.Context, rm *RequestMeta) context.Context {
	if rm == nil {
		return ctx
	}
	return context.WithValue(ctx, requestMetaKey{}, rm)
}

// RequestMetaFromContext extracts RequestMeta from the context.
func RequestMetaFromContext(ctx context.Context) *RequestMeta {
	rm, _ := ctx.Value(requestMetaKey{}).(*RequestMeta)
	return rm
}

// --- Source metadata ---

type sourceMetaKey struct{}

// SourceMeta describes what triggered a mutation (automation, API call, tool, etc.).
type SourceMeta struct {
	System         string // "api", "automation", "system"
	Component      string // "engine", "cognition", "integration"
	AutomationName string // Name of the automation (if triggered by one)
	FunctionName   string // Name of the function (if triggered by one)
	ToolName       string // Name of the MCP tool (if triggered by one)
	Trigger        string // Event topic that triggered the automation
}

// ContextWithSourceMeta injects SourceMeta into the context.
func ContextWithSourceMeta(ctx context.Context, sm *SourceMeta) context.Context {
	if sm == nil {
		return ctx
	}
	return context.WithValue(ctx, sourceMetaKey{}, sm)
}

// SourceMetaFromContext extracts SourceMeta from the context.
func SourceMetaFromContext(ctx context.Context) *SourceMeta {
	sm, _ := ctx.Value(sourceMetaKey{}).(*SourceMeta)
	return sm
}

// --- AI metadata ---

type aiMetaKey struct{}

// AIMeta carries AI execution context.
type AIMeta struct {
	AgentId   string // Agent node ID
	AgentName string // Agent display name
	Provider  string // AI provider name (e.g., "claudeSonnet")
	Model     string // Model identifier (e.g., "claude-sonnet-4-6")
	TokensIn  int    // Input tokens consumed
	TokensOut int    // Output tokens generated
	LatencyMs int    // AI call duration in milliseconds
	ToolCalls int    // Number of tool calls made
}

// ContextWithAIMeta injects AIMeta into the context.
func ContextWithAIMeta(ctx context.Context, si *AIMeta) context.Context {
	if si == nil {
		return ctx
	}
	return context.WithValue(ctx, aiMetaKey{}, si)
}

// AIMetaFromContext extracts AIMeta from the context.
func AIMetaFromContext(ctx context.Context) *AIMeta {
	si, _ := ctx.Value(aiMetaKey{}).(*AIMeta)
	return si
}

// --- Lineage metadata ---

type lineageMetaKey struct{}

// LineageMeta carries data provenance context.
type LineageMeta struct {
	CausedBy     string // Node ID that triggered this mutation
	ReplyTo      string // Node ID this is a reply to
	BatchId      string // Batch operation ID
	ImportSource string // External import source identifier
}

// ContextWithLineageMeta injects LineageMeta into the context.
func ContextWithLineageMeta(ctx context.Context, lm *LineageMeta) context.Context {
	if lm == nil {
		return ctx
	}
	return context.WithValue(ctx, lineageMetaKey{}, lm)
}

// LineageMetaFromContext extracts LineageMeta from the context.
func LineageMetaFromContext(ctx context.Context) *LineageMeta {
	lm, _ := ctx.Value(lineageMetaKey{}).(*LineageMeta)
	return lm
}

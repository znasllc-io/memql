// Package agent hosts the agent-node reply pipeline: prompt rendering, the
// bounded tool-calling loop, and the non-streaming fallback. Cognition no
// longer generates agent replies locally -- it ships AgentGenerateTurnMsg
// to an agent node via NodeService.AiForwardRequest and streams deltas back
// for client fan-out.
package agent

import (
	"context"

	"github.com/visionarys-io/memql/component/memql"
	"github.com/visionarys-io/memql/core/common"
)

// ComponentName identifies this package in logs and environment lookups.
const ComponentName = common.ComponentName("agentReplier")

// MemQLEngine is the narrow engine interface the agent replier needs. It is
// intentionally a subset of memql.IntegrationEngineAccess + the SI invocation
// surface; the concrete adapter in app/ supplies all methods.
type MemQLEngine interface {
	// InvokeSI runs a prompt template by ID and returns the assistant's reply.
	// Used by the non-streaming path (voice / polyphon-text turns).
	InvokeSI(ctx context.Context, templateId string, data map[string]any) (any, error)
	// RenderPrompt renders a prompt template with the given data and returns
	// the rendered text. Used to build the system prompt for the streaming
	// tool loop.
	RenderPrompt(templateId string, data map[string]any) (string, error)
	// ChatStreamWithToolsProviderByName returns a named provider that supports
	// streaming chat with tools. Used by the streaming tool loop.
	ChatStreamWithToolsProviderByName(name string) common.ChatStreamWithToolsProvider
	// ToolDefinitionsForNames returns tool definitions for the given tool names.
	ToolDefinitionsForNames(names []string) []common.ToolDefinition
	// ExecuteToolByName looks up a tool by name and executes it with args.
	ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)
	// RegisterIntegration registers an IntegrationProvider with the engine.
	RegisterIntegration(provider memql.IntegrationProvider) error
	// Execute runs a MemQL query/builtin call and returns the raw result.
	// Used by the RAG retrieval path in handleStreaming to invoke the
	// similarTo builtin.
	Execute(ctx context.Context, query string) (any, error)
}

// DeltaSink is what Handle() emits into as a turn produces output. The gRPC
// handler satisfies this by converting each call into an
// AgentGenerateTurnDelta proto message and writing it to the NodeService
// stream back to cognition.
//
// TextDelta is called with incremental text chunks as they stream from the
// model. ToolCall / ToolResult are called when the model invokes a tool and
// when the result is fed back on the next iteration, respectively. Complete
// is called exactly once at the end of the turn.
type DeltaSink interface {
	TextDelta(text string)
	ToolCall(id, name, argumentsJSON string)
	ToolResult(id, resultJSON, errMsg string)
}

// TurnResult summarises a completed turn. Handle() returns this so the caller
// can emit a final AgentGenerateTurnComplete with full-text + audit trail.
//
// Citations carry the structured source-attribution list extracted from
// the agent's terminal `respondToUser` call. They flow through
// AgentGenerateTurnComplete to cognition, get stamped on the committed
// utterance, and surface to the frontend chat renderer which wraps each
// `MatchedPhrase` with a clickable source chip.
//
// RetrievedChunks is the full pool the LLM saw -- what RAG pulled BEFORE
// the model decided what to cite. Distinct from Citations: a chunk can
// be in RetrievedChunks but absent from Citations (the model considered
// it but didn't draw on it). Frontend uses both to render the
// "Show details" expander beneath each reply.
type TurnResult struct {
	FinalText       string
	TextChunks      int
	ToolCalls       []common.ToolCall
	Iterations      int
	Citations       []Citation
	RetrievedChunks []RetrievedChunk
}

// RetrievedChunk is one entry in the RAG retrieval pool surfaced to the
// frontend's "Show details" expander. The agent collects these from the
// similarTo result before invoking the LLM; they include passages the
// model considered but didn't cite, which is the whole point of the
// expander (without RetrievedChunks the user can't tell the difference
// between "RAG found nothing" and "RAG found things and the model chose
// not to use them").
type RetrievedChunk struct {
	DomainId    string
	SourceRef   string
	Similarity  float64
	TextPreview string
	Citation    string
}

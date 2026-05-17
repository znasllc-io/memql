package memql

import (
	"context"
	"encoding/json"

	"github.com/znasllc-io/memql/core/common"
)

// IntegrationEngineAccess is the interface integrations receive instead of the
// full MemQLEngine. It provides capability registration, protocol-level
// streaming, MemQL function execution, SI prompt invocation, and prompt
// rendering.
//
// SI invocation (InvokeSI) is exposed here because the cognition integration
// drives score-based agent routing from Go with dynamic per-utterance
// context (participants, recent turns, scoring signals). Expressing that in
// the DSL would require building a query string every turn; calling
// InvokeSI directly is cleaner. The DSL `si()` form is still the right
// choice inside shape() projection contexts.
type IntegrationEngineAccess interface {
	// RegisterIntegration registers an IntegrationProvider, making its
	// capabilities callable as builtin functions from the MemQL DSL.
	RegisterIntegration(provider IntegrationProvider) error

	// Execute runs a MemQL query or function call and returns the result
	// bundle. Use this for calling registered MemQL functions (e.g.,
	// spaceParticipants, agentById) and for insert mutations after
	// streaming completes.
	Execute(ctx context.Context, query string) (*ExecuteResult, error)

	// InvokeSI runs a prompt template by ID with the given data map,
	// routing through the engine's SI provider registry (including cache
	// and provider-override plumbing). Equivalent to `si("<id>", {...data})`
	// in the DSL, but callable from Go without having to build and parse
	// a query string. The DSL form is only valid inside shape() projection
	// contexts; InvokeSI is the top-level equivalent for integration code.
	InvokeSI(ctx context.Context, templateId string, data map[string]any) (any, error)

	// InvokeSIStructured renders a prompt template and invokes its
	// default chat provider with a JSON schema the model must match.
	// Returns the raw JSON string. Used for routing, classification,
	// suggestion -- any "logic" prompt where shape correctness matters
	// more than prose quality. Mirrors the cognition integration's
	// MemQLEngine interface (which historically had this method
	// before it was promoted to the common surface) so integrations
	// no longer have to declare a local engine-interface superset.
	InvokeSIStructured(
		ctx context.Context,
		templateId string,
		data map[string]any,
		schemaName string,
		schema json.RawMessage,
		strict bool,
	) (string, error)

	// RenderPrompt renders a prompt template with the given data.
	// Used by streaming handlers to prepare the system prompt before
	// feeding it to a streaming provider.
	RenderPrompt(templateId string, data map[string]any) (string, error)

	// ChatStreamProvider returns the default streaming chat provider, or nil.
	ChatStreamProvider() common.ChatStreamProvider

	// ChatStreamProviderByName returns a named streaming chat provider, or nil.
	ChatStreamProviderByName(name string) common.ChatStreamProvider

	// ChatStreamWithToolsProviderByName returns a named provider that supports
	// streaming chat with tool calling, or nil.
	ChatStreamWithToolsProviderByName(name string) common.ChatStreamWithToolsProvider

	// ToolDefinitionsForNames returns tool definitions for the given tool names.
	ToolDefinitionsForNames(names []string) []common.ToolDefinition

	// ExecuteToolByName looks up a tool by name and executes it with the given args.
	ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)
}

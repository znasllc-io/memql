package memql

import (
	"context"
	"encoding/json"

	"github.com/znasllc-io/memql/core/common"
)

// IntegrationEngineAccess is the interface integrations receive instead of the
// full MemQLEngine. It provides capability registration, protocol-level
// streaming, MemQL function execution, AI prompt invocation, and prompt
// rendering.
//
// AI invocation (InvokeAI) is exposed here because an integration drives model
// calls from Go with context it assembles itself, and expressing that in the
// DSL would mean building a query string every turn.
//
// It is ALSO the seam the DSL's own `ai` builtin calls
// (integrations/agents/ai.go), so this is one call with two front doors. The
// DSL one is a STATEMENT and nothing else: `builtin ai(templateId:
// "docSummary", data: { ... })`, written with its kind and its arguments by
// name. There is NO projection form -- a prompt called once per row of a result
// is a different feature with a different budget, so a query filter, a
// `refine`, a sort and a spec or trait body all refuse the call, and a query
// filter names it `lower_refused` at load. See docs/public/language/memql.md,
// "Calling a prompt".
type IntegrationEngineAccess interface {
	// RegisterIntegration registers an IntegrationProvider, making its
	// capabilities callable as builtin functions from the MemQL DSL.
	RegisterIntegration(provider IntegrationProvider) error

	// Execute runs a MemQL query or function call and returns the result
	// bundle. Use this for calling registered MemQL functions (e.g.,
	// spaceParticipants, agentById) and for insert mutations after
	// streaming completes.
	Execute(ctx context.Context, query string) (*ExecuteResult, error)

	// InvokeAI runs a prompt by name with the given data map: it resolves the
	// prompt, validates the data against the prompt's own body (that body IS
	// the input schema), renders the template, routes by the prompt's @level
	// through core/airoute, serves from the exact-render cache when one is
	// warm, and journals the call.
	//
	// The DSL's `builtin ai(templateId:, data:)` is a thin wrapper around this
	// exact method, so the two agree by construction rather than by
	// maintenance. Go callers use it directly when they assemble the data
	// themselves; a .memql body calls the builtin.
	InvokeAI(ctx context.Context, templateId string, data map[string]any) (any, error)

	// InvokeAIStructured renders a prompt template and invokes its
	// default chat provider with a JSON schema the model must match.
	// Returns the raw JSON string. Used for routing, classification,
	// suggestion -- any "logic" prompt where shape correctness matters
	// more than prose quality. Mirrors the cognition integration's
	// MemQLEngine interface (which historically had this method
	// before it was promoted to the common surface) so integrations
	// no longer have to declare a local engine-interface superset.
	InvokeAIStructured(
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

	// THERE IS NO PROVIDER LOOKUP ON THIS SURFACE (epic memql#5127, design
	// D2). ChatStreamProvider, ChatStreamProviderByName and
	// ChatStreamWithToolsProviderByName were declared here, implemented by
	// app/'s adapter, and called by nothing -- an integration that wants a
	// model asks the router for one by declaring a level and a modality, and
	// handing out a provider by name here would be a second way to reach the
	// registry that records no decision and no rule can see.

	// ToolDefinitionsForNames returns tool definitions for the given tool names.
	ToolDefinitionsForNames(names []string) []common.ToolDefinition

	// ExecuteToolByName looks up a tool by name and executes it with the given args.
	ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)

	// ResolveSkills returns the unioned (domain, tool, liveSource)
	// surface for a list of v1:skills:skill ids -- the canonical
	// helper every consumer of the new agent shape calls before
	// reading capabilities (Phase 2 cut: #158). Unknown ids are
	// warn-logged and skipped; empty input yields an empty bundle.
	ResolveSkills(ctx context.Context, skillIds []string) (SkillBundle, error)
}

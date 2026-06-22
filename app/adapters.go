package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// AudioEngineAdapter wraps MemQLEngine to satisfy audiows.MemQLExecutor.
type AudioEngineAdapter struct {
	Engine *memql.MemQLEngine
}

func (a *AudioEngineAdapter) ExecuteInsert(ctx context.Context, query string) error {
	_, err := a.Engine.Execute(ctx, query)
	return err
}

// CognitionEngineAdapter wraps MemQLEngine to satisfy cognition.MemQLEngine.
// The key difference: memql.MemQLEngine.Execute returns (*ExecuteResult, error)
// but cognition.MemQLEngine expects (any, error), so this adapter converts the result.
type CognitionEngineAdapter struct {
	Engine *memql.MemQLEngine
}

func (a *CognitionEngineAdapter) Execute(ctx context.Context, query string) (any, error) {
	result, err := a.Engine.Execute(ctx, query)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	converted := ConvertExecuteResultToMap(result)
	return converted, nil
}

func (a *CognitionEngineAdapter) RegisterIntegration(provider memql.IntegrationProvider) error {
	if a == nil || a.Engine == nil {
		return fmt.Errorf("memql engine not configured")
	}
	return a.Engine.RegisterIntegration(provider)
}

func (a *CognitionEngineAdapter) InvokeAI(ctx context.Context, templateId string, data map[string]any) (any, error) {
	if a == nil || a.Engine == nil {
		return nil, fmt.Errorf("memql engine not configured")
	}
	return a.Engine.InvokeAI(ctx, templateId, data)
}

// InvokeAIStructured delegates to the engine's structured-output path.
// Used by the cognition AI router, prediction analyzer, and any other
// "logic" prompt that wants provider-enforced JSON shape.
func (a *CognitionEngineAdapter) InvokeAIStructured(
	ctx context.Context,
	templateId string,
	data map[string]any,
	schemaName string,
	schema json.RawMessage,
	strict bool,
) (string, error) {
	if a == nil || a.Engine == nil {
		return "", fmt.Errorf("memql engine not configured")
	}
	return a.Engine.InvokeAIStructured(ctx, templateId, data, schemaName, schema, strict)
}

// InvokeAIChatWithFilteredTools delegates to the engine's bounded
// tool-calling loop, restricted to the named tool set. Used by the
// planner integration's trainSpecialist dispatcher to drive the
// Trainer Agent's web-search + chunk-write tool loop.
func (a *CognitionEngineAdapter) InvokeAIChatWithFilteredTools(
	ctx context.Context,
	templateId string,
	data map[string]any,
	toolNames []string,
) (string, error) {
	if a == nil || a.Engine == nil {
		return "", fmt.Errorf("memql engine not configured")
	}
	return a.Engine.InvokeAIChatWithFilteredTools(ctx, templateId, data, toolNames)
}

// RunBundleDryRun delegates to the engine's Gate-2 behavioral dry-run entry
// point (memql#958). Used by the planner authoring loop's Gate-2 handoff
// (#960): it runs a Gate-1-clean bundle's automation under the tiered
// side-effect interception layer (reads real + metered, mutations isolated to
// an ephemeral sandbox partition, webhooks recorded-and-blocked) and returns
// the approval artifact. The adapter exists because RunBundleDryRun needs the
// concrete *MemQLEngine, which the planner's narrow Engine interface does not
// expose.
func (a *CognitionEngineAdapter) RunBundleDryRun(ctx context.Context, req memql.DryRunRequest) (memql.BundleDryRunReport, error) {
	if a == nil || a.Engine == nil {
		return memql.BundleDryRunReport{}, fmt.Errorf("memql engine not configured")
	}
	return memql.RunBundleDryRun(ctx, a.Engine, req)
}

// CompileBundle delegates to the engine's Gate-1 sandbox compile+bind harness
// (memql#956). Used by the planner authoring loop's emit/repair pass (#960) and
// the everyday-task capture orchestrator (#1161): it compiles + binds a
// candidate bundle's constructs in isolation (never the live engine Init path)
// and returns per-construct diagnostics. The adapter exists because
// SandboxCompileBundleWithEngine needs the concrete *MemQLEngine, which the
// planner's narrow Engine interface does not expose.
func (a *CognitionEngineAdapter) CompileBundle(constructs []memql.SandboxConstruct) memql.SandboxReport {
	if a == nil || a.Engine == nil {
		return memql.SandboxReport{OK: false}
	}
	return memql.SandboxCompileBundleWithEngine(constructs, a.Engine)
}

// CatalogNearMatches delegates to the engine's compose-first fuzzy retrieval
// (memql#957): embed a need's match text and rank the caller's cataloged
// constructs by cosine similarity. Used by the authoring design pass to decide
// reuse-vs-author. The adapter exists because CatalogNearMatches is a method on
// the concrete *MemQLEngine, which the planner's narrow Engine interface does
// not expose.
func (a *CognitionEngineAdapter) CatalogNearMatches(ctx context.Context, matchText string, limit int) ([]memql.CatalogNearMatch, error) {
	if a == nil || a.Engine == nil {
		return nil, fmt.Errorf("memql engine not configured")
	}
	return a.Engine.CatalogNearMatches(ctx, matchText, limit)
}

func (a *CognitionEngineAdapter) RenderPrompt(templateId string, data map[string]any) (string, error) {
	if a == nil || a.Engine == nil {
		return "", fmt.Errorf("memql engine not configured")
	}
	return a.Engine.RenderPrompt(templateId, data)
}

func (a *CognitionEngineAdapter) ChatStreamProvider() common.ChatStreamProvider {
	if a == nil || a.Engine == nil {
		return nil
	}
	return a.Engine.ChatStreamProvider()
}

func (a *CognitionEngineAdapter) ChatStreamProviderByName(name string) common.ChatStreamProvider {
	if a == nil || a.Engine == nil {
		return nil
	}
	return a.Engine.ChatStreamProviderByName(name)
}

func (a *CognitionEngineAdapter) ChatStreamWithToolsProviderByName(name string) common.ChatStreamWithToolsProvider {
	if a == nil || a.Engine == nil {
		return nil
	}
	return a.Engine.ChatStreamWithToolsProviderByName(name)
}

func (a *CognitionEngineAdapter) ToolDefinitionsForNames(names []string) []common.ToolDefinition {
	if a == nil || a.Engine == nil {
		return nil
	}
	return a.Engine.ToolDefinitionsForNames(names)
}

func (a *CognitionEngineAdapter) ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error) {
	if a == nil || a.Engine == nil {
		return "", fmt.Errorf("memql engine not configured")
	}
	return a.Engine.ExecuteToolByName(ctx, name, args)
}

func (a *CognitionEngineAdapter) ResolveSkills(ctx context.Context, skillIds []string) (memql.SkillBundle, error) {
	if a == nil || a.Engine == nil {
		return memql.SkillBundle{}, fmt.Errorf("memql engine not configured")
	}
	return a.Engine.ResolveSkills(ctx, skillIds)
}

// CognitionProviderAdapter wraps MemQLEngine.s provider registry to satisfy cognition.AIProviderRegistry.
type CognitionProviderAdapter struct {
	Engine *memql.MemQLEngine
}

func (a *CognitionProviderAdapter) GetProvider(name string) (any, bool) {
	if a.Engine == nil {
		return nil, false
	}
	entry, ok := a.Engine.ProviderEntry(name)
	if !ok || entry == nil || !entry.Available {
		return nil, false
	}
	return entry.Client, true
}

func (a *CognitionProviderAdapter) DefaultProvider() (any, bool) {
	if a.Engine == nil {
		return nil, false
	}
	defaultName := a.Engine.DefaultProviderName()
	if defaultName == "" {
		return nil, false
	}
	return a.GetProvider(defaultName)
}

// AttachmentEngineAdapter wraps MemQLEngine to satisfy server.MemQLExecutor.
type AttachmentEngineAdapter struct {
	Engine *memql.MemQLEngine
}

func (a *AttachmentEngineAdapter) Execute(ctx context.Context, query string) (any, error) {
	return a.Engine.Execute(ctx, query)
}

// ConvertExecuteResultToMap converts *memql.ExecuteResult to map[string]any
// so that the cognition integration can access it uniformly.
func ConvertExecuteResultToMap(result *memql.ExecuteResult) map[string]any {
	if result == nil {
		return nil
	}

	response := make(map[string]any)

	// Include shaped output data if available (from shape() queries)
	output := result.OutputPayload()
	if output != nil && output != result.Bundle {
		response["data"] = output
	}

	bundle := result.Bundle
	if bundle == nil {
		return response
	}

	bundleMap := make(map[string]any)

	// Convert nodes - MUST be []any for type assertion in extraction code
	nodes := make([]any, 0, len(bundle.Nodes))
	for _, node := range bundle.Nodes {
		if node == nil {
			continue
		}
		nodeMap := map[string]any{
			"id":      node.Id,
			"concept": node.Concept,
			"type":    node.Type,
		}

		if node.Payload != nil {
			nodeMap["payload"] = node.Payload.AsMap()
		}

		nodes = append(nodes, nodeMap)
	}
	bundleMap["nodes"] = nodes
	bundleMap["rootIds"] = bundle.RootIds

	response["bundle"] = bundleMap

	return response
}

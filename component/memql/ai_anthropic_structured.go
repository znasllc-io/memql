package memql

// Provider-enforced structured output for the Anthropic providers, via forced
// tool-use -- the native mode core/common/ai.go's interface doc names for
// Anthropic and which was never implemented. The gap was invisible while every
// structured prompt's @defaultProvider was an OpenAI model: on the first
// Claude-only cluster, InvokeAIStructured fell through to the best-effort chat
// fallback, and the model wrapped perfect JSON in a ```json fence the caller's
// json.Unmarshal then refused (prod 2026-08-26, plan 27e004720928a8f8,
// "invalid character '`'").
//
// Tool-use removes the whole failure class rather than the one symptom: the
// schema rides as the forced tool's input_schema, the model must emit exactly
// one tool_use block, and the returned JSON is that block's input verbatim --
// no prose, no fences, nothing to strip.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/znasllc-io/memql/core/common"
)

// Compile-time: both Anthropic providers now serve the structured surface, so
// StructuredChatProviderByName resolves them and the chat fallback becomes
// what it was meant to be -- a last resort, not the Anthropic path.
var (
	_ common.ChatStructuredProvider      = (*anthropicProvider)(nil)
	_ common.ChatStructuredProvider      = (*anthropicStreamProvider)(nil)
	_ common.ChatStructuredUsageProvider = (*anthropicProvider)(nil)
	_ common.ChatStructuredUsageProvider = (*anthropicStreamProvider)(nil)
)

func (p *anthropicProvider) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	content, _, err := anthropicCallStructured(ctx, p.client, p.model, p.params, messages, schema)
	return content, err
}

func (p *anthropicStreamProvider) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	content, _, err := anthropicCallStructured(ctx, p.client, p.model, p.params, messages, schema)
	return content, err
}

// The usage-reporting half (epic memql#4661). Delegating in this direction --
// the plain method calls the reporting one, never the reverse -- keeps ONE
// request builder and one error taxonomy.
func (p *anthropicProvider) CallChatStructuredWithUsage(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, common.ChatUsage, error) {
	return anthropicCallStructured(ctx, p.client, p.model, p.params, messages, schema)
}

func (p *anthropicStreamProvider) CallChatStructuredWithUsage(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, common.ChatUsage, error) {
	return anthropicCallStructured(ctx, p.client, p.model, p.params, messages, schema)
}

func anthropicCallStructured(ctx context.Context, client anthropic.Client, model string, params map[string]any, messages []common.ChatMessage, schema common.StructuredSchema) (string, common.ChatUsage, error) {
	reqParams, err := anthropicStructuredParams(model, params, messages, schema)
	if err != nil {
		return "", common.ChatUsage{}, err
	}
	resp, err := client.Messages.New(ctx, reqParams)
	if err != nil {
		return "", common.ChatUsage{}, fmt.Errorf("anthropic structured (%s): %w", schema.Name, err)
	}
	// Reported=true even when both counts are zero: the API SAID something,
	// and "the provider reported zero" is a different fact from "the provider
	// said nothing".
	//
	// Cache-creation and cache-read tokens are NOT folded into the input
	// count. They are billed at different rates, so adding them would produce
	// a number that is neither the tokens sent nor the tokens charged --
	// wrong in a way that looks precise. A caller that needs the cached
	// breakdown reads the provider's own log line.
	usage := common.ChatUsage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		Model:        string(resp.Model),
		Reported:     true,
	}
	for _, block := range resp.Content {
		if block.Type == "tool_use" && strings.TrimSpace(block.Name) == anthropicStructuredToolName(schema) {
			return strings.TrimSpace(string(block.Input)), usage, nil
		}
	}
	// The forced tool choice makes this near-impossible; when it happens
	// anyway (a refusal, an API change), the text is the only evidence.
	return "", usage, fmt.Errorf("anthropic structured (%s): no tool_use block in the reply; text was: %.200s",
		schema.Name, extractAnthropicText(resp))
}

// anthropicStructuredParams assembles the request: the conversation as usual
// (toAnthropicMessages, whose guard supplies the user turn a system-only
// invocation lacks), plus ONE tool carrying the schema, with tool choice
// FORCED to it. Split from the call so the assembly is testable without a
// live client.
func anthropicStructuredParams(model string, params map[string]any, messages []common.ChatMessage, schema common.StructuredSchema) (anthropic.MessageNewParams, error) {
	anthropicMessages, systemBlocks := toAnthropicMessages(messages)

	inputSchema := anthropic.ToolInputSchemaParam{Properties: map[string]any{}}
	if len(schema.Schema) > 0 {
		var schemaMap map[string]any
		if err := json.Unmarshal(schema.Schema, &schemaMap); err != nil {
			return anthropic.MessageNewParams{}, fmt.Errorf("anthropic structured (%s): schema is not a JSON object: %w", schema.Name, err)
		}
		if props, ok := schemaMap["properties"]; ok {
			if propsMap, ok := props.(map[string]any); ok {
				inputSchema.Properties = propsMap
			}
		}
		if req, ok := schemaMap["required"].([]any); ok {
			required := make([]string, 0, len(req))
			for _, r := range req {
				if s, ok := r.(string); ok {
					required = append(required, s)
				}
			}
			inputSchema.Required = required
		}
	}

	toolName := anthropicStructuredToolName(schema)
	description := strings.TrimSpace(schema.Description)
	if description == "" {
		description = "Produce the structured result."
	}

	maxTokens := int64(4096)
	if mt, ok := intParam(params["maxTokens"]); ok {
		maxTokens = int64(mt)
	} else if mt, ok := intParam(params["maxCompletionTokens"]); ok {
		maxTokens = int64(mt)
	}

	reqParams := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: maxTokens,
		Messages:  anthropicMessages,
		Tools: []anthropic.ToolUnionParam{{
			OfTool: &anthropic.ToolParam{
				Name:        toolName,
				Description: anthropic.String(description),
				InputSchema: inputSchema,
			},
		}},
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{
				Name:                   toolName,
				DisableParallelToolUse: anthropic.Bool(true),
			},
		},
	}
	if len(systemBlocks) > 0 {
		if boolParam(params["enablePromptCache"]) {
			last := len(systemBlocks) - 1
			systemBlocks[last].CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		reqParams.System = systemBlocks
	}
	if _, ok := params["temperature"]; ok {
		reqParams.Temperature = anthropic.Float(numberParam(params["temperature"], 1.0))
	}
	return reqParams, nil
}

// anthropicStructuredToolName maps the schema name onto Anthropic's tool-name
// grammar (^[a-zA-Z0-9_-]{1,64}$). Schema names are short identifiers like
// "agentFactoryDecision" and pass through untouched; anything else is
// sanitized rather than refused, because the name is a label, not a contract.
func anthropicStructuredToolName(schema common.StructuredSchema) string {
	name := strings.TrimSpace(schema.Name)
	if name == "" {
		name = "structured_result"
	}
	cleaned := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			cleaned = append(cleaned, r)
		default:
			cleaned = append(cleaned, '_')
		}
	}
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return string(cleaned)
}

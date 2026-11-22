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
//
// TWO WAYS TO ASK, AND THE RECORD SAYS WHICH (design record 2026-11-21, R3).
// The models current on 2026-11-22 -- Claude Opus 5.5, Sonnet 5.5 and the
// Fable 5 line -- answer a forced tool choice with a 400 ("tool_choice: type
// tool and any are not supported for this model"). For them the vendor's
// structured outputs are the documented way to extract JSON: the schema rides
// `output_config.format` and the reply's text IS the JSON. A record opts in
// with
//
//	params { structuredOutput "format" }
//
// and a record that says nothing keeps the forced tool it always sent, so no
// older model's request changes. The param is checked when the record is
// loaded (validateAnthropicStructuredParam).
//
// The format accepts a SUBSET of JSON Schema: every object closed
// (additionalProperties false), and a short list of keywords (no numeric,
// string-length or array-size limits, no pattern). The engine's schemas were
// written for the tool path, so the schema is made to fit on the way out
// (anthropicFormatSchema): only the keywords the vendor lists are kept, and
// what was removed stays in the field's description, where the model still
// reads it. The cost is that a removed limit is asked for, not enforced: a
// caller that depended on the vendor enforcing `maximum` now depends on the
// model honouring a sentence. One thing is not removed but REFUSED: an object
// the schema leaves open (a map, or an object that names no property) has no
// closed equivalent, and the call fails here, before a request is made, so the
// route's next source serves (anthropicFormatSchema).
//
// READ FROM THE VENDOR'S DOCUMENTATION, NOT RUN AGAINST IT. Nothing here was
// exercised with a real account. If the format request is wrong in a detail,
// the call fails as a vendor error and the route's next source serves.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
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
	// A DECLINE IS NOT AN ANSWER, in either mode (ai_anthropic_declined.go).
	if err := anthropicDeclined(string(resp.StopReason)); err != nil {
		return "", usage, fmt.Errorf("anthropic structured (%s): %w", schema.Name, err)
	}
	if anthropicStructuredMode(params) == anthropicStructuredByFormat {
		// NEITHER IS A REPLY CUT OFF AT THE OUTPUT LIMIT. The format constrains
		// what the model writes, not how much room it has: thinking counts
		// toward the limit on these models, and text that stops mid-object is
		// not the JSON that was asked for, whatever part of it arrived.
		if resp.StopReason == anthropic.StopReasonMaxTokens {
			return "", usage, fmt.Errorf("anthropic structured (%s): the reply was cut off at the output limit before the JSON was complete; raise the record's maxTokens or shorten the request",
				schema.Name)
		}
		texts := make([]string, 0, len(resp.Content))
		for _, block := range resp.Content {
			if block.Type == "text" {
				texts = append(texts, block.Text)
			}
		}
		if reply, ok := anthropicFormatReply(texts); ok {
			return reply, usage, nil
		}
		return "", usage, fmt.Errorf("anthropic structured (%s): the reply held no JSON; the model stopped with %q",
			schema.Name, resp.StopReason)
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
	if anthropicStructuredMode(params) == anthropicStructuredByFormat {
		return anthropicFormatParams(model, params, messages, schema)
	}
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

// The two ways a record asks for structured output.
const (
	anthropicStructuredParam = "structuredOutput"
	// anthropicStructuredByTool forces one tool whose input is the schema.
	// The default, and what every record sent before the param existed.
	anthropicStructuredByTool = "tool"
	// anthropicStructuredByFormat sends the schema as output_config.format.
	// For the models that refuse a forced tool choice.
	anthropicStructuredByFormat = "format"
)

// anthropicStructuredMode is how this record asks for structured output.
func anthropicStructuredMode(params map[string]any) string {
	if v, ok := params[anthropicStructuredParam].(string); ok &&
		strings.EqualFold(strings.TrimSpace(v), anthropicStructuredByFormat) {
		return anthropicStructuredByFormat
	}
	return anthropicStructuredByTool
}

// validateAnthropicStructuredParam refuses a `structuredOutput` the provider
// would not act on: a value that is neither word, or the param on a record
// that is not an Anthropic text model. A misspelled value would otherwise fall
// back to the forced tool, which the models this param exists for refuse.
func validateAnthropicStructuredParam(cfg ProviderConfig) error {
	raw, declared := cfg.Params[anthropicStructuredParam]
	if !declared {
		return nil
	}
	if !isVendorTextRecord(cfg) || vendorOfType(cfg.Type) != VendorAnthropic {
		return fmt.Errorf("provider %q: params.%s is set on a record that is not a text model of Anthropic, which is the only kind that reads it. Remove it",
			cfg.Name, anthropicStructuredParam)
	}
	word, _ := raw.(string)
	switch strings.ToLower(strings.TrimSpace(word)) {
	case anthropicStructuredByTool, anthropicStructuredByFormat:
		return nil
	}
	return fmt.Errorf("provider %q: params.%s must be %q or %q", cfg.Name, anthropicStructuredParam,
		anthropicStructuredByTool, anthropicStructuredByFormat)
}

// anthropicFormatParams assembles a structured request the vendor's way: the
// conversation, and the schema as the format of the reply. No tool and no
// tool choice ride it.
func anthropicFormatParams(model string, params map[string]any, messages []common.ChatMessage, schema common.StructuredSchema) (anthropic.MessageNewParams, error) {
	fitted, err := anthropicFormatSchema(schema.Schema)
	if err != nil {
		return anthropic.MessageNewParams{}, fmt.Errorf("anthropic structured (%s): %w", schema.Name, err)
	}
	anthropicMessages, systemBlocks := toAnthropicMessages(messages)

	maxTokens := int64(4096)
	if mt, ok := intParam(params["maxTokens"]); ok {
		maxTokens = int64(mt)
	} else if mt, ok := intParam(params["maxCompletionTokens"]); ok {
		maxTokens = int64(mt)
	}
	reqParams := anthropic.MessageNewParams{
		Model:        anthropic.Model(model),
		MaxTokens:    maxTokens,
		Messages:     anthropicMessages,
		OutputConfig: anthropic.OutputConfigParam{Format: anthropic.JSONOutputFormatParam{Schema: fitted}},
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

// anthropicFormatReply picks the structured answer out of a reply's text
// blocks: the first that holds anything.
func anthropicFormatReply(texts []string) (string, bool) {
	for _, text := range texts {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return trimmed, true
		}
	}
	return "", false
}

// formatKept are the schema keywords the vendor's structured outputs document
// as accepted. The fit is a list of what is KEPT, not of what is refused: the
// vendor's page names what it accepts, and a keyword nobody listed is a 400
// waiting for the first schema that uses it. Everything else is removed and
// kept in the field's description, where the model still reads it.
//
// `additionalProperties` is not here because it is not carried: every object
// is closed. `oneOf` is not here because it is carried as `anyOf`, which is
// what the vendor's own SDKs do with it.
var formatKept = map[string]bool{
	"$defs": true, "$ref": true, "allOf": true, "anyOf": true, "const": true,
	"definitions": true, "description": true, "enum": true, "items": true,
	"properties": true, "required": true, "title": true, "type": true,
}

// formatStringFormats are the string formats the vendor accepts.
var formatStringFormats = map[string]bool{
	"date-time": true, "time": true, "date": true, "duration": true, "email": true,
	"hostname": true, "uri": true, "ipv4": true, "ipv6": true, "uuid": true,
}

// anthropicFormatSchema returns a copy of a schema made to fit what the
// vendor's structured outputs accept: every object closed, and only the
// keywords the vendor lists. The caller's schema is not touched.
//
// AN OPEN OBJECT IS REFUSED, NOT CLOSED. The format takes closed objects only.
// An object the caller left open on purpose -- a map of one type
// (`additionalProperties: {schema}`), a map of anything (`true`), an object
// that names no property at all -- has no closed equivalent: closing it makes
// the vendor enforce an EMPTY object, and the caller is handed `{}` by a call
// recorded as served. So it is an error here, naming the field, before any
// request is made, and the route's next source serves the call. An object that
// names its properties and says nothing about extras is closed, which is what
// its author meant and what the vendor's own SDKs do with it.
//
// NOT FIXED HERE: a schema that refers to itself. The vendor refuses a
// recursive schema and there is no equivalent to send in its place, so such a
// call fails as a vendor error and the route's next source serves.
func anthropicFormatSchema(raw json.RawMessage) (map[string]any, error) {
	schema := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &schema); err != nil {
			return nil, fmt.Errorf("schema is not a JSON object: %w", err)
		}
	}
	if len(schema) == 0 {
		return nil, openObjectError(formatRootName)
	}
	if err := fitFormatSchema(schema, formatRootName); err != nil {
		return nil, err
	}
	return schema, nil
}

// formatRootName is how an error names the schema's own top object.
const formatRootName = "the schema"

func openObjectError(where string) error {
	return fmt.Errorf("the object at %s is open: it takes properties the schema does not name, and this model's structured output takes closed objects only", where)
}

// schemaIsObject reports whether a node describes an object: by its type
// (alone or among several) or by carrying properties.
func schemaIsObject(node map[string]any) bool {
	if _, ok := node["properties"].(map[string]any); ok {
		return true
	}
	switch t := node["type"].(type) {
	case string:
		return t == "object"
	case []any:
		for _, one := range t {
			if one == "object" {
				return true
			}
		}
	}
	return false
}

// fitFormatSchema fits one node in place. where names it for an error: the
// property or definition it sits under, or the schema itself.
func fitFormatSchema(node map[string]any, where string) error {
	if branches, ok := node["oneOf"].([]any); ok {
		if _, taken := node["anyOf"]; !taken {
			node["anyOf"] = branches
			delete(node, "oneOf")
		}
	}

	// Open or closed, decided before anything is removed.
	if schemaIsObject(node) {
		extras, said := node["additionalProperties"]
		props, _ := node["properties"].(map[string]any)
		switch {
		case said && extras != false:
			return openObjectError(where)
		case !said && len(props) == 0:
			return openObjectError(where)
		}
	}

	var removed []string
	for keyword, value := range node {
		switch {
		case formatKept[keyword]:
			continue
		case keyword == "additionalProperties":
			// Set below for every object, never described: it is the format's
			// own rule and says nothing the model needs.
			delete(node, keyword)
			continue
		case keyword == "format":
			if word, _ := value.(string); formatStringFormats[word] {
				continue
			}
		case keyword == "minItems":
			if n, ok := value.(float64); ok && (n == 0 || n == 1) {
				continue
			}
		}
		delete(node, keyword)
		removed = append(removed, fmt.Sprintf("%s %s", keyword, formatValue(value)))
	}
	if len(removed) > 0 {
		sort.Strings(removed)
		note := strings.Join(removed, ", ")
		if desc, _ := node["description"].(string); strings.TrimSpace(desc) != "" {
			node["description"] = strings.TrimSpace(desc) + " (" + note + ")"
		} else {
			node["description"] = note
		}
	}

	if schemaIsObject(node) {
		node["additionalProperties"] = false
	}
	// Children in name order, so the first refusal named is the same one on
	// every replica.
	if props, ok := node["properties"].(map[string]any); ok {
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if m, ok := props[name].(map[string]any); ok {
				if err := fitFormatSchema(m, strconv.Quote(name)); err != nil {
					return err
				}
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		if err := fitFormatSchema(items, where); err != nil {
			return err
		}
	}
	for _, keyword := range []string{"anyOf", "allOf"} {
		if list, ok := node[keyword].([]any); ok {
			for _, child := range list {
				if m, ok := child.(map[string]any); ok {
					if err := fitFormatSchema(m, where); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, keyword := range []string{"$defs", "definitions"} {
		if defs, ok := node[keyword].(map[string]any); ok {
			names := make([]string, 0, len(defs))
			for name := range defs {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if m, ok := defs[name].(map[string]any); ok {
					if err := fitFormatSchema(m, "the definition "+strconv.Quote(name)); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// formatValue writes a removed keyword's value the way a person would read it
// in a description: a word or a number bare, anything larger as JSON.
func formatValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64, bool, nil:
		return fmt.Sprint(v)
	}
	if b, err := json.Marshal(value); err == nil {
		return string(b)
	}
	return fmt.Sprint(value)
}

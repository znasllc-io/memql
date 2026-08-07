package memql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/znasllc-io/memql/component/safety"
	"github.com/znasllc-io/memql/core/common"
)

// remainingPlaceholderRegex matches a `$args.<identifier>` placeholder
// the substituter passes left untouched (because the LLM omitted that
// arg key). Used by substituteArgsInMemqlQuery's clean-up pass to
// stamp those holes with `null` so the rendered query stays
// syntactically valid for the MemQL parser.
//
// This is defensive only -- typed dispatch (handler type=function)
// is the right architecture for tools authored with nested args. The
// type=query path stays for legacy / simple cases where string
// substitution is fine.
var remainingPlaceholderRegex = regexp.MustCompile(`\$args\.[a-zA-Z_][a-zA-Z0-9_]*`)

// toolSchemaCache stores compiled JSON-Schema validators keyed by
// tool name. The InputSchema on a Tool is small (a few KB of JSON);
// recompiling on every dispatch is wasteful. Compiling once at
// first dispatch + caching is fine because tool definitions are
// immutable once loaded -- cluster reload replaces the engine
// entirely.
//
// Concurrent map access is guarded by sync.Map. Cache misses go
// through compileToolSchema which does the actual compile under
// santhosh-tekuri/jsonschema's own concurrency guarantees.
var toolSchemaCache sync.Map // map[string]*jsonschema.Schema

// validateToolArgs runs the parsed LLM tool-call args through the
// tool's declared @input JSON Schema (compiled lazily; cached).
// Returns nil on success; a structured error string suitable for
// returning to the LLM as a tool-result on failure ("missing
// required field 'args'", "field 'requestedScope' must be one of
// observe / interact / full", etc.). The LLM can read these and
// retry with corrected args.
//
// Before validating, runs coerceArgsToSchema -- a small-model
// resilience layer that normalises common shape mistakes (a
// JSON-stringified object passed as a property declared `object`,
// for instance). Without coercion, chat54Mini and similar small
// models burn dispatch attempts cycling through string vs object
// representations of the same nested args.
//
// Tools without a compiled schema (no @input declaration, or
// schema compile error at load time) skip validation -- the dispatch
// path runs unchanged and the integration handler can do its own
// per-tool checks.
func (e *MemQLEngine) validateToolArgs(tool *Tool, args map[string]any) error {
	if tool == nil || len(tool.InputSchema) == 0 {
		return nil
	}
	schema, err := compileToolSchema(tool)
	if err != nil {
		// Compile failure is logged but not fatal -- the tool's
		// own dispatch handler runs anyway. Validating against a
		// half-compiled schema would be worse than skipping.
		if e != nil && e.Component != nil && e.Logger != nil {
			e.Logger.Warn("tool schema compile failed; skipping validation",
				"tool", tool.Name, "error", err)
		}
		return nil
	}
	if schema == nil {
		return nil
	}
	// Normalise common LLM-side shape errors before validating
	// against the strict schema. This is in-place: args is the same
	// map the caller (streaming.go) hands to ExecuteTool, so the
	// downstream dispatch sees the coerced shape too. Without this,
	// validation rejects + the LLM-facing error fires; with it,
	// recoverable shape mistakes (object encoded as JSON string,
	// number as numeric string) become valid before the validator
	// runs. Strict cases that can't be auto-coerced (wrong enum
	// value, wholly-missing required field) still surface to the
	// LLM as before.
	coerceArgsToSchema(tool.InputSchema, args)

	// jsonschema validates against encoding/json-native types:
	// map[string]any / []any / string / float64 / bool / nil.
	// parseToolArgs already returns this shape.
	var argsForValidation any
	if args == nil {
		argsForValidation = map[string]any{}
	} else {
		argsForValidation = args
	}
	if err := schema.Validate(argsForValidation); err != nil {
		// memql#3182. This validator is compiled from the SAME ArgsSchema that
		// carries FunctionArgsField.Secret, so every declaration the function
		// validator redacts (memql#3036) has a generated-tool twin here -- and
		// this one runs FIRST on the agent path (ExecuteTool calls it before
		// the handler), so it is the surface a rejected secret actually
		// reaches. Both exits below carry caller data and both are redacted:
		//
		//   - the RETURNED message is handed to the model verbatim by
		//     ExecuteTool (IsError: true -> tool result text), and the
		//     jsonschema keywords the generated schema emits interpolate the
		//     instance value ("must be >= %v but found %v", "%v is not valid
		//     %s").
		//   - the WARN below serialized the ENTIRE args map, not just the
		//     rejected field -- so values that PASSED validation were logged
		//     too.
		secretNames, classified := e.toolSecretArgNames(tool)
		redactedMessage := redactSecretArgValues(err.Error(), secretNames, args)
		if e != nil && e.Component != nil && e.Logger != nil {
			// Log the structured failure so we can diagnose why the LLM's
			// call didn't match the schema, plus what was actually sent.
			// The LLM-facing message is shorter; the operator gets the
			// fuller picture here.
			//
			// WARN DECISION (memql#3182 asks which of the two options this
			// takes, and why): redact PER KEY from the ArgsSchema and KEEP
			// the args attribute -- but only when the schema could actually
			// be consulted. When it could not, the attribute degrades to a
			// KEY LIST instead of being logged raw.
			//
			// Reason. Dropping args outright would cost the attribute its
			// whole reason for existing: "which args did the model send"
			// is the first question asked of a tool-dispatch failure, and
			// the non-secret args are the answer. Per-key redaction keeps
			// that and removes exactly the values that must not be in a log.
			// The key-list fallback is the honest handling of "cannot
			// classify": a log line is durable and ships to an aggregator,
			// so an args map nothing has vouched for does not go into one.
			// The keys still say which arguments were supplied, which is
			// most of the diagnostic value, and the error attribute still
			// names the offending instance location and the constraint it
			// failed.
			attrs := []any{"tool", tool.Name}
			if classified {
				attrs = append(attrs, "args", marshalArgsForLog(redactArgsForLog(args, secretNames)))
			} else {
				attrs = append(attrs, "argKeys", sortedArgKeys(args))
			}
			attrs = append(attrs, "error", redactedMessage)
			e.Logger.Warn("tool args validation failed", attrs...)
		}
		return formatToolValidationError(tool.Name, redactedMessage)
	}
	return nil
}

// toolSecretArgNames resolves the argument names this tool's arguments are
// declared @secret on, plus whether the declaration could be consulted AT ALL
// (memql#3182).
//
// The Secret flag lives on FunctionArgsField, stamped once at load by
// markSecretArgsFields from the bound concept's @secret fields. Reading it
// from the function registry at the point of use -- rather than copying the
// names onto the Tool at generation time -- keeps ONE source for the flag,
// with no copy that can drift from the declaration it was derived from. It
// also covers an explicitly-authored `tool` whose @handler names the same
// function, not just the auto-registered twin registerFunctionTools produces.
//
// The second return distinguishes "consulted, no secret args" (true, empty)
// from "could not consult" (false): a tool with no function handler, an engine
// with no function registry, a function the registry cannot resolve. The
// caller treats the two differently -- the message path fails OPEN on an
// unclassified tool (matching memql#3036's posture: a diagnostic must not
// start over-redacting because a registry lookup missed), the log path fails
// CLOSED (see the WARN decision above).
func (e *MemQLEngine) toolSecretArgNames(tool *Tool) (map[string]struct{}, bool) {
	if e == nil || e.functions == nil || tool == nil || tool.Handler == nil {
		return nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(tool.Handler.Type), "function") {
		return nil, false
	}
	name := strings.TrimSpace(tool.Handler.FunctionName)
	if name == "" {
		return nil, false
	}
	fn, err := e.functions.Get(name)
	if err != nil || fn == nil {
		return nil, false
	}
	return secretArgNamesFromSchema(fn.ArgsSchema), true
}

// secretArgNamesFromSchema collects every args-field name carrying the Secret
// flag, including nested-object and array-item fields (markSecretArgsFields
// stamps those by name too).
func secretArgNamesFromSchema(schema *ArgsSchemaConfig) map[string]struct{} {
	names := map[string]struct{}{}
	if schema == nil {
		return names
	}
	for _, field := range schema.Fields {
		collectSecretArgNames(field, names)
	}
	return names
}

func collectSecretArgNames(field *FunctionArgsField, out map[string]struct{}) {
	if field == nil {
		return
	}
	if field.Secret {
		if n := strings.TrimSpace(field.Name); n != "" {
			out[n] = struct{}{}
		}
	}
	for _, nested := range field.Nested {
		collectSecretArgNames(nested, out)
	}
	collectSecretArgNames(field.Items, out)
}

// redactSecretArgValues removes the VALUE of every secret argument from a
// validation message, leaving everything that makes the message actionable --
// which argument failed and which declared constraint it failed -- intact. A
// declared constraint (a minimum, an enum's members, a format name) comes from
// the schema, never from caller data, so it is never redacted.
//
// It scrubs by value rather than by parsing the jsonschema message, because
// the set of keywords that interpolate the instance value is the LIBRARY's to
// change: v5 does it in minimum / exclusiveMinimum / maximum /
// exclusiveMaximum / multipleOf / format today, and a keyword added tomorrow
// is covered here without anyone noticing it needed to be. Matching on the
// value is what makes that true.
//
// Over-redaction is the deliberate failure direction. A secret whose rendering
// is a short numeral can also blank an unrelated numeral in the same message
// (a minimum of 10 against a secret value of 1). That reads oddly and never
// discloses, which is the correct trade for a diagnostic.
func redactSecretArgValues(message string, secretNames map[string]struct{}, args map[string]any) string {
	if message == "" || len(secretNames) == 0 || len(args) == 0 {
		return message
	}
	seen := map[string]struct{}{}
	collectSecretRenderings(args, secretNames, false, seen)
	renderings := make([]string, 0, len(seen))
	for rendering := range seen {
		if rendering == "" || rendering == redactedArgValue {
			continue
		}
		renderings = append(renderings, rendering)
	}
	// Longest first, so a quoted rendering is consumed whole rather than left
	// as an orphaned pair of quotes around the placeholder. Sorting also makes
	// the output deterministic, which map iteration would not.
	sort.Slice(renderings, func(i, j int) bool {
		if len(renderings[i]) != len(renderings[j]) {
			return len(renderings[i]) > len(renderings[j])
		}
		return renderings[i] < renderings[j]
	})
	for _, rendering := range renderings {
		message = strings.ReplaceAll(message, rendering, redactedArgValue)
	}
	return message
}

// collectSecretRenderings walks the args tree and records every textual form a
// secret value could have been interpolated as. `inSecret` propagates down a
// secret-keyed subtree: an object or array held by a secret argument is secret
// in every leaf.
func collectSecretRenderings(value any, secretNames map[string]struct{}, inSecret bool, out map[string]struct{}) {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			_, keyIsSecret := secretNames[key]
			collectSecretRenderings(item, secretNames, inSecret || keyIsSecret, out)
		}
	case []any:
		for _, item := range v {
			collectSecretRenderings(item, secretNames, inSecret, out)
		}
	default:
		if !inSecret || value == nil {
			return
		}
		// The renderings jsonschema can produce for an instance value:
		// bare %v (numbers, booleans), single-quoted (its quote() helper for
		// strings in the format message), Go-quoted, and %#v.
		out[fmt.Sprintf("%v", value)] = struct{}{}
		if s, ok := value.(string); ok {
			out[strconv.Quote(s)] = struct{}{}
			out["'"+s+"'"] = struct{}{}
		}
		out[fmt.Sprintf("%#v", value)] = struct{}{}
	}
}

// redactArgsForLog returns a copy of the args map with every secret-argument
// value replaced by the redaction placeholder, so the WARN keeps its
// debugging value without carrying a credential into a durable log.
func redactArgsForLog(args map[string]any, secretNames map[string]struct{}) map[string]any {
	if len(args) == 0 {
		return args
	}
	out := make(map[string]any, len(args))
	for key, value := range args {
		if _, secret := secretNames[key]; secret {
			out[key] = redactedArgValue
			continue
		}
		switch v := value.(type) {
		case map[string]any:
			out[key] = redactArgsForLog(v, secretNames)
		case []any:
			items := make([]any, len(v))
			for i, item := range v {
				if nested, ok := item.(map[string]any); ok {
					items[i] = redactArgsForLog(nested, secretNames)
					continue
				}
				items[i] = item
			}
			out[key] = items
		default:
			out[key] = value
		}
	}
	return out
}

// marshalArgsForLog renders the redacted args map for the WARN attribute with
// HTML escaping OFF. json.Marshal would emit the placeholder as
// "<redacted>", which is the one thing an operator scanning this
// line needs to recognise at a glance.
func marshalArgsForLog(args map[string]any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}

// sortedArgKeys is the values-free form of the args attribute: which
// arguments the caller supplied, and nothing about what was in them.
func sortedArgKeys(args map[string]any) []string {
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// coerceArgsToSchema mutates `args` in-place to align common
// small-model shape mistakes with the tool's declared @input
// JSON Schema. Specifically:
//
//   - If the schema declares a property as `object` and the LLM
//     supplied a string that parses as a JSON object, replace the
//     string with the parsed map. chat54Mini and other small
//     models repeatedly send `args: "{\"cmd\":\"...\"}"` (an
//     escaped string) instead of `args: {cmd: "..."}` (a nested
//     object); without this coercion, validation rejects each
//     attempt and the model loops through 8+ retries on the same
//     misshape, exhausting the iteration budget without ever
//     dispatching the work.
//
//   - Same for `array`-typed properties whose value is a string
//     parseable as a JSON array.
//
//   - Number properties whose value is a numeric string get
//     parsed too -- another frequent small-model mistake.
//
// Strict mismatches that can't be safely auto-coerced (object
// vs number, wrong enum value, missing required fields) are left
// alone so the validator still surfaces them to the LLM as
// concrete errors.
//
// Schema is the json.RawMessage form (uncompiled, since we just
// need to read property types); we re-decode on every call which
// is cheap relative to the AI dispatch happening downstream.
// Falls through silently when the schema doesn't carry usable
// `properties`.
func coerceArgsToSchema(schemaJSON []byte, args map[string]any) {
	if len(schemaJSON) == 0 || args == nil {
		return
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return
	}
	for name, raw := range props {
		propSchema, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		expectedType, _ := propSchema["type"].(string)
		current, present := args[name]
		if !present {
			continue
		}
		strVal, isString := current.(string)
		if !isString {
			continue
		}
		trimmed := strings.TrimSpace(strVal)
		if trimmed == "" {
			continue
		}
		switch expectedType {
		case "object":
			// Only coerce if the string parses as a JSON object
			// (not array / scalar). Defensive: if the string
			// happens to parse as an array, leaving it alone
			// keeps validation honest about the type mismatch.
			var parsed map[string]any
			if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
				args[name] = parsed
			}
		case "array":
			var parsed []any
			if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
				args[name] = parsed
			}
		case "number", "integer":
			var parsed float64
			if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
				args[name] = parsed
			}
		case "boolean":
			var parsed bool
			if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
				args[name] = parsed
			}
		}
	}
}

// compileToolSchema resolves (compile-once + cache) the JSON Schema
// for a tool's @input block. Returns nil schema when the tool has
// no InputSchema declared. Identical pattern to the prompt schema
// compile in ai_prompts.go, just keyed by tool name instead of
// prompt name.
func compileToolSchema(tool *Tool) (*jsonschema.Schema, error) {
	if tool == nil || len(tool.InputSchema) == 0 {
		return nil, nil
	}
	if cached, ok := toolSchemaCache.Load(tool.Name); ok {
		if cached == nil {
			return nil, nil
		}
		return cached.(*jsonschema.Schema), nil
	}
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2019
	resourceId := "tool:" + tool.Name
	if err := compiler.AddResource(resourceId, bytes.NewReader(tool.InputSchema)); err != nil {
		return nil, fmt.Errorf("add resource: %w", err)
	}
	compiled, err := compiler.Compile(resourceId)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	toolSchemaCache.Store(tool.Name, compiled)
	return compiled, nil
}

// formatToolValidationError renders a jsonschema validation message
// into a single-paragraph string the LLM can read and react to.
//
// It takes the message TEXT rather than the error so the only thing that
// reaches this function is the text the caller has already run through
// redactSecretArgValues (memql#3182). Handing it the error instead would
// leave one call away from returning err.Error() unredacted, which is the
// shape the leak had.
//
// jsonschema's Error() output is multi-line + path-prefixed
// ("jsonschema: ” does not validate ..." \n "  at '/foo': required
// field missing"). Strategy:
//  1. Walk the lines, skip empty + the leading "jsonschema:" header.
//  2. Take the FIRST real rule failure (most specific concrete
//     reason) and stamp the tool name on it.
//  3. Strip the leading "- " bullet that jsonschema adds for
//     nested rule failures.
//
// Result example:
//
//	`tool "requestComputerUseScope": invalid arguments: at '': missing property 'intent'`
func formatToolValidationError(toolName string, msg string) error {
	if strings.TrimSpace(msg) == "" {
		return nil
	}
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip the outer "jsonschema: ... does not validate with ..."
		// header line -- it carries the resource URL but not the
		// concrete rule. The lines AFTER it are the rule failures.
		if strings.HasPrefix(line, "jsonschema:") {
			continue
		}
		return fmt.Errorf("tool %q: invalid arguments: %s", toolName, line)
	}
	// Defensive: if every line looked like a header, fall back to
	// the raw message so the LLM at least sees something.
	return fmt.Errorf("tool %q: invalid arguments: %s", toolName, strings.TrimSpace(msg))
}

// ExecuteTool executes a tool definition with the provided arguments.
// This is the shared execution path for internal callers (cognition) and gRPC.
//
// Tools are an agent-only invocation surface. Every entry point that
// reaches this function MUST stamp an acting-agent role onto ctx
// (via WithActingAgentRole). Callers that need to do non-agent work
// at the engine layer should use queries, mutations, or integration
// capabilities directly -- not tools. This boundary keeps "tools"
// meaningful as the orchestration surface agents reason about and
// makes the role-based AllowedRoles gate load-bearing on every path.
//
// The enforcement is a hard reject (returns an error) rather than a
// log-only warning so non-agent callers fail loudly during development
// instead of silently bypassing the contract.
func (e *MemQLEngine) ExecuteTool(ctx context.Context, tool *Tool, args map[string]any) (*ToolCallResult, error) {
	if tool == nil {
		return nil, fmt.Errorf("tool is nil")
	}

	// Central auto-injection chokepoint (memql#107 / memql#1503). Merge the
	// server-provided ToolDefaults from ctx over the (LLM-supplied) args so
	// every dispatch path -- the streaming/non-streaming tool loops AND the
	// realtime-voice CallTool proxy hop (component/grpc handleCallTool, which
	// does NOT pre-merge) -- stamps the tool's @autoInjected fields
	// (ownerUserId / partitionId / agentId / ...). For @autoInjected fields the
	// server default ALWAYS wins (forged LLM values dropped); other defaults
	// are fill-if-missing. Idempotent for callers that already pre-merged
	// (ai_tool_loop.go), so it is safe defense-in-depth there too.
	args = applyToolDefaults(ctx, tool, args, common.ToolDefaultsFromContext(ctx))

	// Universal agent-only enforcement. Without an acting-agent role
	// on ctx, the call is by definition not from an agent and the
	// tool layer rejects it. The role string is what AllowedRoles
	// downstream checks against; absence of a role here means there
	// is no agent at all.
	callerRole := ActingAgentRoleFromContext(ctx)
	if callerRole == "" {
		return nil, fmt.Errorf("tool %q: tools are agent-only -- no acting agent on context. Use queries, mutations, or integration capabilities for non-agent paths", tool.Name)
	}
	if !tool.IsAllowedForRole(callerRole) {
		return nil, fmt.Errorf("tool %q is not allowed for caller role %q", tool.Name, callerRole)
	}

	// Client-executed tools: the tool body lives in the connected
	// client (browser / app). Delegate to the ClientToolInvoker
	// carried on ctx -- typically the BFF streamSession that owns the
	// originating request. The invoker handles the ClientToolCall <->
	// ClientToolResult round-trip. Role gate already enforced above.
	if tool.ClientExecution {
		invoker := ClientToolInvokerFromContext(ctx)
		if invoker == nil {
			return nil, fmt.Errorf("tool %q requires client execution but no ClientToolInvoker is attached to the context", tool.Name)
		}
		argsJSON := "{}"
		if len(args) > 0 {
			if b, err := json.Marshal(args); err == nil {
				argsJSON = string(b)
			}
		}
		return invoker.InvokeClientTool(ctx, tool.Name, argsJSON, callerRole)
	}

	// If no handler is defined, return tool info as a text response.
	if tool.Handler == nil {
		return &ToolCallResult{
			Content: []ToolResultContent{
				{Type: "text", Text: fmt.Sprintf("Tool %q has no handler defined", tool.Name)},
			},
		}, nil
	}

	// Schema-validate the parsed args against the tool's @input
	// JSON Schema BEFORE dispatching. Two wins:
	//   1. Malformed calls (missing required fields, wrong type,
	//      enum violation) get rejected with a structured message
	//      the LLM can read and correct on retry.
	//   2. The integration handler's own input checks become
	//      defense-in-depth, not the primary error surface --
	//      cleaner separation of "args fit the contract" from
	//      "the action itself failed."
	// Validation errors flow back as a tool result with
	// isError=true so the streaming tool-loop hands the message
	// to the LLM as the tool's "response," same shape as a
	// dispatcher-side error. The LLM iterates with the corrected
	// args without ever leaving the tool-calling loop.
	if validationErr := e.validateToolArgs(tool, args); validationErr != nil {
		return &ToolCallResult{
			IsError: true,
			Content: []ToolResultContent{
				{Type: "text", Text: validationErr.Error()},
			},
		}, nil
	}

	handler := tool.Handler

	// Safety classifier (memql#229). Runs AFTER args are schema-valid
	// (so the classifier sees the same args the handler will) and
	// BEFORE any dispatch -- query / function / webhook / delegate
	// all flow past this gate.
	//
	// Surface picks the right blast-radius class: webhook handlers
	// fire outbound HTTP; everything else is engine-internal. The
	// rule layer's URL rules only fire on http_fetch/webhook
	// actions, but every tool gets recorded in shadow mode so the
	// telemetry stays uniform across handler types.
	//
	// Fail-OPEN on classifier error here. Per-handler validation +
	// the dispatch path's own checks are the primary surface; the
	// worker pre-dispatch gate is the fail-closed one (highest
	// blast radius). #231's decision policy supplies the real
	// allow/ask/deny matrix; until then decide() returns Allow and
	// this path is observation-only.
	toolSurface := safety.SurfaceToolIntegration
	if strings.EqualFold(strings.TrimSpace(handler.Type), "webhook") {
		toolSurface = safety.SurfaceToolWebhook
	}
	toolDesc := safety.NewToolAction(toolSurface, tool.Name, args, safety.CallerContext{
		Capability: callerRole,
	})
	toolGate := safety.DefaultGate()
	decision, cls, classErr := toolGate.Evaluate(ctx, toolDesc)
	// #235: per-surface fail-closed posture via env override.
	// Default for tool surfaces stays fail-OPEN (sandboxed handlers
	// + own validation).
	failClosed := safety.FailClosedForSurface(toolSurface)
	if proceed, reason := toolGate.EnforceDecision(toolSurface, decision, cls, classErr, failClosed); !proceed {
		return &ToolCallResult{
			IsError: true,
			Content: []ToolResultContent{
				{Type: "text", Text: fmt.Sprintf("tool %q: %s", tool.Name, reason)},
			},
		}, nil
	}

	switch strings.ToLower(strings.TrimSpace(handler.Type)) {
	case "query":
		query := strings.TrimSpace(handler.Query)
		if query == "" {
			return nil, fmt.Errorf("query handler has no query defined")
		}
		if args != nil {
			query = substituteArgsInMemqlQuery(query, args)
		}
		result, err := e.Execute(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("query execution failed: %w", err)
		}
		text, err := executeResultToToolJSON(result)
		if err != nil {
			return nil, err
		}
		return &ToolCallResult{Content: []ToolResultContent{{Type: "text", Text: text}}}, nil

	case "function":
		name := strings.TrimSpace(handler.FunctionName)
		if name == "" {
			return nil, fmt.Errorf("function handler has no function name defined")
		}

		query, err := buildFunctionCallQuery(name, args)
		if err != nil {
			return nil, err
		}
		result, err := e.Execute(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("function execution failed: %w", err)
		}
		text, err := executeResultToToolJSON(result)
		if err != nil {
			return nil, err
		}
		return &ToolCallResult{Content: []ToolResultContent{{Type: "text", Text: text}}}, nil

	case "webhook":
		return e.executeWebhook(ctx, tool.Name, handler, args)

	default:
		return nil, fmt.Errorf("unknown handler type %q", handler.Type)
	}
}

// executeWebhook performs an HTTP request for a webhook-type tool handler.
func (e *MemQLEngine) executeWebhook(ctx context.Context, toolName string, handler *ToolHandler, args map[string]any) (*ToolCallResult, error) {
	resolvedURL := strings.TrimSpace(handler.URL)
	if resolvedURL == "" {
		return nil, fmt.Errorf("webhook handler for tool %q has no URL", toolName)
	}

	// Substitute $args.key placeholders in URL.
	if args != nil {
		resolvedURL = substituteArgsInQuery(resolvedURL, args)
	}

	// Validate host against allowlist.
	if err := e.validateWebhookHost(resolvedURL); err != nil {
		return nil, fmt.Errorf("webhook tool %q: %w", toolName, err)
	}

	method := strings.TrimSpace(handler.Method)
	if method == "" {
		method = "POST"
	}

	// Build request body.
	var bodyReader io.Reader
	if method == "POST" || method == "PUT" || method == "PATCH" {
		var bodyData any
		if handler.Body != nil {
			// Use explicit body template with $args substitution.
			bodyData = substituteArgsInBody(handler.Body, args)
		} else if args != nil {
			// No explicit body template: send all args as the request body.
			bodyData = args
		}
		if bodyData != nil {
			bodyBytes, err := json.Marshal(bodyData)
			if err != nil {
				return nil, fmt.Errorf("webhook tool %q: marshal body: %w", toolName, err)
			}
			bodyReader = bytes.NewReader(bodyBytes)
		}
	}

	// Create request with timeout context.
	timeout := time.Duration(e.config.WebhookTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(defaultWebhookTimeoutSeconds) * time.Second
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(timeoutCtx, method, resolvedURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("webhook tool %q: create request: %w", toolName, err)
	}

	// Set headers.
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range handler.Headers {
		req.Header.Set(k, v)
	}

	// Execute request.
	resp, err := webhookHTTPClient.Do(req)
	if err != nil {
		return &ToolCallResult{
			Content: []ToolResultContent{{Type: "text", Text: fmt.Sprintf("webhook request failed: %v", err)}},
			IsError: true,
		}, nil
	}
	defer resp.Body.Close()

	// Read response (cap at 1MB to prevent memory issues).
	const maxResponseSize = 1 << 20
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return &ToolCallResult{
			Content: []ToolResultContent{{Type: "text", Text: fmt.Sprintf("webhook read response failed: %v", err)}},
			IsError: true,
		}, nil
	}

	text := string(respBody)
	if resp.StatusCode >= 400 {
		return &ToolCallResult{
			Content: []ToolResultContent{{Type: "text", Text: fmt.Sprintf("webhook returned status %d: %s", resp.StatusCode, text)}},
			IsError: true,
		}, nil
	}

	return &ToolCallResult{
		Content: []ToolResultContent{{Type: "text", Text: text}},
	}, nil
}

// validateWebhookHost gates the webhook URL's host. An explicit allowlist is
// the operator escape valve (an approved host:port is always permitted). On top
// of that, an SSRF default-deny floor blocks any non-allowlisted host that
// targets an internal endpoint (loopback / link-local incl. cloud metadata /
// private / mDNS) -- even when NO allowlist is configured, so the webhook tool
// can't be used to pivot to internal services once the engine is network-
// exposed (the MCP node, memql#1561). With no allowlist, remaining public hosts
// are permitted; with an allowlist, a non-allowlisted public host is rejected.
func (e *MemQLEngine) validateWebhookHost(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}

	// Explicit allowlist wins: an operator-approved host:port is permitted even
	// if it resolves to a private address (a deliberately-wired internal target).
	host := parsed.Host
	for _, allowed := range e.config.WebhookAllowedHosts {
		if strings.EqualFold(host, allowed) {
			return nil
		}
	}

	// SSRF default-deny floor (memql#1561): blocks loopback / link-local
	// (incl. 169.254.169.254) / private / mDNS targets whether or not an
	// allowlist is configured. Allowlist a specific internal host above to
	// override.
	if internal, reason := safety.IsInternalNetworkHost(parsed.Hostname()); internal {
		return fmt.Errorf("webhook host %q blocked: %s", parsed.Hostname(), reason)
	}

	// No allowlist configured -> any remaining (public) host is permitted.
	if len(e.config.WebhookAllowedHosts) == 0 {
		return nil
	}
	// Allowlist configured and the public host is not on it.
	return fmt.Errorf("host %q not in allowed webhook hosts", host)
}

// substituteArgsInBody recursively substitutes $args.key references in a body template map.
func substituteArgsInBody(body map[string]any, args map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	result := make(map[string]any, len(body))
	for k, v := range body {
		result[k] = substituteArgValue(v, args)
	}
	return result
}

// substituteArgValue recursively substitutes $args references in a value.
func substituteArgValue(v any, args map[string]any) any {
	switch val := v.(type) {
	case string:
		return substituteArgsInQuery(val, args)
	case map[string]any:
		return substituteArgsInBody(val, args)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = substituteArgValue(item, args)
		}
		return out
	default:
		return v
	}
}

// webhookHTTPClient is a shared HTTP client for webhook tool calls.
// The per-request timeout is applied via context, not the client.
var webhookHTTPClient = &http.Client{
	Timeout: time.Duration(maxWebhookTimeoutSeconds) * time.Second,
}

func buildFunctionCallQuery(name string, args map[string]any) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("function name is required")
	}
	if len(args) == 0 {
		return name + "()", nil
	}

	// Story 9 (#2335): emit the named-args invocation form `name(k: v, ...)`,
	// not the legacy object-literal wrapper `name({...})` (now rejected by the
	// parser). Keys sorted for a deterministic call string; values JSON-encoded
	// so the engine parser handles escaping. flattenPositionalArgs normalises
	// both forms downstream, so this is behavior-preserving.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('(')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		v, err := json.Marshal(args[k])
		if err != nil {
			return "", fmt.Errorf("function args json encode failed: %w", err)
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.Write(v)
	}
	b.WriteByte(')')
	return b.String(), nil
}

// substituteArgsInQuery replaces $args.<key> references with raw
// string values inside a templated string. Used for the WEBHOOK URL
// + body-template paths where the placeholder is already inside a
// host-language string (URL path segment, JSON field value), so we
// substitute the raw stringified value with NO extra quoting.
//
// For MemQL query-handler text -- where the placeholder is inside
// MemQL source that gets re-parsed -- use substituteArgsInMemqlQuery
// instead; that path JSON-encodes string values so quotes / newlines
// in the value don't break the downstream parser. See the doc on
// substituteArgsInMemqlQuery for the per-context rationale.
func substituteArgsInQuery(query string, args map[string]any) string {
	if args == nil {
		return query
	}
	result := query
	for key, value := range args {
		placeholder := "$args." + key
		var replacement string
		switch v := value.(type) {
		case string:
			replacement = v
		case float64:
			if v == float64(int64(v)) {
				replacement = fmt.Sprintf("%d", int64(v))
			} else {
				replacement = fmt.Sprintf("%v", v)
			}
		case bool:
			replacement = fmt.Sprintf("%t", v)
		default:
			if jsonBytes, err := json.Marshal(v); err == nil {
				replacement = string(jsonBytes)
			} else {
				replacement = fmt.Sprintf("%v", v)
			}
		}
		result = strings.ReplaceAll(result, placeholder, replacement)
	}
	return result
}

// substituteArgsInMemqlQuery replaces $args.<key> placeholders inside
// a MemQL query-handler template with values that the engine's MemQL
// parser will re-parse. Two surrounding contexts have to be handled:
//
//  1. **Pre-quoted form** -- `"$args.key"`. Tool handlers wrap string
//     placeholders this way (e.g. `summary: "$args.summary"`). The
//     intent is "substitute this string into a MemQL string-literal
//     position." We REPLACE THE WHOLE QUOTED TOKEN with a
//     JSON-encoded value -- json.Marshal already adds the outer
//     quotes AND escapes inner quotes, backslashes, control chars,
//     etc. The previous implementation just dropped the raw value
//     between the existing surrounding quotes, so a value containing
//     a `"` or a backslash broke the lexer downstream
//     ("expected ',' between arguments at position 378"). The
//     scope-elevation `summary` field surfaced this because it's the
//     first long natural-language string the LLM generates as a tool
//     arg.
//
//  2. **Bare form** -- `$args.key` without surrounding quotes. Used
//     when the placeholder is in a non-string position (e.g.
//     `args: $args.args` where `args` is a nested object). Substitute
//     the JSON-encoded value directly; for objects / arrays the
//     produced JSON is valid MemQL too.
//
// Both passes are stable: the pre-quoted pass runs first and replaces
// the whole `"$args.key"` token, so the second pass finds no matching
// `$args.key` in the surviving text.
//
// Webhook URL / body templating uses substituteArgsInQuery instead --
// those callers want the raw stringified value (no extra JSON
// quoting) because the placeholder is already inside the host
// language's string slot.
func substituteArgsInMemqlQuery(query string, args map[string]any) string {
	result := query
	if args != nil {
		for key, value := range args {
			encoded := encodeForMemqlSubstitution(value)
			quotedPlaceholder := `"$args.` + key + `"`
			barePlaceholder := "$args." + key
			result = strings.ReplaceAll(result, quotedPlaceholder, encoded)
			result = strings.ReplaceAll(result, barePlaceholder, encoded)
		}
	}
	// Clean up any remaining `$args.<name>` placeholders the LLM
	// didn't fill in. Without this pass, an omitted optional arg
	// (planId / taskId / correlationId on workerHost; any tool with
	// optional fields really) leaves the literal `$args.foo` in the
	// rendered query, and the MemQL parser fails on the bare `$`
	// token ("unexpected token \"$\" at position 48"). Also covers
	// the pre-quoted form: `"$args.foo"` collapses to `null` (an
	// unquoted JSON null), which the parser accepts in any
	// expression position the original quoted-string slot allowed.
	// Same for embedded substring placeholders (`"prefix-$args.foo"`)
	// -- the regex is anchored to the placeholder shape only, so it
	// surgically removes just the `$args.x` part. Result: missing
	// args degrade to null instead of crashing the dispatch.
	result = remainingPlaceholderRegex.ReplaceAllStringFunc(result, func(_ string) string {
		return "null"
	})
	// Sweep up any now-empty `""` introduced by replacing
	// pre-quoted `"$args.foo"` (which became `"null"` after the
	// inner replacement only happens if the pre-quoted form
	// matches; the regex above would have NOT matched once the
	// outer quotes were already there because `$args.foo` is
	// surrounded by `"`). Net effect of the regex pass on a
	// pre-quoted unfilled placeholder `"$args.foo"`: becomes
	// `"null"`. That parses as the string literal `"null"`, which
	// is acceptable behavior for missing string args -- the engine
	// will see the literal four-char string.
	return result
}

// encodeForMemqlSubstitution renders a tool-arg value as a MemQL-safe
// literal (always JSON-encoded -- strings are quoted + escaped,
// numbers / bools / objects / arrays use their canonical JSON form).
func encodeForMemqlSubstitution(value any) string {
	switch v := value.(type) {
	case nil:
		return `""`
	case string:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%q", v)
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%v", v)
	case bool:
		return fmt.Sprintf("%t", v)
	default:
		if jsonBytes, err := json.Marshal(v); err == nil {
			return string(jsonBytes)
		}
		return fmt.Sprintf("%v", v)
	}
}

func executeResultToToolJSON(result *ExecuteResult) (string, error) {
	if result == nil {
		return "{}", nil
	}
	bundle, data, err := result.ToAPIResult()
	if err != nil {
		return "", err
	}
	// The LLM tool loop is a seam toward a foreign brain: bare-ify so the
	// model sees + hands back BARE shortIds (inbound bare args resolve via
	// A1). ToAPIResult itself stays canonical for in-process callers
	// (#2441 seam 4).
	bundle = WireBareifyBundle(bundle)
	data = WireBareifyData(data)
	output := map[string]any{}
	if bundle != nil {
		nodes := make([]map[string]any, 0, len(bundle.Nodes))
		for _, node := range bundle.Nodes {
			nodeMap := map[string]any{
				"id":      node.Id,
				"concept": node.Concept,
			}
			if node.Payload != nil {
				nodeMap["payload"] = node.Payload.AsMap()
			}
			nodes = append(nodes, nodeMap)
		}
		output["nodes"] = nodes
	}
	if len(data) > 0 {
		dataSlice := make([]any, 0, len(data))
		for _, d := range data {
			dataSlice = append(dataSlice, d.AsInterface())
		}
		output["data"] = dataSlice
	}
	jsonBytes, err := json.Marshal(output)
	if err != nil {
		return "", err
	}
	return string(jsonBytes), nil
}

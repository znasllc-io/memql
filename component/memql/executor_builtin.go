package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/docs"
)

func (e *MemQLEngine) initBuiltinExecutorHandlers() error {
	handlers := map[string]builtinExecutorHandler{
		BuiltinExecutorConcepts: func(ctx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error) {
			return e.evaluateConceptsExpression(ctx, target, args)
		},
		BuiltinExecutorMemqlDocs: func(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return e.evaluateDocsExpression(ctx)
		},
		BuiltinExecutorValidate: func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return e.evaluateValidateExpression(ctx, args)
		},
		BuiltinExecutorFunctions: func(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return e.evaluateFunctionsExpression(ctx)
		},
		BuiltinExecutorTools: func(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return e.evaluateToolsExpression(ctx)
		},
		BuiltinExecutorHelp: func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return e.evaluateHelpExpression(ctx, args)
		},
		BuiltinExecutorShapeTemplates: func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return e.evaluateShapeTemplatesExpression(ctx, args)
		},
		BuiltinExecutorShapeHelp: func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return e.evaluateShapeHelpExpression(ctx, args)
		},
		BuiltinExecutorContentId: func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return e.evaluateContentIdExpression(ctx, args)
		},
		BuiltinExecutorPreviewInsert: func(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return e.evaluatePreviewInsertExpression(ctx, args)
		},
		BuiltinExecutorServiceVersion: func(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return e.evaluateServiceVersionExpression(ctx)
		},
		BuiltinExecutorError: func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
			return nil, builtinErrorFromArgs(args)
		},
	}

	if e.functions != nil {
		for _, fn := range e.functions.Snapshot() {
			if fn == nil || !fn.IsBuiltin() {
				continue
			}
			// A @disabled builtin skips executor validation (#2608 review):
			// retiring a builtin whose Go executor is gone is the single
			// most likely reason to disable one, and must not brick boot.
			if !fn.Enabled {
				continue
			}
			// Integration capability executors (integration.*) are registered
			// dynamically via RegisterIntegration after engine startup.
			if strings.HasPrefix(fn.Executor, "integration.") {
				continue
			}
			if _, ok := handlers[fn.Executor]; !ok {
				return fmt.Errorf("builtin function %q declares unknown executor %q", fn.Name, fn.Executor)
			}
		}
	}
	e.builtinExecutorHandlers = handlers
	return nil
}

// evaluateBuiltinFunctionExpression dispatches to the appropriate executor for builtin functions.
func (e *MemQLEngine) evaluateBuiltinFunctionExpression(ctx context.Context, expr *BuiltinFunctionExpression, target int) ([]memorynodes.MemoryNode, error) {
	if expr == nil {
		return nil, fmt.Errorf("builtin function expression is nil")
	}
	if e.builtinExecutorHandlers == nil {
		if err := e.initBuiltinExecutorHandlers(); err != nil {
			return nil, err
		}
	}
	handler, ok := e.builtinExecutorHandlers[expr.Executor]
	if !ok || handler == nil {
		return nil, fmt.Errorf("unknown builtin executor %q", expr.Executor)
	}
	nodes, err := handler(ctx, expr.Args, target)
	if err != nil {
		return nodes, err
	}

	// PreserveOrder: the handler already ordered its result slice and
	// doesn't want the downstream nodesToMap -> mapToSortedSlice pass to
	// shuffle it. We stamp monotonically decreasing CreatedAt on each
	// returned node so the default "sort by CreatedAt DESC" fallback in
	// mapToSortedSlice reproduces the handler's slice order. A caller
	// that applies an explicit sort() directive still wins; this only
	// restores the expected behaviour in the common default-sort case.
	// See IntegrationCapability.PreserveOrder.
	if e.builtinPreserveOrder != nil && e.builtinPreserveOrder[expr.Executor] && len(nodes) > 1 {
		base := time.Now().UTC()
		for i := range nodes {
			nodes[i].CreatedAt = base.Add(-time.Duration(i) * time.Nanosecond)
		}
	}

	return nodes, nil
}

func (e *MemQLEngine) evaluateServiceVersionExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}

	schemaBytes, err := json.Marshal(map[string]any{
		"description": "Current memQL service version.",
	})
	if err != nil {
		return nil, err
	}

	payloadBytes, err := json.Marshal(map[string]any{
		"version": e.currentServiceVersion(),
	})
	if err != nil {
		return nil, err
	}

	node := memorynodes.MemoryNode{
		ID:        "memql:version",
		Concept:   "memql:version",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: actor,
		Schema:    schemaBytes,
		Payload:   payloadBytes,
	}

	return []memorynodes.MemoryNode{node}, nil
}

func builtinErrorFromArgs(args map[string]any) error {
	if args == nil {
		return fmt.Errorf("%w: error() requires a message", ErrInvalidArgument)
	}
	raw, ok := args["message"]
	if !ok {
		return fmt.Errorf("%w: error() requires a message", ErrInvalidArgument)
	}
	message, ok := raw.(string)
	if !ok || strings.TrimSpace(message) == "" {
		return fmt.Errorf("error() message must be a non-empty string")
	}
	return fmt.Errorf("%s", message)
}

// evaluateDocsExpression returns embedded MemQL documentation as queryable nodes.
func (e *MemQLEngine) evaluateDocsExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}

	schemaBytes, err := json.Marshal(map[string]any{
		"description": "Embedded MemQL reference documentation.",
	})
	if err != nil {
		return nil, err
	}

	payloadBytes, err := json.Marshal(map[string]any{
		"format":  "markdown",
		"content": docs.MemqlGuide(),
	})
	if err != nil {
		return nil, err
	}

	node := memorynodes.MemoryNode{
		ID:        "memql:docs",
		Concept:   "memql:docs",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: actor,
		Schema:    schemaBytes,
		Payload:   payloadBytes,
	}

	return []memorynodes.MemoryNode{node}, nil
}

// evaluateValidateExpression validates a payload against a concept's JSON schema.
// Returns a single memory node containing the validation result with:
// - valid: boolean indicating if validation passed
// - errors: array of validation error objects (empty if valid)
// - schema: the JSON schema used for validation
// - required: array of required field names from the schema
// - provided: array of field names present in the payload
func (e *MemQLEngine) evaluateValidateExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e.concepts == nil {
		return nil, fmt.Errorf("concept registry is not initialized")
	}

	// Extract concept name from args
	conceptName, ok := args["concept"].(string)
	if !ok || conceptName == "" {
		return nil, fmt.Errorf("%w: validate() requires 'concept' field as a string", ErrInvalidArgument)
	}

	// Extract payload from args
	payload, ok := args["payload"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: validate() requires 'payload' field as an object", ErrInvalidArgument)
	}

	// Look up the concept
	concept, err := e.concepts.Get(conceptName)
	if err != nil {
		return nil, fmt.Errorf("concept %q not found: %w", conceptName, err)
	}

	// Get the definition schema
	schemaRaw, err := concept.DefinitionSchema()
	if err != nil {
		return nil, fmt.Errorf("failed to get schema for concept %q: %w", conceptName, err)
	}

	// Get required fields from the schema (sorted for deterministic output)
	requiredFields := concept.RequiredFields()
	sort.Strings(requiredFields)

	// Normalize payload BEFORE validation (same as insert/previewInsert behavior)
	// This strips reserved fields so validation matches insert() behavior
	normalizedPayload := normalizePayloadForId(payload)

	// Get provided fields from normalized payload (after stripping reserved fields)
	providedFields := make([]string, 0, len(normalizedPayload))
	for key := range normalizedPayload {
		providedFields = append(providedFields, key)
	}
	sort.Strings(providedFields)

	// Compile and validate the schema
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2019

	// Use hyphen-separated ID to avoid URL parsing issues with colons in concept names
	schemaId := fmt.Sprintf("validate://%s/definition", strings.ReplaceAll(conceptName, ":", "-"))
	if err := compiler.AddResource(schemaId, strings.NewReader(string(schemaRaw))); err != nil {
		return nil, fmt.Errorf("failed to compile schema for concept %q: %w", conceptName, err)
	}

	schema, err := compiler.Compile(schemaId)
	if err != nil {
		return nil, fmt.Errorf("failed to compile schema for concept %q: %w", conceptName, err)
	}

	// Perform validation on normalized payload
	validationErr := schema.Validate(normalizedPayload)

	// Build the result
	var validationErrors []map[string]any
	isValid := validationErr == nil

	if !isValid {
		// Type assert to get detailed error information
		if ve, ok := validationErr.(*jsonschema.ValidationError); ok {
			basicOutput := ve.BasicOutput()
			for _, basicErr := range basicOutput.Errors {
				validationErrors = append(validationErrors, map[string]any{
					"instanceLocation": basicErr.InstanceLocation,
					"keywordLocation":  basicErr.KeywordLocation,
					"error":            basicErr.Error,
				})
			}
		} else {
			// Fallback for unexpected error types
			validationErrors = append(validationErrors, map[string]any{
				"error": validationErr.Error(),
			})
		}
	}

	// Build result payload
	resultPayload := map[string]any{
		"valid":    isValid,
		"errors":   validationErrors,
		"required": requiredFields,
		"provided": providedFields,
	}

	// Include schema summary (not full schema to keep response compact)
	var schemaSummary map[string]any
	if err := json.Unmarshal(schemaRaw, &schemaSummary); err == nil {
		// Extract key schema info for the response
		schemaInfo := map[string]any{
			"$id": schemaSummary["$id"],
		}
		if props, ok := schemaSummary["properties"].(map[string]any); ok {
			propNames := make([]string, 0, len(props))
			for name := range props {
				propNames = append(propNames, name)
			}
			sort.Strings(propNames)
			schemaInfo["properties"] = propNames
		}
		resultPayload["schema"] = schemaInfo
	}

	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}

	resultSchemaBytes, err := json.Marshal(map[string]any{
		"description": "Validation result from validate() builtin.",
	})
	if err != nil {
		return nil, err
	}

	payloadBytes, err := json.Marshal(resultPayload)
	if err != nil {
		return nil, err
	}

	node := memorynodes.MemoryNode{
		ID:        fmt.Sprintf("memql:validate:%s", conceptName),
		Concept:   "memql:validate",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: actor,
		Schema:    resultSchemaBytes,
		Payload:   payloadBytes,
	}

	return []memorynodes.MemoryNode{node}, nil
}

// evaluateFunctionsExpression returns a minimal list of all registered functions.
// Per RFC: returns name+description only to minimize payload size for agent discovery.
func (e *MemQLEngine) evaluateFunctionsExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	if e.functions == nil {
		return nil, fmt.Errorf("function registry is not initialized")
	}

	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}

	snapshot := e.functions.Snapshot()
	if snapshot == nil {
		snapshot = make(map[string]*Function)
	}

	// Build minimal function list (name + description only)
	functionList := make([]map[string]any, 0, len(snapshot))
	names := make([]string, 0, len(snapshot))
	for name := range snapshot {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		fn := snapshot[name]
		if fn == nil || !fn.Enabled {
			continue
		}
		functionList = append(functionList, map[string]any{
			"name":        fn.Name,
			"description": fn.Description,
			"kind":        fn.FunctionKind,
		})
	}

	resultPayload := map[string]any{
		"functions": functionList,
		"count":     len(functionList),
	}

	schemaBytes, err := json.Marshal(map[string]any{
		"description": "Minimal function list from functions() builtin.",
	})
	if err != nil {
		return nil, err
	}

	payloadBytes, err := json.Marshal(resultPayload)
	if err != nil {
		return nil, err
	}

	node := memorynodes.MemoryNode{
		ID:        "memql:functions",
		Concept:   "memql:functions",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: actor,
		Schema:    schemaBytes,
		Payload:   payloadBytes,
	}

	return []memorynodes.MemoryNode{node}, nil
}

// evaluateToolsExpression returns MCP-compatible tool definitions.
// Per RFC: returns name, description, and inputSchema for AI model compatibility.
func (e *MemQLEngine) evaluateToolsExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	if e.tools == nil {
		return nil, fmt.Errorf("tool registry is not initialized")
	}

	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}

	toolList := e.tools.List()

	// Build MCP-compatible tool list (name + description + inputSchema)
	mcpTools := make([]map[string]any, 0, len(toolList))
	for _, tool := range toolList {
		if tool == nil {
			continue
		}
		mcpTool := map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
		}
		// Include inputSchema if present
		if len(tool.InputSchema) > 0 {
			var schema any
			if err := json.Unmarshal(tool.InputSchema, &schema); err == nil {
				mcpTool["inputSchema"] = schema
			}
		}
		mcpTools = append(mcpTools, mcpTool)
	}

	resultPayload := map[string]any{
		"tools": mcpTools,
		"count": len(mcpTools),
	}

	schemaBytes, err := json.Marshal(map[string]any{
		"description": "MCP-compatible tool list from tools() builtin.",
	})
	if err != nil {
		return nil, err
	}

	payloadBytes, err := json.Marshal(resultPayload)
	if err != nil {
		return nil, err
	}

	node := memorynodes.MemoryNode{
		ID:        "memql:tools",
		Concept:   "memql:tools",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: actor,
		Schema:    schemaBytes,
		Payload:   payloadBytes,
	}

	return []memorynodes.MemoryNode{node}, nil
}

// evaluateHelpExpression returns full details for a specific function or tool.
// Searches functions first, then tools. Returns all metadata and configuration.
// functionHelpPayload builds the describeFunction/help introspection payload for
// a named function. Its `inputSchema` is rendered by FunctionInputJSONSchema --
// the SAME single source of truth the @mcp typed-wrapper surface uses
// (memql#1713) -- so describeFunction and the typed MCP wrappers can never
// advertise a divergent contract. The legacy flat `argsSchema` list is retained
// for back-compat (also derived from the same fields) but the authoritative
// machine-readable contract is `inputSchema`.
func functionHelpPayload(fn *Function) map[string]any {
	if fn == nil {
		return nil
	}
	payload := map[string]any{
		"type":        "function",
		"name":        fn.Name,
		"description": fn.Description,
		"kind":        fn.FunctionKind,
		"enabled":     fn.Enabled,
	}
	// Add optional fields if present
	if fn.Version != "" {
		payload["version"] = fn.Version
	}
	if fn.Deprecated != "" {
		payload["deprecated"] = fn.Deprecated
	}
	if fn.Timeout != "" {
		payload["timeout"] = fn.Timeout
	}
	if fn.CacheTTL != "" {
		payload["cacheTTL"] = fn.CacheTTL
	}
	if fn.ArgsSchema != nil && len(fn.ArgsSchema.Fields) > 0 {
		argsSchema := make([]map[string]any, 0, len(fn.ArgsSchema.Fields))
		for _, field := range fn.ArgsSchema.Fields {
			argsSchema = append(argsSchema, map[string]any{
				"name":     field.Name,
				"type":     field.Type,
				"optional": field.Optional,
			})
		}
		payload["argsSchema"] = argsSchema
	}
	// inputSchema is the authoritative JSON-Schema contract, generated from the
	// single source of truth shared with the @mcp typed wrappers. Always present
	// (an args-less function yields {"type":"object"}).
	payload["inputSchema"] = FunctionInputJSONSchema(fn)
	if strings.TrimSpace(fn.UsageDoc) != "" {
		payload["usageDoc"] = strings.TrimSpace(fn.UsageDoc)
	}
	// Canonical query result envelope (memql#1710): every query returns its rows
	// as an array regardless of cardinality -- [] for no match, [x] for a single
	// match, [x, y, ...] for many. A single match is NEVER unwrapped to a bare
	// object, so callers iterate one shape.
	if strings.EqualFold(strings.TrimSpace(fn.FunctionKind), "query") {
		payload["resultEnvelope"] = "array: query results are always a JSON array (0 results -> [], 1 -> [x], many -> [x, y, ...]); a single match is never collapsed to a bare object"
	}
	if excerpt := strings.TrimSpace(functionSchemaReferenceExcerpt()); excerpt != "" {
		payload["schemaReferenceExcerpt"] = excerpt
	}
	if fn.Idempotent {
		payload["idempotent"] = true
	}
	if fn.Audit {
		payload["audit"] = true
	}
	return payload
}

func (e *MemQLEngine) evaluateHelpExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	name, ok := args["name"].(string)
	if !ok || name == "" {
		return nil, fmt.Errorf("%w: help() requires 'name' argument", ErrInvalidArgument)
	}

	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}

	var resultPayload map[string]any

	// Try functions first
	if e.functions != nil {
		if fn, err := e.functions.Get(name); err == nil && fn != nil {
			resultPayload = functionHelpPayload(fn)
		}
	}

	// Try tools if not found in functions
	if resultPayload == nil && e.tools != nil {
		if tool, err := e.tools.Get(name); err == nil && tool != nil {
			resultPayload = map[string]any{
				"type":        "tool",
				"name":        tool.Name,
				"description": tool.Description,
			}
			// Include inputSchema
			if len(tool.InputSchema) > 0 {
				var schema any
				if err := json.Unmarshal(tool.InputSchema, &schema); err == nil {
					resultPayload["inputSchema"] = schema
				}
			}
			// Include handler info
			if tool.Handler != nil {
				resultPayload["handlerType"] = tool.Handler.Type
			}
			// Include annotations
			if tool.Annotations != nil {
				annotations := map[string]any{}
				if tool.Annotations.Destructive {
					annotations["destructive"] = true
				}
				if tool.Annotations.RequiresConfirmation {
					annotations["requiresConfirmation"] = true
				}
				if tool.Annotations.ExecutionTime != "" {
					annotations["executionTime"] = tool.Annotations.ExecutionTime
				}
				if len(annotations) > 0 {
					resultPayload["annotations"] = annotations
				}
			}
		}
	}

	if resultPayload == nil {
		return nil, fmt.Errorf("no function or tool found with name %q", name)
	}

	schemaBytes, err := json.Marshal(map[string]any{
		"description": "Full details from help() builtin.",
	})
	if err != nil {
		return nil, err
	}

	payloadBytes, err := json.Marshal(resultPayload)
	if err != nil {
		return nil, err
	}

	node := memorynodes.MemoryNode{
		ID:        fmt.Sprintf("memql:help:%s", name),
		Concept:   "memql:help",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: actor,
		Schema:    schemaBytes,
		Payload:   payloadBytes,
	}

	return []memorynodes.MemoryNode{node}, nil
}

func (e *MemQLEngine) evaluateConceptsExpression(ctx context.Context, target int, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e.concepts == nil {
		return nil, fmt.Errorf("concept registry is not initialized")
	}

	// Extract optional pattern filter
	var pattern string
	if args != nil {
		if p, ok := args["pattern"].(string); ok {
			pattern = p
		}
	}

	list := e.concepts.List()
	sort.Slice(list, func(i, j int) bool {
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})

	// Filter by pattern if provided
	if pattern != "" {
		filtered := make([]*memorynodes.Concept, 0, len(list))
		patternLower := strings.ToLower(pattern)
		for _, c := range list {
			if c == nil {
				continue
			}
			// Match if pattern is contained in concept name (case-insensitive)
			if strings.Contains(strings.ToLower(c.Name), patternLower) {
				filtered = append(filtered, c)
			}
		}
		list = filtered
	}

	max := len(list)
	if target > 0 && target < max {
		max = target
	}

	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	schemaBytes, err := json.Marshal(map[string]any{
		"description": "Concept metadata exposed via concepts().",
	})
	if err != nil {
		return nil, err
	}

	nodes := make([]memorynodes.MemoryNode, 0, max)
	for idx := 0; idx < max; idx++ {
		c := list[idx]
		if c == nil {
			continue
		}

		payload := e.buildConceptPayload(c)
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}

		node := memorynodes.MemoryNode{
			ID:        fmt.Sprintf("memql:concept:%s", c.Name),
			Concept:   "memql:concept",
			Type:      memorynodes.NodeTypeObject,
			CreatedAt: now,
			CreatedBy: actor,
			Schema:    schemaBytes,
			Payload:   payloadBytes,
		}
		nodes = append(nodes, node)
	}

	return nodes, nil
}

func (e *MemQLEngine) evaluateShapeTemplatesExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e.shapes == nil {
		return nil, fmt.Errorf("shape registry is not initialized")
	}

	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}

	_ = args // concept-filter argument retired with the @concepts feature
	shapes := e.shapes.List()

	// Build minimal shape list (name + description only)
	shapeList := make([]map[string]any, 0, len(shapes))
	for _, shape := range shapes {
		shapeList = append(shapeList, map[string]any{
			"name":        shape.Name,
			"description": shape.Description,
		})
	}

	payloadBytes, err := json.Marshal(map[string]any{
		"shapes": shapeList,
		"count":  len(shapeList),
	})
	if err != nil {
		return nil, err
	}

	node := memorynodes.MemoryNode{
		ID:        "memql:shapeTemplates",
		Concept:   "memql:shapeTemplates",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: actor,
		Payload:   payloadBytes,
	}

	return []memorynodes.MemoryNode{node}, nil
}

// evaluateShapeHelpExpression returns full details for a shape template by name.
func (e *MemQLEngine) evaluateShapeHelpExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e.shapes == nil {
		return nil, fmt.Errorf("shape registry is not initialized")
	}

	if args == nil {
		return nil, fmt.Errorf("%w: shapeHelp() requires arguments (args is nil)", ErrInvalidArgument)
	}

	name, ok := args["name"].(string)
	if !ok || name == "" {
		return nil, fmt.Errorf("%w: shapeHelp() requires 'name' argument (got args: %v)", ErrInvalidArgument, args)
	}

	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}

	shape, found := e.shapes.Get(name)
	if !found {
		return nil, fmt.Errorf("shape %q not found", name)
	}

	// Build structured result with full template
	result := map[string]any{
		"name":        shape.Name,
		"description": shape.Description,
		"template":    shape.Template,
	}

	payloadBytes, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}

	node := memorynodes.MemoryNode{
		ID:        "memql:shapeHelp:" + name,
		Concept:   "memql:shapeHelp",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: actor,
		Payload:   payloadBytes,
	}

	return []memorynodes.MemoryNode{node}, nil
}

// evaluateContentIdExpression predicts the content-addressed ID for a concept+payload.
func (e *MemQLEngine) evaluateContentIdExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}

	conceptName, _ := args["concept"].(string)
	payload, _ := args["payload"].(map[string]any)

	// Build result - use error codes for consistency with previewInsert
	if conceptName == "" {
		return preflightResultNode(actor, map[string]any{
			"valid":  false,
			"error":  "MISSING_REQUIRED_FIELD",
			"target": "concept",
		})
	}

	concept, err := e.concepts.Get(conceptName)
	if err != nil {
		return preflightResultNode(actor, map[string]any{
			"valid":  false,
			"error":  "CONCEPT_NOT_FOUND",
			"target": conceptName,
		})
	}

	// Normalize payload same as insert(): clone and strip reserved fields
	normalizedPayload := normalizePayloadForId(payload)
	id := concept.DeriveContentId(normalizedPayload)

	return preflightResultNode(actor, map[string]any{
		"valid":   true,
		"id":      id,
		"concept": conceptName,
	})
}

// evaluatePreviewInsertExpression validates payload, predicts ID, and checks existence.
func (e *MemQLEngine) evaluatePreviewInsertExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	actor, err := mutationActor(ctx)
	if err != nil {
		return nil, err
	}

	conceptName, _ := args["concept"].(string)
	payload, _ := args["payload"].(map[string]any)

	// Validate concept exists
	if conceptName == "" {
		return preflightResultNode(actor, map[string]any{
			"valid":  false,
			"error":  "MISSING_REQUIRED_FIELD",
			"target": "concept",
		})
	}

	concept, err := e.concepts.Get(conceptName)
	if err != nil {
		return preflightResultNode(actor, map[string]any{
			"valid":  false,
			"error":  "CONCEPT_NOT_FOUND",
			"target": conceptName,
		})
	}

	// Validate against schema (same approach as validate() function)
	schemaRaw, err := concept.DefinitionSchema()
	if err != nil {
		return preflightResultNode(actor, map[string]any{
			"valid":  false,
			"error":  "SCHEMA_ERROR",
			"target": conceptName,
		})
	}

	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2019
	// Use hyphen-separated ID to avoid URL parsing issues with colons in concept names
	schemaId := fmt.Sprintf("preflight://%s/definition", strings.ReplaceAll(conceptName, ":", "-"))
	if err := compiler.AddResource(schemaId, strings.NewReader(string(schemaRaw))); err != nil {
		return preflightResultNode(actor, map[string]any{
			"valid":  false,
			"error":  "SCHEMA_ERROR",
			"target": conceptName,
		})
	}

	schema, err := compiler.Compile(schemaId)
	if err != nil {
		return preflightResultNode(actor, map[string]any{
			"valid":  false,
			"error":  "SCHEMA_ERROR",
			"target": conceptName,
		})
	}

	// Normalize payload BEFORE validation (same as insert behavior)
	// This strips reserved fields so validation doesn't fail on them
	normalizedPayload := normalizePayloadForId(payload)

	validationErr := schema.Validate(normalizedPayload)
	if validationErr != nil {
		var validationErrors []map[string]any
		if ve, ok := validationErr.(*jsonschema.ValidationError); ok {
			basicOutput := ve.BasicOutput()
			for _, basicErr := range basicOutput.Errors {
				validationErrors = append(validationErrors, map[string]any{
					"instanceLocation": basicErr.InstanceLocation,
					"keywordLocation":  basicErr.KeywordLocation,
					"error":            basicErr.Error,
				})
			}
		} else {
			validationErrors = append(validationErrors, map[string]any{
				"error": validationErr.Error(),
			})
		}
		return preflightResultNode(actor, map[string]any{
			"valid":   false,
			"error":   "SCHEMA_VALIDATION_FAILED",
			"details": validationErrors,
		})
	}

	// Derive content ID using the already-normalized payload
	id := concept.DeriveContentId(normalizedPayload)

	// Check if exists
	exists := e.checkNodeExists(ctx, conceptName, id)

	return preflightResultNode(actor, map[string]any{
		"valid":    true,
		"id":       id,
		"exists":   exists,
		"warnings": []string{},
	})
}

// normalizePayloadForId clones the payload and strips reserved fields to match insert() behavior.
// This ensures contentId() and previewInsert() predict the same ID as insert().
func normalizePayloadForId(payload map[string]any) map[string]any {
	if len(payload) == 0 {
		return map[string]any{}
	}
	// Clone to avoid mutating input
	cloned := make(map[string]any, len(payload))
	for k, v := range payload {
		cloned[k] = v
	}
	// Strip reserved fields same as insert()
	return memorynodes.StripReservedPayloadFields(cloned)
}

// preflightResultNode creates a result node for preflight functions.
func preflightResultNode(actor string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	node := memorynodes.MemoryNode{
		ID:        "memql:preflight",
		Concept:   "memql:preflight",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		CreatedBy: actor,
		Payload:   payloadBytes,
	}

	return []memorynodes.MemoryNode{node}, nil
}

// checkNodeExists checks if a node with the given concept and ID exists.

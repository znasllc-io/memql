package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component"
	"github.com/znasllc-io/memql/component/bus"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/provenance"
	"github.com/znasllc-io/memql/core/common"
)

// MemQLEngine is the default implementation of the MemQLEngine interface.
type MemQLEngine struct {
	*component.Component
	relationships relationshipRegistry
	concepts      concept.Registry
	specs         *SpecRegistry
	functions     *FunctionRegistry
	tools         *ToolRegistry
	prompts       *PromptRegistry
	agents          *AgentRegistry
	seeds           *SeedRegistry
	seedMaterializer *SeedMaterializer
	providers       *ProviderRegistry
	policies      *PolicyRegistry
	// policyFunctions holds parsed `func (Policy)` definitions
	// (cross-cutting decision policies under dsl/v1/policies/
	// {core,bff}/...). Separate from `policies` which holds the
	// older SI Router routing-policy block syntax under
	// dsl/v1/policies/v1/.
	policyFunctions *PolicyFunctionRegistry
	// configSnapshot is the bus-distributed ConfigSnapshot that
	// backs ctx.config.* inside policy bodies. Optional; nil
	// resolves every allow-listed key to its zero value (sensitive
	// -> false, non-sensitive -> ""). Wired via SetConfigSnapshot
	// from app bootstrap.
	configSnapshot interface{} // *busv1.ConfigSnapshot, kept as interface{} to avoid an import cycle through engine.go itself.
	shapes         *ShapeRegistry
	schemaIdx     *schemaIndex
	db            *bun.DB
	dbGetter      func() *bun.DB // Function that returns current DB (handles reconnection)
	dbMu          sync.RWMutex
	initialized   bool
	config        engineConfig
	siCacheConfig siCacheConfig
	cache         *resultCache
	siRuntime     *siRuntime
	eventBus      *events.Bus
	serviceVersion string
	builtinExecutorHandlers map[string]builtinExecutorHandler
	// builtinPreserveOrder lists the FQNs (integration.X.Y) whose
	// handlers return pre-ordered slices. Populated alongside
	// builtinExecutorHandlers at registration time; read from the
	// dispatch path to decide whether to stamp monotonic CreatedAt on
	// the returned nodes. See IntegrationCapability.PreserveOrder.
	builtinPreserveOrder    map[string]bool
	integrations            *IntegrationRegistry
	wiring                  *bus.Wiring
	partition               string // active partition for data isolation
	metadataCollector       metadataCollectorInterface
	// logicRunner wires multi-step Logic dispatch through the
	// automation step runner. Set via SetLogicRunner from app bootstrap;
	// when nil, multi-step Logic invocations fall back to the
	// "function dispatcher does not support multi-step" error path so
	// stripped-down binaries that don't load automations still get an
	// actionable failure mode.
	logicRunner LogicRunner
}

// LogicRunner is the cross-package bridge that lets the memql engine
// dispatch a multi-step Logic call into the automation step runner.
// Implemented in component/automations/ (logic_runner.go); wired at
// app bootstrap. The runner takes the parsed *AutomationDef body
// (the parser's representation of `body { ... ; return <expr> }`),
// walks the intermediate `name := <call>` steps in order, binds each
// result for later steps to reference, and returns the `_return`
// step's evaluated value.
//
// caller args are passed under both `args` (the canonical author-
// facing form) and `ctx` (the legacy runtime form still produced
// by the rewriter) so step bodies referencing either resolve.
type LogicRunner interface {
	RunLogic(ctx context.Context, fnName string, body *languageParser.AutomationDef, args map[string]any) (any, error)
}

const ComponentName = common.ComponentName("memQLEngine")

// New constructs a MemQLEngine instance backed by the default implementation.
func New(db *bun.DB, args ...component.Arg) (*MemQLEngine, error) {
	cfg := loadEngineConfigFromEnv()
	aiCacheCfg := loadSICacheConfigFromEnv()
	cache, err := newResultCache(cfg.CacheSize)
	if err != nil {
		return nil, fmt.Errorf("memory engine cache initialization failed: %w", err)
	}

	base, err := component.New(ComponentName, args...)
	if err != nil {
		return nil, err
	}

	memql := &MemQLEngine{
		Component:     base,
		relationships: relationshipRegistry{},
		concepts:      nil,
		specs:         nil,
		schemaIdx:     nil,
		db:            db,
		initialized:   false,
		config:        cfg,
		siCacheConfig: aiCacheCfg,
		cache:         cache,
		integrations:  newIntegrationRegistry(),
	}

	if err := memql.Component.ConfigureLifecycle(
		component.WithRunHook(memql.run),
		component.WithOnStartedHook(memql.onStarted),
		component.WithOnStopHook(memql.onStop),
	); err != nil {
		return nil, err
	}

	return memql, nil
}

// SetLogicRunner wires the multi-step Logic dispatcher. Called from
// app bootstrap once the automations package's runner is constructed
// against the live step registry + evaluator. Set to nil to revert
// to the "multi-step bodies are not executable" error path.
func (e *MemQLEngine) SetLogicRunner(runner LogicRunner) {
	e.logicRunner = runner
}

// LogicRunner returns the wired runner (may be nil).
func (e *MemQLEngine) LogicRunner() LogicRunner {
	return e.logicRunner
}

// SetEventBus wires the event bus used for publishing events.
func (e *MemQLEngine) SetEventBus(bus *events.Bus) {
	e.eventBus = bus
	if e.siRuntime != nil {
		e.siRuntime.SetEventBus(bus)
	}
}

// EventBus returns the event bus used by the engine.
func (e *MemQLEngine) EventBus() *events.Bus {
	return e.eventBus
}

// SetPartition configures the active partition for data isolation.
// All queries and mutations will be scoped to this partition.
func (e *MemQLEngine) SetPartition(partition string) {
	e.partition = strings.TrimSpace(partition)
	if e.partition == "" {
		e.partition = "default"
	}
}

// Partition returns the engine's active partition.
func (e *MemQLEngine) Partition() string {
	if e.partition == "" {
		return "default"
	}
	return e.partition
}

// Tools returns the tool registry.
func (e *MemQLEngine) Tools() *ToolRegistry {
	return e.tools
}

// Functions returns the function registry.
func (e *MemQLEngine) Functions() *FunctionRegistry {
	return e.functions
}

// Concepts returns the concept registry.
func (e *MemQLEngine) Concepts() concept.Registry {
	return e.concepts
}

// Specs returns the spec registry.
func (e *MemQLEngine) Specs() *SpecRegistry {
	return e.specs
}

// Prompts returns the prompt registry.
func (e *MemQLEngine) Prompts() *PromptRegistry {
	return e.prompts
}

// Agents returns the agent registry populated by LoadUnifiedAgents
// at engine startup. Used by the `agent(name, args)` builtin's
// executor to resolve agent names to their compiled AgentDefinition.
func (e *MemQLEngine) Agents() *AgentRegistry {
	return e.agents
}

// Seeds returns the seed registry populated by LoadUnifiedSeeds at
// engine startup. The SeedMaterializer reads from this registry on
// the startup sweep + on v1:identity:user create events.
func (e *MemQLEngine) Seeds() *SeedRegistry {
	return e.seeds
}

// SeedMaterializer returns the materializer that turns registered
// seed declarations into rows. Start() should be invoked once the
// database is up + the engine's Execute path is functional; the
// callback for runtime user-create events is registered as part of
// Start.
func (e *MemQLEngine) SeedMaterializer() *SeedMaterializer {
	return e.seedMaterializer
}

// Shapes returns the shape registry.
func (e *MemQLEngine) Shapes() *ShapeRegistry {
	return e.shapes
}

// metadataCollectorInterface abstracts the metadata collector to avoid circular imports.
type metadataCollectorInterface interface {
	Collect(ctx context.Context) map[string]string
}

// SetMetadataCollector sets the metadata collector for enriching mutations with contextual metadata.
func (e *MemQLEngine) SetMetadataCollector(mc metadataCollectorInterface) {
	e.metadataCollector = mc
}

// resolveNamedShape looks up a named shape definition from the registry and
// converts its template into the compiled shapeTemplate type used at runtime.
// Shape definition templates contain string expressions like "node(\"id\")"
// that need to be parsed into shapeNodeFunc references.
func (e *MemQLEngine) resolveNamedShape(name string) (shapeTemplate, error) {
	if e.shapes == nil {
		return nil, fmt.Errorf("shape registry is not initialized")
	}
	def, ok := e.shapes.Get(name)
	if !ok {
		return nil, fmt.Errorf("shape template %q not found", name)
	}
	if def.Template == nil {
		return nil, fmt.Errorf("shape template %q has no template definition", name)
	}
	return convertShapeDefinitionTemplate(def.Template)
}

// convertShapeDefinitionTemplate converts a ShapeDefinition template
// (map[string]any with string expression values like "node(\"id\")")
// into the compiled shapeTemplate type used at runtime.
func convertShapeDefinitionTemplate(tmpl map[string]any) (shapeTemplate, error) {
	result := &shapeObject{
		Fields: make(map[string]shapeTemplate),
	}
	for key, value := range tmpl {
		switch v := value.(type) {
		case string:
			// Parse string expressions like "node(\"id\")" or "node(\"payload.name\")"
			parsed, err := parseShapeExpressionString(v)
			if err != nil {
				return nil, fmt.Errorf("shape field %q: %w", key, err)
			}
			result.Fields[key] = parsed
		case map[string]any:
			// Nested object template
			nested, err := convertShapeDefinitionTemplate(v)
			if err != nil {
				return nil, fmt.Errorf("shape field %q: %w", key, err)
			}
			result.Fields[key] = nested
		default:
			result.Fields[key] = &shapeLiteral{Value: v}
		}
	}
	return result, nil
}

// parseShapeExpressionString parses a shape template expression string into
// a shapeTemplate. Accepts both forms a loaded ShapeDefinition.Template can
// carry:
//
//   - node("field") -- when the shape file is JSON (values arrive unescaped
//     after json.Unmarshal).
//   - node(\"field\") -- when the shape file is .memql (the relaxed parser
//     in shape_parser.go escapes embedded quotes so the raw slice is safe
//     as a JSON-string value).
//
// The unescape pass below normalizes the two paths to a single form before
// field extraction. Without it, the relaxed-parser form left `\"` in the
// extracted field name and every node() field silently rendered as nil at
// runtime, because names like `\"id\` didn't match any metadata field.
func parseShapeExpressionString(expr string) (shapeTemplate, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return &shapeLiteral{Value: ""}, nil
	}

	// Normalize relaxed-parser escapes to the canonical form.
	expr = strings.ReplaceAll(expr, `\"`, `"`)

	// Handle node("field") or node("field1", "field2")
	if strings.HasPrefix(expr, "node(") && strings.HasSuffix(expr, ")") {
		inner := expr[5 : len(expr)-1] // strip node( and )
		inner = strings.TrimSpace(inner)
		if inner == "" {
			return &shapeNodeFunc{Fields: nil}, nil
		}
		// Parse comma-separated quoted field references
		var fields []FieldReference
		for _, part := range strings.Split(inner, ",") {
			part = strings.TrimSpace(part)
			// Strip quotes
			part = strings.Trim(part, "\"")
			if part == "" {
				continue
			}
			fields = append(fields, FieldReference{Raw: part, Parts: strings.Split(part, ".")})
		}
		return &shapeNodeFunc{Fields: fields}, nil
	}

	// Default: treat as literal string value
	return &shapeLiteral{Value: expr}, nil
}

// Integrations returns the integration registry.

// publishEvent emits an event to the event bus if configured.
func (e *MemQLEngine) publishEvent(topic string, kind events.Kind, payload map[string]any) {
	if e.eventBus == nil {
		return
	}
	event := events.NewEvent(topic, kind, payload)
	e.eventBus.Publish(event)
}

// emitQueryExecutedEvent emits a query executed event with timing and result info.
func (e *MemQLEngine) emitQueryExecutedEvent(startTime time.Time, result *ExecuteResult, cached bool) {
	if e.eventBus == nil {
		return
	}

	duration := time.Since(startTime)
	resultCount := 0
	if result != nil && result.Bundle != nil {
		resultCount = len(result.Bundle.Nodes)
	}

	e.publishEvent(
		events.TopicQueryExecuted,
		events.KindQueryExecuted,
		map[string]any{
			"durationMs":  duration.Milliseconds(),
			"resultCount": resultCount,
			"cached":      cached,
		},
	)
}

func (e *MemQLEngine) Execute(ctx context.Context, query string) (*ExecuteResult, error) {
	startTime := time.Now()

	if !e.canResolve() {
		return nil, ErrEngineNotInitialized
	}
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	plan, err := e.Parse(query)
	if err != nil {
		return nil, err
	}

	// Top-level multi-step Logic call: dispatch through the wired
	// LogicRunner. When no runner is wired (e.g. tests, stripped-down
	// binaries), surface an actionable error pointing at app bootstrap.
	if plan.LogicCall != nil {
		result, err := e.executeLogicFunctionCall(ctx, plan.LogicCall)
		if err != nil {
			return nil, err
		}
		e.emitQueryExecutedEvent(startTime, result, false)
		return result, nil
	}

	// Top-level mutation function call: evaluate template and execute one insert.
	if plan.MutationCall != nil {
		// Disallow any query directives or modifiers around mutation calls.
		if plan.Timestamp != nil || plan.UseLatest || plan.Limit != nil || plan.Offset != nil || plan.Depth != nil || len(plan.Sort) > 0 ||
			len(plan.Fields) > 0 || plan.ShapeTemplate != nil || plan.PayloadSelect || (len(plan.Metadata.Fields) > 0 && !plan.Metadata.IncludeAll) {
			return nil, fmt.Errorf("mutation functions cannot be combined with query directives (sort/paginate/select/shape/asOf/withDepth)")
		}
		if len(plan.Mutations) > 0 || plan.Root != nil {
			return nil, fmt.Errorf("mutation function call cannot be combined with other query/mutation expressions")
		}
		result, err := e.executeMutationFunctionCall(ctx, plan.MutationCall)
		if err != nil {
			return nil, err
		}
		e.emitQueryExecutedEvent(startTime, result, false)
		return result, nil
	}

	if len(plan.Mutations) > 0 {
		if plan.Root != nil {
			return nil, fmt.Errorf("MemQL query cannot mix insert() with filter/relationship expressions in this version")
		}
		if len(plan.Mutations) > 1 {
			return nil, fmt.Errorf("multiple insert() / update() mutations in a single MemQL query are not supported yet")
		}
		// Default provenance for raw insert()/update() calls -- not
		// wrapped in a named mutation, so attribute as a direct
		// engine-level insert against the concept. Higher-level
		// callers (e.g. tests that build a plan directly) should
		// stamp provenance themselves before reaching this branch.
		if provenance.FromContext(ctx).IsZero() {
			ctx = provenance.ContextWithProvenance(ctx, provenance.Direct("rawInsert:"+plan.Mutations[0].Concept))
		}
		return e.executeMutation(ctx, plan.Mutations[0])
	}

	if plan.Root == nil {
		return nil, fmt.Errorf("query must include at least one filter or relationship expression")
	}

	effectiveTimestamp := plan.Timestamp

	limit, offset := e.effectiveWindow(plan.Limit, plan.Offset)
	depth := e.effectiveDepth(plan.Depth)

	sorter, err := compileSortFields(plan.Sort)
	if err != nil {
		return nil, err
	}

	var cacheKey string
	useCache := e.cache != nil && len(plan.Mutations) == 0
	signature := ""
	if plan.Root != nil {
		signature = canonicalExpression(plan.Root)
	}
	fieldSignature := projectionSignature(plan.Fields, plan.ConceptFields, plan.Metadata)
	// Resolve named shape reference to a compiled template.
	if plan.ShapeTemplateName != "" && plan.ShapeTemplate == nil {
		resolved, resolveErr := e.resolveNamedShape(plan.ShapeTemplateName)
		if resolveErr != nil {
			return nil, resolveErr
		}
		plan.ShapeTemplate = resolved
	}

	shapeSignature := "graph-bundle"
	if plan.ShapeTemplate != nil {
		shapeSignature = shapeTemplateSignature(plan.ShapeTemplate)
	}
	if useCache {
		cacheKey = e.cacheKey(signature, effectiveTimestamp, limit, offset, depth, sorter.signatureValue(), fieldSignature, shapeSignature)
		if cached, ok := e.cache.get(cacheKey); ok {
			e.emitQueryExecutedEvent(startTime, cached, true)
			return cached, nil
		}
	}

	nodes, err := e.evaluateExpression(ctx, plan.Root, effectiveTimestamp, limit, offset, sorter)
	if err != nil {
		return nil, err
	}

	bundle, err := e.buildGraphBundle(ctx, nodes, depth, effectiveTimestamp)
	if err != nil {
		return nil, err
	}

	result := newExecuteResult(bundle)
	applyPlanProjection(result.Bundle, plan)
	if plan.ShapeTemplate != nil {
		var specs map[string]*Spec
		if e.specs != nil {
			specs = e.specs.Snapshot()
		}
		shaped, err := applyShapeTemplate(ctx, result.Bundle, plan.ShapeTemplate, e.siRuntime, specs)
		if err != nil {
			return nil, err
		}
		result.setOutput(shaped)
		result.setIncludeBundle(plan.IncludeBundle)
	}

	if useCache && result.Bundle != nil {
		if ttl := e.cacheTTLForBundle(result.Bundle, plan.CacheHints); ttl > 0 {
			e.cache.set(cacheKey, result, ttl)
		}
	}

	// Emit query executed event
	e.emitQueryExecutedEvent(startTime, result, false)

	return result, nil
}

// executeLogicFunctionCall dispatches a top-level multi-step Logic
// call through the wired LogicRunner. The runner walks the parsed
// AutomationDef body's intermediate `name := <call>` steps in order
// (via the automation step registry), binds each step's result so
// later steps can reference it, and returns the `_return` step's
// evaluated value as the Logic's return.
//
// When no runner is wired the call surfaces an actionable error
// pointing at the bootstrap wiring; this matches the pre-F.5 path
// so stripped-down binaries that don't load automations still fail
// loudly instead of returning nil silently.
func (e *MemQLEngine) executeLogicFunctionCall(ctx context.Context, call *FunctionCallExpression) (*ExecuteResult, error) {
	if call == nil {
		return nil, fmt.Errorf("logic call is nil")
	}
	if e.functions == nil {
		return nil, fmt.Errorf("function registry is not initialized")
	}

	fn, err := e.functions.Get(call.Name)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, fmt.Errorf("function %q not found", call.Name)
	}
	if !strings.EqualFold(strings.TrimSpace(fn.FunctionKind), "logic") {
		return nil, fmt.Errorf("function %q is not a logic", call.Name)
	}
	if !fn.Enabled {
		return nil, fmt.Errorf("function %q is disabled", call.Name)
	}
	if fn.LogicSteps == nil {
		return nil, fmt.Errorf("function %q has no multi-step body (LogicSteps unset)", call.Name)
	}
	if e.logicRunner == nil {
		return nil, fmt.Errorf("function %q has a multi-step logic body but no LogicRunner is wired -- call engine.SetLogicRunner from app bootstrap (the automations package registers one against the live step registry)", call.Name)
	}

	args := call.Args
	if args == nil {
		args = make(map[string]any)
	}

	// Validate args using the function's args-block schema.
	validator := newFunctionValidator(e.functions.Snapshot(), nil)
	if err := validator.validateFunctionArgs(fn, args); err != nil {
		return nil, err
	}

	out, err := e.logicRunner.RunLogic(ctx, fn.Name, fn.LogicSteps, args)
	if err != nil {
		return nil, fmt.Errorf("logic %q: %w", fn.Name, err)
	}

	result := newExecuteResult(nil)
	result.setOutput(out)
	return result, nil
}

func (e *MemQLEngine) executeMutationFunctionCall(ctx context.Context, call *FunctionCallExpression) (*ExecuteResult, error) {
	if call == nil {
		return nil, fmt.Errorf("mutation call is nil")
	}
	if e.functions == nil {
		return nil, fmt.Errorf("function registry is not initialized")
	}

	fn, err := e.functions.Get(call.Name)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, fmt.Errorf("function %q not found", call.Name)
	}
	if !strings.EqualFold(strings.TrimSpace(fn.FunctionKind), "mutation") {
		return nil, fmt.Errorf("function %q is not a mutation", call.Name)
	}
	if !fn.Enabled {
		return nil, fmt.Errorf("function %q is disabled", call.Name)
	}
	if fn.MutationTemplate == nil {
		return nil, fmt.Errorf("function %q has no mutation template", call.Name)
	}

	args := call.Args
	if args == nil {
		args = make(map[string]any)
	}

	// Validate args using the function's args-block schema.
	validator := newFunctionValidator(e.functions.Snapshot(), nil)
	if err := validator.validateFunctionArgs(fn, args); err != nil {
		return nil, err
	}

	mutation, err := e.renderMutationTemplate(ctx, fn.MutationTemplate, args)
	if err != nil {
		return nil, fmt.Errorf("function %q: %w", fn.Name, err)
	}

	// Stamp default provenance on ctx if the caller didn't already
	// supply one. The default {kind: "mutation", name: <mutationName>}
	// captures the actual named mutation that executed -- meaningful
	// attribution for direct calls (cockpit, web UI, CLI) without
	// requiring every gRPC handler to thread it explicitly. Higher-
	// level callers (SeedMaterializer, automation step runner) wrap
	// ctx with provenance.Seed / provenance.Automation BEFORE
	// Execute, so their more-specific intent wins here.
	if provenance.FromContext(ctx).IsZero() {
		ctx = provenance.ContextWithProvenance(ctx, provenance.Mutation(fn.Name))
	}

	// Dispatch on Kind: insert appends a fresh full-payload row;
	// update reads the latest existing row, splats the partial
	// payload on top, validates the merged result, and inserts.
	result, err := e.executeMutation(ctx, mutation)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (e *MemQLEngine) buildConceptPayload(c *concept.Concept) map[string]any {
	if c == nil {
		return nil
	}

	metadata := map[string]any{
		"name":        c.Name,
		"description": c.Description,
		"type":        c.NodeType,
	}
	if len(c.Relationships) > 0 {
		// Convert []RelationshipDefinition to []any for protobuf compatibility
		relationships := make([]any, len(c.Relationships))
		for i, rel := range c.Relationships {
			relationships[i] = map[string]any{
				"type":          rel.Type,
				"field":         rel.Field,
				"fieldSource":   rel.FieldSource,
				"targetConcept": rel.TargetConcept,
				"direction":     rel.Direction,
			}
		}
		metadata["relationships"] = relationships
	}

	// Convert map[string]json.RawMessage to map[string]any for protobuf compatibility
	schemas := make(map[string]any, len(c.Schemas))
	for key, raw := range c.Schemas {
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err == nil {
			schemas[key] = parsed
		}
	}

	return map[string]any{
		"concept":  c.Name,
		"metadata": metadata,
		"schemas":  schemas,
	}
}

func (e *MemQLEngine) SetConfigSnapshot(snapshot any) {
	if e == nil {
		return
	}
	e.configSnapshot = snapshot
}

// ConfigSnapshot returns the stashed snapshot as an opaque any.
// Callers (the policy evaluator) cast to the bus-protobuf type.
// nil indicates no snapshot has been wired yet — policy bodies see
// the all-zero ctx.config in that state.
func (e *MemQLEngine) ConfigSnapshot() any {
	if e == nil {
		return nil
	}
	return e.configSnapshot
}

// ResolvePartitionFromContext returns the active partition for the given context.
// Priority: context override > engine default > "default".
func (e *MemQLEngine) ResolvePartitionFromContext(ctx context.Context) string {
	return e.resolvePartition(ctx)
}

func (e *MemQLEngine) resolvePlanSpecs(plan *QueryPlan, inline map[string]*Spec) error {
	if plan == nil {
		return nil
	}
	inlineCount := len(inline)
	if inlineCount > 0 && e.specs != nil {
		for name := range inline {
			if e.specs.Has(name) {
				return fmt.Errorf("inline spec %q conflicts with registered spec", name)
			}
		}
	}

	var globalSpecs map[string]*Spec
	if e.specs != nil {
		globalSpecs = e.specs.Snapshot()
	}
	validator := newSpecValidator(e.schemaIdx, globalSpecs, inline)

	if inlineCount > 0 {
		for name := range inline {
			if err := validator.validateInlineSpec(name); err != nil {
				return err
			}
		}
	}

	allowInline := inlineCount > 0
	resolvedRoot, err := validator.resolveExpression(plan.Root, allowInline)
	if err != nil {
		return err
	}
	plan.Root = resolvedRoot
	if inlineCount > 0 {
		plan.InlineSpecs = cloneSpecMap(inline)
	}
	return nil
}

func (e *MemQLEngine) cacheTTLForBundle(bundle *memqlv1.GraphBundle, hints map[string]int64) time.Duration {
	if bundle == nil {
		return 0
	}

	globalMax := e.globalCacheMaxTTL()

	// If explicit cache hints are provided, use them directly.
	// The query author knows what they want - if they say @cache(30), cache for 30s.
	// We use the minimum positive hint value. If any hint is 0, caching is disabled.
	if len(hints) > 0 {
		var hintTTL time.Duration
		for _, seconds := range hints {
			// Explicit @cache(0) means "don't cache"
			if seconds == 0 {
				return 0
			}

			// Skip negative values (invalid)
			if seconds < 0 {
				continue
			}

			hint := time.Duration(seconds) * time.Second

			// Clamp to global max if configured
			if globalMax > 0 && hint > globalMax {
				hint = globalMax
			}

			// Use minimum of all hints
			if hintTTL == 0 || hint < hintTTL {
				hintTTL = hint
			}
		}
		return hintTTL
	}

	// No hints means no cache. Concept-level @cache fallback was
	// retired alongside the @cache annotation in the concept audit;
	// query authors opt in explicitly via @cache(N) on the query.
	return 0
}

func (e *MemQLEngine) globalCacheMaxTTL() time.Duration {
	if e.config.CacheMaxTTLSeconds <= 0 {
		return 0
	}
	return time.Duration(e.config.CacheMaxTTLSeconds) * time.Second
}

func hasRelationshipOfType(defs []RelationshipDefinition, types ...string) bool {
	if len(defs) == 0 || len(types) == 0 {
		return false
	}

	typeSet := make(map[string]struct{}, len(types))
	for _, t := range types {
		if canonical, ok := canonicalRelationshipType(t); ok {
			typeSet[canonical] = struct{}{}
		}
	}
	if len(typeSet) == 0 {
		return false
	}

	for _, def := range defs {
		if _, ok := typeSet[def.Type]; ok {
			return true
		}
	}

	return false
}

func normalizeRelationshipDefinition(def RelationshipDefinition) (RelationshipDefinition, error) {
	relType, ok := canonicalRelationshipType(def.Type)
	if !ok {
		if strings.TrimSpace(def.Type) == "" {
			return RelationshipDefinition{}, fmt.Errorf("type is required")
		}
		return RelationshipDefinition{}, fmt.Errorf("relationship type %q is invalid", def.Type)
	}

	field := strings.TrimSpace(def.Field)
	if field == "" {
		return RelationshipDefinition{}, fmt.Errorf("field is required")
	}

	if strings.TrimSpace(def.FieldSource) != "" {
		return RelationshipDefinition{}, fmt.Errorf("fieldSource is no longer supported; remove it from concept metadata")
	}

	fieldSource, err := deriveRelationshipFieldSource(field)
	if err != nil {
		return RelationshipDefinition{}, err
	}
	if fieldSource == concept.FieldSourceTable && relType != relationshipTypeCreatedBy {
		return RelationshipDefinition{}, fmt.Errorf("relationship type %q does not support metadata field %q", def.Type, field)
	}

	target := strings.TrimSpace(def.TargetConcept)
	if target == "" {
		return RelationshipDefinition{}, fmt.Errorf("targetConcept is required")
	}

	direction := strings.TrimSpace(def.Direction)
	direction = strings.ToLower(direction)
	switch direction {
	case relationshipDirectionOutgoing, relationshipDirectionIncoming, relationshipDirectionBidirectional:
	default:
		return RelationshipDefinition{}, fmt.Errorf("direction %q is invalid", def.Direction)
	}

	if relType == relationshipTypeContains && direction == relationshipDirectionIncoming {
		return RelationshipDefinition{}, fmt.Errorf("contains relationships must declare outgoing or bidirectional direction")
	}

	return RelationshipDefinition{
		Type:          relType,
		Field:         field,
		FieldSource:   fieldSource,
		TargetConcept: target,
		Direction:     direction,
	}, nil
}

func checkDuplicateRelationship(tracker map[string]map[string]map[string]struct{}, conceptName string, def RelationshipDefinition) error {
	fieldMap, ok := tracker[conceptName]
	if !ok {
		fieldMap = make(map[string]map[string]struct{})
		tracker[conceptName] = fieldMap
	}

	fieldKey := def.Field + "|" + def.FieldSource
	typeSet, ok := fieldMap[fieldKey]
	if !ok {
		typeSet = make(map[string]struct{})
		fieldMap[fieldKey] = typeSet
	}

	if _, exists := typeSet[def.Type]; exists {
		return fmt.Errorf("concept %q relationship field %q: duplicate type %q", conceptName, def.Field, def.Type)
	}

	typeSet[def.Type] = struct{}{}
	return nil
}

func checkRelationshipDirection(edges map[string]relationshipEdge, conceptName string, def RelationshipDefinition) error {
	reverseKey := relationshipKey(def.TargetConcept, conceptName, def.Type, def.FieldSource)

	if existing, ok := edges[reverseKey]; ok {
		if !relationshipDirectionsCompatible(existing.Definition.Direction, def.Direction) {
			return fmt.Errorf(
				"concept %q relationship field %q type %q to %q conflicts with direction %q declared in concept %q",
				conceptName,
				def.Field,
				def.Type,
				def.TargetConcept,
				existing.Definition.Direction,
				existing.Source,
			)
		}
	}

	forwardKey := relationshipKey(conceptName, def.TargetConcept, def.Type, def.FieldSource)

	edges[forwardKey] = relationshipEdge{
		Source:     conceptName,
		Definition: def,
	}

	return nil
}

func deriveRelationshipFieldSource(field string) (string, error) {
	if strings.TrimSpace(field) == "" {
		return "", fmt.Errorf("field is required")
	}

	firstSegment := strings.ToLower(strings.TrimSpace(field))
	if dot := strings.Index(firstSegment, "."); dot >= 0 {
		firstSegment = firstSegment[:dot]
	}

	if concept.IsReservedPayloadField(firstSegment) {
		if strings.Contains(field, ".") {
			return "", fmt.Errorf("relationship field %q cannot reference nested paths under reserved property %q", field, firstSegment)
		}
		return concept.FieldSourceTable, nil
	}

	return concept.FieldSourcePayload, nil
}

func relationshipKey(source, target, relType, fieldSource string) string {
	return source + "|" + target + "|" + relType + "|" + fieldSource
}

func relationshipDirectionsCompatible(a, b string) bool {
	if a == relationshipDirectionBidirectional || b == relationshipDirectionBidirectional {
		return true
	}

	return (a == relationshipDirectionOutgoing && b == relationshipDirectionIncoming) ||
		(a == relationshipDirectionIncoming && b == relationshipDirectionOutgoing)
}

func (e *MemQLEngine) run(ctx context.Context, markStarted func()) error {
	if !e.initialized {
		e.Logger.Error("start aborted; memory engine not initialized")

		return ErrEngineNotInitialized
	}

	if e.database() == nil {
		e.Logger.Error("start aborted; database handle not configured")

		return fmt.Errorf("memory engine database not configured")
	}

	// Start the bus handler goroutines if wiring is configured
	if e.wiring != nil {
		go e.runBus(ctx)
		go e.runIntegrationDispatcher(ctx)
	}

	// Phase 0 of the llm-driven-decisions plan: each cache logs its
	// hit/miss/eviction stats every 5 minutes so we can baseline
	// real-world cache effectiveness before migrating decisions to
	// LLM+cache. See docs/planning/cache-audit-phase-0.md. Emitters
	// stay silent when the cache hasn't been touched -- no log
	// noise on a quiet system.
	const statsInterval = 5 * time.Minute
	if e.cache != nil {
		e.cache.startStatsEmitter(ctx, e.Logger, statsInterval)
	}
	if e.siRuntime != nil && e.siRuntime.cache != nil {
		e.siRuntime.cache.startStatsEmitter(ctx, e.Logger, statsInterval)
	}

	markStarted()

	<-common.EnsureContext(ctx).Done()

	return nil
}

func (e *MemQLEngine) onStarted() {
	e.Logger.Info("memory engine ready")
}

func (e *MemQLEngine) onStop() {
	if e.cache != nil {
		e.cache.close()
	}
	e.Logger.Info("memory engine stopped")
}

func (e *MemQLEngine) invalidateCache() {
	if e.cache == nil || e.cache.cache == nil {
		return
	}

	e.cache.mu.Lock()
	defer e.cache.mu.Unlock()

	e.cache.cache.Clear()
}

// SetDatabaseGetter configures a function that returns the current DB handle.
// The getter function is called for each query, ensuring the engine always uses
// the current database connection even after reconnection.
func (e *MemQLEngine) SetDatabaseGetter(getter func() *bun.DB) {
	e.dbMu.Lock()
	e.dbGetter = getter
	e.db = nil // Clear static reference when using getter
	e.dbMu.Unlock()
}

// SetServiceVersion configures the service version exposed by memqlVersion()/queryVersion().
func (e *MemQLEngine) SetServiceVersion(version string) {
	if e == nil {
		return
	}
	e.serviceVersion = strings.TrimSpace(version)
}

func (e *MemQLEngine) currentServiceVersion() string {
	if e == nil {
		return "dev"
	}
	if v := strings.TrimSpace(e.serviceVersion); v != "" {
		return v
	}
	return "dev"
}

func (e *MemQLEngine) database() *bun.DB {
	e.dbMu.RLock()
	defer e.dbMu.RUnlock()

	// Prefer getter function if set (handles reconnection)
	if e.dbGetter != nil {
		return e.dbGetter()
	}
	return e.db
}

func (e *MemQLEngine) effectiveWindow(limitPtr, offsetPtr *int) (int, int) {
	limit := e.config.MaxResults
	if limitPtr != nil && *limitPtr > 0 {
		limit = *limitPtr
	}

	offset := 0
	if offsetPtr != nil && *offsetPtr > 0 {
		offset = *offsetPtr
	}

	if offset > e.config.MaxWindow {
		offset = e.config.MaxWindow
	}

	if offset+limit > e.config.MaxWindow {
		if e.config.MaxWindow > offset {
			limit = e.config.MaxWindow - offset
		} else {
			limit = 0
		}
	}

	if limit < 0 {
		limit = 0
	}

	return limit, offset
}

func (e *MemQLEngine) effectiveDepth(depthPtr *int) int {
	if depthPtr == nil || *depthPtr <= 0 {
		return 1
	}
	return *depthPtr
}

func (e *MemQLEngine) fetchTarget(limit, offset int) int {
	target := limit
	if target <= 0 {
		target = e.config.MaxResults
	}
	if offset > 0 {
		target += offset
	}
	if target > e.config.MaxWindow {
		target = e.config.MaxWindow
	}
	if target <= 0 {
		target = e.config.MaxResults
	}
	return target
}

func (e *MemQLEngine) cacheKey(query string, timestamp *time.Time, limit, offset, depth int, sortSignature, selectSignature, shapeSignature string) string {
	tsKey := "latest"
	if timestamp != nil {
		tsKey = timestamp.UTC().Format(time.RFC3339Nano)
	}
	if strings.TrimSpace(sortSignature) == "" {
		sortSignature = "createdAt:desc"
	}
	if strings.TrimSpace(selectSignature) == "" {
		selectSignature = "none"
	}
	if strings.TrimSpace(shapeSignature) == "" {
		shapeSignature = "graph-bundle"
	}
	return string(cacheIdEngine.MustFromMap(map[string]any{
		"query":     query,
		"timestamp": tsKey,
		"limit":     limit,
		"offset":    offset,
		"depth":     depth,
		"sort":      sortSignature,
		"select":    selectSignature,
		"shape":     shapeSignature,
	}))
}

// logBootValidationSummary emits a structured log entry summarizing component loading at boot.
// This provides visibility into system health from startup logs alone.
func (e *MemQLEngine) logBootValidationSummary(
	functions *FunctionRegistry,
	shapes *ShapeRegistry,
	specs *SpecRegistry,
	providers *ProviderRegistry,
) {
	if e.Logger == nil {
		return
	}

	conceptCount := 0
	if e.concepts != nil {
		conceptCount = len(e.concepts.List())
	}
	functionCount := 0
	if functions != nil {
		functionCount = functions.Count()
	}
	shapeCount := 0
	if shapes != nil {
		shapeCount = shapes.Count()
	}
	specCount := 0
	if specs != nil {
		specCount = specs.Count()
	}
	providerCount := 0
	if providers != nil {
		providerCount = providers.Count()
	}

	e.Logger.Info("boot validation complete",
		"component", ComponentName,
		"status", "healthy",
		"concepts", conceptCount,
		"functions", functionCount,
		"specs", specCount,
		"shapes", shapeCount,
		"providers", providerCount,
	)
}

package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/component"
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
	relationships    relationshipRegistry
	concepts         concept.Registry
	specs            *SpecRegistry
	functions        *FunctionRegistry
	tools            *ToolRegistry
	prompts          *PromptRegistry
	agents           *AgentRegistry
	seeds            *SeedRegistry
	seedMaterializer *SeedMaterializer
	providers        *ProviderRegistry
	policies         *PolicyRegistry
	// configSnapshot is the bus-distributed ConfigSnapshot that
	// backs ctx.config.* inside spec bodies. Optional; nil
	// resolves every allow-listed key to its zero value (sensitive
	// -> false, non-sensitive -> ""). Wired via SetConfigSnapshot
	// from app bootstrap.
	configSnapshot          interface{} // *busv1.ConfigSnapshot, kept as interface{} to avoid an import cycle through engine.go itself.
	shapes                  *ShapeRegistry
	schemaIdx               *schemaIndex
	db                      *bun.DB
	dbGetter                func() *bun.DB // Function that returns current DB (handles reconnection)
	dbMu                    sync.RWMutex
	initialized             bool
	config                  engineConfig
	aiCacheConfig           aiCacheConfig
	cache                   *resultCache
	aiRuntime               *aiRuntime
	eventBus                *events.Bus
	serviceVersion          string
	builtinExecutorHandlers map[string]builtinExecutorHandler
	// builtinPreserveOrder lists the FQNs (integration.X.Y) whose
	// handlers return pre-ordered slices. Populated alongside
	// builtinExecutorHandlers at registration time; read from the
	// dispatch path to decide whether to stamp monotonic CreatedAt on
	// the returned nodes. See IntegrationCapability.PreserveOrder.
	builtinPreserveOrder map[string]bool
	integrations         *IntegrationRegistry
	wiring               *bus.Wiring
	partition            string // active partition for data isolation
	metadataCollector    metadataCollectorInterface
	// logicRunner wires multi-step Logic dispatch through the
	// automation step runner. Set via SetLogicRunner from app bootstrap;
	// when nil, multi-step Logic invocations fall back to the
	// "function dispatcher does not support multi-step" error path so
	// stripped-down binaries that don't load automations still get an
	// actionable failure mode.
	logicRunner LogicRunner
	// promotedAuthored records the (kind:name) of constructs promoted into the
	// shared registries via PromoteAuthoredConstruct, so re-promotion replaces
	// the prior promotion while a name a SEALED core construct owns is still
	// refused (core-first). Zero-value sync.Map is ready to use.
	promotedAuthored sync.Map
	// loadReport is the structured account of the DSL load pass built by
	// Init (epic #2351 / S2, memql#2357): every unified loader's skipped
	// constructs + the S5 uniqueness-gate duplicates. Init refuses to boot
	// the embedded core tree + registered packs when it has problems,
	// unless MEMQL_DSL_ALLOW_SKIPS is set. The post-Init durable-bundle
	// re-hydration path also records quarantines here (they do NOT fail
	// boot). Fresh per Init; nil before the first Init.
	loadReport *LoadReport
}

// #250: the useLegacyMemqlParser field + UseLangparserRuntime
// setter + LangparserRuntimeEnabled getter are gone. Langparser is
// the sole runtime parser; there's nothing to toggle. The
// pre-#250 API was a soak-period rollback handle that the legacy
// parser deletion makes meaningless.

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
	aiCacheCfg := loadAICacheConfigFromEnv()
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
		aiCacheConfig: aiCacheCfg,
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
	if e.aiRuntime != nil {
		e.aiRuntime.SetEventBus(bus)
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
		// Explicit cross-concept reference form `concept.name` (DSL grammar
		// redesign C2/#2043): a query/shape slot can qualify a shape that
		// belongs to ANOTHER concept as `<concept>.<shape>`. Shapes register
		// under their bare name, so strip the concept qualifier and retry.
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			if d2, ok2 := e.shapes.Get(name[i+1:]); ok2 {
				def, ok = d2, true
			}
		}
		if !ok {
			return nil, fmt.Errorf("shape template %q not found", name)
		}
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

// publishEventWithActor is publishEvent plus the acting identity stamped on
// the event envelope's Metadata (G4, memql#2366 / event-payload-binding ADR
// Decision 4). Automations read it as `event.actor.id`; emitters no longer
// need to hand-stamp a `triggeredBy` field into payloads for the envelope's
// benefit. Empty actorId degrades to a plain publishEvent.
func (e *MemQLEngine) publishEventWithActor(topic string, kind events.Kind, payload map[string]any, actorId string) {
	if e.eventBus == nil {
		return
	}
	event := events.NewEvent(topic, kind, payload)
	if actorId != "" {
		event = event.WithMetadata("actor", actorId)
	}
	e.eventBus.Publish(event)
}

// publishCacheInvalidate emits the dedicated cache-invalidation event
// (epic 5, issue 5.6 / memql#1970) for a written concept on the
// separate cache.invalidate.<concept> channel. ONLY the result-cache
// evictor subscribes to this channel, and a single broadcast routing
// rule (cache.invalidate.*) forwards it to every node -- so cross-node
// eviction is decoupled from per-concept graph-write forwarding and
// carries zero side effects (no automations, no other consumers). The
// payload is just the concept; eviction is index-keyed off it.
func (e *MemQLEngine) publishCacheInvalidate(concept string) {
	concept = strings.TrimSpace(concept)
	if concept == "" {
		return
	}
	e.publishEvent(
		events.TopicCacheInvalidateForConcept(concept),
		events.KindCacheInvalidate,
		map[string]any{"concept": concept},
	)
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
	return e.executeWith(ctx, query, e.functions, nil, false)
}

// executeWith is Execute with the function registry made explicit so the
// authored-overlay path (ExecuteAuthored) can resolve + run session-authored
// constructs by name. fns is used for parse-time classification/inlining and
// for the top-level mutation/logic dispatch; specOverlay (when non-nil) lets the
// same authored path resolve a session-authored spec referenced inside an
// authored query's filter -- core-first, owner-scoped, never-shadow (see
// buildAuthoredSpecOverlay + resolveAuthoredSpecOverlay). Everything else (db,
// cache, shapes, provenance) is unchanged. Execute delegates with e.functions
// and a nil specOverlay, so the default path is byte-for-byte the prior
// behaviour. allowInline (MCP Tier-3 #1535) relaxes the inline-shape
// pre-rejection; default false everywhere else.
func (e *MemQLEngine) executeWith(ctx context.Context, query string, fns *FunctionRegistry, specOverlay map[string]*Spec, allowInline bool) (*ExecuteResult, error) {
	startTime := time.Now()

	if !e.canResolve() {
		return nil, ErrEngineNotInitialized
	}
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	// #2800: the origin is read ONCE here, from the context, and handed to
	// the (ctx-free) parse path. auth.OriginFromContext defaults to
	// OriginClient for an unstamped context, so every existing caller keeps
	// the restricted treatment until it explicitly opts in.
	origin := auth.OriginFromContext(ctx)

	// memql#3024: the ambient envelope is read ONCE here too, by the same
	// route and for the same reason as the origin above -- the parse path is
	// ctx-free, so a cond predicate rooted at `actor.` / `config.` /
	// `partition` / `now` can only resolve if the values are handed down.
	// Without it such a predicate falls through to a nil scope and takes the
	// else branch for every input: the silent constant memql#2962 is about,
	// surviving in the namespace that report's own motivation is about.
	//
	// buildAmbientEnvelope is built unconditionally (#2801), so an absent auth
	// context yields the DENYING envelope with every key present rather than
	// absent keys a negated predicate could read as true.
	ambient := buildAmbientEnvelope(ctx, e)

	plan, err := e.parseWithFunctionsAmbient(query, fns, specOverlay, allowInline, origin, ambient)
	if err != nil {
		return nil, err
	}

	// Top-level multi-step Logic call: dispatch through the wired
	// LogicRunner. When no runner is wired (e.g. tests, stripped-down
	// binaries), surface an actionable error pointing at app bootstrap.
	if plan.LogicCall != nil {
		result, err := e.executeLogicFunctionCall(ctx, plan.LogicCall, fns)
		if err != nil {
			return nil, err
		}
		e.emitQueryExecutedEvent(startTime, result, false)
		return result, nil
	}

	// Top-level mutation function call: evaluate template and execute one insert.
	if plan.MutationCall != nil {
		// Disallow any query directives or modifiers around mutation calls.
		if plan.Timestamp != nil || plan.UseLatest || plan.Limit != nil || plan.Depth != nil || len(plan.Sort) > 0 ||
			len(plan.Fields) > 0 || plan.ShapeTemplate != nil || plan.PayloadSelect || (len(plan.Metadata.Fields) > 0 && !plan.Metadata.IncludeAll) {
			return nil, fmt.Errorf("mutation functions cannot be combined with query directives (sort/paginate/select/shape/asOf/withDepth)")
		}
		if len(plan.Mutations) > 0 || plan.Root != nil {
			return nil, fmt.Errorf("mutation function call cannot be combined with other query/mutation expressions")
		}
		result, err := e.executeMutationFunctionCall(ctx, plan.MutationCall, fns)
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

	// Top-level builtin call: a Logic whose body is a single
	// `return <builtin>({...})` expands at resolve time to a bare
	// BuiltinFunctionExpression at plan.Root. The query path below
	// would treat it as a filter and trip "expected collection
	// literal" at the SQL compile step, so dispatch it to the
	// builtin executor directly here -- same path the inline
	// expression evaluator uses (executor.go's
	// `case *BuiltinFunctionExpression`). The result is wrapped as
	// the function's return value, matching how
	// `executeLogicFunctionCall` packages a multi-step Logic's
	// output.
	if builtinCall, ok := plan.Root.(*BuiltinFunctionExpression); ok {
		nodes, err := e.evaluateBuiltinFunctionExpression(ctx, builtinCall, 0)
		if err != nil {
			return nil, err
		}
		result := newExecuteResult(nil)
		result.setOutput(nodesToMap(nodes))
		e.emitQueryExecutedEvent(startTime, result, false)
		return result, nil
	}

	// Top-level literal value: a Logic whose body returns a bare scalar /
	// arg path (e.g. `return args.event.topic` or `return 1`) resolves --
	// after function-call arg substitution folds the ArgRefExpression into
	// a concrete value, or after the parser reduces a quoted literal -- to a
	// *LiteralValueNode at plan.Root. There is no node-set to evaluate: the
	// scalar IS the result. Wrap it as the function's return value, the same
	// way the BuiltinFunctionExpression branch above and
	// executeLogicFunctionCall package a Logic's output. Without this branch
	// the literal falls through to the node-set evaluator
	// (evaluateExpressionSetWithContext), which has no LiteralValueNode case
	// and fails the whole call with "unsupported expression node
	// *memql.LiteralValueNode" -- the memql#1705 consolidateMemory
	// dry-run failure. (#1090 short-circuits the bare-literal case earlier in
	// the LogicRunner; this is the engine-level backstop that also covers the
	// single-return logic path, which never routes through the LogicRunner.)
	if literal, ok := plan.Root.(*LiteralValueNode); ok {
		result := newExecuteResult(nil)
		result.setOutput(literal.Value)
		e.emitQueryExecutedEvent(startTime, result, false)
		return result, nil
	}

	// Top-level collection-method chain (Story 4 / #2302 / ADR §2.2): a
	// Logic whose body is a single `return args.X.where(...).count()`
	// expands -- after the receiver's ArgRefExpression folds to a concrete
	// collection literal -- to a *CollectionMethodExpression at plan.Root.
	// The chain is evaluated in-memory here and wrapped as the function's
	// return value, the same way the BuiltinFunctionExpression and
	// LiteralValueNode branches above package a Logic's output.
	if collExpr, ok := plan.Root.(*CollectionMethodExpression); ok {
		val, err := evaluateCollectionMethodExpression(collExpr, nil)
		if err != nil {
			return nil, err
		}
		result := newExecuteResult(nil)
		result.setOutput(val)
		e.emitQueryExecutedEvent(startTime, result, false)
		return result, nil
	}

	// Top-level dot access (#2542 item 4): a Logic whose body is a single
	// `return args.rows.first().createdAt` resolves -- after the receiver's
	// ArgRefExpression folds to a concrete collection literal -- to a
	// *DotAccessExpression at plan.Root. Evaluated in-memory (the object is
	// a call result, never a SQL-compilable filter) and wrapped as the
	// function's return value, like the CollectionMethodExpression branch
	// above. An empty collection's .first() yields nil, so the field access
	// returns a clean nil rather than erroring.
	if dotExpr, ok := plan.Root.(*DotAccessExpression); ok {
		val, err := evalCollScalar(dotExpr, nil, nil)
		if err != nil {
			return nil, err
		}
		result := newExecuteResult(nil)
		result.setOutput(val)
		e.emitQueryExecutedEvent(startTime, result, false)
		return result, nil
	}

	// Top-level arithmetic (#2316): a Logic whose body is a single
	// `return args.a + args.b` resolves -- after expandExpressionWithArgs
	// folds the operand ArgRefExpressions into concrete literals -- to a
	// *ArithmeticExpression at plan.Root. There is no node-set to evaluate:
	// the computed scalar IS the result. Evaluated in-memory and wrapped as
	// the function's return value, like the LiteralValueNode and
	// CollectionMethodExpression branches above.
	if arithExpr, ok := plan.Root.(*ArithmeticExpression); ok {
		val, err := evalCollScalar(arithExpr, nil, nil)
		if err != nil {
			return nil, err
		}
		result := newExecuteResult(nil)
		result.setOutput(val)
		e.emitQueryExecutedEvent(startTime, result, false)
		return result, nil
	}

	// Top-level expression-led comparison (#2542 item 5 residual): a Logic
	// whose body is a single `return (args.a - args.b) > 0` resolves -- after
	// expandExpressionWithArgs folds the operand ArgRefExpressions into
	// concrete literals -- to a *BinaryComparisonExpression at plan.Root. The
	// computed boolean IS the result; evaluate it in-memory and wrap it as the
	// function's return value, like the ArithmeticExpression branch above.
	if cmpExpr, ok := plan.Root.(*BinaryComparisonExpression); ok {
		val, err := evalCollScalar(cmpExpr, nil, nil)
		if err != nil {
			return nil, err
		}
		result := newExecuteResult(nil)
		result.setOutput(val)
		e.emitQueryExecutedEvent(startTime, result, false)
		return result, nil
	}

	// Top-level date/duration builtin (#2541): a Logic whose body is a single
	// `return addDuration(args.start, "P1D")` resolves -- after
	// expandExpressionWithArgs folds the operand ArgRefExpressions and
	// short-circuits the builtin call -- to a *FunctionCallExpression naming
	// a date builtin at plan.Root. Evaluated in-memory and wrapped as the
	// function's return value, like the ArithmeticExpression branch above.
	//
	// The same plan-root branch also serves a single-return `return cond(...)`
	// (#2542 item 2): expandExpressionWithArgs short-circuits the cond call and
	// folds its branch operands, so a cond wrapping a collection chain resolves
	// in-memory here.
	// coalesce / concat (#2870) join cond here for the same reason: a logic body
	// whose single statement is `return args.gate.passed ?? false` leaves a
	// coalesce call at plan.Root and evaluates in-memory exactly as cond does.
	if callExpr, ok := plan.Root.(*FunctionCallExpression); ok && (IsDateBuiltin(callExpr.Name) ||
		callExpr.Name == "cond" || callExpr.Name == "coalesce" || callExpr.Name == "concat") {
		val, err := evalCollScalar(callExpr, nil, nil)
		if err != nil {
			return nil, err
		}
		result := newExecuteResult(nil)
		result.setOutput(val)
		e.emitQueryExecutedEvent(startTime, result, false)
		return result, nil
	}

	if plan.Root == nil {
		return nil, fmt.Errorf("query must include at least one filter or relationship expression")
	}

	// Row-authz enforcement, the half that needs the request context
	// (memql#3172 finding 4): an owned tier injected against a caller
	// with no identity would compare `ownerUserId == ''` and MATCH every
	// row stored with an empty owner. Refused here, where the actor is
	// readable, rather than degraded into a filter that quietly returns
	// somebody's rows to nobody in particular.
	if err := refuseRowAuthzWithoutActor(ctx, plan); err != nil {
		return nil, err
	}

	effectiveTimestamp := plan.Timestamp

	limit := e.effectiveWindow(plan.Limit, e.defaultListLimit(plan))
	depth := e.effectiveDepth(plan.Depth)

	sorter, err := compileSortFields(plan.Sort)
	if err != nil {
		return nil, err
	}

	// Keyset cursor (5.12): lift an opaque inbound cursor from the request
	// context onto the plan, decode it against the query's sort signature, and
	// stash the resolved keyset position so the executor pushes a
	// `WHERE (createdAt, id) <keyset> (?, ?)` predicate into SQL. A cursor
	// minted under a different ordering is rejected with a typed error rather
	// than silently returning a wrong page. A continuation supersedes the
	// first-page LIMIT window.
	if plan.After == nil {
		if c := cursorFromContext(ctx); c != "" {
			plan.After = &c
		}
	}
	keysetActive := false
	if plan.After != nil && strings.TrimSpace(*plan.After) != "" {
		pos, decErr := decodeCursor(*plan.After, sorter.signatureValue())
		if decErr != nil {
			return nil, decErr
		}
		eligible, _ := keysetEligibleSort(sorter)
		if eligible {
			ctx = contextWithKeyset(ctx, pos)
			keysetActive = true
		}
	}

	// #1730: aggregate count directive. The query was authored with a
	// `count` clause (no shape), so return a self-describing {count: N}
	// envelope computed server-side instead of materializing rows. The
	// count reflects the deduped, latest-version, post-filtered set --
	// the same pipeline normal queries use -- because a raw SQL COUNT(*)
	// would over-count under the time-series versioning model (multiple
	// versions per id) and skip the in-process post-filter / actor folds.
	if plan.Count {
		return e.executeCountPlan(ctx, plan, effectiveTimestamp, sorter, startTime)
	}

	var cacheKey string
	useCache := e.cache != nil && len(plan.Mutations) == 0
	signature := e.planCacheSignature(ctx, plan)
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
		// Keyset cursor (5.12 / 5.5): the cursor identifies which page of a
		// paginated query this call wants. Two different pages of the same
		// `@cache`'d query share every other key field (query / sort / limit
		// / shape / ...), so without the cursor they collide on a single
		// cache entry and page 2 would be served the cached page-1 rows. Fold
		// the resolved cursor token into the cache key so each page is keyed
		// independently. Non-paginated reads carry an empty token.
		cursorKey := ""
		if plan.After != nil {
			cursorKey = strings.TrimSpace(*plan.After)
		}
		cacheKey = e.cacheKey(signature, effectiveTimestamp, limit, depth, sorter.signatureValue(), fieldSignature, shapeSignature, cursorKey)
		if cached, ok := e.cache.get(cacheKey); ok {
			e.emitQueryExecutedEvent(startTime, cached, true)
			return cached, nil
		}
	}

	nodes, err := e.evaluateExpression(ctx, plan.Root, effectiveTimestamp, limit, sorter)
	if err != nil {
		return nil, err
	}

	bundle, err := e.buildGraphBundle(ctx, nodes, depth, effectiveTimestamp)
	if err != nil {
		return nil, err
	}

	result := newExecuteResult(bundle)

	// Keyset cursor (5.12): when paginating and the page came back full, mint a
	// nextCursor from the last row's keyset position so the caller can continue.
	// An exhausted set (a short page) leaves the cursor empty. Only emit for
	// keyset-eligible orderings; payload-sorted full-scan queries fall back to
	// the in-memory path and carry no SQL keyset cursor.
	if limit > 0 && len(nodes) >= limit {
		if eligible, _ := keysetEligibleSort(sorter); eligible && (keysetActive || plan.After != nil || len(plan.Sort) > 0 || plan.Limit != nil) {
			if next, encErr := encodeCursor(nodes[len(nodes)-1], sorter.signatureValue()); encErr == nil {
				result.SetCursor(next)
				result.SetHasMore(true)
			}
		}
	}

	applyPlanProjection(result.Bundle, plan)
	if plan.ShapeTemplate != nil {
		var specs map[string]*Spec
		if e.specs != nil {
			specs = e.specs.Snapshot()
		}
		shaped, err := applyShapeTemplate(ctx, result.Bundle, plan.ShapeTemplate, e.aiRuntime, specs)
		if err != nil {
			return nil, err
		}
		result.setOutput(shaped)
		result.setIncludeBundle(plan.IncludeBundle)
	}

	if useCache && result.Bundle != nil {
		if ttl := e.cacheTTLForBundle(result.Bundle, plan.CacheHints); ttl > 0 {
			// Record the concept(s) this result depends on so a write to
			// any of them evicts the key (5.4, via the cache.invalidate.*
			// channel). Only cache when we can name at least one dependency
			// concept -- an un-invalidatable cached result would go stale on
			// the next write with no way to drop it.
			deps := dependencyConceptsForResult(plan, result.Bundle)
			// 5.6 default-on (memql#1970): never default-cache a result that
			// depends on a denylisted concept (auth/identity must read live).
			// An EXPLICIT positive @cache(ttl=N) still wins -- the author
			// opted in knowingly -- so the denylist gates only the no-hint
			// default path. plan.CacheHints is non-empty iff the query
			// carried @cache.
			denylisted := anyConceptCacheDenylisted(deps)
			explicitHint := len(plan.CacheHints) > 0
			if len(deps) > 0 && (explicitHint || !denylisted) {
				e.cache.set(cacheKey, result, ttl, deps)
			}
		}
	}

	// Emit query executed event
	e.emitQueryExecutedEvent(startTime, result, false)

	return result, nil
}

// planCacheSignature renders the query half of the result-cache key.
//
// CORRECTNESS for default-on caching (epic 5, issue 5.6 / memql#1970):
// the canonical signature renders an actor reference identically for
// every caller (the userId/role resolves later and never reaches the
// signature). An owned query (`payload.ownerUserId == actor.userId`)
// would otherwise collide across users and serve caller B caller A's
// rows. When the plan depends on the actor, the resolved actor identity
// is folded into the signature so each caller keys independently.
// Actor-independent reads keep one shared key across callers.
//
// ROW-AUTHZ (memql#3172 finding 2): an enforced plan is actor-dependent
// BY CONSTRUCTION, so the flag folds the caller in on its own rather
// than relying on the injected node landing somewhere
// planReferencesActor happens to walk. That reliance is what made the
// refused attempt a cross-user leak: caching is default-on with a 60s
// TTL and a `v1:identity:`-only denylist, and on main the same query
// returns every row to everyone -- so it is enforcement that creates the
// divergence a shared key then serves.
func (e *MemQLEngine) planCacheSignature(ctx context.Context, plan *QueryPlan) string {
	if plan == nil || plan.Root == nil {
		return ""
	}
	signature := canonicalExpression(plan.Root)
	if plan.RowAuthzInjected || e.planReferencesActor(plan.Root) {
		signature = "actor:" + actorCacheKeyComponent(ctx) + "\x1f" + signature
	}
	return signature
}

// executeCountPlan computes a numeric {count: N} aggregate for a query
// authored with the `count` directive (#1730). It runs the same
// dedup + latest-version + post-filter pipeline a normal query uses
// (via evaluateExpressionSet), then returns the cardinality of the
// matching set rather than the rows. We fetch up to MaxWindow
// candidates so the count is not silently capped at the smaller
// default page size (MaxResults) the row path uses. A true SQL-pushdown
// COUNT(*) is a future optimization tracked separately -- it cannot be
// a naive COUNT because the versioned hypertable stores multiple rows
// per id and the latest-version re-filter happens in Go.
func (e *MemQLEngine) executeCountPlan(ctx context.Context, plan *QueryPlan, timestamp *time.Time, sorter *compiledSort, startTime time.Time) (*ExecuteResult, error) {
	if plan.Root == nil {
		return nil, ErrEmptyQuery
	}

	set, err := e.evaluateExpressionSet(ctx, plan.Root, timestamp, e.config.MaxWindow, sorter)
	if err != nil {
		return nil, err
	}

	count := int64(len(set))
	result := newExecuteResult(nil)
	result.setOutput(map[string]any{"count": count})
	result.setIncludeBundle(false)
	result.SetCount(count)

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
func (e *MemQLEngine) executeLogicFunctionCall(ctx context.Context, call *FunctionCallExpression, fns *FunctionRegistry) (*ExecuteResult, error) {
	if call == nil {
		return nil, fmt.Errorf("logic call is nil")
	}
	if fns == nil {
		return nil, fmt.Errorf("function registry is not initialized")
	}

	fn, err := fns.Get(call.Name)
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
	// #2800: @serverOnly bars client-originated calls. Mutations and logic
	// dispatch here rather than through the query expansion path, so the gate
	// is repeated at each entry point the same way @disabled is -- a single
	// check in one of the three would leave the other two open.
	if fn.ServerOnly && !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("function %q is server-only and cannot be called by a client", call.Name)
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
	validator := newFunctionValidator(fns.Snapshot(), nil)
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

func (e *MemQLEngine) executeMutationFunctionCall(ctx context.Context, call *FunctionCallExpression, fns *FunctionRegistry) (*ExecuteResult, error) {
	if call == nil {
		return nil, fmt.Errorf("mutation call is nil")
	}
	if fns == nil {
		return nil, fmt.Errorf("function registry is not initialized")
	}

	fn, err := fns.Get(call.Name)
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
	// #2800: @serverOnly bars client-originated calls. Mutations and logic
	// dispatch here rather than through the query expansion path, so the gate
	// is repeated at each entry point the same way @disabled is -- a single
	// check in one of the three would leave the other two open.
	if fn.ServerOnly && !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("function %q is server-only and cannot be called by a client", call.Name)
	}
	if fn.MutationTemplate == nil {
		return nil, fmt.Errorf("function %q has no mutation template", call.Name)
	}

	args := call.Args
	if args == nil {
		args = make(map[string]any)
	}

	// Strict unknown-arg rejection at the MCP boundary (memql#1633): a
	// caller-supplied arg the mutation never declares would otherwise be
	// silently dropped on the way into the template -- losing valid data
	// with no signal, and in some cases leaving a @required concept field
	// unsatisfied. Surface it as an error instead, mirroring the unknown-
	// tool rejection from memql#1602. Only active when the caller opted in
	// via WithStrictUnknownArgs (the MCP run_mutation / @mcp path); internal
	// callers stay lenient.
	if strictUnknownArgs(ctx) {
		if err := rejectUnknownArgs(fn, args); err != nil {
			return nil, err
		}
	}

	// Validate args using the function's args-block schema.
	validator := newFunctionValidator(fns.Snapshot(), nil)
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

	// Role split (#2034 / C4): an authorization `spec` (IsTrait ==
	// false) may not be referenced by bare name in a query filter --
	// only a `trait` (data/state predicate) belongs in the filter slot.
	if err := rejectAuthzSpecInFilter(plan.Root, e.specs); err != nil {
		return err
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

	// No explicit hints: default-on caching (epic 5, issue 5.6 /
	// memql#1970). A pure read (no mutation -- the caller already gated on
	// len(plan.Mutations)==0) caches by default with a conservative
	// backstop TTL, relying on 5.4 invalidation + the cache.invalidate.*
	// broadcast channel for freshness. The two further guards -- the
	// "at least one dependency concept must be named" 5.4 invariant and
	// the staleness denylist -- are enforced at the cache-set call site in
	// execute(), where the dependency concept set is in hand.
	def := time.Duration(defaultResultCacheTTLSeconds) * time.Second
	if globalMax > 0 && def > globalMax {
		def = globalMax
	}
	return def
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
	// LLM+cache. See docs/internal/planning/cache-audit-phase-0.md. Emitters
	// stay silent when the cache hasn't been touched -- no log
	// noise on a quiet system.
	const statsInterval = 5 * time.Minute
	if e.cache != nil {
		e.cache.startStatsEmitter(ctx, e.Logger, statsInterval)
	}
	if e.aiRuntime != nil && e.aiRuntime.cache != nil {
		e.aiRuntime.cache.startStatsEmitter(ctx, e.Logger, statsInterval)
	}

	// 5.4: wire result-cache invalidation to the graph event bus. A
	// write to a concept evicts the dependent cached query results so a
	// cached read can never go stale. Scoped to the engine lifecycle
	// context (the subscription is torn down when ctx is cancelled).
	e.StartCacheInvalidationSubscriber(ctx)

	// #232: wire LIVE cross-node propagation of durable promotions. A durable
	// promote on any node broadcasts authoring.promote.<bundleId>; this
	// subscriber re-hydrates the promoted bundle into this node's shared
	// registry on receipt, so a promote on node A becomes callable on node B
	// within seconds (no restart). Scoped to the engine lifecycle context.
	e.StartAuthoringPromoteSubscriber(ctx)

	// memql#2163: wire LIVE cross-node propagation of durable DEMOTIONS, the
	// inverse of the promote subscriber above. A durable demote on any node
	// broadcasts authoring.demote.<bundleId>; this subscriber removes the
	// demoted bundle's constructs from this node's shared registry on receipt,
	// so a demote on node A takes effect on node B within seconds (no restart).
	e.StartAuthoringDemoteSubscriber(ctx)

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
	e.cache.cache.Clear()
	e.cache.mu.Unlock()

	// Drop the dependency index too -- the keys it points at are gone.
	e.cache.depMu.Lock()
	e.cache.depIndex = make(map[string]map[string]struct{})
	e.cache.depMu.Unlock()
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

// defaultListLimit returns the implicit row limit the engine applies
// when a query declares NO explicit window. A query whose author wrote
// neither a `paginate` directive (plan.Limit set) nor a `sort` directive
// (plan.Sort populated) is an UNMARKED list read: the pagination runtime
// backstop (epic 5, memql#1965) caps it at config.DefaultListCap (50 by
// default) so it can never pull the whole table. Everything else --
// explicit paginate, explicit sort, the count aggregate, or an
// `@unbounded("reason")` query (rewritten to an explicit paginate) --
// states its own window and falls back to MaxResults, the pre-#1965
// behaviour.
//
// The cap is intentionally a backstop, not a hard window: it only
// changes the DEFAULT, so a query that genuinely needs more rows opts
// in by paginating, sorting, or marking @unbounded. This is the "safe to
// ship independently" half of #1965 -- it bounds blast radius without
// requiring the authoring sweep (issue 5.3).
func (e *MemQLEngine) defaultListLimit(plan *QueryPlan) int {
	if plan != nil && plan.Limit == nil && len(plan.Sort) == 0 {
		return e.config.DefaultListCap
	}
	return e.config.MaxResults
}

func (e *MemQLEngine) effectiveWindow(limitPtr *int, defaultLimit int) int {
	limit := defaultLimit
	if limit <= 0 {
		limit = e.config.MaxResults
	}
	if limitPtr != nil && *limitPtr > 0 {
		limit = *limitPtr
	}

	if limit > e.config.MaxWindow {
		limit = e.config.MaxWindow
	}

	if limit < 0 {
		limit = 0
	}

	return limit
}

func (e *MemQLEngine) effectiveDepth(depthPtr *int) int {
	if depthPtr == nil || *depthPtr <= 0 {
		return 1
	}
	return *depthPtr
}

func (e *MemQLEngine) fetchTarget(limit int) int {
	target := limit
	if target <= 0 {
		target = e.config.MaxResults
	}
	if target > e.config.MaxWindow {
		target = e.config.MaxWindow
	}
	if target <= 0 {
		target = e.config.MaxResults
	}
	return target
}

func (e *MemQLEngine) cacheKey(query string, timestamp *time.Time, limit, depth int, sortSignature, selectSignature, shapeSignature, cursor string) string {
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
		"depth":     depth,
		"sort":      sortSignature,
		"select":    selectSignature,
		"shape":     shapeSignature,
		// Keyset cursor (5.12 / 5.5): distinct continuation pages must not
		// collide on one key. Empty for non-paginated reads.
		"cursor": cursor,
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

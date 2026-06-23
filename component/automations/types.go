// Package automations provides an automation engine for MemQL.
// Automations support multi-step processes with conditional branching, iteration,
// parallel execution, and data flow between steps.
package automations

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/logger"
)

// ComponentName identifies this package in logs and environment variable lookups.
const ComponentName = common.ComponentName("automations")

// NewLogger creates a logger for the automations component using common.NewLogger.
// This ensures proper color support and component-level enable/disable via environment variables.
func NewLogger() *slog.Logger {
	return logger.New(ComponentName, os.Stdout, resolveLoggerLevel())
}

// resolveLoggerLevel reads the log level from environment variables.
func resolveLoggerLevel() slog.Level {
	reader := env.NewEnvReader("AUTOMATIONS")
	for _, key := range loggerLevelKeys() {
		if value, ok := reader.String(key); ok {
			if strings.TrimSpace(value) == "" {
				continue
			}
			var level slog.Level
			if err := level.UnmarshalText([]byte(strings.ToLower(value))); err == nil {
				return level
			}
		}
	}
	return slog.LevelInfo
}

func loggerLevelKeys() []string {
	return []string{
		"CAPABILITIES_LOGGING_LOG_LEVEL",
		"LOGGER_LEVEL",
		"LOG_LEVEL",
	}
}

// Automation represents a complete automation definition.
type Automation struct {
	// Name is the unique identifier for this automation (camelCase).
	Name string `json:"name"`

	// Description provides human-readable context for the automation.
	Description string `json:"description,omitempty"`

	// Schedule is an optional cron expression for scheduled execution.
	// If omitted, the automation must be triggered manually or by events.
	Schedule string `json:"schedule,omitempty"`

	// Trigger defines event-based triggers for this automation.
	// When an event matching the pattern is published, this automation runs.
	Trigger *TriggerConfig `json:"trigger,omitempty"`

	// Input defines the initial data query for the automation.
	// The result is available as $input in step expressions.
	Input *AutomationInput `json:"input,omitempty"`

	// Preconditions are first-class deterministic checks (Epic 4 /
	// memql#2139) evaluated -- in order -- at the START of the run, after
	// the trigger fires and the input query (if any) loads, but BEFORE any
	// step executes. Each precondition is a boolean expression over the
	// same deterministic evaluator that powers Step.Condition + trigger
	// filters (no LLM). A precondition that evaluates false is a MISS: it
	// aborts the run cleanly (no steps fire) and emits the structured
	// healing.precondition.missed signal the self-healing repair loop
	// (E4.4) subscribes to. A miss is BOTH the clean repair trigger and
	// the cross-machine portability mechanism -- a literal asserted by a
	// precondition that does not hold on this machine is a precondition
	// that misses here.
	Preconditions []*Precondition `json:"preconditions,omitempty"`

	// Steps are the ordered list of operations to execute.
	Steps []*Step `json:"steps"`

	// OnComplete is an optional step to run after successful completion.
	OnComplete *Step `json:"onComplete,omitempty"`

	// OnError is an optional step to run if the automation fails.
	OnError *Step `json:"onError,omitempty"`

	// Enabled controls whether this automation is active. Defaults to true.
	Enabled *bool `json:"enabled,omitempty"`

	// MCPPromoted is true when the automation carries @mcp: it is promoted into
	// its own first-class MCP tool rather than only being reachable through the
	// generic run_automation dispatcher (epic memql#1529 Phase 4 #1534).
	MCPPromoted bool `json:"mcpPromoted,omitempty"`

	// Origin tracks where this automation was loaded from (file path).
	Origin string `json:"-"`
}

// TriggerConfig defines event-based triggers for an automation.
type TriggerConfig struct {
	// Event is the topic pattern to subscribe to.
	// Supports glob patterns: "graph.node.*", "automation.#", etc.
	Event string `json:"event"`

	// Filter is an optional condition expression that must be true for the trigger to fire.
	// Example: "$event.payload.status == 'completed'"
	Filter string `json:"filter,omitempty"`
}

// ArgsField defines a single field's type assertion.
type ArgsField struct {
	// Name is the field name (e.g., "subject", "email")
	Name string `json:"name"`

	// Type is the expected type: "string", "number", "bool", "object", "array", "any"
	Type string `json:"type"`

	// Optional indicates if the field can be missing (false means required)
	Optional bool `json:"optional,omitempty"`

	// Nested contains nested field assertions for object types
	Nested []*ArgsField `json:"nested,omitempty"`
}

// IsEventTriggered returns true if this automation has an event trigger.
func (a *Automation) IsEventTriggered() bool {
	return a != nil && a.Trigger != nil && a.Trigger.Event != ""
}

// IsEnabled returns whether the automation should run.
func (a *Automation) IsEnabled() bool {
	if a == nil {
		return false
	}
	if a.Enabled == nil {
		return true
	}
	return *a.Enabled
}

// IsScheduled returns true if this automation has a cron schedule.
func (a *Automation) IsScheduled() bool {
	return a != nil && a.Schedule != ""
}

// DefinitionFingerprint computes a content-addressed hash of the automation definition.
// Used to detect if the automation changed between checkpoint creation and resume.
func (a *Automation) DefinitionFingerprint(engine *id.Engine) string {
	if a == nil || engine == nil {
		return ""
	}

	// Extract step IDs for quick comparison
	stepIds := make([]string, len(a.Steps))
	for i, step := range a.Steps {
		stepIds[i] = step.ID
	}

	// Build a deterministic representation of the automation structure
	def := map[string]any{
		"name":    a.Name,
		"stepIds": stepIds,
	}

	// Include step type and key configuration for each step
	stepDefs := make([]map[string]any, len(a.Steps))
	for i, step := range a.Steps {
		stepDef := map[string]any{
			"id":   step.ID,
			"type": string(step.Type),
		}
		if step.Condition != "" {
			stepDef["condition"] = step.Condition
		}
		stepDefs[i] = stepDef
	}
	def["steps"] = stepDefs

	return string(engine.MustFromMap(def))
}

// Precondition is a first-class deterministic check attached to an
// automation (Epic 4 / memql#2139). It is the unit of self-healing
// portability: a precondition asserts that some condition holds before
// the automation's steps run, and a miss is the clean repair trigger.
//
// A precondition is intentionally minimal and DETERMINISTIC -- it never
// calls an LLM. The harness evaluates Check via the same boolean
// evaluator that powers Step.Condition + trigger filters, so the full
// expression grammar ($event.*, $input.*, $config.*, $var.*, comparisons,
// &&/||/!) is available. The deploy spine stays authored/deterministic:
// preconditions guard it but are never themselves LLM-healed.
type Precondition struct {
	// ID is a stable identifier for this precondition within its
	// automation (e.g. "engineImageDigestPinned"). It keys the miss
	// signal so the repair loop (E4.4) and the typed-patch model (E4.3)
	// can target the exact construct to heal. Required.
	ID string `json:"id"`

	// Check is the deterministic boolean expression that must hold for
	// the automation to proceed. Evaluated false => the precondition
	// MISSES. Required. Example: "$config.MEMQL_ENV == \"staging\"" or
	// "$event.payload.imageDigest != \"\"".
	Check string `json:"check"`

	// Literal optionally names the machine-specific literal this
	// precondition asserts (a path, an id, an endpoint). It is the
	// portability hint the repair loop reads when proposing a
	// relativize-literal / rebind-param patch (E4.3): the literal that
	// does not hold here is what must be relativized to travel. Optional;
	// when empty the repair loop infers the literal from Check.
	Literal string `json:"literal,omitempty"`

	// Description is human-readable context surfaced in the miss signal
	// and in the cockpit healed-pack UI (E4.6). Optional.
	Description string `json:"description,omitempty"`
}

// AutomationInput defines the initial data source for an automation.
type AutomationInput struct {
	// Query is a MemQL expression to fetch initial data.
	Query string `json:"query"`

	// Limit optionally caps the number of results.
	Limit int `json:"limit,omitempty"`
}

// StepType defines the kind of step operation.
type StepType string

const (
	// StepTypeQuery executes a MemQL query.
	StepTypeQuery StepType = "query"

	// StepTypeMutation executes a MemQL mutation (insert).
	StepTypeMutation StepType = "mutation"

	// StepTypeShape transforms data using a shape template.
	StepTypeShape StepType = "shape"

	// StepTypeWebhook makes an HTTP request.
	StepTypeWebhook StepType = "webhook"

	// StepTypeEvent publishes an event to the event bus.
	StepTypeEvent StepType = "event"

	// StepTypeFunction invokes a registered MemQL function.
	StepTypeFunction StepType = "function"

	// StepTypeAction replays an action-library action by reference (#1758).
	StepTypeAction StepType = "action"

	// StepTypeForEach iterates over a collection.
	StepTypeForEach StepType = "forEach"

	// StepTypeParallel executes multiple branches concurrently.
	StepTypeParallel StepType = "parallel"

	// StepTypeSwitch provides conditional branching.
	StepTypeSwitch StepType = "switch"

	// StepTypeAutomation invokes another automation.
	StepTypeAutomation StepType = "automation"

	// StepTypeDetectLeadSignal performs deterministic regex-based lead signal detection.
	StepTypeDetectLeadSignal StepType = "detectLeadSignal"

	// StepTypeEmitConceptCard emits a concept card utterance to a conversation.
	StepTypeEmitConceptCard StepType = "emitConceptCard"
)

// ErrorStrategy defines how to handle step failures.
type ErrorStrategy string

const (
	// ErrorStrategyStop halts the automation on error (default).
	ErrorStrategyStop ErrorStrategy = "stop"

	// ErrorStrategyContinue continues to the next step on error.
	ErrorStrategyContinue ErrorStrategy = "continue"

	// ErrorStrategyRetry retries the step (with optional count).
	ErrorStrategyRetry ErrorStrategy = "retry"
)

// Step represents a single operation in an automation.
type Step struct {
	// ID is a unique identifier for this step within the automation.
	// Used to reference results: $steps.{id}.result
	ID string `json:"id"`

	// Name is a human-readable description of the step.
	Name string `json:"name,omitempty"`

	// Type determines which executor handles this step.
	Type StepType `json:"type"`

	// OnError defines error handling strategy. Defaults to "stop".
	OnError ErrorStrategy `json:"onError,omitempty"`

	// RetryCount specifies how many times to retry on failure (when onError is "retry").
	RetryCount int `json:"retryCount,omitempty"`

	// Condition is an optional expression that must be true for the step to run.
	// Example: "$steps.classify.result.length > 0"
	Condition string `json:"condition,omitempty"`

	// Configuration for specific step types (only one should be set):

	// Query configuration (type: "query")
	Query *QueryStepConfig `json:"query,omitempty"`

	// Mutation configuration (type: "mutation")
	Mutation *MutationStepConfig `json:"mutation,omitempty"`

	// Shape configuration (type: "shape")
	Shape *ShapeStepConfig `json:"shape,omitempty"`

	// Webhook configuration (type: "webhook")
	Webhook *WebhookStepConfig `json:"webhook,omitempty"`

	// Event configuration (type: "event")
	Event *EventStepConfig `json:"event,omitempty"`

	// Function configuration (type: "function")
	Function *FunctionStepConfig `json:"function,omitempty"`

	// Action configuration (type: "action") -- action-library replay (#1758).
	Action *ActionStepConfig `json:"action,omitempty"`

	// Automation configuration (type: "automation")
	Automation *AutomationStepConfig `json:"automation,omitempty"`

	// ForEach configuration (type: "forEach")
	ForEach *ForEachStepConfig `json:"forEach,omitempty"`

	// Parallel configuration (type: "parallel")
	Parallel *ParallelStepConfig `json:"parallel,omitempty"`

	// Switch configuration (type: "switch")
	Switch *SwitchStepConfig `json:"switch,omitempty"`

	// DetectLeadSignal configuration (type: "detectLeadSignal")
	DetectLeadSignal *DetectLeadSignalStepConfig `json:"detectLeadSignal,omitempty"`

	// EmitConceptCard configuration (type: "emitConceptCard")
	EmitConceptCard *EmitConceptCardStepConfig `json:"emitConceptCard,omitempty"`

	// Cache configures step-level memoization.
	Cache *CacheConfig `json:"cache,omitempty"`
}

// CacheConfig controls step-level caching behavior.
type CacheConfig struct {
	// Enabled controls whether this step is cached.
	// If nil, defaults based on step type (query/shape/function are cacheable).
	Enabled *bool `json:"enabled,omitempty"`

	// TTL is how long to cache the result (e.g., "5m", "1h").
	// Zero or empty means use executor default.
	TTL string `json:"ttl,omitempty"`
}

// IsCacheable returns true if this step can be cached.
// Returns false for side-effecting steps (mutation, webhook, event).
func (s *Step) IsCacheable() bool {
	if s == nil {
		return false
	}

	// Explicit disable via config
	if s.Cache != nil && s.Cache.Enabled != nil && !*s.Cache.Enabled {
		return false
	}

	// Explicit enable overrides type defaults
	if s.Cache != nil && s.Cache.Enabled != nil && *s.Cache.Enabled {
		return true
	}

	// Default by type: pure steps are cacheable
	switch s.Type {
	case StepTypeQuery, StepTypeShape, StepTypeFunction:
		return true
	default:
		// Mutations, webhooks, events have side effects - not cacheable
		return false
	}
}

// CacheTTL returns the configured cache TTL or zero for default.
func (s *Step) CacheTTL() time.Duration {
	if s == nil || s.Cache == nil || s.Cache.TTL == "" {
		return 0
	}
	d, err := time.ParseDuration(s.Cache.TTL)
	if err != nil {
		return 0
	}
	return d
}

// QueryStepConfig configures a query step.
type QueryStepConfig struct {
	// Query is the MemQL expression to execute.
	// Supports $ expressions for data substitution.
	Query string `json:"query"`
}

// MutationStepConfig configures a mutation (insert) step.
// Unlike QueryStepConfig, this stores structured data that is evaluated at runtime,
// allowing dynamic expressions in the payload to be properly resolved before
// building the final JSON for the MemQL engine.
type MutationStepConfig struct {
	// Concept is the target concept name (e.g., "v1:identity:user").
	Concept string `json:"concept"`

	// ID is an optional explicit ID for the record.
	// Can be a literal string or an expression like "$event.payload.subject".
	ID string `json:"id,omitempty"`

	// Payload is the mutation payload as a structured map.
	// Values can be literals or expression strings (prefixed with $).
	// The evaluator will resolve all expressions at runtime.
	Payload map[string]any `json:"payload"`

	// Parent is an optional parent reference for relationship hints.
	Parent string `json:"parent,omitempty"`

	// AliasOf is an optional alias reference for relationship hints.
	AliasOf string `json:"aliasOf,omitempty"`
}

// ShapeStepConfig configures a shape transformation step.
type ShapeStepConfig struct {
	// Source is a $ expression pointing to the input data.
	// Example: "$input", "$steps.fetch.result"
	Source string `json:"source"`

	// Template is the shape template as a JSON object.
	// Values can use shape helpers: node(), ai(), children(), etc.
	Template json.RawMessage `json:"template"`

	// RequireNotEmpty fails the step if source resolves to an empty array.
	// Useful for asserting that prerequisite data exists before proceeding.
	RequireNotEmpty bool `json:"requireNotEmpty,omitempty"`
}

// WebhookStepConfig configures an HTTP request step.
type WebhookStepConfig struct {
	// URL is the endpoint to call. Supports $ expressions.
	URL string `json:"url"`

	// Method is the HTTP method. Defaults to POST.
	Method string `json:"method,omitempty"`

	// Headers to include in the request. Values support $ expressions.
	Headers map[string]string `json:"headers,omitempty"`

	// Body is a static or templated JSON body.
	// Supports $ expressions for dynamic values.
	Body map[string]any `json:"body,omitempty"`

	// IncludeResult sends the previous step's result as the body.
	IncludeResult bool `json:"includeResult,omitempty"`

	// ResultFrom specifies which step's result to include.
	// Defaults to the immediately preceding step.
	ResultFrom string `json:"resultFrom,omitempty"`

	// Timeout for the request. Defaults to 30s.
	Timeout string `json:"timeout,omitempty"`
}

// EventStepConfig configures an event publication step.
type EventStepConfig struct {
	// Topic is the event topic. Supports $ expressions.
	Topic string `json:"topic"`

	// Kind is the event kind. Defaults to "message".
	Kind string `json:"kind,omitempty"`

	// Payload is the event payload. Supports $ expressions.
	Payload map[string]any `json:"payload,omitempty"`

	// IncludeResult includes a step's result in the payload.
	IncludeResult bool `json:"includeResult,omitempty"`

	// ResultFrom specifies which step's result to include.
	ResultFrom string `json:"resultFrom,omitempty"`
}

// FunctionStepConfig configures a MemQL function invocation.
type FunctionStepConfig struct {
	// Name is the function identifier (without parentheses).
	Name string `json:"name"`
	// Args are optional function arguments passed at invocation time.
	Args map[string]any `json:"args,omitempty"`
}

// ActionStepConfig configures an action-library replay step (#1758, epic
// #1734).
type ActionStepConfig struct {
	// Ref is the pinned-by-default action reference (id@version, or a bare id
	// to float to the latest version).
	Ref string `json:"ref"`
	// Args bind the action's parameters at invocation time.
	Args map[string]any `json:"args,omitempty"`
	// Surface is an optional explicit surface binding the resolver honors
	// before policy/availability.
	Surface string `json:"surface,omitempty"`
}

// AutomationStepConfig configures invoking another automation.
type AutomationStepConfig struct {
	// Name is the automation identifier to invoke.
	Name string `json:"name"`

	// Async runs the automation without waiting for completion.
	// When true, the step returns immediately after triggering.
	Async bool `json:"async,omitempty"`

	// Args are the arguments passed to the sub-automation. They
	// become the sub-automation's input envelope so the
	// sub-automation's `event.X` references resolve against them
	// (i.e. a procedurally-called automation sees the call args
	// the same way an event-triggered one sees the event payload).
	// Optional; absent for invocations that just pass the parent's
	// envelope through unchanged.
	Args map[string]any `json:"args,omitempty"`
}

// ForEachStepConfig configures iteration over a collection.
type ForEachStepConfig struct {
	// Source is a $ expression pointing to the collection.
	// Example: "$input", "$steps.classify.result"
	Source string `json:"source"`

	// Filter is an optional condition to filter items.
	// Example: "item.classification == 'hot'"
	// The current item is available as "item".
	Filter string `json:"filter,omitempty"`

	// As defines the variable name for the current item.
	// Defaults to "item". Available as $item or ${as} in nested steps.
	As string `json:"as,omitempty"`

	// Do contains the steps to execute for each item.
	Do []*Step `json:"do"`

	// Concurrency limits parallel execution. 0 or 1 means sequential.
	Concurrency int `json:"concurrency,omitempty"`
}

// ParallelStepConfig configures concurrent step execution.
type ParallelStepConfig struct {
	// Branches are the steps to execute concurrently.
	Branches []*Step `json:"branches"`

	// Wait strategy: "all" (default), "any", or "none".
	// "all" - wait for all branches to complete
	// "any" - continue when first branch completes
	// "none" - fire and forget
	Wait string `json:"wait,omitempty"`

	// FailFast stops all branches when one fails.
	FailFast bool `json:"failFast,omitempty"`
}

// SwitchStepConfig configures conditional branching.
type SwitchStepConfig struct {
	// Expression is evaluated to determine which case to execute.
	// Example: "$item.classification"
	Expression string `json:"expression"`

	// Cases maps expression values to steps.
	Cases map[string]*SwitchCase `json:"cases"`

	// Default is executed if no case matches.
	Default *SwitchCase `json:"default,omitempty"`
}

// SwitchCase defines what to execute for a switch case.
type SwitchCase struct {
	// Steps to execute when this case matches.
	Steps []*Step `json:"steps,omitempty"`

	// Step is a shorthand for a single step.
	Step *Step `json:"step,omitempty"`
}

// DetectLeadSignalStepConfig configures a detect lead signal step.
type DetectLeadSignalStepConfig struct {
	// Source is a $ expression pointing to the text to analyze.
	Source string `json:"source"`

	// CustomIntentKeywords allows adding deployment-specific intent signals.
	CustomIntentKeywords []string `json:"customIntentKeywords,omitempty"`

	// CustomProductKeywords allows adding deployment-specific product mentions.
	CustomProductKeywords []string `json:"customProductKeywords,omitempty"`
}

// EmitConceptCardStepConfig configures a concept card emission step.
// Concept cards are system-generated utterances that surface automation output in conversation.
type EmitConceptCardStepConfig struct {
	// CardType identifies the kind of card (e.g., "lead_captured", "lead_updated").
	CardType string `json:"cardType"`

	// PartitionId is the space to emit the card to.
	// Supports $ expressions.
	PartitionId string `json:"partitionId"`

	// ConceptRef is a reference to the created concept (e.g., the lead ID).
	// Supports $ expressions.
	ConceptRef string `json:"conceptRef"`

	// Data contains fields to display on the card.
	// Values support $ expressions.
	Data map[string]any `json:"data,omitempty"`
}

// StepResult contains the outcome of executing a step.
type StepResult struct {
	// StepId identifies which step produced this result.
	StepId string `json:"stepId"`

	// Status is "success", "failed", "skipped".
	Status string `json:"status"`

	// Result contains the step's output data.
	Result any `json:"result,omitempty"`

	// Error contains any error message.
	Error string `json:"error,omitempty"`

	// Duration is how long the step took.
	Duration time.Duration `json:"duration"`

	// StartedAt is when the step began.
	StartedAt time.Time `json:"startedAt"`

	// CompletedAt is when the step finished.
	CompletedAt time.Time `json:"completedAt"`

	// Children contains results from nested steps (forEach, parallel, switch).
	Children []*StepResult `json:"children,omitempty"`

	// Metadata contains additional step-specific information.
	Metadata map[string]any `json:"metadata,omitempty"`

	// Chain linkage fields (populated when ChainTrackingEnabled is true)

	// ContentId is the deterministic fingerprint of this step's execution.
	ContentId string `json:"contentId,omitempty"`

	// PreviousChainHead links to the chain state before this step executed.
	PreviousChainHead string `json:"previousChainHead,omitempty"`

	// ChildFingerprints contains ordered fingerprints for ForEach/Parallel children.
	ChildFingerprints []string `json:"childFingerprints,omitempty"`
}

// AutomationExecution represents a complete automation run.
type AutomationExecution struct {
	// ID is a unique identifier for this execution.
	ID string `json:"id"`

	// AutomationName identifies which automation was run.
	AutomationName string `json:"automationName"`

	// Status is "running", "completed", "failed", "cancelled".
	Status string `json:"status"`

	// Input contains the result of the input query.
	Input any `json:"input,omitempty"`

	// Steps contains results for each executed step.
	Steps map[string]*StepResult `json:"steps"`

	// Error contains the automation-level error if any.
	Error string `json:"error,omitempty"`

	// StartedAt is when the execution began.
	StartedAt time.Time `json:"startedAt"`

	// CompletedAt is when the execution finished.
	CompletedAt time.Time `json:"completedAt"`

	// Duration is the total execution time.
	Duration time.Duration `json:"duration"`

	// TriggeredBy indicates what started this execution.
	// "schedule", "manual", "event:{topic}"
	TriggeredBy string `json:"triggeredBy,omitempty"`

	// Chain tracking fields (populated when ChainTrackingEnabled is true)

	// StepOrder preserves the declared execution sequence for verification.
	StepOrder []string `json:"stepOrder,omitempty"`

	// ChainHead is the final content-addressed state after all steps.
	ChainHead string `json:"chainHead,omitempty"`

	// InitialChainHead captures the starting state (automation + trigger context).
	InitialChainHead string `json:"initialChainHead,omitempty"`

	// InputFingerprint is the deterministic hash of the input query result.
	InputFingerprint string `json:"inputFingerprint,omitempty"`
}

// NewExecution creates a new automation execution.
func NewExecution(automationName, triggeredBy string) *AutomationExecution {
	return &AutomationExecution{
		ID:             generateExecutionId(),
		AutomationName: automationName,
		Status:         "running",
		Steps:          make(map[string]*StepResult),
		StartedAt:      time.Now(),
		TriggeredBy:    triggeredBy,
	}
}

// Complete marks the execution as successfully completed.
func (e *AutomationExecution) Complete() {
	e.Status = "completed"
	e.CompletedAt = time.Now()
	e.Duration = e.CompletedAt.Sub(e.StartedAt)
}

// Fail marks the execution as failed.
func (e *AutomationExecution) Fail(err error) {
	e.Status = "failed"
	if err != nil {
		e.Error = err.Error()
	}
	e.CompletedAt = time.Now()
	e.Duration = e.CompletedAt.Sub(e.StartedAt)
}

// Cancel marks the execution as cancelled.
func (e *AutomationExecution) Cancel() {
	e.Status = "cancelled"
	e.CompletedAt = time.Now()
	e.Duration = e.CompletedAt.Sub(e.StartedAt)
}

// AddStepResult records a step's result.
func (e *AutomationExecution) AddStepResult(result *StepResult) {
	if e.Steps == nil {
		e.Steps = make(map[string]*StepResult)
	}
	e.Steps[result.StepId] = result
}

// generateExecutionId returns a fresh opaque execution identifier.
// Delegates to core/id.NewShortId so the executionId (which is also
// the v1:memql:checkpoint row's shortId) shares the same UUIDv4
// format every other instance row in the system uses. The previous
// "20060102-150405-XXXXXX" timestamp+suffix shape was the last
// non-UUID instance-id format in memql core; aligning it with the
// rest closes the gap memql#103 tracked.
func generateExecutionId() string {
	return id.NewShortId()
}

// ExecutionCheckpoint captures the state of a failed automation for later resume.
// Stored as a MemQL node with concept v1:memql:checkpoint.
type ExecutionCheckpoint struct {
	// ExecutionId is the unique ID of the failed execution.
	ExecutionId string `json:"executionId"`

	// AutomationName identifies which automation was running.
	AutomationName string `json:"automationName"`

	// AutomationFingerprint is a content-addressed hash of the automation definition.
	// Used to detect if the automation changed between failure and resume.
	AutomationFingerprint string `json:"automationFingerprint"`

	// StepIndex is the index in Steps slice where failure occurred.
	StepIndex int `json:"stepIndex"`

	// ChainHead is the chain state before the failed step.
	ChainHead string `json:"chainHead,omitempty"`

	// InitialChainHead captures the starting state for chain validation.
	InitialChainHead string `json:"initialChainHead,omitempty"`

	// StepResults contains results for all completed steps before failure.
	// Uses MinimalStepResult to avoid storing large payloads.
	StepResults map[string]*MinimalStepResult `json:"stepResults"`

	// StepOrder preserves execution sequence for verification.
	StepOrder []string `json:"stepOrder,omitempty"`

	// FailedAt contains details about the failure.
	FailedAt *StepFailure `json:"failedAt"`

	// Input is the automation input data (for evaluator restoration).
	Input any `json:"input,omitempty"`

	// InputFingerprint is the hash of input for verification.
	InputFingerprint string `json:"inputFingerprint,omitempty"`

	// TriggerContext captures the original trigger for event replay.
	TriggerContext *TriggerContext `json:"triggerContext,omitempty"`

	// SavedAt is when the checkpoint was saved.
	SavedAt time.Time `json:"savedAt"`

	// ExpiresAt is when the checkpoint becomes invalid (SavedAt + 24h).
	ExpiresAt time.Time `json:"expiresAt"`
}

// MinimalStepResult stores only fields needed for evaluator rehydration.
// Avoids storing large query result payloads in checkpoint.
type MinimalStepResult struct {
	StepId    string         `json:"stepId"`
	Status    string         `json:"status"`
	Result    any            `json:"result,omitempty"`
	Error     string         `json:"error,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	ContentId string         `json:"contentId,omitempty"`
}

// StepFailure describes where and why an automation failed.
type StepFailure struct {
	StepId    string    `json:"stepId"`
	Error     string    `json:"error"`
	ErrorCode string    `json:"errorCode,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// TriggerContext captures the original trigger for evaluator restoration.
type TriggerContext struct {
	TriggeredBy string         `json:"triggeredBy"`
	Event       map[string]any `json:"event,omitempty"`
}

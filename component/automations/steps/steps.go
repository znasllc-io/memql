// Package steps provides step executors for the automation engine.
package steps

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// formatDuration returns a human-readable duration string in milliseconds.
// Examples: "1.234ms", "456.789ms", "1234.567ms"
func formatDuration(d time.Duration) string {
	ms := float64(d.Nanoseconds()) / float64(time.Millisecond)
	return fmt.Sprintf("%.3fms", ms)
}

// StepRecordData contains the data for a step execution record.
type StepRecordData struct {
	RunId          string  // Execution ID of the parent automation run
	StepId         string  // Step identifier from the automation definition
	StepType       string  // Type of step (query, event, webhook, etc.)
	Sequence       int     // Execution order
	Status         string  // success, failed, skipped
	Query          string  // The resolved query that was executed (for query steps)
	ResultQuery    string  // Query to retrieve the result
	ItemCount      int     // Number of items in result
	Duration       float64 // Duration in milliseconds
	Error          string  // Error message if failed
	ParentStepId   string  // For nested steps, the parent step record ID
	IterationIndex int     // For forEach iterations, the index
	Topic          string  // For event steps
	URL            string  // For webhook steps
	StatusCode     int     // For webhook steps
	FunctionName   string  // For function steps
}

// RecordStepExecution inserts a v1:memql:automation:step record and returns the retrieval query.
// Returns empty string if recording fails (errors are logged but not fatal).
func RecordStepExecution(ctx context.Context, engine *memql.MemQLEngine, data StepRecordData) string {
	if engine == nil {
		return ""
	}

	// Build the payload fields
	payloadParts := []string{
		fmt.Sprintf(`runId: %s`, jsonString(data.RunId)),
		fmt.Sprintf(`stepId: %s`, jsonString(data.StepId)),
		fmt.Sprintf(`stepType: %s`, jsonString(data.StepType)),
		fmt.Sprintf(`status: %s`, jsonString(data.Status)),
	}

	if data.Sequence > 0 {
		payloadParts = append(payloadParts, fmt.Sprintf(`sequence: %d`, data.Sequence))
	}
	if data.Query != "" {
		payloadParts = append(payloadParts, fmt.Sprintf(`query: %s`, jsonString(data.Query)))
	}
	if data.ResultQuery != "" {
		payloadParts = append(payloadParts, fmt.Sprintf(`resultQuery: %s`, jsonString(data.ResultQuery)))
	}
	if data.ItemCount > 0 {
		payloadParts = append(payloadParts, fmt.Sprintf(`itemCount: %d`, data.ItemCount))
	}
	if data.Duration > 0 {
		payloadParts = append(payloadParts, fmt.Sprintf(`duration: %.2f`, data.Duration))
	}
	if data.Error != "" {
		payloadParts = append(payloadParts, fmt.Sprintf(`error: %s`, jsonString(data.Error)))
	}
	if data.ParentStepId != "" {
		payloadParts = append(payloadParts, fmt.Sprintf(`parentStepId: %s`, jsonString(data.ParentStepId)))
	}
	if data.IterationIndex > 0 {
		payloadParts = append(payloadParts, fmt.Sprintf(`iterationIndex: %d`, data.IterationIndex))
	}
	if data.Topic != "" {
		payloadParts = append(payloadParts, fmt.Sprintf(`topic: %s`, jsonString(data.Topic)))
	}
	if data.URL != "" {
		payloadParts = append(payloadParts, fmt.Sprintf(`url: %s`, jsonString(data.URL)))
	}
	if data.StatusCode > 0 {
		payloadParts = append(payloadParts, fmt.Sprintf(`statusCode: %d`, data.StatusCode))
	}
	if data.FunctionName != "" {
		payloadParts = append(payloadParts, fmt.Sprintf(`functionName: %s`, jsonString(data.FunctionName)))
	}

	// Build the insert query
	insertQuery := fmt.Sprintf(`insert("%s", { payload: { %s } })`,
		memoryNodes.ConceptMemQLAutomationStep, strings.Join(payloadParts, ", "))

	// Execute the insert
	_, err := engine.Execute(ctx, insertQuery)
	if err != nil {
		// Log error but don't fail the step
		return ""
	}

	// Return the retrieval query
	return fmt.Sprintf(`concept==%s;payload.runId==%s;payload.stepId==%s`,
		memoryNodes.ConceptMemQLAutomationStep, jsonString(data.RunId), jsonString(data.StepId))
}

// jsonString renders a MemQL string literal for the step-execution record's
// insert.
//
// One line, deliberately: it is an ALIAS for langparser.QuoteString, not a
// second implementation. The body was json.Marshal with HTML escaping left on
// -- never a live parse defect, since json.Marshal emits exactly the escapes
// the lexer implements, but a second definition of the escape set sitting in
// the same package as renderMemQLValue, which DID carry the %q bug. Two
// definitions is the cost memql#3035 demonstrated; this collapses one of them.
// memql#3192.
//
// The only visible change is that <, > and & now go out raw rather than
// unicode-escaped: QuoteString turns HTML escaping off, the lexer accepts all
// three raw, and the parsed value is identical either way. Nothing built here
// reaches a browser.
func jsonString(s string) string {
	return langparser.QuoteString(s)
}

// BuildResultQuery constructs a query to retrieve the result of a step.
// For queries, it returns the original query.
// For mutations, it builds a query to retrieve the inserted record.
func BuildResultQuery(stepType, query string, result any) string {
	// For query steps, the result query is just the original query
	if stepType == "query" && !strings.HasPrefix(strings.TrimSpace(query), "insert(") {
		return query
	}

	// For mutations (inserts), try to extract concept and id from the result
	if er, ok := result.(*memql.ExecuteResult); ok && er != nil && er.Bundle != nil {
		nodes := er.Bundle.GetNodes()
		if len(nodes) > 0 {
			node := nodes[0]
			concept := node.GetConcept()
			id := node.GetId()
			if concept != "" && id != "" {
				return fmt.Sprintf(`concept==%s;id==%s`, concept, jsonString(id))
			}
		}
	}

	// Fallback: return the original query
	return query
}

// Context is an alias for automations.StepContext for convenience.
type Context = automations.StepContext

// Executor is the interface that all step executors implement.
type Executor interface {
	// Execute runs the step and returns the result.
	Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error)
}

// Registry holds all registered step executors.
type Registry struct {
	executors map[automations.StepType]Executor
}

// NewRegistry creates a new step executor registry with all built-in executors.
func NewRegistry() *Registry {
	r := &Registry{
		executors: make(map[automations.StepType]Executor),
	}

	// Register built-in executors
	r.Register(automations.StepTypeQuery, &QueryExecutor{})
	r.Register(automations.StepTypeMutation, &MutationExecutor{})
	r.Register(automations.StepTypeWebhook, &WebhookExecutor{})
	r.Register(automations.StepTypeEvent, &EventExecutor{})
	r.Register(automations.StepTypeFunction, &FunctionExecutor{})
	r.Register(automations.StepTypeAction, &ActionExecutor{})
	r.Register(automations.StepTypeAutomation, &AutomationExecutor{})
	r.Register(automations.StepTypeForEach, &ForEachExecutor{Registry: r})
	r.Register(automations.StepTypeParallel, &ParallelExecutor{Registry: r})
	r.Register(automations.StepTypeSwitch, &SwitchExecutor{Registry: r})
	r.Register(automations.StepTypeShape, &ShapeExecutor{})
	r.Register(automations.StepTypeDetectLeadSignal, &DetectLeadSignalExecutor{})
	r.Register(automations.StepTypeEmitConceptCard, &EmitConceptCardExecutor{})

	return r
}

// Register adds an executor for a step type.
func (r *Registry) Register(stepType automations.StepType, executor Executor) {
	r.executors[stepType] = executor
}

// Get retrieves an executor for a step type.
func (r *Registry) Get(stepType automations.StepType) (Executor, bool) {
	exec, ok := r.executors[stepType]
	return exec, ok
}

// Execute finds and runs the appropriate executor for a step.
func (r *Registry) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	executor, ok := r.Get(step.Type)
	if !ok {
		return nil, &UnknownStepTypeError{Type: step.Type}
	}
	return executor.Execute(ctx, step, stepCtx)
}

// UnknownStepTypeError indicates an unregistered step type.
type UnknownStepTypeError struct {
	Type automations.StepType
}

func (e *UnknownStepTypeError) Error() string {
	return "unknown step type: " + string(e.Type)
}

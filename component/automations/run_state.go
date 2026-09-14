package automations

// run_state.go -- an automation run's state (epic memql#5363, memql#5367).
//
// Evaluator holds what a run has bound and recorded: the triggering event and
// every other seeded root, the step results, the forEach loop item, and the
// resolvers behind `var` / `secret` and canonicalId(). It evaluates nothing
// itself. Every expression of a run -- a step condition, a trigger filter, a
// precondition, an argument, a forEach source -- is edition-2026 source,
// parsed once at load (PrepareExpressions) and evaluated by memql.EvalExpr
// over RunScope, which reads this state (run_scope.go). The string evaluator
// that used to live beside it, reading `$`-paths and legacy condition text,
// is gone with the legacy grammar.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
)

// VariableResolver resolves a variable name to its value: `var.NAME`,
// `systemVar.NAME`, `secret.NAME`, `systemSecret.NAME`.
type VariableResolver func(ctx context.Context, name string) (string, error)

// CanonicalIdResolver normalizes an id-shaped value to canonical
// form for the named concept (`<partition>:<conceptType>:<bareSlug>`).
// Wired by the engine boot path so automation steps can call
// canonicalId(value, "<conceptType>") and get the same answer as
// mutations / queries. When unset, callers should fall back to the
// raw value (degraded but safe).
type CanonicalIdResolver func(ctx context.Context, value, conceptType string) (string, error)

// Evaluator is an automation run's state (see the file comment).
type Evaluator struct {
	// input holds the automation input query result.
	input any

	// steps holds results from completed steps, keyed by step ID.
	steps map[string]*StepResult

	// item holds the current item in a forEach loop.
	item any

	// itemName is the variable name for the current item (default "item").
	itemName string

	// custom holds the seeded roots: event, args, actor, config, ...
	custom map[string]any

	// variableResolver resolves `var.NAME` to v1:platform:partitionVariable,
	// falling back to v1:platform:globalVariable.
	variableResolver VariableResolver

	// systemVariableResolver resolves `systemVar.NAME` to
	// v1:platform:globalVariable (global plaintext). No fallback.
	systemVariableResolver VariableResolver

	// secretResolver resolves `secret.NAME` to v1:platform:partitionSecret
	// (partition-scoped encrypted), falling back to v1:platform:globalSecret.
	// Returns the decrypted plaintext.
	secretResolver VariableResolver

	// systemSecretResolver resolves `systemSecret.NAME` to
	// v1:platform:globalSecret (global encrypted). Returns decrypted plaintext.
	systemSecretResolver VariableResolver

	// canonicalIdResolver normalizes an id-shaped value to canonical form for
	// a named concept: canonicalId(value, "<conceptType>").
	canonicalIdResolver CanonicalIdResolver

	// logger for warnings.
	logger *slog.Logger

	// names is the name frame of a statement-body run (statement_scope.go,
	// epic memql#5370): non-nil puts RunScope in statement resolution. A
	// Clone shares it; a loop iteration or a parallel branch opens a child
	// frame of its own (childFrame).
	names *nameFrame
}

// SetCanonicalIdResolver wires the engine's id-canonicalization helper
// into the run. Required for canonicalId() to work inside automation step
// expressions; without it, callers fall back to the raw value (which means
// automation-derived ids may diverge from mutation-derived ids when callers
// pass bare slugs).
func (e *Evaluator) SetCanonicalIdResolver(r CanonicalIdResolver) {
	if e == nil {
		return
	}
	e.canonicalIdResolver = r
}

// CanonicalIdResolver returns the wired resolver (may be nil).
func (e *Evaluator) CanonicalIdResolver() CanonicalIdResolver {
	if e == nil {
		return nil
	}
	return e.canonicalIdResolver
}

// NewEvaluator creates an empty run state.
func NewEvaluator() *Evaluator {
	return &Evaluator{
		steps:    make(map[string]*StepResult),
		itemName: "item",
		custom:   make(map[string]any),
	}
}

// SetInput sets the automation input data.
func (e *Evaluator) SetInput(input any) {
	e.input = input
}

// SetStepResult records a step's result.
func (e *Evaluator) SetStepResult(stepId string, result *StepResult) {
	e.steps[stepId] = result
}

// SetItem sets the current forEach item.
func (e *Evaluator) SetItem(item any, name string) {
	e.item = item
	if name != "" {
		e.itemName = name
	} else {
		e.itemName = "item"
	}
}

// ClearItem clears the current forEach item.
func (e *Evaluator) ClearItem() {
	e.item = nil
	e.itemName = "item"
}

// SetCustom seeds a root.
func (e *Evaluator) SetCustom(name string, value any) {
	e.custom[name] = value
}

// Clone creates a copy of the run state for a nested context (a forEach item,
// a parallel branch).
//
// Every resolver travels with the copy, the canonicalId one included. It used
// to be left behind, so a canonicalId() inside a forEach or parallel branch
// silently fell back to the raw value while the same call one level up
// canonicalised it; no shipped automation calls canonicalId inside a loop,
// so carrying it changes no run in the tree (memql#5367).
func (e *Evaluator) Clone() *Evaluator {
	clone := &Evaluator{
		input:                  e.input,
		steps:                  make(map[string]*StepResult),
		item:                   e.item,
		itemName:               e.itemName,
		custom:                 make(map[string]any),
		variableResolver:       e.variableResolver,
		systemVariableResolver: e.systemVariableResolver,
		secretResolver:         e.secretResolver,
		systemSecretResolver:   e.systemSecretResolver,
		canonicalIdResolver:    e.canonicalIdResolver,
		logger:                 e.logger,
		names:                  e.names,
	}
	for k, v := range e.steps {
		clone.steps[k] = v
	}
	for k, v := range e.custom {
		clone.custom[k] = v
	}
	return clone
}

// SetVariableResolver sets the resolver behind `var.NAME`.
func (e *Evaluator) SetVariableResolver(resolver VariableResolver) {
	e.variableResolver = resolver
}

// SetSystemVariableResolver sets the resolver behind `systemVar.NAME`
// (v1:platform:globalVariable).
func (e *Evaluator) SetSystemVariableResolver(resolver VariableResolver) {
	e.systemVariableResolver = resolver
}

// SetSecretResolver sets the resolver behind `secret.NAME`
// (v1:platform:partitionSecret with fallback to v1:platform:globalSecret).
// Returns decrypted plaintext.
func (e *Evaluator) SetSecretResolver(resolver VariableResolver) {
	e.secretResolver = resolver
}

// SetSystemSecretResolver sets the resolver behind `systemSecret.NAME`
// (v1:platform:globalSecret). Returns decrypted plaintext.
func (e *Evaluator) SetSystemSecretResolver(resolver VariableResolver) {
	e.systemSecretResolver = resolver
}

// SetLogger sets the logger for warning messages.
func (e *Evaluator) SetLogger(logger *slog.Logger) {
	e.logger = logger
}

// GetStepNodes returns the node list of a step's result: a collection-valued
// result is its list, and a query or mutation result is its bundle's nodes.
// ok is false when the step did not run, or its result carries no node list.
func (e *Evaluator) GetStepNodes(stepId string) ([]any, bool) {
	stepResult, ok := e.steps[stepId]
	if !ok || stepResult == nil {
		return nil, false
	}

	result := stepResult.Result
	if result == nil {
		return nil, false
	}

	// A collection-valued step result (Story 4 collection chain bound by a
	// `:=` step, #2317) is already the node list. A Bundle / *ExecuteResult
	// keeps the envelope-aware path below.
	switch v := result.(type) {
	case []any:
		return v, true
	case []map[string]any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out, true
	}

	if resultMap, ok := result.(map[string]any); ok {
		if bundle, ok := resultMap["Bundle"].(map[string]any); ok {
			if nodes, ok := bundle["nodes"].([]any); ok {
				return nodes, true
			}
		}
	}

	// A struct result (*memql.ExecuteResult) is read through its JSON form.
	jsonBytes, err := json.Marshal(result)
	if err != nil {
		return nil, false
	}
	var resultMap map[string]any
	if err := json.Unmarshal(jsonBytes, &resultMap); err != nil {
		return nil, false
	}
	if bundle, ok := resultMap["Bundle"].(map[string]any); ok {
		if nodes, ok := bundle["nodes"].([]any); ok {
			return nodes, true
		}
	}

	return nil, false
}

// HasStep returns true if the run has a result for the given step ID.
func (e *Evaluator) HasStep(stepId string) bool {
	_, ok := e.steps[stepId]
	return ok
}

// FormatValue renders a value as text: a string is itself, a scalar its
// canonical spelling, anything else its JSON.
func FormatValue(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case bool:
		return strconv.FormatBool(val)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case json.Number:
		return val.String()
	default:
		data, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(data)
	}
}

// ToSlice reads a value as a list: a list is itself, and a query result is
// its bundle's nodes.
func ToSlice(v any) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	switch val := v.(type) {
	case []any:
		return val, nil
	case []map[string]any:
		result := make([]any, len(val))
		for i, item := range val {
			result[i] = item
		}
		return result, nil
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("cannot convert %T to slice", v)
		}
		var arr []any
		if err := json.Unmarshal(data, &arr); err == nil {
			return arr, nil
		}
		// A MemQL ExecuteResult: {"Bundle":{"nodes":[...],...}}.
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err == nil {
			if bundle, ok := obj["Bundle"].(map[string]any); ok {
				if nodes, ok := bundle["nodes"].([]any); ok {
					return nodes, nil
				}
			}
		}
		return nil, fmt.Errorf("cannot convert %T to slice", v)
	}
}

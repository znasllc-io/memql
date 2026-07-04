package automations

// args_binding.go implements G1 of the event-payload-binding epic
// (memql#2352, story #2363): fire-time binding of an event's payload into an
// automation's declared `args { }` contract, with loud refusal on violation.
//
// An automation that declares an `args { }` block gets its trigger payload
// (or, for invoke-by-reference, the caller's params) validated BEFORE any
// step runs (ADR Decision 2):
//
//   - a missing @required field, a type mismatch, an @enum violation, a
//     @maxLength overrun, or a @pattern miss makes the automation REFUSE to
//     fire -- a Warn log naming automation/topic/field/reason plus a skip
//     counter (refusedFires). Never a silent nil, never a partial run.
//   - unknown extra payload fields are TOLERATED (tolerant-reader): they are
//     simply not bound; a Debug note records them.
//
// The validated map is exposed to the evaluator as `args` (SetCustom) so G1
// bodies may read `args.X`. Bare-field resolution + shadowing is G2 (#2364).
//
// The bind/validate logic lives in the automations package (not the shared
// memql function validator) to avoid a memql <- automations import cycle; the
// rule set mirrors component/memql/function_validator.go.

import (
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/events"
)

// argBindError carries the failing field + reason so the caller can log
// automation/topic/field/reason for the operator (and the #2357 report).
type argBindError struct {
	Field  string
	Reason string
}

func (e *argBindError) Error() string {
	return fmt.Sprintf("arg %q: %s", e.Field, e.Reason)
}

// bindEventArgs binds a triggering event's payload into the automation's
// declared args contract and validates it. It returns:
//
//   - bound:  the validated map of DECLARED fields present in the payload
//     (exposed to the evaluator as `args`);
//   - extras: undeclared payload keys (tolerated, not bound) for a Debug note;
//   - err:    the first contract violation (*argBindError), or nil.
//
// Automations with no args block return (nil, nil, nil) -- untyped
// event.payload access is unchanged (backward compatible).
func bindEventArgs(automation *Automation, event *events.Event) (bound map[string]any, extras []string, err error) {
	if automation == nil || automation.Args == nil || len(automation.Args.Fields) == 0 {
		return nil, nil, nil
	}

	var payload map[string]any
	if event != nil {
		payload = event.Payload
	}
	if payload == nil {
		payload = map[string]any{}
	}

	declared := make(map[string]struct{}, len(automation.Args.Fields))
	bound = make(map[string]any, len(automation.Args.Fields))
	for _, field := range automation.Args.Fields {
		if field == nil || strings.TrimSpace(field.Name) == "" {
			continue
		}
		declared[field.Name] = struct{}{}
		if verr := validateAutomationArg(payload, field); verr != nil {
			return nil, nil, verr
		}
		if v, ok := payload[field.Name]; ok {
			bound[field.Name] = v
		}
	}

	// Tolerant-reader (ADR Decision 2): collect, but never bind, payload keys
	// the contract does not declare.
	for k := range payload {
		if _, ok := declared[k]; !ok {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	return bound, extras, nil
}

// validateAutomationArg validates one declared field against the payload,
// mirroring the memql function validator's rule set (presence / type / enum /
// maxLength / pattern). Object + array element schemas are validated
// shallowly (type only) -- deep nested validation is not needed by any G1
// contract and stays out of scope.
func validateAutomationArg(payload map[string]any, field *ArgsField) error {
	value, exists := payload[field.Name]
	if !exists || value == nil {
		if !field.Optional {
			return &argBindError{Field: field.Name, Reason: "required field is missing from the event payload"}
		}
		return nil // optional + absent is fine
	}
	if !argTypeMatches(value, field.Type) {
		return &argBindError{Field: field.Name, Reason: fmt.Sprintf("expected %s, got %s", field.Type, argTypeName(value))}
	}
	if len(field.Enum) > 0 && !argInEnum(value, field.Enum) {
		return &argBindError{Field: field.Name, Reason: fmt.Sprintf("value %v is not one of the allowed values %v", value, field.Enum)}
	}
	if field.MaxLength > 0 {
		if s, ok := value.(string); ok && utf8.RuneCountInString(s) > field.MaxLength {
			return &argBindError{Field: field.Name, Reason: fmt.Sprintf("value too long (%d runes, max %d)", utf8.RuneCountInString(s), field.MaxLength)}
		}
	}
	if strings.TrimSpace(field.Pattern) != "" {
		if s, ok := value.(string); ok {
			if re := compilePattern(field.Pattern); re != nil && !re.MatchString(s) {
				return &argBindError{Field: field.Name, Reason: fmt.Sprintf("value %q does not match pattern %q", s, field.Pattern)}
			}
		}
	}
	return nil
}

// argTypeMatches reports whether value satisfies the declared arg type. The
// vocabulary matches the args-block field grammar (parseArgsBlockField) and
// the memql validator: string / number / int|integer / bool|boolean /
// object / array / any.
func argTypeMatches(value any, typ string) bool {
	switch typ {
	case "", "any":
		return true
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			return true
		}
		return false
	case "int", "integer":
		switch n := value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			return float64(int64(n)) == n
		case float32:
			return float32(int32(n)) == n
		}
		return false
	case "bool", "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		switch value.(type) {
		case []any, []map[string]any, []string:
			// []string: a Go emitter (the cockpit deploy command's
			// engineNodeTypes default) marshals typed slices; the contract
			// means "an array", not "an []any" (#2380 first live deploy).
			return true
		}
		return false
	default:
		// Unknown declared type: accept (the parser already constrains the
		// vocabulary; this only guards against a future additive type).
		return true
	}
}

// argTypeName returns a human-readable type name for an error message.
func argTypeName(value any) string {
	if value == nil {
		return "null"
	}
	switch value.(type) {
	case string:
		return "string"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return "number"
	case bool:
		return "bool"
	case map[string]any:
		return "object"
	case []any, []map[string]any:
		return "array"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// argInEnum reports whether value equals one of the allowed enum options.
// Enum options originate from @enum("a", "b") string literals; numeric
// equality is compared loosely so an int payload value matches a numeric
// enum option regardless of int/float representation.
func argInEnum(value any, options []any) bool {
	for _, opt := range options {
		if value == opt {
			return true
		}
		if s1, ok1 := value.(string); ok1 {
			if s2, ok2 := opt.(string); ok2 && s1 == s2 {
				return true
			}
		}
	}
	return false
}

// compilePattern compiles + caches a @pattern regex. A pattern that fails to
// compile yields nil (treated as no constraint) -- an invalid pattern is a
// parse/load concern, not a fire-time refusal cause.
func compilePattern(pattern string) *regexp.Regexp {
	if v, ok := patternCache.Load(pattern); ok {
		re, _ := v.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = nil
	}
	patternCache.Store(pattern, re)
	return re
}

var patternCache sync.Map // pattern string -> *regexp.Regexp (nil on compile error)

// fireRefusalCounter counts automations that refused to fire because their
// event payload violated the declared args contract (ADR Decision 2). It is a
// process-wide counter today; the load/run report machinery (#2357) will
// surface it -- per automation via CountFor -- once that lands.
type fireRefusalCounter struct {
	total  atomic.Int64
	mu     sync.Mutex
	byName map[string]int64
}

func (c *fireRefusalCounter) record(name string) {
	c.total.Add(1)
	c.mu.Lock()
	if c.byName == nil {
		c.byName = map[string]int64{}
	}
	c.byName[name]++
	c.mu.Unlock()
}

// Count returns the total number of fire refusals recorded process-wide.
func (c *fireRefusalCounter) Count() int64 { return c.total.Load() }

// CountFor returns the number of fire refusals recorded for one automation.
func (c *fireRefusalCounter) CountFor(name string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byName[name]
}

// refusedFires is the shared skip counter for args-contract fire refusals.
var refusedFires = &fireRefusalCounter{}

// refuseFireForArgs records the skip counter and emits the loud Warn for an
// automation whose event payload violated its args contract. `topic` names
// the triggering event (or the invoke-by-reference source) for the operator.
// Both the scheduler (before the @filter) and the executor (the universal
// entry gate) call this so the refusal reads identically everywhere.
func refuseFireForArgs(logger *slog.Logger, automation, topic string, err error) {
	field, reason := "", err.Error()
	var abe *argBindError
	if e, ok := err.(*argBindError); ok {
		abe = e
		field, reason = abe.Field, abe.Reason
	}
	refusedFires.record(automation)
	if logger != nil {
		logger.Warn("automation refused to fire: event payload violates its declared args contract (event-payload-binding ADR Decision 2, memql#2363)",
			"component", ComponentName,
			"automation", automation,
			"topic", topic,
			"field", field,
			"reason", reason,
		)
	}
}

package memql

import (
	"fmt"
	"time"
)

// date_builtins.go is the single name-keyed dispatch for the date/duration
// builtins (#2541): addDuration, daysBetween, subtractTimestamps, year,
// quarter, month, dayOfMonth, isAnniversary, isFirstDayOfQuarter. The Go
// implementations live on the RuntimeEvaluator (runtime_evaluator.go) and were
// previously reachable only from the mutation-template evaluator
// (mutation_templates.go). Logic bodies serialize these builtins to source
// text at the compile/re-parse boundary, so every position a logic evaluates
// them in (terminal return, step RHS, condition operand, arithmetic operand,
// collection-lambda comparison value) resolves operand VALUES first and then
// dispatches here by name.
//
// Like arithmetic (#2316), the date builtins are IN-MEMORY only: the AST
// converter admits them solely under WithCollectionMethods (logic bodies /
// collection lambdas) and they are never pushed into SQL.

// dateBuiltinArity maps each date/duration builtin to its argument count.
var dateBuiltinArity = map[string]int{
	"addDuration":         2,
	"daysBetween":         2,
	"subtractTimestamps":  2,
	"year":                1,
	"quarter":             1,
	"month":               1,
	"dayOfMonth":          1,
	"isAnniversary":       2,
	"isFirstDayOfQuarter": 1,
}

// IsDateBuiltin reports whether name is one of the date/duration builtins
// evaluated by EvaluateDateBuiltin.
func IsDateBuiltin(name string) bool {
	_, ok := dateBuiltinArity[name]
	return ok
}

// EvaluateDateBuiltin evaluates one date/duration builtin over
// already-resolved operand values. Operands are stringified before parsing so
// a step result or arg value carried as a string reaches the same parsers the
// mutation-template evaluator uses. Timestamps parse flexibly (RFC3339 /
// date-only -- the RuntimeEvaluator's parseDate set); addDuration mirrors the
// automations condition evaluator's #2256 semantics (flexible timestamp +
// ISO-8601 duration) rather than the strict-RFC3339 template path, so a
// date-only operand behaves identically in a condition and in a logic body.
func EvaluateDateBuiltin(name string, args []any) (any, error) {
	arity, ok := dateBuiltinArity[name]
	if !ok {
		return nil, fmt.Errorf("%s() is not a date/duration builtin", name)
	}
	if len(args) != arity {
		return nil, fmt.Errorf("%s() expects %d args, got %d", name, arity, len(args))
	}
	str := make([]string, len(args))
	for i, a := range args {
		if a == nil {
			return nil, fmt.Errorf("%s() arg %d resolved to nil", name, i)
		}
		str[i] = fmt.Sprintf("%v", a)
	}
	rt := NewRuntimeEvaluator(nil)
	switch name {
	case "addDuration":
		t, err := parseDate(str[0])
		if err != nil {
			return nil, fmt.Errorf("addDuration() invalid timestamp %q: %w", str[0], err)
		}
		d, err := parseISO8601Duration(str[1])
		if err != nil {
			return nil, fmt.Errorf("addDuration() invalid duration %q: %w", str[1], err)
		}
		return t.Add(d).Format(time.RFC3339), nil
	case "daysBetween":
		return rt.EvaluateDaysBetween(str[0], str[1])
	case "subtractTimestamps":
		t1, err := parseDate(str[0])
		if err != nil {
			return nil, fmt.Errorf("subtractTimestamps() invalid timestamp t1 %q: %w", str[0], err)
		}
		t2, err := parseDate(str[1])
		if err != nil {
			return nil, fmt.Errorf("subtractTimestamps() invalid timestamp t2 %q: %w", str[1], err)
		}
		return formatISO8601Duration(t1.Sub(t2)), nil
	case "year":
		return rt.EvaluateYear(str[0])
	case "quarter":
		return rt.EvaluateQuarter(str[0])
	case "month":
		return rt.EvaluateMonth(str[0])
	case "dayOfMonth":
		return rt.EvaluateDayOfMonth(str[0])
	case "isAnniversary":
		return rt.EvaluateIsAnniversary(str[0], str[1])
	case "isFirstDayOfQuarter":
		return rt.EvaluateIsFirstDayOfQuarter(str[0])
	}
	// Unreachable -- the arity table and this switch carry the same names.
	return nil, fmt.Errorf("date builtin %q has no evaluator", name)
}

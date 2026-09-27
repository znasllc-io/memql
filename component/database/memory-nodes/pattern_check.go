package memoryNodes

import (
	"fmt"
	"regexp"
)

// pattern_check.go -- `@pattern` is a regular expression, and the load says so
// (memql#5426).
//
// A concept field's pattern reaches the definition schema as the JSON-Schema
// `pattern` keyword, and the schema is compiled lazily, on the concept's first
// write (compileSchema). The compiler checks every pattern against the
// meta-schema's `format: regex` there -- Go's RE2 syntax, the same engine that
// validates a matching value -- so an invalid expression used to load clean and
// then refuse EVERY write to the concept with a compile error naming a JSON
// pointer, from the first write on. An args field's pattern was already
// compiled at load (component/memql's convertArgsField); this is the one check
// both receivers call, so the two halves of the language cannot answer the
// same `@pattern` differently.

// CodePatternInvalid is the stable rule id of a `@pattern` whose value is not a
// regular expression, on a concept field or an args field alike.
const CodePatternInvalid = "pattern_invalid"

// PatternError is the refusal of a `@pattern` that does not compile. It names
// the pattern as written, why it does not compile, and what to do; the owning
// field is named by the caller, which knows which receiver it is.
type PatternError struct {
	Pattern string
	Err     error
}

// Error renders the refusal with its rule id last, in brackets (D24).
func (e *PatternError) Error() string {
	return fmt.Sprintf("invalid @pattern %q: %v -- a pattern is a regular expression in Go's RE2 syntax, "+
		"which is also what validates a value against it; fix the expression or delete the annotation [%s]",
		e.Pattern, e.Err, CodePatternInvalid)
}

// Unwrap exposes the regexp package's own error.
func (e *PatternError) Unwrap() error { return e.Err }

// RuleCode is the refusal's stable rule id (baseloader.CodedRefusal).
func (e *PatternError) RuleCode() string { return CodePatternInvalid }

// CompilePattern compiles a `@pattern` value, refusing one that is not a
// regular expression with a *PatternError.
func CompilePattern(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, &PatternError{Pattern: pattern, Err: err}
	}
	return re, nil
}

package memql

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	// ErrEngineNotInitialized indicates the engine has not yet been initialized with concept metadata.
	ErrEngineNotInitialized = errors.New("memory engine not initialized")
	// ErrInvalidQuerySyntax indicates the provided query failed initial validation.
	ErrInvalidQuerySyntax = errors.New("invalid query syntax")
	// ErrEmptyQuery indicates the supplied MemQL string did not contain any expressions.
	ErrEmptyQuery = errors.New("query is empty")
	// ErrExecutionNotImplemented indicates execution is not yet available.
	ErrExecutionNotImplemented = errors.New("execution not implemented")
)

// ErrorCode is a fixed enumeration of structured error codes.
type ErrorCode string

const (
	// ErrorCodeValidationFailed indicates payload doesn't match schema.
	ErrorCodeValidationFailed ErrorCode = "VALIDATION_FAILED"
	// ErrorCodeMissingRequiredFields indicates required fields are absent.
	ErrorCodeMissingRequiredFields ErrorCode = "MISSING_REQUIRED_FIELDS"
	// ErrorCodeInvalidFieldType indicates a field has the wrong type.
	ErrorCodeInvalidFieldType ErrorCode = "INVALID_FIELD_TYPE"
	// ErrorCodeUnknownConcept indicates the concept is not registered.
	ErrorCodeUnknownConcept ErrorCode = "UNKNOWN_CONCEPT"
	// ErrorCodeUnknownFunction indicates the function is not found.
	ErrorCodeUnknownFunction ErrorCode = "UNKNOWN_FUNCTION"
	// ErrorCodeSyntaxError indicates query parse failure.
	ErrorCodeSyntaxError ErrorCode = "SYNTAX_ERROR"
	// ErrorCodeInvalidOperator indicates unknown comparison operator.
	ErrorCodeInvalidOperator ErrorCode = "INVALID_OPERATOR"
	// ErrorCodeRelationshipNotFound indicates relationship type not defined.
	ErrorCodeRelationshipNotFound ErrorCode = "RELATIONSHIP_NOT_FOUND"
	// ErrorCodeInvalidArgument indicates an invalid argument was provided.
	ErrorCodeInvalidArgument ErrorCode = "INVALID_ARGUMENT"
	// ErrorCodeNotFound indicates a requested resource was not found.
	ErrorCodeNotFound ErrorCode = "NOT_FOUND"
)

// suggestionTemplates maps error codes to static suggestion templates.
var suggestionTemplates = map[ErrorCode]string{
	ErrorCodeMissingRequiredFields: "Add the missing required fields: %s",
	ErrorCodeUnknownConcept:        "Check available concepts with: concepts()",
	ErrorCodeUnknownFunction:       "Check available functions with: functions()",
	ErrorCodeSyntaxError:           "Check MemQL syntax with: memqlDocs()",
	ErrorCodeInvalidOperator:       "Supported operators: ==, !=, >, >=, <, <=, in, not in, has",
	ErrorCodeRelationshipNotFound:  "Check concept relationships with: help(\"conceptName\")",
	ErrorCodeValidationFailed:      "Validate payload before insert with: validate(\"concept\", payload)",
	ErrorCodeInvalidFieldType:      "Check expected field types in the concept schema",
	ErrorCodeInvalidArgument:       "Check function signature with: help(\"%s\")",
	ErrorCodeNotFound:              "Verify the resource exists before querying",
}

// StructuredError provides machine-actionable error information.
type StructuredError struct {
	// ErrorType is a high-level error category.
	ErrorType string `json:"error"`
	// Code is the specific error code from the fixed set.
	Code ErrorCode `json:"code"`
	// Message is a human-readable description.
	Message string `json:"message"`
	// Details contains error-specific structured data.
	Details map[string]any `json:"details,omitempty"`
	// Suggestion provides recovery guidance.
	Suggestion *ErrorSuggestion `json:"suggestion,omitempty"`
	// Position is the character offset in the query where the error occurred.
	Position *int `json:"position,omitempty"`
	// Context is a query fragment around the error position.
	Context string `json:"context,omitempty"`
}

// ErrorSuggestion provides static recovery guidance.
type ErrorSuggestion struct {
	// Description explains how to fix the error.
	Description string `json:"description"`
	// Template is a MemQL template with placeholders.
	Template string `json:"template,omitempty"`
}

// Error implements the error interface.
func (e *StructuredError) Error() string {
	return e.Message
}

// ToMap converts the structured error to a map for serialization.
func (e *StructuredError) ToMap() map[string]any {
	result := map[string]any{
		"error":   e.ErrorType,
		"code":    string(e.Code),
		"message": e.Message,
	}
	if len(e.Details) > 0 {
		result["details"] = e.Details
	}
	if e.Suggestion != nil {
		suggestion := map[string]any{
			"description": e.Suggestion.Description,
		}
		if e.Suggestion.Template != "" {
			suggestion["template"] = e.Suggestion.Template
		}
		result["suggestion"] = suggestion
	}
	if e.Position != nil {
		result["position"] = *e.Position
	}
	if e.Context != "" {
		result["context"] = e.Context
	}
	return result
}

// ToJSON returns the JSON representation of the structured error.
func (e *StructuredError) ToJSON() string {
	b, _ := json.Marshal(e.ToMap())
	return string(b)
}

// ToMetadata converts the structured error to a flat metadata map for gRPC QueryError.
func (e *StructuredError) ToMetadata() map[string]string {
	meta := map[string]string{
		"error_type": e.ErrorType,
	}
	if len(e.Details) > 0 {
		if b, err := json.Marshal(e.Details); err == nil {
			meta["details"] = string(b)
		}
	}
	if e.Suggestion != nil {
		meta["suggestion_description"] = e.Suggestion.Description
		if e.Suggestion.Template != "" {
			meta["suggestion_template"] = e.Suggestion.Template
		}
	}
	if e.Position != nil {
		meta["position"] = fmt.Sprintf("%d", *e.Position)
	}
	if e.Context != "" {
		meta["context"] = e.Context
	}
	return meta
}

// NewStructuredError creates a new structured error with the given code and message.
func NewStructuredError(code ErrorCode, message string) *StructuredError {
	return &StructuredError{
		ErrorType: string(code),
		Code:      code,
		Message:   message,
	}
}

// WithDetails adds details to the structured error.
func (e *StructuredError) WithDetails(details map[string]any) *StructuredError {
	e.Details = details
	return e
}

// WithSuggestion adds a suggestion to the structured error.
func (e *StructuredError) WithSuggestion(description, template string) *StructuredError {
	e.Suggestion = &ErrorSuggestion{
		Description: description,
		Template:    template,
	}
	return e
}

// WithPosition adds position and context to the structured error.
func (e *StructuredError) WithPosition(pos int, context string) *StructuredError {
	e.Position = &pos
	e.Context = context
	return e
}

// WrapError converts a standard error into a structured error if possible.
// It uses pattern matching to identify known error types and enrich them.
func WrapError(err error) error {
	if err == nil {
		return nil
	}

	// If already a structured error, return as-is
	if se, ok := err.(*StructuredError); ok {
		return se
	}

	msg := err.Error()

	// Pattern: concept not found
	if strings.Contains(msg, "not found") && strings.Contains(msg, "concept") {
		concept := extractQuotedValue(msg)
		se := NewStructuredError(ErrorCodeUnknownConcept, msg)
		if concept != "" {
			se.WithDetails(map[string]any{"concept": concept})
		}
		se.WithSuggestion(
			"Check available concepts",
			"concepts()",
		)
		return se
	}

	// Pattern: function not found
	if strings.Contains(msg, "no function or tool found") {
		name := extractQuotedValue(msg)
		se := NewStructuredError(ErrorCodeUnknownFunction, msg)
		if name != "" {
			se.WithDetails(map[string]any{"name": name})
		}
		se.WithSuggestion(
			"Check available functions and tools",
			"functions()",
		)
		return se
	}

	// Pattern: relationship not found
	if strings.Contains(msg, "relationship") && (strings.Contains(msg, "not defined") || strings.Contains(msg, "not found") || strings.Contains(msg, "no ") || strings.Contains(msg, "does not define")) {
		relType := extractRelationshipType(msg)
		se := NewStructuredError(ErrorCodeRelationshipNotFound, msg)
		if relType != "" {
			se.WithDetails(map[string]any{"relationshipType": relType})
		}
		se.WithSuggestion(
			"Check concept relationship definitions",
			"concepts()",
		)
		return se
	}

	// Pattern: operator not supported
	if strings.Contains(msg, "operator") && (strings.Contains(msg, "not supported") || strings.Contains(msg, "unknown")) {
		op := extractQuotedValue(msg)
		se := NewStructuredError(ErrorCodeInvalidOperator, msg)
		if op != "" {
			se.WithDetails(map[string]any{"operator": op})
		}
		se.WithSuggestion(
			suggestionTemplates[ErrorCodeInvalidOperator],
			"",
		)
		return se
	}

	// Pattern: missing required argument
	if strings.Contains(msg, "requires") && strings.Contains(msg, "argument") {
		fn := extractFunctionName(msg)
		se := NewStructuredError(ErrorCodeInvalidArgument, msg)
		if fn != "" {
			se.WithDetails(map[string]any{"function": fn})
			se.WithSuggestion(
				fmt.Sprintf("Check function signature with: help(\"%s\")", fn),
				fmt.Sprintf("help(\"%s\")", fn),
			)
		}
		return se
	}

	// Pattern: requires field
	if strings.Contains(msg, "requires") && !strings.Contains(msg, "argument") {
		fn := extractFunctionName(msg)
		field := extractFieldName(msg)
		se := NewStructuredError(ErrorCodeInvalidArgument, msg)
		details := map[string]any{}
		if fn != "" {
			details["function"] = fn
		}
		if field != "" {
			details["field"] = field
		}
		if len(details) > 0 {
			se.WithDetails(details)
		}
		return se
	}

	// Pattern: type mismatch / wrong type
	if (strings.Contains(msg, "type") && (strings.Contains(msg, "expected") || strings.Contains(msg, "must be"))) ||
		(strings.Contains(msg, "expected") && strings.Contains(msg, "received")) ||
		(strings.Contains(msg, "must be a string")) {
		se := NewStructuredError(ErrorCodeInvalidFieldType, msg)
		se.WithSuggestion(
			suggestionTemplates[ErrorCodeInvalidFieldType],
			"",
		)
		return se
	}

	// Return original error if no pattern matches
	return err
}

// Helper function to extract quoted values from error messages.
func extractQuotedValue(msg string) string {
	re := regexp.MustCompile(`"([^"]+)"`)
	matches := re.FindStringSubmatch(msg)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// Helper function to extract relationship type from error messages.
func extractRelationshipType(msg string) string {
	// Look for patterns like "no parentOf relationship" or "interactsWith relationship not defined"
	types := []string{"parentOf", "childOf", "contains", "owns", "createdBy", "aliasOf", "equals", "interactsWith"}
	for _, t := range types {
		if strings.Contains(msg, t) {
			return t
		}
	}
	return extractQuotedValue(msg)
}

// Helper function to extract function name from error messages.
func extractFunctionName(msg string) string {
	// Look for patterns like "validate() requires" or "help() requires"
	re := regexp.MustCompile(`(\w+)\(\)`)
	matches := re.FindStringSubmatch(msg)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// Helper function to extract field name from error messages.
func extractFieldName(msg string) string {
	// Look for patterns like "'concept' field" or "'payload' field"
	re := regexp.MustCompile(`'(\w+)'`)
	matches := re.FindStringSubmatch(msg)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

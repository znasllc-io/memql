package memql

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestNewStructuredError(t *testing.T) {
	se := NewStructuredError(ErrorCodeUnknownConcept, "concept not found")

	if se.Code != ErrorCodeUnknownConcept {
		t.Errorf("expected code %s, got %s", ErrorCodeUnknownConcept, se.Code)
	}
	if se.ErrorType != string(ErrorCodeUnknownConcept) {
		t.Errorf("expected error type %s, got %s", ErrorCodeUnknownConcept, se.ErrorType)
	}
	if se.Message != "concept not found" {
		t.Errorf("expected message 'concept not found', got %s", se.Message)
	}
	if se.Error() != "concept not found" {
		t.Errorf("Error() should return message")
	}
}

func TestStructuredErrorWithDetails(t *testing.T) {
	se := NewStructuredError(ErrorCodeUnknownConcept, "concept not found").
		WithDetails(map[string]any{"concept": "v1:test:concept"})

	if se.Details["concept"] != "v1:test:concept" {
		t.Errorf("expected concept detail, got %v", se.Details)
	}
}

func TestStructuredErrorWithSuggestion(t *testing.T) {
	se := NewStructuredError(ErrorCodeUnknownConcept, "concept not found").
		WithSuggestion("Check available concepts", "concepts()")

	if se.Suggestion == nil {
		t.Fatal("expected suggestion to be set")
	}
	if se.Suggestion.Description != "Check available concepts" {
		t.Errorf("expected description, got %s", se.Suggestion.Description)
	}
	if se.Suggestion.Template != "concepts()" {
		t.Errorf("expected template, got %s", se.Suggestion.Template)
	}
}

func TestStructuredErrorWithPosition(t *testing.T) {
	se := NewStructuredError(ErrorCodeSyntaxError, "unexpected token").
		WithPosition(42, "concept==test;badtoken")

	if se.Position == nil || *se.Position != 42 {
		t.Errorf("expected position 42, got %v", se.Position)
	}
	if se.Context != "concept==test;badtoken" {
		t.Errorf("expected context, got %s", se.Context)
	}
}

func TestStructuredErrorToMap(t *testing.T) {
	se := NewStructuredError(ErrorCodeUnknownConcept, "concept \"v1:test\" not found").
		WithDetails(map[string]any{"concept": "v1:test"}).
		WithSuggestion("Check available concepts", "concepts()")

	m := se.ToMap()

	if m["error"] != string(ErrorCodeUnknownConcept) {
		t.Errorf("expected error field, got %v", m["error"])
	}
	if m["code"] != string(ErrorCodeUnknownConcept) {
		t.Errorf("expected code field, got %v", m["code"])
	}
	if m["message"] != "concept \"v1:test\" not found" {
		t.Errorf("expected message field, got %v", m["message"])
	}

	details, ok := m["details"].(map[string]any)
	if !ok {
		t.Fatalf("expected details map, got %T", m["details"])
	}
	if details["concept"] != "v1:test" {
		t.Errorf("expected concept in details, got %v", details)
	}

	suggestion, ok := m["suggestion"].(map[string]any)
	if !ok {
		t.Fatalf("expected suggestion map, got %T", m["suggestion"])
	}
	if suggestion["description"] != "Check available concepts" {
		t.Errorf("expected suggestion description, got %v", suggestion)
	}
	if suggestion["template"] != "concepts()" {
		t.Errorf("expected suggestion template, got %v", suggestion)
	}
}

func TestStructuredErrorToJSON(t *testing.T) {
	se := NewStructuredError(ErrorCodeUnknownConcept, "concept not found").
		WithDetails(map[string]any{"concept": "v1:test"})

	jsonStr := se.ToJSON()

	var parsed map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		t.Fatalf("ToJSON produced invalid JSON: %v", err)
	}

	if parsed["code"] != string(ErrorCodeUnknownConcept) {
		t.Errorf("expected code in JSON, got %v", parsed["code"])
	}
}

func TestStructuredErrorToMetadata(t *testing.T) {
	pos := 42
	se := &StructuredError{
		ErrorType: string(ErrorCodeUnknownConcept),
		Code:      ErrorCodeUnknownConcept,
		Message:   "concept not found",
		Details:   map[string]any{"concept": "v1:test"},
		Suggestion: &ErrorSuggestion{
			Description: "Check available concepts",
			Template:    "concepts()",
		},
		Position: &pos,
		Context:  "badquery",
	}

	meta := se.ToMetadata()

	if meta["error_type"] != string(ErrorCodeUnknownConcept) {
		t.Errorf("expected error_type in metadata")
	}
	if meta["suggestion_description"] != "Check available concepts" {
		t.Errorf("expected suggestion_description in metadata")
	}
	if meta["suggestion_template"] != "concepts()" {
		t.Errorf("expected suggestion_template in metadata")
	}
	if meta["position"] != "42" {
		t.Errorf("expected position in metadata, got %s", meta["position"])
	}
	if meta["context"] != "badquery" {
		t.Errorf("expected context in metadata")
	}
	if meta["details"] == "" {
		t.Errorf("expected details in metadata")
	}
}

func TestWrapError_Nil(t *testing.T) {
	if WrapError(nil) != nil {
		t.Error("WrapError(nil) should return nil")
	}
}

func TestWrapError_AlreadyStructured(t *testing.T) {
	se := NewStructuredError(ErrorCodeUnknownConcept, "already structured")
	wrapped := WrapError(se)

	if wrapped != se {
		t.Error("WrapError should return already-structured errors as-is")
	}
}

func TestWrapError_ConceptNotFound(t *testing.T) {
	err := fmt.Errorf("concept \"v1:crm:lead\" not found")
	wrapped := WrapError(err)

	se, ok := wrapped.(*StructuredError)
	if !ok {
		t.Fatalf("expected StructuredError, got %T", wrapped)
	}
	if se.Code != ErrorCodeUnknownConcept {
		t.Errorf("expected UNKNOWN_CONCEPT code, got %s", se.Code)
	}
	if se.Details["concept"] != "v1:crm:lead" {
		t.Errorf("expected concept in details, got %v", se.Details)
	}
	if se.Suggestion == nil {
		t.Error("expected suggestion to be set")
	}
}

func TestWrapError_FunctionNotFound(t *testing.T) {
	err := fmt.Errorf("no function or tool found with name \"badFunc\"")
	wrapped := WrapError(err)

	se, ok := wrapped.(*StructuredError)
	if !ok {
		t.Fatalf("expected StructuredError, got %T", wrapped)
	}
	if se.Code != ErrorCodeUnknownFunction {
		t.Errorf("expected UNKNOWN_FUNCTION code, got %s", se.Code)
	}
	if se.Details["name"] != "badFunc" {
		t.Errorf("expected name in details, got %v", se.Details)
	}
}

func TestWrapError_RelationshipNotFound(t *testing.T) {
	tests := []struct {
		errMsg  string
		relType string
	}{
		{`concept "v1:test" has no parentOf relationship definitions`, "parentOf"},
		{`concept "v1:test" does not define a 'contains' relationship required by contains()`, "contains"},
		{`relationship type "custom" not defined`, ""},
	}

	for _, tc := range tests {
		err := errors.New(tc.errMsg)
		wrapped := WrapError(err)

		se, ok := wrapped.(*StructuredError)
		if !ok {
			t.Fatalf("expected StructuredError for %q, got %T", tc.errMsg, wrapped)
		}
		if se.Code != ErrorCodeRelationshipNotFound {
			t.Errorf("expected RELATIONSHIP_NOT_FOUND code for %q, got %s", tc.errMsg, se.Code)
		}
		if tc.relType != "" && se.Details["relationshipType"] != tc.relType {
			t.Errorf("expected relationshipType %q in details for %q, got %v", tc.relType, tc.errMsg, se.Details)
		}
	}
}

func TestWrapError_OperatorNotSupported(t *testing.T) {
	err := fmt.Errorf("operator \"=bad=\" is not supported for concept filters")
	wrapped := WrapError(err)

	se, ok := wrapped.(*StructuredError)
	if !ok {
		t.Fatalf("expected StructuredError, got %T", wrapped)
	}
	if se.Code != ErrorCodeInvalidOperator {
		t.Errorf("expected INVALID_OPERATOR code, got %s", se.Code)
	}
	if se.Details["operator"] != "=bad=" {
		t.Errorf("expected operator in details, got %v", se.Details)
	}
}

func TestWrapError_RequiresArgument(t *testing.T) {
	err := fmt.Errorf("help() requires 'name' argument")
	wrapped := WrapError(err)

	se, ok := wrapped.(*StructuredError)
	if !ok {
		t.Fatalf("expected StructuredError, got %T", wrapped)
	}
	if se.Code != ErrorCodeInvalidArgument {
		t.Errorf("expected INVALID_ARGUMENT code, got %s", se.Code)
	}
	if se.Details["function"] != "help" {
		t.Errorf("expected function in details, got %v", se.Details)
	}
}

func TestWrapError_RequiresField(t *testing.T) {
	err := fmt.Errorf("validate() requires 'concept' field as a string")
	wrapped := WrapError(err)

	se, ok := wrapped.(*StructuredError)
	if !ok {
		t.Fatalf("expected StructuredError, got %T", wrapped)
	}
	if se.Code != ErrorCodeInvalidArgument {
		t.Errorf("expected INVALID_ARGUMENT code, got %s", se.Code)
	}
	if se.Details["function"] != "validate" {
		t.Errorf("expected function in details, got %v", se.Details)
	}
	if se.Details["field"] != "concept" {
		t.Errorf("expected field in details, got %v", se.Details)
	}
}

func TestWrapError_TypeMismatch(t *testing.T) {
	tests := []string{
		"expected string, received int",
		"payload path must be a string-compatible value",
	}

	for _, msg := range tests {
		err := errors.New(msg)
		wrapped := WrapError(err)

		se, ok := wrapped.(*StructuredError)
		if !ok {
			t.Fatalf("expected StructuredError for %q, got %T", msg, wrapped)
		}
		if se.Code != ErrorCodeInvalidFieldType {
			t.Errorf("expected INVALID_FIELD_TYPE code for %q, got %s", msg, se.Code)
		}
	}
}

func TestWrapError_UnknownPattern(t *testing.T) {
	err := errors.New("some random error message")
	wrapped := WrapError(err)

	// Should return original error unchanged
	if wrapped != err {
		t.Error("unknown patterns should return original error")
	}
}

func TestErrorCodes(t *testing.T) {
	// Verify all error codes are unique
	codes := []ErrorCode{
		ErrorCodeValidationFailed,
		ErrorCodeMissingRequiredFields,
		ErrorCodeInvalidFieldType,
		ErrorCodeUnknownConcept,
		ErrorCodeUnknownFunction,
		ErrorCodeSyntaxError,
		ErrorCodeInvalidOperator,
		ErrorCodeRelationshipNotFound,
		ErrorCodeInvalidArgument,
		ErrorCodeNotFound,
	}

	seen := make(map[ErrorCode]bool)
	for _, code := range codes {
		if seen[code] {
			t.Errorf("duplicate error code: %s", code)
		}
		seen[code] = true
	}
}

func TestSuggestionTemplatesExist(t *testing.T) {
	// Verify core error codes have suggestion templates
	requiredCodes := []ErrorCode{
		ErrorCodeMissingRequiredFields,
		ErrorCodeUnknownConcept,
		ErrorCodeUnknownFunction,
		ErrorCodeSyntaxError,
		ErrorCodeInvalidOperator,
		ErrorCodeRelationshipNotFound,
	}

	for _, code := range requiredCodes {
		if _, ok := suggestionTemplates[code]; !ok {
			t.Errorf("missing suggestion template for code: %s", code)
		}
	}
}

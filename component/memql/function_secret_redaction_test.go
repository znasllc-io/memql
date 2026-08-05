package memql

import (
	"regexp"
	"strings"
	"testing"
)

// function_secret_redaction_test.go is the enforcement half of memql#3036.
//
// The ruling: `@secret` redacts from VALIDATION ERROR MESSAGES. Not from
// structured logs (no log site carries a concept row -- measured), not from
// query results (an authorization decision deferred under #2803).
//
// These tests drive the redaction itself, not the presence of the x-secret
// key. That distinction is the whole point of the issue: `x-secret` was
// emitted and read by nothing, so a test asserting the key exists would have
// passed against the defect it is meant to catch.

const secretValue = "sk-live-DO-NOT-LEAK-0123456789"

// mustNotLeak fails when the secret's value appears anywhere in the message.
func mustNotLeak(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a validation error, got nil")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatalf("the @secret value leaked into a validation error message:\n  %s", err.Error())
	}
	if !strings.Contains(err.Error(), redactedArgValue) {
		t.Fatalf("expected the redaction placeholder %q in the message, got:\n  %s", redactedArgValue, err.Error())
	}
}

// Every value-quoting site in the validator, driven through the public
// validateFunctionArgs entry point rather than the leaf helper -- a leaf-level
// test is exactly the bypass that let #3043's dead rules look covered.
func TestSecretArgValueIsRedactedFromValidationErrors(t *testing.T) {
	min, max := 1.0, 5.0
	for _, tc := range []struct {
		name  string
		field *FunctionArgsField
		value any
	}{
		{
			name:  "enum",
			field: &FunctionArgsField{Name: "apiKey", Type: "string", Secret: true, Enum: []any{"a", "b"}},
			value: secretValue,
		},
		{
			name:  "pattern",
			field: &FunctionArgsField{Name: "apiKey", Type: "string", Secret: true, Pattern: "^tok_", patternRegex: regexp.MustCompile("^tok_")},
			value: secretValue,
		},
		{
			name:  "date-time format",
			field: &FunctionArgsField{Name: "apiKey", Type: "string", Secret: true, Format: "date-time"},
			value: secretValue,
		},
		{
			name:  "minimum",
			field: &FunctionArgsField{Name: "apiKey", Type: "number", Secret: true, Minimum: &min},
			value: -999.0,
		},
		{
			name:  "maximum",
			field: &FunctionArgsField{Name: "apiKey", Type: "number", Secret: true, Maximum: &max},
			value: 999.0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &functionValidator{}
			fn := &Function{Name: "storeCredential", ArgsSchema: &ArgsSchemaConfig{Fields: []*FunctionArgsField{tc.field}}}
			err := v.validateFunctionArgs(fn, map[string]any{"apiKey": tc.value})
			mustNotLeak(t, err)
			// The field NAME must survive -- a message that redacts which
			// argument failed is useless, and redaction is only defensible
			// because the diagnostic stays actionable.
			if !strings.Contains(err.Error(), `"apiKey"`) {
				t.Fatalf("the argument name must survive redaction, got:\n  %s", err.Error())
			}
		})
	}
}

// The numeric sites redact the offending VALUE while keeping the declared
// bound, which comes from the schema and is not secret data.
func TestSecretRedactionKeepsDeclaredSchemaFacts(t *testing.T) {
	min := 1.0
	v := &functionValidator{}
	fn := &Function{Name: "storeCredential", ArgsSchema: &ArgsSchemaConfig{
		Fields: []*FunctionArgsField{{Name: "apiKey", Type: "number", Secret: true, Minimum: &min}},
	}}
	err := v.validateFunctionArgs(fn, map[string]any{"apiKey": -999.0})
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if strings.Contains(err.Error(), "-999") {
		t.Fatalf("the secret value leaked: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "minimum 1") {
		t.Fatalf("the declared minimum is schema, not secret data, and must survive: %s", err.Error())
	}
}

// The control: an UNANNOTATED field keeps quoting its value. Without this the
// suite would pass just as well if redaction were applied to everything, which
// would destroy every validation diagnostic in the engine.
func TestNonSecretArgValueIsStillQuoted(t *testing.T) {
	v := &functionValidator{}
	fn := &Function{Name: "storeCredential", ArgsSchema: &ArgsSchemaConfig{
		Fields: []*FunctionArgsField{{Name: "label", Type: "string", Enum: []any{"a", "b"}}},
	}}
	err := v.validateFunctionArgs(fn, map[string]any{"label": "zzz"})
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if !strings.Contains(err.Error(), "zzz") {
		t.Fatalf("a non-secret value must still appear in the message, got:\n  %s", err.Error())
	}
	if strings.Contains(err.Error(), redactedArgValue) {
		t.Fatalf("a non-secret field must not be redacted, got:\n  %s", err.Error())
	}
}

// Nested and array element fields carry their own Secret flag through the
// recursion, so a secret inside an object arg is redacted too.
func TestSecretRedactionAppliesToNestedFields(t *testing.T) {
	v := &functionValidator{}
	fn := &Function{Name: "storeCredential", ArgsSchema: &ArgsSchemaConfig{
		Fields: []*FunctionArgsField{{
			Name: "creds",
			Type: "object",
			Nested: []*FunctionArgsField{
				{Name: "apiKey", Type: "string", Secret: true, Enum: []any{"a"}},
			},
		}},
	}}
	err := v.validateFunctionArgs(fn, map[string]any{
		"creds": map[string]any{"apiKey": secretValue},
	})
	mustNotLeak(t, err)
}

// clone() must carry Secret. Array item fields are cloned per element
// (validateArgsField -> field.Items.clone()), so a dropped flag there would
// silently un-redact every element of an array of secrets.
func TestSecretSurvivesArgsFieldClone(t *testing.T) {
	original := &FunctionArgsField{Name: "apiKey", Type: "string", Secret: true}
	if !original.clone().Secret {
		t.Fatal("clone() dropped Secret -- array element validation would un-redact")
	}

	v := &functionValidator{}
	fn := &Function{Name: "storeCredential", ArgsSchema: &ArgsSchemaConfig{
		Fields: []*FunctionArgsField{{
			Name:  "keys",
			Type:  "array",
			Items: &FunctionArgsField{Type: "string", Secret: true, Enum: []any{"a"}},
		}},
	}}
	err := v.validateFunctionArgs(fn, map[string]any{"keys": []any{secretValue}})
	mustNotLeak(t, err)
}

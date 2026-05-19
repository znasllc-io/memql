package memql

import (
	"strings"
	"testing"
)

// validateCognitionActionUtterancePayload enforces shape rules on
// action-type utterances. Non-action utterances pass through; action
// utterances must carry a typed action object with a serializable
// payload under a size cap.

func TestValidateCognitionActionUtterancePayload_NonActionPassthrough(t *testing.T) {
	cases := []map[string]any{
		{"utteranceType": "text", "text": "hello"},
		{"utteranceType": "system_event"},
		{"utteranceType": ""}, // no type at all -- not an action
		{},                    // empty payload, no utteranceType
	}
	for i, payload := range cases {
		if err := validateCognitionActionUtterancePayload(payload); err != nil {
			t.Errorf("case %d: expected nil error, got %v", i, err)
		}
	}
}

func TestValidateCognitionActionUtterancePayload_NilRejected(t *testing.T) {
	if err := validateCognitionActionUtterancePayload(nil); err == nil {
		t.Fatal("expected error for nil payload, got nil")
	}
}

func TestValidateCognitionActionUtterancePayload_ActionRequiresActionObject(t *testing.T) {
	payload := map[string]any{"utteranceType": "action"}
	err := validateCognitionActionUtterancePayload(payload)
	if err == nil {
		t.Fatal("expected error for missing action object, got nil")
	}
	if !strings.Contains(err.Error(), `"action" object`) {
		t.Errorf("error should describe missing action object, got %v", err)
	}
}

func TestValidateCognitionActionUtterancePayload_ActionMustBeObject(t *testing.T) {
	payload := map[string]any{
		"utteranceType": "action",
		"action":        "not-an-object",
	}
	err := validateCognitionActionUtterancePayload(payload)
	if err == nil {
		t.Fatal("expected error for non-object action, got nil")
	}
	if !strings.Contains(err.Error(), `must be an object`) {
		t.Errorf("error should describe type mismatch, got %v", err)
	}
}

func TestValidateCognitionActionUtterancePayload_ActionTypeRequired(t *testing.T) {
	payload := map[string]any{
		"utteranceType": "action",
		"action": map[string]any{
			"payload": map[string]any{"foo": "bar"},
		},
	}
	err := validateCognitionActionUtterancePayload(payload)
	if err == nil {
		t.Fatal("expected error for missing action.type, got nil")
	}
	if !strings.Contains(err.Error(), "action.type") {
		t.Errorf("error should mention action.type, got %v", err)
	}
}

func TestValidateCognitionActionUtterancePayload_ActionPayloadRequired(t *testing.T) {
	payload := map[string]any{
		"utteranceType": "action",
		"action": map[string]any{
			"type": "ui_click",
		},
	}
	err := validateCognitionActionUtterancePayload(payload)
	if err == nil {
		t.Fatal("expected error for missing action.payload, got nil")
	}
	if !strings.Contains(err.Error(), "action.payload") {
		t.Errorf("error should mention action.payload, got %v", err)
	}
}

func TestValidateCognitionActionUtterancePayload_PayloadSizeCapEnforced(t *testing.T) {
	// Build an action payload bigger than the default 64 KB cap.
	big := make(map[string]any, 5000)
	for i := 0; i < 5000; i++ {
		big["k"+strings.Repeat("x", 20)+itoa(i)] = strings.Repeat("v", 50)
	}
	payload := map[string]any{
		"utteranceType": "action",
		"action": map[string]any{
			"type":    "ui_click",
			"payload": big,
		},
	}
	err := validateCognitionActionUtterancePayload(payload)
	if err == nil {
		t.Fatal("expected size-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error should mention size cap, got %v", err)
	}
}

func TestValidateCognitionActionUtterancePayload_GoldenPath(t *testing.T) {
	payload := map[string]any{
		"utteranceType": "action",
		"action": map[string]any{
			"type":    "ui_click",
			"payload": map[string]any{"target": "#submit"},
		},
	}
	if err := validateCognitionActionUtterancePayload(payload); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestHasAnyPrefix(t *testing.T) {
	if !hasAnyPrefix("ui_click", []string{"ui_", "fs_"}) {
		t.Error("ui_click should match ui_ prefix")
	}
	if hasAnyPrefix("network_request", []string{"ui_", "fs_"}) {
		t.Error("network_request should not match")
	}
	if hasAnyPrefix("", []string{"ui_"}) {
		t.Error("empty input should not match")
	}
	if hasAnyPrefix("ui_click", nil) {
		t.Error("nil prefix list should not match")
	}
}

func TestStringFromAny(t *testing.T) {
	if stringFromAny(nil) != "" {
		t.Error("nil should yield empty string")
	}
	if stringFromAny("hello") != "hello" {
		t.Error("string passthrough failed")
	}
	if stringFromAny(42) != "42" {
		t.Error("int should stringify")
	}
}

// itoa is a tiny helper used by the size-cap test (avoiding strconv
// in the test sources keeps the imports minimal).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(b[pos:])
}

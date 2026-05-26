package safety

import "testing"

func TestIsSecretKey(t *testing.T) {
	cases := map[string]bool{
		"Authorization": true,
		"AUTHORIZATION": true,
		"authorization": true,
		"X-API-Token":   true,
		"apiKey":        true,
		"API_KEY":       true,
		"PASSWORD":      true,
		"passwd":        true,
		"PRIVATE_KEY":   true,
		"privateKey":    true,
		"OAUTH_SECRET":  true,
		"safe":          false,
		"name":          false,
		"hostname":      false,
		"":              false,
	}
	for k, want := range cases {
		if got := IsSecretKey(k); got != want {
			t.Errorf("IsSecretKey(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestRedactArgsNested(t *testing.T) {
	in := map[string]any{
		"safe":          "ok",
		"Authorization": "Bearer abc",
		"nested": map[string]any{
			"apiKey": "xyz",
			"keep":   42,
		},
	}
	out := RedactArgs(in)
	if out["safe"] != "ok" {
		t.Errorf("non-secret mutated: %v", out["safe"])
	}
	if out["Authorization"] != "[REDACTED]" {
		t.Errorf("top-level secret not redacted: %v", out["Authorization"])
	}
	nested, ok := out["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested missing or wrong type: %T", out["nested"])
	}
	if nested["apiKey"] != "[REDACTED]" {
		t.Errorf("nested secret not redacted: %v", nested["apiKey"])
	}
	if nested["keep"] != 42 {
		t.Errorf("nested non-secret mutated: %v", nested["keep"])
	}
	// Aliasing guarantee: mutating the output must not touch the input.
	out["safe"] = "tampered"
	if in["safe"] != "ok" {
		t.Error("RedactArgs returned aliased map -- output mutation leaked to input")
	}
}

func TestRedactArgsEmpty(t *testing.T) {
	if got := RedactArgs(nil); got != nil {
		t.Errorf("nil input should yield nil, got %v", got)
	}
	if got := RedactArgs(map[string]any{}); got != nil {
		t.Errorf("empty input should yield nil, got %v", got)
	}
}

func TestRedactedPayloadBodyScrubbed(t *testing.T) {
	p := Payload{
		Command: "echo hi",
		URL:     "https://example.com",
		Body:    `{"token":"abc"}`,
		Args:    map[string]any{"apiKey": "xyz", "ok": 1},
	}
	out := RedactedPayload(p)
	if out.Body != "[REDACTED]" {
		t.Errorf("Body not redacted: %q", out.Body)
	}
	if out.Command != "echo hi" || out.URL != "https://example.com" {
		t.Errorf("pass-through fields mutated: %+v", out)
	}
	if out.Args["apiKey"] != "[REDACTED]" {
		t.Errorf("Args secret not redacted: %v", out.Args["apiKey"])
	}
}

func TestRedactedPayloadEmptyBodyUnchanged(t *testing.T) {
	p := Payload{Command: "ls"}
	out := RedactedPayload(p)
	if out.Body != "" {
		t.Errorf("empty body should stay empty, got %q", out.Body)
	}
}

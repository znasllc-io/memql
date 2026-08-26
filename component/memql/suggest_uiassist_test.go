package memql

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"text/template"
)

// The `uiAssist` suggest domain (memql#4654).
//
// Three things are worth pinning here and the third is the reason the file
// exists: that the domain is REACHABLE under its wire name, that its
// instruction actually renders from .memql rather than from a Go string, and
// that a field outside the caller's scope cannot survive the round trip. The
// last one is not a quality question -- a patch is applied to a form somebody
// is about to submit, so an invented field is a value written into something
// the caller never offered.

func uiAssistScope() map[string]any {
	return map[string]any{
		"scope": map[string]any{
			"id":    "fleet.addMachine",
			"label": "Add a machine",
			"fields": []any{
				map[string]any{"name": "displayName", "type": "text", "label": "Name", "value": ""},
				map[string]any{
					"name":        "labels",
					"type":        "text",
					"label":       "Labels",
					"value":       "",
					"constraints": "comma separated",
				},
			},
		},
		"prompt": "call it the studio mac and label it macos, gpu",
		"page":   "Fleet, Machines",
	}
}

func renderingTo(out string) func(string, map[string]any) (string, error) {
	return func(string, map[string]any) (string, error) { return out, nil }
}

func TestUIAssistDomainIsRegisteredUnderItsWireName(t *testing.T) {
	// Registered from init(), which is the whole contract: the gRPC handler
	// looks the wire `domain` string up and has no switch to add a case to.
	if LookupSuggestDomain("uiAssist") == nil {
		t.Fatal("uiAssist is not in the suggest-domain registry -- AiSuggestMsg with " +
			"domain \"uiAssist\" would come back as the typed unsupported-domain error")
	}
	// And it is in the list the unsupported-domain error prints, so a client
	// that misspells the domain is told what this binary does carry.
	found := false
	for _, d := range RegisteredSuggestDomains() {
		if d == "uiAssist" {
			found = true
		}
	}
	if !found {
		t.Errorf("uiAssist missing from RegisteredSuggestDomains(): %v", RegisteredSuggestDomains())
	}
}

func TestUIAssistRefusesARequestItCannotAnswer(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantMsg string
	}{
		{"no prompt", func(p map[string]any) { p["prompt"] = "   " }, "prompt is required"},
		{"no scope", func(p map[string]any) { delete(p, "scope") }, "scope is required"},
		{"scope with no fields", func(p map[string]any) {
			p["scope"] = map[string]any{"id": "x", "fields": []any{}}
		}, "at least one field"},
		{"fields with no names", func(p map[string]any) {
			p["scope"] = map[string]any{"id": "x", "fields": []any{map[string]any{"type": "text"}}}
		}, "at least one field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := uiAssistScope()
			tc.mutate(payload)
			_, err := buildUIAssistSuggest(SuggestContext{
				Payload:      payload,
				RenderPrompt: renderingTo("rendered"),
			})
			var ve *SuggestValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want a SuggestValidationError (which the handler turns into "+
					"InvalidArgument rather than into a 500), got %v", err)
			}
			if !strings.Contains(ve.Message, tc.wantMsg) {
				t.Errorf("message %q does not mention %q", ve.Message, tc.wantMsg)
			}
		})
	}
}

func TestUIAssistSaysSoWhenItCannotReachItsPrompt(t *testing.T) {
	// NOT a validation error: the caller's request was fine, this node simply
	// has no AI runtime. Reporting it as InvalidArgument would send somebody
	// to check their payload for a problem that is not in it.
	_, err := buildUIAssistSuggest(SuggestContext{Payload: uiAssistScope()})
	if err == nil {
		t.Fatal("want an error when no prompt renderer is available")
	}
	var ve *SuggestValidationError
	if errors.As(err, &ve) {
		t.Fatal("a missing prompt renderer must not read as a bad request")
	}
	if !strings.Contains(err.Error(), "prompt renderer") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

func TestUIAssistBuildsItsPlanFromTheRenderedPrompt(t *testing.T) {
	var gotID string
	var gotData map[string]any
	plan, err := buildUIAssistSuggest(SuggestContext{
		Payload: uiAssistScope(),
		RenderPrompt: func(id string, data map[string]any) (string, error) {
			gotID, gotData = id, data
			return "THE RENDERED INSTRUCTION", nil
		},
	})
	if err != nil {
		t.Fatalf("buildUIAssistSuggest: %v", err)
	}
	if gotID != "uiAssistFill" {
		t.Errorf("rendered prompt id = %q, want uiAssistFill", gotID)
	}
	// The scope reaches the template as DATA, which is what keeps the domain
	// product-neutral: nothing in the engine knows what these fields mean.
	scope, _ := gotData["scope"].(map[string]any)
	fields, _ := scope["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("template got %d fields, want 2", len(fields))
	}
	if len(plan.Messages) != 2 || plan.Messages[0].Role != "system" || plan.Messages[1].Role != "user" {
		t.Fatalf("want system+user, got %+v", plan.Messages)
	}
	if plan.Messages[0].Content != "THE RENDERED INSTRUCTION" {
		t.Errorf("the system message is not the rendered template: %q", plan.Messages[0].Content)
	}
	if !strings.Contains(plan.Messages[1].Content, "studio mac") {
		t.Errorf("the user message is not the person's own words: %q", plan.Messages[1].Content)
	}
	if plan.SchemaName == "" || len(plan.Schema) == 0 || plan.PostProcess == nil {
		t.Error("a plan without a schema or a post-process would let raw model output through")
	}
}

func TestUIAssistDropsAPatchTheCallerNeverOffered(t *testing.T) {
	plan, err := buildUIAssistSuggest(SuggestContext{
		Payload:      uiAssistScope(),
		RenderPrompt: renderingTo("rendered"),
	})
	if err != nil {
		t.Fatalf("buildUIAssistSuggest: %v", err)
	}
	suggestion := map[string]any{
		"patches": []any{
			map[string]any{"field": "displayName", "value": "  Studio Mac  "},
			// The failure this exists for: a field nobody described. Applied,
			// it writes a value into a form the caller did not offer.
			map[string]any{"field": "workerToken", "value": "mql_wkr_pretend"},
			map[string]any{"field": "", "value": "nameless"},
			"not even an object",
		},
		"note": "  ok  ",
	}
	plan.PostProcess(suggestion)

	patches, _ := suggestion["patches"].([]any)
	if len(patches) != 1 {
		t.Fatalf("want exactly the one in-scope patch, got %d: %+v", len(patches), patches)
	}
	kept, _ := patches[0].(map[string]any)
	if kept["field"] != "displayName" || kept["value"] != "Studio Mac" {
		t.Errorf("kept patch is %+v; want the trimmed displayName", kept)
	}
	if suggestion["note"] != "ok" {
		t.Errorf("note = %q, want it trimmed", suggestion["note"])
	}
}

func TestUIAssistAlwaysLeavesAPatchesKey(t *testing.T) {
	plan, err := buildUIAssistSuggest(SuggestContext{
		Payload:      uiAssistScope(),
		RenderPrompt: renderingTo("rendered"),
	})
	if err != nil {
		t.Fatalf("buildUIAssistSuggest: %v", err)
	}
	// "The model returned nothing" and "the key is missing" have the same
	// remedy for a caller and different code, so the reply never makes them
	// tell the difference.
	suggestion := map[string]any{}
	plan.PostProcess(suggestion)
	patches, ok := suggestion["patches"].([]any)
	if !ok {
		t.Fatal("patches must always be present, even when empty")
	}
	if len(patches) != 0 {
		t.Errorf("want an empty list, got %+v", patches)
	}
}

// The DSL half: the prompt this domain names must actually exist, carry a
// template, and render against the data the handler hands it.
//
// Without this, a typo in either the construct name or the template path is a
// runtime failure on the first real suggestion -- the handler would report
// "rendering uiAssistFill" and nothing would say the file was never there.
func TestUIAssistFillPromptRendersAgainstItsSchema(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))

	registry, err := loadPromptRegistry(logger)
	if err != nil {
		t.Fatalf("loadPromptRegistry: %v", err)
	}
	if _, err := LoadUnifiedPrompts(logger, registry, template.New("partials")); err != nil {
		t.Fatalf("LoadUnifiedPrompts: %v", err)
	}

	prompt, ok := registry.Get(uiAssistPromptID)
	if !ok {
		t.Fatalf("%s did not register -- check dsl/portalviews/prompts.memql", uiAssistPromptID)
	}
	if prompt.TemplateSource == "" {
		t.Fatalf("%s registered with an empty template -- "+
			"@templateFile(\"prompts/uiAssistFill.tmpl\") did not resolve", uiAssistPromptID)
	}

	data := map[string]any{
		"scope": map[string]any{
			"id":    "fleet.addMachine",
			"label": "Add a machine",
			"fields": []any{
				map[string]any{"name": "displayName", "type": "text", "label": "Name", "value": "", "constraints": ""},
				map[string]any{"name": "labels", "type": "text", "label": "Labels", "value": "macos", "constraints": "comma separated"},
			},
		},
		"prompt": "call it the studio mac",
		"page":   "Fleet, Machines",
	}
	normalized, err := normalizeAIData(data)
	if err != nil {
		t.Fatalf("normalizeAIData: %v", err)
	}
	if err := prompt.ValidateData(normalized); err != nil {
		t.Fatalf("the handler's data does not satisfy the prompt's own input schema: %v", err)
	}
	out, err := prompt.Render(normalized)
	if err != nil {
		t.Fatalf("rendering %s: %v", uiAssistPromptID, err)
	}

	// The two things the template MUST carry, asserted on the rendered output
	// rather than on the file: the caller's fields, and the no-invention rule
	// that is this domain's first line of defence.
	for _, want := range []string{"displayName", "labels", "comma separated", "call it the studio mac"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered prompt does not carry %q", want)
		}
	}
	if !strings.Contains(out, "Never invent an identifier") {
		t.Error("the rendered prompt has lost its no-invention instruction, which is the " +
			"first of the three defences against a plausible-looking made-up id")
	}
}

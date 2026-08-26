package memql

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// suggest_views_test.go -- the two view suggest domains (task memql#4667).
//
// THE BUG THIS FILE EXISTS TO KEEP CLOSED. `viewArrangement` was called by the
// portal from the day the composer shipped and registered by nothing. The
// registry answered with its typed unknown-domain error, the composer reported
// "suggestions are not available on this cluster" -- the same sentence a
// cluster with no AI provider gets -- and so a missing server half was
// indistinguishable from a deliberate configuration for its entire life.
//
// The first test is therefore not a formality: registration IS the feature.

func TestViewSuggestDomainsAreRegistered(t *testing.T) {
	for _, domain := range []string{ViewArrangementSuggestDomain, ViewComposeSuggestDomain} {
		if LookupSuggestDomain(domain) == nil {
			t.Errorf("no handler registered for %q. The portal calls this domain by "+
				"name; an unregistered one comes back as the typed unknown-domain "+
				"error, which the composer reports as 'suggestions are not available "+
				"on this cluster' -- indistinguishable from a cluster with no "+
				"provider configured.", domain)
		}
	}
	// The wire spelling, pinned. A constant that drifted from the client's
	// string is a silent no-op, which is the exact failure mode above.
	if ViewArrangementSuggestDomain != "viewArrangement" {
		t.Errorf("the arrangement domain is spelled %q on the wire; the portal sends "+
			"\"viewArrangement\" (clients/portal/src/compose/suggest.ts).",
			ViewArrangementSuggestDomain)
	}
	if ViewComposeSuggestDomain != "viewCompose" {
		t.Errorf("the compose domain is spelled %q on the wire.", ViewComposeSuggestDomain)
	}
	if !slices.Contains(RegisteredSuggestDomains(), ViewArrangementSuggestDomain) {
		t.Error("viewArrangement is missing from the registered-domain list, so the " +
			"unknown-domain error would not even name it as available.")
	}
}

// renderer returns a RenderPrompt that records what it was asked for and
// returns a rendering. Standing in for the engine, which a unit test has none
// of -- the point of the seam is that a handler declares WHICH prompt and
// WHICH data, and both are assertable without an AI runtime.
func renderer(seen *map[string]any, id *string) func(string, map[string]any) (string, error) {
	return func(templateId string, data map[string]any) (string, error) {
		*id = templateId
		*seen = data
		return "rendered: " + templateId, nil
	}
}

func TestViewArrangementRendersTheDeclaredPrompt(t *testing.T) {
	var data map[string]any
	var promptId string

	plan, err := buildViewArrangementSuggest(SuggestContext{
		RenderPrompt: renderer(&data, &promptId),
		Payload: map[string]any{
			"concept":       map[string]any{"id": "v1:identity:user", "entity": "user"},
			"rowCount":      float64(12),
			"fields":        []any{map[string]any{"field": "role"}},
			"bands":         []any{map[string]any{"band": "reading"}},
			"layouts":       []any{map[string]any{"layout": "dashboard"}},
			"candidates":    []any{map[string]any{"element": "table"}},
			"baseline":      map[string]any{"conceptId": "v1:identity:user"},
			"currentLayout": "stack",
			"hint":          "  more visual  ",
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// The DSL's prompt, not a Go string. A handler carrying its own copy would
	// make dsl/portalviews/prompts.memql decoration that reads exactly like
	// the live thing.
	if promptId != "composeViewArrangement" {
		t.Errorf("rendered %q; the declaration is composeViewArrangement in "+
			"dsl/portalviews/prompts.memql", promptId)
	}
	if len(plan.Messages) != 1 || !strings.Contains(plan.Messages[0].Content, "rendered: composeViewArrangement") {
		t.Errorf("the plan does not carry the rendered prompt: %+v", plan.Messages)
	}

	// The layout vocabulary and the hint are what this task ADDED to the
	// prompt's inputs; without them the reshaped template renders a section
	// that says nothing.
	if data["layouts"] == nil {
		t.Error("the layout vocabulary did not reach the prompt")
	}
	if got := data["hint"]; got != "more visual" {
		t.Errorf("hint = %q, want it trimmed to %q", got, "more visual")
	}
	if got := data["currentLayout"]; got != "stack" {
		t.Errorf("currentLayout = %q", got)
	}

	// NO ROW VALUES REACH THE PROMPT. Not a style rule: a model asked to lay
	// out a screen has no business reading somebody's data, and the input set
	// is closed by naming the keys rather than passing the payload through --
	// so this asserts the closure rather than the absence of one example.
	for key := range data {
		switch key {
		case "concept", "rowCount", "fields", "bands", "layouts", "candidates",
			"baseline", "currentLayout", "hint":
		default:
			t.Errorf("an undeclared key %q reached the prompt. The input set is "+
				"named one by one so a caller cannot widen it.", key)
		}
	}
}

func TestViewArrangementRefusesAnIncompletePayload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"no concept", map[string]any{"candidates": []any{}}},
		{"a concept with no id", map[string]any{
			"concept":    map[string]any{"entity": "user"},
			"candidates": []any{},
		}},
		{"no candidates", map[string]any{
			"concept": map[string]any{"id": "v1:identity:user"},
		}},
	} {
		var data map[string]any
		var id string
		_, err := buildViewArrangementSuggest(SuggestContext{
			RenderPrompt: renderer(&data, &id),
			Payload:      tc.payload,
		})
		var ve *SuggestValidationError
		if !asValidation(err, &ve) {
			t.Errorf("%s: got %v, want a validation error (which the handler maps to "+
				"InvalidArgument rather than to a failed AI call)", tc.name, err)
		}
	}
}

func TestViewComposeRendersTheDeclaredPromptAndNeedsARegistry(t *testing.T) {
	var data map[string]any
	var promptId string

	plan, err := buildViewComposeSuggest(SuggestContext{
		RenderPrompt: renderer(&data, &promptId),
		Payload: map[string]any{
			"description": "  the agents that failed something this week  ",
			"registry":    []any{map[string]any{"id": "v1:agents:agent"}},
			"elements":    []any{map[string]any{"element": "table"}},
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if promptId != "composeView" {
		t.Errorf("rendered %q, want composeView", promptId)
	}
	if got := data["description"]; got != "the agents that failed something this week" {
		t.Errorf("description = %q, want it trimmed", got)
	}
	if plan.SchemaName != "viewCompose" {
		t.Errorf("schema name = %q", plan.SchemaName)
	}

	// A draft with no registry to choose from would have the model INVENT a
	// concept id, which the client then drops -- a round trip that can only
	// fail.
	var ve *SuggestValidationError
	_, err = buildViewComposeSuggest(SuggestContext{
		RenderPrompt: renderer(&data, &promptId),
		Payload:      map[string]any{"description": "anything"},
	})
	if !asValidation(err, &ve) {
		t.Errorf("a compose call with no registry got %v, want a validation error", err)
	}
}

func TestASuggestDomainWithNoRendererFailsRatherThanSubstituting(t *testing.T) {
	// A silent fallback to a built-in Go string would serve a prompt that
	// exists nowhere in the tree, on a node whose operator has no way to
	// discover that the file they are reading is not the one running.
	_, err := buildViewArrangementSuggest(SuggestContext{
		Payload: map[string]any{
			"concept":    map[string]any{"id": "v1:identity:user"},
			"candidates": []any{},
		},
	})
	if err == nil {
		t.Fatal("a handler with no prompt renderer succeeded; it must fail rather " +
			"than substitute a built-in prompt")
	}
	if !strings.Contains(err.Error(), "composeViewArrangement") {
		t.Errorf("the error does not name the prompt it could not render: %v", err)
	}
}

// TestViewSuggestSchemasAreValidAndStrict guards the two structured-output
// schemas. A schema the provider rejects turns every suggest call into an API
// error whose message is about JSON Schema rather than about views.
func TestViewSuggestSchemasAreValidAndStrict(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"viewArrangement": arrangementProposalSchemaJSON,
		"viewCompose":     viewDraftSchemaJSON,
	} {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Errorf("%s schema is not valid JSON: %v", name, err)
			continue
		}
		// Strict structured output requires every object to close itself.
		for path, obj := range objectsIn(doc, name) {
			if obj["additionalProperties"] == false {
				continue
			}
			// A `properties`-less object used as a free map is the one
			// legitimate open shape (the bindings map).
			if _, hasProps := obj["properties"]; !hasProps {
				continue
			}
			t.Errorf("%s: the object at %s does not set additionalProperties:false, "+
				"which strict structured output requires.", name, path)
		}
	}

	// The enums must match the grammar the client parses, or a reply the
	// provider permitted is a reply readArrangement silently repairs away.
	if !strings.Contains(string(arrangementProposalSchemaJSON), `"stack", "dashboard", "split", "focus", "gallery"`) {
		t.Error("the arrangement schema's layout enum has drifted from the five " +
			"layouts (sdk/ts-viewkit/src/arrangement.ts SECTION_LAYOUTS)")
	}
	if !strings.Contains(string(arrangementProposalSchemaJSON), `"hero", "supporting", "standard"`) {
		t.Error("the arrangement schema's role enum has drifted from ENTRY_ROLES")
	}
}

func objectsIn(doc map[string]any, path string) map[string]map[string]any {
	out := map[string]map[string]any{}
	if doc["type"] == "object" {
		out[path] = doc
	}
	for key, value := range doc {
		switch v := value.(type) {
		case map[string]any:
			for p, o := range objectsIn(v, path+"."+key) {
				out[p] = o
			}
		case []any:
			for i, item := range v {
				if m, ok := item.(map[string]any); ok {
					for p, o := range objectsIn(m, fmt.Sprintf("%s.%s[%d]", path, key, i)) {
						out[p] = o
					}
				}
			}
		}
	}
	return out
}

func asValidation(err error, target **SuggestValidationError) bool {
	if err == nil {
		return false
	}
	ve, ok := err.(*SuggestValidationError)
	if ok {
		*target = ve
	}
	return ok
}

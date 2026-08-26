package memql

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// suggest_uiassist.go -- the `uiAssist` suggest domain (memql#4654).
//
// ===========================================================================
// WHAT IT IS
// ===========================================================================
// A generic "fill this described form from what the person said". A client
// registers a SCOPE -- an id, a label a human would recognise, and a list of
// fields with their types, current values and constraints -- sends a
// free-text prompt, and gets back PATCHES: field/value pairs to apply. It
// never returns prose to parse, and it never returns a field the caller did
// not describe.
//
// The portal's Synapse affordance is its first caller, but nothing here knows
// that. The domain is described a form and fills it; it does not know what
// page the form is on, what the product is called, or what any particular
// field means (`TestEngineIsProductNeutral` territory, and the reason the
// scope is data rather than a switch).
//
// ===========================================================================
// THE INSTRUCTION LIVES IN .memql, NOT HERE
// ===========================================================================
// The `knowledge` domain beside this one builds its prompt with fmt.Sprintf,
// which is honest for a prompt nobody outside Go was ever going to edit. This
// one is different: what it says is a PRODUCT decision about how literally to
// read a person's words, and it belongs in dsl/portalviews/prompts.memql with
// the rest of the authorable surface. So the handler renders `uiAssistFill`
// through the engine and this file carries the wiring, the schema and the
// validation -- not the words.
//
// ===========================================================================
// TWO LINES OF DEFENCE AGAINST AN INVENTED FIELD, AND BOTH ARE HERE
// ===========================================================================
// The template's own instruction is the first (values only from the person's
// prompt and the described scope; never invent an identifier). The second is
// postProcessUIAssist below, which DROPS any patch naming a field the scope
// did not declare, before the reply is serialized. The client validates a
// third time.
//
// That is not belt-and-braces for its own sake. A patch is applied to a form
// a person is about to submit, so an invented field is not a bad suggestion
// -- it is a value written into something the caller did not offer, and the
// caller is the only party that knows what its own form does.

func init() {
	RegisterSuggestDomain("uiAssist", buildUIAssistSuggest)
}

// The prompt this domain renders. One constant, because the .memql file names
// it and a second spelling would be a silent "prompt not found" at the first
// real call.
const uiAssistPromptID = "uiAssistFill"

// uiAssistField is one field of the caller's form, as the caller described it.
type uiAssistField struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Label       string `json:"label"`
	Value       string `json:"value"`
	Constraints string `json:"constraints"`
}

func buildUIAssistSuggest(ctx SuggestContext) (SuggestPlan, error) {
	prompt := strings.TrimSpace(ctx.String("prompt"))
	if prompt == "" {
		return SuggestPlan{}, SuggestValidationErrorf("prompt is required in payload")
	}

	scope, ok := ctx.Payload["scope"].(map[string]any)
	if !ok {
		return SuggestPlan{}, SuggestValidationErrorf("scope is required in payload")
	}
	fields := readUIAssistFields(scope["fields"])
	if len(fields) == 0 {
		// Not an internal error: a scope with no fields is a caller that has
		// nothing for this domain to fill, and answering "here are your zero
		// patches" would look like the model declined.
		return SuggestPlan{}, SuggestValidationErrorf("scope.fields must describe at least one field")
	}

	if ctx.RenderPrompt == nil {
		return SuggestPlan{}, fmt.Errorf("uiAssist: no prompt renderer on this node")
	}
	rendered, err := ctx.RenderPrompt(uiAssistPromptID, map[string]any{
		"scope": map[string]any{
			"id":     stringField(scope, "id"),
			"label":  stringField(scope, "label"),
			"fields": fieldsAsData(fields),
		},
		"prompt": prompt,
		"page":   ctx.String("page"),
	})
	if err != nil {
		return SuggestPlan{}, fmt.Errorf("uiAssist: rendering %s: %w", uiAssistPromptID, err)
	}

	return SuggestPlan{
		Messages: []common.ChatMessage{
			{Role: "system", Content: rendered},
			{Role: "user", Content: prompt},
		},
		Schema:      uiAssistSuggestSchemaJSON,
		SchemaName:  "uiAssistFill",
		PostProcess: postProcessUIAssist(fields),
	}, nil
}

// uiAssistSuggestSchemaJSON is the structured-output contract.
//
// `value` IS A STRING FOR EVERY FIELD, whatever the field's declared type,
// and that is deliberate. A schema that varied the value's type per field
// would have to be generated per request -- and providers differ in how much
// of a union they enforce, so the one that enforces least would be the one
// that decided the contract. A string is what every provider round-trips
// identically; the CALLER coerces to its own field type and drops what will
// not coerce, which is the only place that knows what "a number" means for
// that input.
var uiAssistSuggestSchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["patches"],
  "properties": {
    "patches": {
      "type": "array",
      "description": "The fields to fill, in the order they should be applied. Empty when the prompt asked for nothing this scope can express.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["field", "value"],
        "properties": {
          "field": {
            "type": "string",
            "description": "The name of a field from the provided scope, exactly. Any other name is discarded."
          },
          "value": {
            "type": "string",
            "description": "The value to put in that field, as text. Taken from the user's prompt or from the field's own constraints -- never invented."
          }
        }
      }
    },
    "note": {
      "type": "string",
      "description": "One short sentence for the person, when something was asked for that this form cannot express. Empty otherwise."
    }
  }
}`)

// postProcessUIAssist drops any patch naming a field outside the scope, and
// trims what survives.
//
// It returns a closure over the DECLARED field set rather than reading the
// payload again, so the set it checks against is exactly the set the prompt
// was shown.
func postProcessUIAssist(fields []uiAssistField) func(map[string]any) {
	known := make(map[string]bool, len(fields))
	for _, f := range fields {
		known[f.Name] = true
	}
	return func(suggestion map[string]any) {
		raw, _ := suggestion["patches"].([]any)
		kept := make([]any, 0, len(raw))
		for _, item := range raw {
			patch, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, _ := patch["field"].(string)
			name = strings.TrimSpace(name)
			if name == "" || !known[name] {
				continue
			}
			value, _ := patch["value"].(string)
			patch["field"] = name
			patch["value"] = strings.TrimSpace(value)
			kept = append(kept, patch)
		}
		// Always present, even when empty: a caller reading `patches` should
		// never have to tell "the model returned nothing" from "the key is
		// missing", because those have the same remedy and different code.
		suggestion["patches"] = kept
		if note, ok := suggestion["note"].(string); ok {
			suggestion["note"] = strings.TrimSpace(note)
		}
	}
}

// readUIAssistFields pulls the scope's field descriptors out of the payload.
//
// A descriptor with no `name` is DROPPED rather than defaulted: the name is
// the only part a patch can be matched against, so a nameless field is one
// nothing could ever be applied to.
func readUIAssistFields(raw any) []uiAssistField {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]uiAssistField, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := stringField(m, "name")
		if name == "" {
			continue
		}
		out = append(out, uiAssistField{
			Name:        name,
			Type:        stringField(m, "type"),
			Label:       stringField(m, "label"),
			Value:       stringField(m, "value"),
			Constraints: stringField(m, "constraints"),
		})
	}
	return out
}

func fieldsAsData(fields []uiAssistField) []any {
	out := make([]any, 0, len(fields))
	for _, f := range fields {
		out = append(out, map[string]any{
			"name":        f.Name,
			"type":        f.Type,
			"label":       f.Label,
			"value":       f.Value,
			"constraints": f.Constraints,
		})
	}
	return out
}

// stringField is authoring_concept_diff.go's, reused rather than redeclared:
// it already trims, which is what every read here wants.

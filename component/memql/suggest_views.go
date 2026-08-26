package memql

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// suggest_views.go -- the `viewArrangement` and `viewCompose` domains
// (epic memql#4661, task memql#4667).
//
// ===========================================================================
// WHY viewArrangement HAD NEVER WORKED
// ===========================================================================
// The portal has called this domain since the composer shipped
// (clients/portal/src/compose/suggest.ts). Nothing ever registered it. Every
// press of "Suggest an arrangement" reached the registry, missed, and came
// back as the typed unknown-domain error -- which the composer reports as
// "suggestions are not available on this cluster", the same sentence a cluster
// with no AI provider gets. So the feature looked configured-off rather than
// absent, on every cluster, for its whole life.
//
// ===========================================================================
// CORE, BESIDE `knowledge`
// ===========================================================================
// Composing a view is a platform surface: the concept browser, the composer
// and every arranged page are engine features, not product ones. So these
// register from core like `knowledge` does, and an engine-only build carries
// them.
//
// ===========================================================================
// THE PROMPT IS THE DSL'S, NOT A GO STRING
// ===========================================================================
// Unlike `knowledge`, whose prompt exists nowhere else, both prompts here are
// DECLARED in dsl/portalviews/prompts.memql -- loaded, schema-validated at
// boot, versioned with the tree and readable by anyone looking at dsl/. A Go
// handler that carried its own copy would make those declarations decoration
// that reads exactly like the live thing, and the copy would drift on the
// first edit that touched only one of them.
//
// So the handlers render through SuggestContext.RenderPrompt, which validates
// the data against the prompt's own declared inputs. A missing renderer is an
// ERROR rather than a fallback: a silent fallback to a built-in string is how
// a cluster ends up serving a prompt nobody can find in the tree.

func init() {
	RegisterSuggestDomain(ViewArrangementSuggestDomain, buildViewArrangementSuggest)
	RegisterSuggestDomain(ViewComposeSuggestDomain, buildViewComposeSuggest)
}

// The wire domain strings. Constants because the client names them too
// (clients/portal/src/compose/suggest.ts) and a second spelling is a silent
// no-op -- which is precisely how viewArrangement spent its life.
const (
	ViewArrangementSuggestDomain = "viewArrangement"
	ViewComposeSuggestDomain     = "viewCompose"
)

// The prompt names, as declared in dsl/portalviews/prompts.memql.
const (
	viewArrangementPromptId = "composeViewArrangement"
	viewComposePromptId     = "composeView"
)

// buildViewArrangementSuggest improves an arrangement of ONE concept the
// person already chose.
//
// The payload is view-kit's `arrangementRequest` value verbatim -- the concept
// identity, the field profile, the bands, the layout vocabulary, the fitted
// candidates and the arrangement the system already built. It carries NO ROW
// VALUES, deliberately and permanently: a model asked to lay out a screen has
// no business reading somebody's data, and a payload of rows would put
// whatever the concept holds in front of a provider for a layout decision.
func buildViewArrangementSuggest(ctx SuggestContext) (SuggestPlan, error) {
	concept, ok := ctx.Payload["concept"].(map[string]any)
	if !ok || strings.TrimSpace(str(concept["id"])) == "" {
		return SuggestPlan{}, SuggestValidationErrorf("concept.id is required in payload")
	}
	if _, ok := ctx.Payload["candidates"].([]any); !ok {
		return SuggestPlan{}, SuggestValidationErrorf("candidates is required in payload")
	}

	// The prompt's declared inputs, filled from the payload. Named one by one
	// rather than passed through, so a caller cannot widen the prompt's input
	// set by adding a key -- RenderPrompt validates against the declaration,
	// and an undeclared key is a load-time question, not a runtime one.
	data := map[string]any{
		"concept":       concept,
		"rowCount":      ctx.Payload["rowCount"],
		"fields":        arrayOr(ctx.Payload["fields"]),
		"bands":         arrayOr(ctx.Payload["bands"]),
		"layouts":       arrayOr(ctx.Payload["layouts"]),
		"candidates":    arrayOr(ctx.Payload["candidates"]),
		"baseline":      ctx.Payload["baseline"],
		"currentLayout": str(ctx.Payload["currentLayout"]),
		"hint":          strings.TrimSpace(str(ctx.Payload["hint"])),
	}
	if data["rowCount"] == nil {
		data["rowCount"] = 0
	}
	if data["baseline"] == nil {
		data["baseline"] = map[string]any{}
	}

	rendered, err := renderSuggestPrompt(ctx, viewArrangementPromptId, data)
	if err != nil {
		return SuggestPlan{}, err
	}

	return SuggestPlan{
		Messages:   []common.ChatMessage{{Role: "system", Content: rendered}},
		Schema:     arrangementProposalSchemaJSON,
		SchemaName: "viewArrangement",
	}, nil
}

// buildViewComposeSuggest drafts a WHOLE view from a sentence.
//
// The difference from the arrangement domain is which decision the model is
// making: there, the concept was already chosen and the question is layout;
// here the concept is the decision, from a compact digest of what the cluster
// publishes.
//
// THE DIGEST IS COMPACT BY DESIGN -- ids, prose, field names with kinds,
// relationship labels. Not full schemas: a cluster publishes hundreds of
// concepts, and the whole of their JSON Schema would be most of a context
// window spent on constraint keywords no layout decision reads.
func buildViewComposeSuggest(ctx SuggestContext) (SuggestPlan, error) {
	description := strings.TrimSpace(ctx.String("description"))
	if description == "" {
		return SuggestPlan{}, SuggestValidationErrorf("description is required in payload")
	}
	registry := arrayOr(ctx.Payload["registry"])
	if len(registry) == 0 {
		return SuggestPlan{}, SuggestValidationErrorf("registry is required in payload")
	}

	data := map[string]any{
		"description": description,
		"registry":    registry,
		"elements":    arrayOr(ctx.Payload["elements"]),
		"layouts":     arrayOr(ctx.Payload["layouts"]),
		"bands":       arrayOr(ctx.Payload["bands"]),
	}

	rendered, err := renderSuggestPrompt(ctx, viewComposePromptId, data)
	if err != nil {
		return SuggestPlan{}, err
	}

	return SuggestPlan{
		Messages:   []common.ChatMessage{{Role: "system", Content: rendered}},
		Schema:     viewDraftSchemaJSON,
		SchemaName: "viewCompose",
	}, nil
}

// renderSuggestPrompt renders a DSL prompt, or says why it could not.
//
// A NIL RENDERER IS AN ERROR, not a fallback. Falling back to a built-in
// string would serve a prompt that exists nowhere in the tree, on a node whose
// operator has no way to discover that the file they are reading is not the
// one running.
func renderSuggestPrompt(ctx SuggestContext, promptId string, data map[string]any) (string, error) {
	if ctx.RenderPrompt == nil {
		return "", fmt.Errorf("no prompt renderer is available on this node, so the %q prompt cannot be rendered", promptId)
	}
	rendered, err := ctx.RenderPrompt(promptId, data)
	if err != nil {
		return "", fmt.Errorf("render %q: %w", promptId, err)
	}
	if strings.TrimSpace(rendered) == "" {
		return "", fmt.Errorf("the %q prompt rendered empty", promptId)
	}
	return rendered, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// arrayOr coerces a payload value to a slice, never nil.
//
// Never nil because the prompt templates RANGE over these, and a nil in a
// `[]object!` input fails the prompt's own validation -- so a caller that
// omitted an optional list would get a rendering error rather than a section
// that says nothing.
func arrayOr(v any) []any {
	arr, ok := v.([]any)
	if !ok {
		return []any{}
	}
	return arr
}

// arrangementProposalSchemaJSON is the structured-output schema for
// `viewArrangement`.
//
// IT MIRRORS ARRANGEMENT_PROPOSAL_SCHEMA in sdk/ts-viewkit/src/arrangement.ts,
// which is the parser's own statement of the same shape. Two descriptions of
// one shape in two languages is a real risk, and the mitigation is that
// neither is trusted: readArrangement puts every reply through exactly the
// validation a hand-built arrangement goes through, so a schema that drifted
// would produce a repaired arrangement rather than a broken view.
//
// `additionalProperties: false` throughout, because strict structured output
// requires it and because a provider inventing a key is a provider whose reply
// this side would silently ignore.
var arrangementProposalSchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["elements", "reasoning"],
  "properties": {
    "reasoning": {
      "type": "string",
      "description": "One or two sentences on why this arrangement suits these rows. Written for the person composing the view."
    },
    "layout": {
      "type": "string",
      "enum": ["stack", "dashboard", "split", "focus", "gallery"],
      "description": "How the section places its bands. Omit for a plain vertical stack, which is always a correct answer."
    },
    "elements": {
      "type": "array",
      "description": "The elements of the view, in the order they should be read.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["element", "band"],
        "properties": {
          "element": {
            "type": "string",
            "description": "The id of one of the offered candidate elements."
          },
          "band": {
            "type": "string",
            "enum": ["reading", "shape", "roll"],
            "description": "Which question this element answers."
          },
          "role": {
            "type": "string",
            "enum": ["hero", "supporting", "standard"],
            "description": "How much of the page this element carries. At most ONE hero per section. Omit for standard."
          },
          "title": {
            "type": "string",
            "description": "An optional caption for the band, when the element's own title is not the clearest thing to say."
          },
          "bindings": {
            "type": "object",
            "description": "Optional per-slot field overrides: slot name -> the field names to use. An empty list declines the slot.",
            "additionalProperties": { "type": "array", "items": { "type": "string" } }
          }
        }
      }
    }
  }
}`)

// viewDraftSchemaJSON is the structured-output schema for `viewCompose`: a
// whole draft rather than one section's arrangement.
var viewDraftSchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "sections", "reasoning"],
  "properties": {
    "name": {
      "type": "string",
      "description": "A short title for this view, two to five words."
    },
    "reasoning": {
      "type": "string",
      "description": "One or two sentences: what you understood them to want, and what you chose. Written for the person who typed the description."
    },
    "sections": {
      "type": "array",
      "description": "One section per population the view covers, in reading order. Usually one.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["conceptId", "elements"],
        "properties": {
          "conceptId": {
            "type": "string",
            "description": "The id of one of the concepts in the supplied registry, exactly."
          },
          "layout": {
            "type": "string",
            "enum": ["stack", "dashboard", "split", "focus", "gallery"],
            "description": "How this section places its bands. Omit for a plain vertical stack."
          },
          "elements": {
            "type": "array",
            "description": "The elements of this section, in the order they should be read.",
            "items": {
              "type": "object",
              "additionalProperties": false,
              "required": ["element", "band"],
              "properties": {
                "element": { "type": "string", "description": "An element id from the supplied library, exactly." },
                "band": {
                  "type": "string",
                  "enum": ["reading", "shape", "roll"],
                  "description": "Which question this element answers."
                },
                "role": {
                  "type": "string",
                  "enum": ["hero", "supporting", "standard"],
                  "description": "How much of the section this element carries. At most ONE hero per section."
                },
                "title": { "type": "string", "description": "An optional caption." },
                "bindings": {
                  "type": "object",
                  "description": "Optional per-slot field overrides. An empty list declines the slot.",
                  "additionalProperties": { "type": "array", "items": { "type": "string" } }
                }
              }
            }
          }
        }
      }
    }
  }
}`)

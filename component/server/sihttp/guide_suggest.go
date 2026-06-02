package sihttp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// guide_suggest.go -- the "author a Guide from a voice intake" suggestion
// path (epic visionarys-io/copresent#186, issue copresent#194).
//
// A Guide is an immersive, voice-driven walkthrough made of ordered Scenes
// (see the v1:guide:guide / v1:guide:scene concepts, copresent#556). This
// path takes the summary of a voice intake (industry / role / interests /
// goals) plus the Guide kind (demo | teach | walkthrough) and authors a
// bounded Scene sequence. The structured result is returned to copresent,
// which PERSISTS it via the typed mutationCreateGuide / mutationCreateScene
// SDK methods and hands the persisted Guide to the client Guide runtime
// (copresent#190) to run.
//
// Generation deliberately focuses on NARRATION + pointing at EXISTING UI
// (canvas/UI annotation directives keyed by data-op-id) rather than
// publishing bespoke Canvas cards: narration + "look here" annotation is the
// reliable, schema-simple core of a walkthrough. Richer publish directives
// can be layered on later; a Scene with no canvasActions is a valid
// narration-only Scene.

// GuideSuggestInput is the server-side projection of the intake payload.
type GuideSuggestInput struct {
	// Intake is the gathered summary of the voice conversation (industry,
	// role, interests, goals). Free text; the model authors Scenes from it.
	Intake string
	// Kind is demo | teach | walkthrough. Drives tone + the avatarEnabled
	// default (demo / teach default avatar-on, copresent#192).
	Kind string
	// UserName personalises the opening narration when known. Optional.
	UserName string
}

// GUIDE_SCENE_MIN / MAX bound the authored Scene count: enough to be a real
// walkthrough, few enough to stay short and voice-paced.
const (
	guideSceneMin = 3
	guideSceneMax = 8
)

var guideKinds = map[string]bool{"demo": true, "teach": true, "walkthrough": true}

// GuideSuggestSchemaJSON is the strict JSON Schema for an authored Guide.
// Every property is required (strict structured output): the model fills
// "" / [] when a field does not apply. PostProcessGuideSuggestion enforces
// bounds + supplies a deterministic fallback.
var GuideSuggestSchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "description", "kind", "avatarEnabled", "scenes"],
  "properties": {
    "name": { "type": "string", "description": "3-6 word Guide title, Title Case, no trailing punctuation." },
    "description": { "type": "string", "description": "One-sentence summary of what the Guide covers." },
    "kind": { "type": "string", "enum": ["demo", "teach", "walkthrough"], "description": "Experience category." },
    "avatarEnabled": { "type": "boolean", "description": "Whether the assistant avatar should engage. Default true for demo/teach." },
    "scenes": {
      "type": "array",
      "description": "Ordered Scenes (3 to 8). Each is one narrated beat of the walkthrough.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["slug", "title", "narrationIntent", "canvasActions", "interruptible", "allowsQuestions"],
        "properties": {
          "slug": { "type": "string", "description": "Short kebab-case id within the Guide (e.g. 'intro', 'show-spaces')." },
          "title": { "type": "string", "description": "Short Scene title for the progress affordance." },
          "narrationIntent": { "type": "string", "description": "What the assistant should CONVEY over voice this Scene (intent, not a verbatim script)." },
          "canvasActions": {
            "type": "array",
            "description": "Ordered UI-annotation directives to perform while narrating. Empty for a narration-only Scene.",
            "items": {
              "type": "object",
              "additionalProperties": false,
              "required": ["type", "shape", "target", "label"],
              "properties": {
                "type": { "type": "string", "enum": ["annotate"], "description": "Directive kind. Only 'annotate' is generated." },
                "shape": { "type": "string", "enum": ["point", "arrow", "circle", "highlight"], "description": "Annotation shape." },
                "target": { "type": "string", "description": "data-op-id of the element to mark (e.g. 'nav.spaces')." },
                "label": { "type": "string", "description": "Optional short caption; empty string if none." }
              }
            }
          },
          "interruptible": { "type": "boolean", "description": "Whether the user may barge in during this Scene." },
          "allowsQuestions": { "type": "boolean", "description": "Whether a queued raise-hand is taken at this Scene boundary." }
        }
      }
    }
  }
}`)

// BuildGuideSuggestMessages builds the prompts that author a Guide from the
// intake. The system prompt fixes the bounds + voice-first tone; the user
// prompt carries the intake summary + kind.
func BuildGuideSuggestMessages(input GuideSuggestInput) []common.ChatMessage {
	kind := strings.TrimSpace(strings.ToLower(input.Kind))
	if !guideKinds[kind] {
		kind = "walkthrough"
	}
	systemPrompt := fmt.Sprintf(`You author an immersive, voice-driven Guide for the CoPresent app: an ordered sequence of Scenes the assistant narrates aloud while pointing at parts of the live UI.

A Guide has %d to %d Scenes. Each Scene is ONE short spoken beat:
- narrationIntent: what to CONVEY (intent, not a word-for-word script) -- warm, concise, colleague-on-a-call tone, one or two sentences of intent. Write it as natural teaching language; NEVER reference scene numbers, the word "scene", or scene titles in the narration -- the structure stays invisible to the user.
- canvasActions: zero or more "annotate" directives that point at on-screen elements by data-op-id while you talk ({type:"annotate", shape:"point|arrow|circle|highlight", target:"<data-op-id>", label:"<short caption or empty>"}). Use point/circle to draw the eye, highlight to emphasise. Leave the array empty for a pure narration beat. Only reference data-op-ids you are confident exist (common ones: nav.spaces, nav.agents, nav.settings, nav.chat, chat.input, spaces.new, agents.new); when unsure, prefer a narration-only Scene over guessing an op-id.
- interruptible: true for normal Scenes (the user may speak); set false only for a short scripted beat that must not be interrupted.
- allowsQuestions: true when the Scene is a natural place to pause for questions.

Rules:
- The walkthrough must read as ONE continuous lesson: phrase transitions naturally ("let's get started on...", "next, let me show you...") and never announce scene boundaries, numbers, or titles aloud.
- First Scene welcomes the user and frames the Guide; last Scene wraps up and hands back control.
- Tailor the content to the intake (industry, role, interests, goals). Make it feel personal, not generic.
- Keep it tight: %d-%d Scenes, short narration, no filler.
- kind is %q. For demo/teach set avatarEnabled true; for walkthrough you may set it false.
- Return ONLY valid JSON matching the schema.`, guideSceneMin, guideSceneMax, guideSceneMin, guideSceneMax, kind)

	intake := strings.TrimSpace(input.Intake)
	if intake == "" {
		intake = "(no intake captured; author a short generic first-run walkthrough of CoPresent: spaces, the assistant, chat, and creating things.)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Guide kind: %s\n", kind)
	if name := strings.TrimSpace(input.UserName); name != "" {
		fmt.Fprintf(&b, "User's name: %s\n", name)
	}
	fmt.Fprintf(&b, "Intake summary:\n%s", intake)

	return []common.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: b.String()},
	}
}

// PostProcessGuideSuggestion enforces the Scene-count bounds, normalises
// per-Scene defaults + ordering, and -- crucially -- supplies a
// DETERMINISTIC FALLBACK when generation is thin (no/!enough scenes), so the
// caller always receives a runnable Guide. Mutates `suggestion` in place to
// the shape copresent persists via mutationCreateGuide + mutationCreateScene.
func PostProcessGuideSuggestion(suggestion map[string]any) {
	// kind: validate, default walkthrough.
	kind, _ := suggestion["kind"].(string)
	kind = strings.TrimSpace(strings.ToLower(kind))
	if !guideKinds[kind] {
		kind = "walkthrough"
	}
	suggestion["kind"] = kind

	// name / description: trim + clamp.
	suggestion["name"] = clampGuideText(stringOr(suggestion["name"], "Your CoPresent Walkthrough"), 80)
	suggestion["description"] = clampGuideText(
		stringOr(suggestion["description"], "A quick guided tour of CoPresent."), 240)

	// avatarEnabled: default by kind when missing/non-bool.
	if _, ok := suggestion["avatarEnabled"].(bool); !ok {
		suggestion["avatarEnabled"] = kind == "demo" || kind == "teach"
	}

	// scenes: normalise, bound, fallback.
	rawScenes, _ := suggestion["scenes"].([]any)
	scenes := make([]any, 0, len(rawScenes))
	for _, rs := range rawScenes {
		sc, ok := rs.(map[string]any)
		if !ok {
			continue
		}
		intent := clampGuideText(stringOr(sc["narrationIntent"], ""), 600)
		if intent == "" {
			continue // a Scene with no narration intent is unusable
		}
		sc["narrationIntent"] = intent
		sc["title"] = clampGuideText(stringOr(sc["title"], "Step"), 80)
		sc["slug"] = normalizeSlug(stringOr(sc["slug"], ""), len(scenes))
		sc["canvasActions"] = sanitizeCanvasActions(sc["canvasActions"])
		sc["interruptible"] = boolOr(sc["interruptible"], true)
		sc["allowsQuestions"] = boolOr(sc["allowsQuestions"], true)
		scenes = append(scenes, sc)
		if len(scenes) >= guideSceneMax {
			break
		}
	}

	if len(scenes) < guideSceneMin {
		scenes = fallbackScenes(stringOr(suggestion["name"], "CoPresent"))
		suggestion["name"] = clampGuideText(stringOr(suggestion["name"], "Your CoPresent Walkthrough"), 80)
	}

	// Re-sequence order 0..n-1 so it is always a clean ascending run.
	for i, rs := range scenes {
		if sc, ok := rs.(map[string]any); ok {
			sc["order"] = i
		}
	}
	suggestion["scenes"] = scenes
}

// sanitizeCanvasActions keeps only well-formed annotate directives.
func sanitizeCanvasActions(raw any) []any {
	items, _ := raw.([]any)
	out := make([]any, 0, len(items))
	validShape := map[string]bool{"point": true, "arrow": true, "circle": true, "highlight": true}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		shape := strings.TrimSpace(strings.ToLower(stringOr(m["shape"], "")))
		target := strings.TrimSpace(stringOr(m["target"], ""))
		if !validShape[shape] || target == "" {
			continue
		}
		out = append(out, map[string]any{
			"type":   "annotate",
			"shape":  shape,
			"target": target,
			"label":  clampGuideText(stringOr(m["label"], ""), 80),
		})
	}
	return out
}

// fallbackScenes is the deterministic minimal Guide when generation is thin.
func fallbackScenes(_ string) []any {
	mk := func(slug, title, intent string, actions []any) map[string]any {
		return map[string]any{
			"slug": slug, "title": title, "narrationIntent": intent,
			"canvasActions": actions, "interruptible": true, "allowsQuestions": true,
		}
	}
	return []any{
		mk("welcome", "Welcome",
			"Welcome the user warmly and explain that you'll give them a quick tour of CoPresent.", []any{}),
		mk("spaces", "Spaces",
			"Explain that Spaces are where they collaborate with you, and point at the Spaces navigation.",
			[]any{map[string]any{"type": "annotate", "shape": "point", "target": "nav.spaces", "label": "Spaces"}}),
		mk("chat", "Chatting with your assistant",
			"Explain they can talk or type to you here, and point at the chat input.",
			[]any{map[string]any{"type": "annotate", "shape": "circle", "target": "chat.input", "label": "Talk to me here"}}),
		mk("wrap-up", "You're set",
			"Wrap up, invite them to ask for anything, and hand control back.", []any{}),
	}
}

func clampGuideText(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'“”‘’")
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = strings.TrimSpace(s[:max])
	}
	return s
}

func stringOr(v any, fallback string) string {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}

func boolOr(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

func normalizeSlug(s string, index int) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '-':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return fmt.Sprintf("scene-%d", index)
	}
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}
	return out
}

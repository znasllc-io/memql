package sihttp

import "testing"

// scenesOf pulls the normalised scenes slice out of a suggestion map.
func scenesOf(t *testing.T, suggestion map[string]any) []map[string]any {
	t.Helper()
	raw, ok := suggestion["scenes"].([]any)
	if !ok {
		t.Fatalf("scenes missing or wrong type: %T", suggestion["scenes"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("scene is not a map: %T", r)
		}
		out = append(out, m)
	}
	return out
}

func TestPostProcessGuideSuggestion_FallbackWhenThin(t *testing.T) {
	// Generation returned too few usable scenes -> deterministic fallback.
	s := map[string]any{
		"name":          "",
		"description":   "",
		"kind":          "bogus",
		"avatarEnabled": "not-a-bool",
		"scenes":        []any{map[string]any{"narrationIntent": "only one"}},
	}
	PostProcessGuideSuggestion(s)

	if s["kind"] != "walkthrough" {
		t.Errorf("kind = %v, want walkthrough (invalid normalised)", s["kind"])
	}
	if _, ok := s["avatarEnabled"].(bool); !ok {
		t.Errorf("avatarEnabled not coerced to bool: %T", s["avatarEnabled"])
	}
	scenes := scenesOf(t, s)
	if len(scenes) < guideSceneMin {
		t.Fatalf("fallback produced %d scenes, want >= %d", len(scenes), guideSceneMin)
	}
	// order must be a clean ascending run from 0.
	for i, sc := range scenes {
		if sc["order"] != i {
			t.Errorf("scene %d has order %v, want %d", i, sc["order"], i)
		}
		if sc["interruptible"] != true || sc["allowsQuestions"] != true {
			t.Errorf("scene %d missing interruptible/allowsQuestions defaults", i)
		}
	}
}

func TestPostProcessGuideSuggestion_BoundsAndSanitize(t *testing.T) {
	// Over-long scene list is clamped to the max; bad canvasActions dropped;
	// a scene with no narrationIntent is discarded; demo kind => avatar on.
	rawScenes := make([]any, 0, 12)
	for i := 0; i < 12; i++ {
		rawScenes = append(rawScenes, map[string]any{
			"slug":            "",
			"title":           "T",
			"narrationIntent": "do a thing",
			"interruptible":   true,
			"allowsQuestions": true,
			"canvasActions": []any{
				map[string]any{"type": "annotate", "shape": "point", "target": "nav.spaces", "label": "x"},
				map[string]any{"type": "annotate", "shape": "bogus", "target": "nav.spaces"}, // bad shape -> dropped
				map[string]any{"type": "annotate", "shape": "circle", "target": ""},          // empty target -> dropped
			},
		})
	}
	rawScenes = append(rawScenes, map[string]any{"narrationIntent": ""}) // unusable -> dropped
	s := map[string]any{
		"name":          "  My Guide  ",
		"description":   "A tour.",
		"kind":          "demo",
		"avatarEnabled": false,
		"scenes":        rawScenes,
	}
	PostProcessGuideSuggestion(s)

	scenes := scenesOf(t, s)
	if len(scenes) != guideSceneMax {
		t.Fatalf("got %d scenes, want clamp to %d", len(scenes), guideSceneMax)
	}
	for i, sc := range scenes {
		actions, _ := sc["canvasActions"].([]any)
		if len(actions) != 1 {
			t.Errorf("scene %d: got %d canvasActions, want 1 valid", i, len(actions))
		}
		slug, _ := sc["slug"].(string)
		if slug == "" {
			t.Errorf("scene %d: slug not defaulted", i)
		}
	}
	// Explicit avatarEnabled=false is honoured (it was a real bool).
	if s["avatarEnabled"] != false {
		t.Errorf("avatarEnabled = %v, want false (explicit bool honoured)", s["avatarEnabled"])
	}
	if s["name"] != "My Guide" {
		t.Errorf("name = %q, want trimmed 'My Guide'", s["name"])
	}
}

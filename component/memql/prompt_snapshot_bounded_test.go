package memql

import (
	"strings"
	"testing"
)

func TestCapturedPromptBoundedPreflightMatchesNativeRenderAndRefusesExpansion(t *testing.T) {
	_, snapshot := capturedPromptFixture(t)
	data := map[string]any{"content": "exact patch"}
	p, err := restoreCapturedPrompt(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := p.Render(data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := RenderCapturedPromptBounded(snapshot, data, len(plain))
	if err != nil || got != plain {
		t.Fatalf("bounded render differs: %q %v", got, err)
	}
	for _, limit := range []int{0, -1, len(plain) - 1, (1 << 20) + 1} {
		if _, err = RenderCapturedPromptBounded(snapshot, data, limit); err == nil {
			t.Fatal("invalid/too-small limit accepted", limit)
		}
	}
	snapshot["templates"].(map[string]any)["body"] = strings.Repeat("{{.content}}", 16)
	if _, err = RenderCapturedPromptBounded(snapshot, map[string]any{"content": strings.Repeat("x", 1<<17)}, 1<<20); err == nil {
		t.Fatal("expanded prompt escaped bounded writer")
	}
}
func TestCapturedPromptBoundedInvocationPreservesSingleStructuredSeam(t *testing.T) {
	_, snapshot := capturedPromptFixture(t)
	e, model, _ := overrideSeamEngine(t, aiCacheConfig{})
	if _, err := e.InvokeCapturedPromptStructuredBounded(userCtx("alice"), snapshot, map[string]any{"content": "exact patch"}, "verdict", `{"type":"object"}`, true, 8); err == nil || model.calls != 0 {
		t.Fatal("overflow reached model", err)
	}
	if _, err := e.InvokeCapturedPromptStructuredBounded(userCtx("alice"), snapshot, map[string]any{"content": "exact patch"}, "verdict", `{"type":"object"}`, true, 1024); err != nil || model.calls != 1 {
		t.Fatal("single structured call changed", err)
	}
	if model.messages[0].Content != "Original rules exact patch" {
		t.Fatal("captured content changed")
	}
}
func TestCapturedPromptBoundedRefusesIntermediateWorkBeforeEvaluation(t *testing.T) {
	for _, source := range []string{`{{printf "%1000000000s" .content}}`, `{{range .}}{{.}}{{end}}`, `{{template "reviewExact" .}}`, `{{$x := .content}}{{$x}}`} {
		t.Run(source, func(t *testing.T) {
			_, snapshot := capturedPromptFixture(t)
			snapshot["templates"].(map[string]any)["reviewExact"] = source
			if _, err := RenderCapturedPromptBounded(snapshot, map[string]any{"content": "small"}, 1024); err == nil {
				t.Fatal("unbounded intermediate operation accepted")
			}
		})
	}
	_, snapshot := capturedPromptFixture(t)
	if _, err := RenderCapturedPromptBounded(snapshot, map[string]any{"content": strings.Repeat("x", (1<<20)+1)}, 1024); err == nil {
		t.Fatal("oversized input accepted")
	}
}

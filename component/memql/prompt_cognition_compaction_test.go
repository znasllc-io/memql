package memql

import (
	"log/slog"
	"testing"
	"text/template"
)

// TestCognitionCompactionPromptRegisters is the memql#1069 guard: the
// cognitionCompaction prompt (used by Polyphon context compaction via
// component/polyphon/context_builder.go) must register with a non-empty
// template body. It previously shipped as an empty `prompt
// cognitionCompaction {}` with no @templateFile, so the unified prompt
// loader skipped it with "template or templateFile is required" and
// conversation-history compaction silently no-oped. This test fails if
// that regression returns -- either the prompt is missing from the
// registry or its template resolved empty.
func TestCognitionCompactionPromptRegisters(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))

	registry, err := loadPromptRegistry(logger)
	if err != nil {
		t.Fatalf("loadPromptRegistry: %v", err)
	}
	if _, err := LoadUnifiedPrompts(logger, registry, template.New("partials")); err != nil {
		t.Fatalf("LoadUnifiedPrompts: %v", err)
	}

	prompt, ok := registry.Get("cognitionCompaction")
	if !ok {
		t.Fatal("cognitionCompaction prompt did not register -- the unified prompt loader skipped it " +
			"(check dsl/cognition/prompts.memql declares @templateFile and an input schema)")
	}
	if prompt.TemplateSource == "" {
		t.Fatal("cognitionCompaction registered with an empty template body -- " +
			"@templateFile(\"prompts/cognitionCompaction.tmpl\") did not resolve")
	}
}

// testWriter adapts *testing.T to io.Writer so loader logs surface on a
// failing run without polluting passing output.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

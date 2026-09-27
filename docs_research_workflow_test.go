package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestResearchWorkflowExcerptsMatchSource(t *testing.T) {
	// The example is two files: its automation is in automations.memql, the one
	// file of a domain the automation loader reads, and everything it calls is
	// in brief.memql (memql#5437). The excerpts come from either.
	var source []byte
	for _, file := range []string{"brief.memql", "automations.memql"} {
		b, err := os.ReadFile("examples/research-desk/research/" + file)
		if err != nil {
			t.Fatal(err)
		}
		source = append(append(source, b...), '\n')
	}
	guide, err := os.ReadFile("docs/public/language/research-workflow.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := extractMemqlBlocks(string(guide))
	names := []string{"search", "ai", "cache", "automation"}
	if len(blocks) != len(names) {
		t.Fatalf("want four excerpt blocks, got %d", len(blocks))
	}
	for i, name := range names {
		_, rest, ok := strings.Cut(string(source), "// showcase:"+name+":start\n")
		if !ok {
			t.Fatalf("missing %s region", name)
		}
		snippet, _, ok := strings.Cut(rest, "// showcase:"+name+":end")
		if !ok || strings.TrimSpace(blocks[i].body) != strings.TrimSpace(snippet) {
			t.Errorf("%s excerpt differs from the complete executable source", name)
		}
	}
}

// Single-file parsing missed a retired annotation in the old first-program
// example. Exercise directory mode: the actual CLI overlays core DSL and runs
// engine initialization, including imports, statement bodies and domain pins.
func TestEditorExamplesPassEngineLoader(t *testing.T) {
	for _, dir := range []string{"examples/reading-list", "examples/research-desk"} {
		t.Run(dir, func(t *testing.T) {
			out, err := exec.Command("go", "run", "./cmd/memqllint", dir).CombinedOutput()
			if err != nil {
				t.Fatalf("engine loader rejected %s: %v\n%s", dir, err, out)
			}
			if !strings.Contains(string(out), "no diagnostics") {
				t.Fatalf("missing successful loader result: %s", out)
			}
		})
	}
}

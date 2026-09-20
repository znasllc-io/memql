package mcp

// source_size_test.go -- the MCP surface refuses DSL source over the bound
// before the handler that would lex it runs.

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	// The FILE bound, which run_inline_automation's one automation earns.
	mcpFileBound    = 512 << 10
	mcpFileTooLarge = "over the 512 KiB limit: send one file at a time, or split it into smaller files [source_too_large]"
	// The BUNDLE bound, which define's concatenated constructs earn.
	mcpBundleBound    = 2 << 20
	mcpBundleTooLarge = "over the 2 MiB limit: send one file at a time, or split it into smaller files [source_too_large]"
)

// The two tools that take DSL source refuse it over the bound each one earns,
// and the runner behind run_inline_automation is never reached.
func TestOversizedToolSourceIsRefusedBeforeTheHandler(t *testing.T) {
	eng := newFakeEngine()

	r := &fakeRunner{}
	res := callMCPTool(withRunnerCtx("owner-1", r), eng, "owner", TierInline, "", toolRunInlineAutomation,
		map[string]any{"source": strings.Repeat("a", mcpFileBound+1)})
	if !isError(res) {
		t.Fatalf("run_inline_automation must refuse an oversized source, got %v", res)
	}
	if !strings.Contains(resultText(res), mcpFileTooLarge) {
		t.Errorf("refusal wording: %s", resultText(res))
	}
	if r.inlineCalled {
		t.Error("the runner must never see an oversized source")
	}

	res = callMCPTool(context.Background(), eng, "owner", TierAuthoring, "", toolDefine,
		map[string]any{"bundle": strings.Repeat("a", mcpBundleBound+1)})
	if !isError(res) || !strings.Contains(resultText(res), mcpBundleTooLarge) {
		t.Errorf("define must refuse an oversized bundle, got %v", res)
	}
}

// The two bounds are not one: a bundle between them is a bundle define
// accepts and a source run_inline_automation refuses.
func TestMCPBundleBetweenTheBoundsIsAcceptedByDefineOnly(t *testing.T) {
	between := strings.Repeat("a", mcpFileBound+1)
	if err := oversizedToolSource(toolDefine, map[string]any{"bundle": between}); err != nil {
		t.Errorf("define carries a bundle and must accept this: %v", err)
	}
	if err := oversizedToolSource(toolRunInlineAutomation, map[string]any{"source": between}); err == nil {
		t.Error("run_inline_automation carries one automation and must refuse this")
	}
}

// A source at the bound passes the gate and reaches the runner: the bound is a
// bound, not a wall.
func TestToolSourceAtTheBoundReachesTheHandler(t *testing.T) {
	const decl = "\n@trigger(event=\"x\") automation boundProbe { }"
	source := strings.Repeat("// x\n", (mcpFileBound-len(decl))/5)
	source += strings.Repeat("/", mcpFileBound-len(source)-len(decl)) + decl
	if len(source) != mcpFileBound {
		t.Fatalf("probe source is %d bytes, want exactly the bound", len(source))
	}

	r := &fakeRunner{}
	res := callMCPTool(withRunnerCtx("owner-1", r), newFakeEngine(), "owner", TierInline, "", toolRunInlineAutomation,
		map[string]any{"source": source})
	if isError(res) {
		t.Fatalf("a source of exactly the bound must reach the handler, got %v", res)
	}
	if !r.inlineCalled {
		t.Error("the runner was not reached")
	}
}

// Every other tool is untouched: the bound is keyed on the tools that take DSL
// source, not on every string argument.
func TestToolsThatTakeNoDSLSourceAreUntouched(t *testing.T) {
	over := strings.Repeat("a", mcpBundleBound+1)
	for _, name := range []string{toolRunQuery, toolRunMutation, toolRunAutomation} {
		if err := oversizedToolSource(name, map[string]any{"source": over, "bundle": over}); err != nil {
			t.Errorf("%s must not be held to the DSL source bound: %v", name, err)
		}
	}
}

// The refusal reads a length and nothing else.
func TestMCPRefusalOfA20MiBSourceNeverLexesIt(t *testing.T) {
	source := strings.Repeat("1+", (20<<20)/2)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	err := oversizedToolSource(toolRunInlineAutomation, map[string]any{"source": source})
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatal("20 MiB must be refused")
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("refused in %v, allocating %d bytes", elapsed, allocated)
	if elapsed > time.Millisecond || allocated > 1<<20 {
		t.Errorf("the refusal took %v and allocated %d bytes; it compares a length", elapsed, allocated)
	}
}

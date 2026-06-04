package agent

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

func TestIsResume(t *testing.T) {
	cases := []struct {
		hints map[string]string
		want  bool
	}{
		{nil, false},
		{map[string]string{"plan_id": "p1"}, false},
		{map[string]string{ResumeHintKey: "false"}, false},
		{map[string]string{ResumeHintKey: "true"}, true},
		{map[string]string{ResumeHintKey: "True"}, true},
		{map[string]string{ResumeHintKey: " true "}, true},
	}
	for _, tc := range cases {
		if got := IsResume(tc.hints); got != tc.want {
			t.Fatalf("IsResume(%v) = %v, want %v", tc.hints, got, tc.want)
		}
	}
}

func TestFormatResumeContext(t *testing.T) {
	row := map[string]any{
		"reasoningChain": "Outlined the report and gathered the Q3 figures.",
		"toolCallHistory": []any{
			map[string]any{"toolName": "workbenchHost", "args": `{"action":"fs_write"}`, "result": "wrote intro.md"},
			map[string]any{"toolName": "webSearch", "args": `{"q":"q3"}`},
		},
	}
	block := formatResumeContext(row)
	for _, want := range []string{
		"RESUMING A PAUSED TASK",
		"Outlined the report",
		"workbenchHost(",
		"wrote intro.md",
		"do not repeat these",
		"Continue from this point",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("resume block missing %q\n---\n%s", want, block)
		}
	}
}

func TestFormatResumeContext_EmptyRow(t *testing.T) {
	if got := formatResumeContext(map[string]any{}); got != "" {
		t.Fatalf("empty taskState row should yield no block, got %q", got)
	}
	// Reasoning-only (no tool history) still produces a block.
	if got := formatResumeContext(map[string]any{"reasoningChain": "did stuff"}); got == "" {
		t.Fatal("reasoning-only row should still produce a resume block")
	}
}

func TestToolHistoryEntries_TruncatesAndSkips(t *testing.T) {
	long := strings.Repeat("x", 500)
	entries := toolHistoryEntries([]any{
		map[string]any{"toolName": "t1", "args": "{}", "result": long},
		map[string]any{"args": "{}"}, // no toolName -> skipped
		"not-a-map",                  // skipped
	})
	if len(entries) != 1 {
		t.Fatalf("expected 1 valid entry, got %d: %v", len(entries), entries)
	}
	if len(entries[0]) > resumeContextMaxResultChars+100 {
		t.Fatalf("entry should be truncated, got len %d", len(entries[0]))
	}
}

func TestInjectResumeContext(t *testing.T) {
	// Inserts right after a leading system message.
	msgs := []common.ChatMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "goal"},
	}
	out := injectResumeContext(msgs, "RESUME BLOCK")
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	if out[0].Role != "system" || out[1].Role != "user" || out[1].Content != "RESUME BLOCK" || out[2].Content != "goal" {
		t.Fatalf("resume message must land right after system, got %+v", out)
	}

	// Prepends when there's no system message.
	out2 := injectResumeContext([]common.ChatMessage{{Role: "user", Content: "goal"}}, "RB")
	if out2[0].Content != "RB" {
		t.Fatalf("expected resume block prepended, got %+v", out2)
	}

	// Empty block is a no-op.
	same := injectResumeContext(msgs, "  ")
	if len(same) != 2 {
		t.Fatalf("empty block must not change messages, got %d", len(same))
	}
}

func TestLatestTaskStateRow_PicksNewest(t *testing.T) {
	res := map[string]any{"data": []any{
		map[string]any{"id": "s1", "createdAt": "2026-06-01T00:00:00Z", "reasoningChain": "old"},
		map[string]any{"id": "s2", "createdAt": "2026-06-04T00:00:00Z", "reasoningChain": "new"},
	}}
	row := latestTaskStateRow(res)
	if row == nil || row["reasoningChain"] != "new" {
		t.Fatalf("expected newest row (s2), got %+v", row)
	}
	// Single-row (unwrapped object) shape.
	single := map[string]any{"data": map[string]any{"id": "s1", "reasoningChain": "only"}}
	if r := latestTaskStateRow(single); r == nil || r["reasoningChain"] != "only" {
		t.Fatalf("single-row shape not handled, got %+v", r)
	}
	// No match.
	if r := latestTaskStateRow(map[string]any{"data": []any{}}); r != nil {
		t.Fatalf("empty data should yield nil, got %+v", r)
	}
}

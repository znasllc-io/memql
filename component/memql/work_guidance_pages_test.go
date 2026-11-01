package memql

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/common"
)

func TestWorkGuidancePagePrefixDetectsLateInsertionAndKeepsUnicode(t *testing.T) {
	rows := make([]WorkGuidance, 80)
	for n := range rows {
		rows[n] = WorkGuidance{ID: fmt.Sprintf("message-%03d", n), Prompt: strings.Repeat("é", 900) + fmt.Sprint(n)}
	}
	var recovered []WorkGuidance
	cursor := WorkGuidanceCursor{}
	for {
		page := WorkGuidancePageAfter(rows, cursor)
		require.False(t, page.Reset)
		require.LessOrEqual(t, len(page.Messages), maxWorkGuidanceMessages)
		runes := 0
		for _, row := range page.Messages {
			runes += utf8.RuneCountInString(row.Prompt)
		}
		require.LessOrEqual(t, runes, maxWorkGuidanceRunes)
		recovered = append(recovered, page.Messages...)
		cursor = page.Next
		if !page.More {
			break
		}
	}
	require.Equal(t, rows, recovered)
	require.Empty(t, WorkGuidancePageAfter(rows, cursor).Messages)
	changed := append([]WorkGuidance{{ID: "late-older-correction", Prompt: "Do not send the document."}}, rows...)
	page := WorkGuidancePageAfter(changed, cursor)
	require.True(t, page.Reset, "timestamp-only cursors would miss this instruction")
	require.Equal(t, changed[0], page.Messages[0])
	rows[0].Prompt = "The source revision changed."
	require.True(t, WorkGuidancePageAfter(rows, cursor).Reset)
	long := []WorkGuidance{{ID: "indivisible", Prompt: strings.Repeat("é", maxWorkGuidanceRunes+1)}}
	require.Equal(t, long, WorkGuidancePageAfter(long, WorkGuidanceCursor{}).Messages, "never clip one instruction")
}

func TestWorkGuidancePagesReadAllSourcesAcrossReplicasAndAcknowledgeExactPrefix(t *testing.T) {
	a, b, ctx, conversation, run, write := guidanceFixture(t)
	actor, _ := auth.AccessFromContext(ctx)
	for n := range 40 {
		recordApprovalGuidance(write, actor.UserId, run, fmt.Sprintf("Correction %d: %s", n, strings.Repeat("retain the receipt; ", 40)))
	}
	for n := range 65 {
		lateOptionalAnswer(write, actor.UserId, run, "original-plan", n)
	}
	for n := range 20 {
		routeTurn(t, a, ctx, conversation, AskTurn{ID: fmt.Sprintf("route-%02d", n), Routed: &AskRouting{Targets: []AskRouteTarget{{GoalID: "fixture-goal", RunID: run, Status: routeQueued, Text: fmt.Sprintf("Keep document revision %d for review.", n)}}}})
	}
	runtime := guidanceRunContext(ctx, run)
	all, err := a.ReadWorkGuidance(runtime)
	require.NoError(t, err)
	require.Len(t, all, 125, "first-page query limits must not hide older instructions")
	var got []WorkGuidance
	cursor := WorkGuidanceCursor{}
	for {
		page, err := b.ReadWorkGuidancePage(runtime, cursor)
		require.NoError(t, err)
		require.False(t, page.Reset)
		got = append(got, page.Messages...)
		cursor = page.Next
		if !page.More {
			break
		}
	}
	require.Equal(t, all, got)
	require.NoError(t, b.AcknowledgeWorkGuidancePage(runtime, cursor))
	require.Equal(t, routeReceived, storedTurn(t, a, ctx, conversation, "route-19").Routed.Targets[0].Status)
	restored, err := a.ReadWorkGuidancePage(runtime, cursor)
	require.NoError(t, err)
	require.Empty(t, restored.Messages, "acknowledgment does not change source identity")
	routeTurn(t, a, ctx, conversation, AskTurn{ID: "newest", Routed: &AskRouting{Targets: []AskRouteTarget{{GoalID: "fixture-goal", RunID: run, Status: routeQueued, Text: "Leave the draft unpublished."}}}})
	newest, err := b.ReadWorkGuidancePage(runtime, cursor)
	require.NoError(t, err)
	require.Len(t, newest.Messages, 1)
	require.Equal(t, "newest", newest.Messages[0].ID)
	require.NoError(t, b.AcknowledgeWorkGuidancePage(runtime, cursor))
	require.Equal(t, routeQueued, storedTurn(t, a, ctx, conversation, "newest").Routed.Targets[0].Status, "an earlier prefix cannot acknowledge later input")

	model := &checkpointModel{}
	contextTestModel(a, model)
	review, err := a.reviewGuidanceContext(runtime, "Prepare a reviewable draft.")
	require.NoError(t, err)
	require.LessOrEqual(t, len(review), replyReviewMaxBytes)
	require.Contains(t, review, "Leave the draft unpublished.")
	require.Contains(t, review, "[Memory checkpoint ")
	require.Positive(t, model.calls)
	// A peer recovers the exact archived older instruction page, not just the
	// model-derived review text. The owner's immutable approval is untouched.
	firstPage := WorkGuidancePageAfter(all, WorkGuidanceCursor{})
	original := common.ChatMessage{Role: "user", Content: WorkGuidanceText(firstPage.Messages)}

	// Checkpoints also bind their relevance task; use their native source index
	// to find the exact source rather than inventing a hash or copying a summary.
	call, err := parser.RenderCall("workContextSourcesForOwnerRun", map[string]any{"runId": run, "fingerprint": ""})
	require.NoError(t, err)
	var sources []map[string]any
	err = WalkQueryPages(runtime, b.Execute, "query "+call, 16, func(result *ExecuteResult) error {
		sources = append(sources, MaterializeRows(result.OutputPayload())...)
		return nil
	})
	require.NoError(t, err)
	found := false
	for _, source := range sources {
		data, _ := source["data"].(map[string]any)
		var messages []common.ChatMessage
		if json.Unmarshal([]byte(stringArg(data, "sourceMessages")), &messages) == nil {
			for _, message := range messages {
				if message.Role == original.Role && message.Content == original.Content {
					found = true
				}
			}
		}
	}
	require.True(t, found, "exact original guidance page survives bounded review on another replica")
}

package work

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

type skillCatalogPageEngine struct {
	*recordingEngine
	cursor string
}

func (e *skillCatalogPageEngine) IsInstalledSkillBaseline(row map[string]any) bool {
	return row["verifiedFixture"] == true && row["instructions"] != nil
}

func (e *skillCatalogPageEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.cursor = memql.CursorFromContext(ctx)
	result, err := e.recordingEngine.Execute(ctx, query)
	if result != nil {
		result.SetMeta(&memql.ResultMeta{Cursor: "next-page", HasMore: true})
	}
	return result, err
}

func TestSkillCatalogPageVerifiesFullRowsButReturnsCompactPage(t *testing.T) {
	i, records := newTestIntegration(t)
	engine := &skillCatalogPageEngine{recordingEngine: records}
	i.engine = engine
	records.reply("skillsForTrainingPage", map[string]any{
		"id": "test-skill", "name": "Testing", "description": "Check the result", "version": 3,
		"verifiedFixture": true, "instructions": map[string]any{"body": "Long content"},
		"scripts": []any{"private-to-detail"}, "resources": []any{"large attachment"},
	})
	rows, err := i.handleSkillCatalogPage(callerContext("reader"), map[string]any{"search": "  test  ", "cursor": "previous-page"}, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var result map[string]any
	require.NoError(t, json.Unmarshal(rows[0].Payload, &result))
	require.Equal(t, "next-page", result["nextCursor"])
	page := result["skills"].([]any)
	require.Len(t, page, 1)
	skill := page[0].(map[string]any)
	require.Equal(t, true, skill["reviewedBaseline"])
	require.Equal(t, "Testing", skill["name"])
	require.NotContains(t, skill, "instructions")
	require.NotContains(t, skill, "scripts")
	require.NotContains(t, skill, "resources")
	require.Equal(t, "previous-page", engine.cursor)
	calls := records.recorded()
	require.Len(t, calls, 1)
	require.Equal(t, "reader", calls[0].Actor)
	require.Equal(t, auth.OriginClient, calls[0].Origin)
	require.Equal(t, "test", calls[0].Args(t)["search"])
}

func TestSkillCatalogEntryReadsOnlyRequestedSkill(t *testing.T) {
	i, records := newTestIntegration(t)
	i.engine = &skillCatalogPageEngine{recordingEngine: records}
	records.reply("skillById", map[string]any{"id": "one", "instructions": map[string]any{"body": "Exact current content"}, "verifiedFixture": true})
	rows, err := i.handleSkillCatalogEntry(callerContext("reader"), map[string]any{"skillId": "one"}, 0)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(rows[0].Payload, &result))
	skill := result["skill"].(map[string]any)
	require.Equal(t, true, skill["reviewedBaseline"])
	require.Equal(t, "Exact current content", skill["instructions"].(map[string]any)["body"])
	require.Len(t, records.recorded(), 1)
	require.Equal(t, "one", records.recorded()[0].Args(t)["skillId"])
}

func TestSkillCatalogPagesRequireIdentityBeforeReading(t *testing.T) {
	i, records := newTestIntegration(t)
	_, err := i.handleSkillCatalogPage(context.Background(), nil, 0)
	require.Error(t, err)
	_, err = i.handleSkillCatalogEntry(context.Background(), map[string]any{"skillId": "one"}, 0)
	require.Error(t, err)
	require.Empty(t, records.recorded())
}

func TestSkillCatalogBrowsePayloadBudget(t *testing.T) {
	i, records := newTestIntegration(t)
	i.engine = &skillCatalogPageEngine{recordingEngine: records}
	fixture := make([]map[string]any, 200)
	for n := range fixture {
		fixture[n] = map[string]any{"id": fmt.Sprintf("skill-%d", n), "name": fmt.Sprintf("Skill %d", n), "description": "A shared skill", "version": 1, "instructions": map[string]any{"body": strings.Repeat("Instruction content. ", 800)}, "verifiedFixture": true}
	}
	records.reply("skillsForTraining", fixture...)
	records.reply("skillsForTrainingPage", fixture[:10]...)
	before, err := i.handleSkillCatalog(callerContext("reader"), nil, 0)
	require.NoError(t, err)
	after, err := i.handleSkillCatalogPage(callerContext("reader"), nil, 0)
	require.NoError(t, err)
	require.Less(t, len(after[0].Payload), len(before[0].Payload)/100)
	require.Len(t, records.recorded(), 2, "one named authorized catalog query for each measured path")
	t.Logf("skill catalog fixture=200 rows/instruction_body_bytes=%d old_wire_bytes=%d first_ten_summary_bytes=%d old_list_queries=1 new_list_queries=1; selection history requests move off list open", len(strings.Repeat("Instruction content. ", 800)), len(before[0].Payload), len(after[0].Payload))
}

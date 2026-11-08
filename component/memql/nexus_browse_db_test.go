package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestNexusBrowsePagesSearchScopeAndPayloadDB(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("nexus-browse")}
	owner := f.person("owner", true)
	stranger := f.person("stranger", true)
	ctx := shareDirActor(context.Background(), owner, auth.RoleOwner)
	heavy := strings.Repeat("execution-only", 2048)
	var goals, runs, approvals, constructs []string
	for n := 0; n < 25; n++ {
		title := fmt.Sprintf("Recent record %02d", n)
		if n < 12 {
			title = fmt.Sprintf("Earlier NEEDLE%%_ record %02d", n)
		}
		goal := f.id("v1:work:goal", fmt.Sprintf("goal-%02d", n))
		goals = append(goals, goal)
		run := f.id("v1:work:run", fmt.Sprintf("run-%02d", n))
		runs = append(runs, run)
		approval := f.id("v1:work:approval", fmt.Sprintf("approval-%02d", n))
		approvals = append(approvals, approval)
		construct := f.id("v1:authoring:construct", fmt.Sprintf("construct-%02d", n))
		constructs = append(constructs, construct)
		f.insert("v1:work:goal", goal, map[string]any{"ownerUserId": owner, "statement": title, "status": "active", "origin": "user", "requestedVia": "nexus", "input": map[string]any{"large": heavy}, "acceptance": map[string]any{"large": heavy}})
		f.insert("v1:work:run", run, map[string]any{"ownerUserId": owner, "goalId": goal, "title": title, "automationName": "example", "status": "running", "input": map[string]any{"large": heavy, "conversation": map[string]any{"id": "conversation-1"}}, "variables": map[string]any{"large": heavy}, "spine": map[string]any{"large": heavy}})
		f.insert("v1:work:approval", approval, map[string]any{"ownerUserId": owner, "runId": run, "kind": "feedback", "question": title, "decision": "", "requestedAt": time.Now().UTC().Add(time.Duration(n) * time.Second).Format(time.RFC3339Nano)})
		target := "owner.test"
		catalogued := true
		if n%2 == 0 {
			target = "procedure"
			catalogued = false
		}
		f.insert("v1:authoring:construct", construct, map[string]any{"ownerUserId": owner, "name": title, "kind": "automation", "targetNamespace": target, "catalogued": catalogued, "status": "active", "source": heavy, "compiledForm": map[string]any{"large": heavy}, "procedure": map[string]any{"title": title, "recordedFrom": map[string]any{"runIds": []string{run}}, "freeParameters": []string{"month"}, "steps": []any{map[string]any{"large": heavy}}}})
	}
	f.insert("v1:work:goal", f.id("v1:work:goal", "foreign"), map[string]any{"ownerUserId": stranger, "statement": "NEEDLE%_ foreign", "requestedVia": "nexus"})
	f.insert("v1:work:run", f.id("v1:work:run", "foreign"), map[string]any{"ownerUserId": stranger, "title": "NEEDLE%_ foreign", "status": "running"})
	f.insert("v1:authoring:construct", f.id("v1:authoring:construct", "foreign"), map[string]any{"ownerUserId": stranger, "name": "NEEDLE%_ foreign", "kind": "automation", "catalogued": true})
	walk := func(call string, sizes []int) []map[string]any {
		t.Helper()
		cursor := ""
		var all []map[string]any
		for page, size := range sizes {
			result, err := eng.Execute(ContextWithCursor(ctx, cursor), call)
			require.NoError(t, err, call)
			rows := MaterializeRows(result.OutputPayload())
			require.Len(t, rows, size, call)
			all = append(all, rows...)
			cursor = result.GetMeta().Cursor
			if page < len(sizes)-1 {
				require.NotEmpty(t, cursor)
			} else {
				require.Empty(t, cursor)
			}
		}
		seen := map[string]bool{}
		for _, row := range all {
			id := fmt.Sprint(row["id"])
			require.False(t, seen[id], "duplicate cursor row")
			seen[id] = true
		}
		return all
	}
	for _, query := range []string{"workGoalsBrowse", "workRunsBrowse", "workApprovalsBrowse", "nexusAutomationsBrowse"} {
		rows := walk("query "+query+"()", []int{10, 10, 5})
		wire, err := json.Marshal(rows[:10])
		require.NoError(t, err)
		require.Less(t, len(wire), 20*1024, "ten rows must not transfer execution inputs or source")
		for _, row := range rows {
			require.NotContains(t, row, "variables")
			require.NotContains(t, row, "input")
			require.NotContains(t, row, "source")
			require.NotContains(t, row, "compiledForm")
			require.NotContains(t, row, "steps")
			require.NotContains(t, row, "acceptance")
		}
		walk("query "+query+`(search: "needle%_")`, []int{10, 2})
	}
	walk(`query workRunsBrowse(search: "needle%_",conversationId: "wrong")`, []int{0})
	walk(`query nexusAutomationsBrowse(origin: "learned")`, []int{10, 3})
	for _, sample := range []struct {
		key, kind, query string
		subject          map[string]any
		options          []any
	}{
		{"access", "scopeElevation", `query workApprovalsBrowse(search: "More access",kindLabels:["scopeElevation"])`, nil, nil},
		{"repository", "planReview", `query workApprovalsBrowse(search: "Repository change",repositoryLabel:true)`, map[string]any{"reviewType": "repository-change"}, nil},
		{"failure", "planReview", `query workApprovalsBrowse(search: "Work stopped",failureLabel:true)`, nil, []any{map[string]any{"value": "retry"}, map[string]any{"value": "abandon"}}},
	} {
		id := f.id("v1:work:approval", sample.key)
		f.insert("v1:work:approval", id, map[string]any{"ownerUserId": owner, "kind": sample.kind, "question": "Inspect this item", "decision": "", "requestedAt": "2030-01-01T00:00:00Z", "subject": sample.subject, "options": sample.options})
		matched := walk(sample.query, []int{1})
		require.Equal(t, id, matched[0]["id"])
	}

	status, err := eng.Execute(ctx, fmt.Sprintf(`query workGoalRunStatuses(goalIds: [%s])`, langparser.QuoteString(goals[0])))
	require.NoError(t, err)
	require.Len(t, MaterializeRows(status.OutputPayload()), 1)
	detail, err := eng.Execute(ctx, fmt.Sprintf(`query workGoalForOwner(goalId: %s)`, langparser.QuoteString(goals[0])))
	require.NoError(t, err)
	require.Contains(t, MaterializeRows(detail.OutputPayload())[0], "input")
	source, err := eng.Execute(ctx, fmt.Sprintf(`query authoringConstructById(constructId: %s)`, langparser.QuoteString(constructs[0])))
	require.NoError(t, err)
	require.Contains(t, MaterializeRows(source.OutputPayload())[0], "source")
	other := shareDirActor(context.Background(), stranger, auth.RoleOwner)
	hidden, err := eng.Execute(other, fmt.Sprintf(`query workGoalForOwner(goalId: %s)`, langparser.QuoteString(goals[0])))
	require.NoError(t, err)
	require.Empty(t, MaterializeRows(hidden.OutputPayload()))
}

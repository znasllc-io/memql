package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestTrainingBrowsePagesSearchAndScopeDB(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("training-browse")}
	owner := f.person("owner", true)
	stranger := f.person("stranger", true)
	ctx := shareDirActor(context.Background(), owner, auth.RoleOwner)
	var fileIDs []string
	for n := 0; n < 25; n++ {
		fileID := f.id("v1:library:file", fmt.Sprint(n))
		fileIDs = append(fileIDs, fileID)
		summary := "new record"
		if n < 12 {
			summary = "old NEEDLE%_ record"
		}
		f.insert("v1:library:file", fileID, map[string]any{"ownerUserId": owner, "name": fmt.Sprintf("File %02d", n), "summary": summary, "archived": false, "blobUrl": "large detail"})
		f.insert("v1:work:run", f.id("v1:work:run", fmt.Sprint(n)), map[string]any{"ownerUserId": owner, "automationName": "libraryAnalyzeFile", "input": map[string]any{"fileId": fileID}, "status": "succeeded", "outcome": map[string]any{"chunks": 2}, "variables": map[string]any{"heavy": "not a row fact"}})
		status := "validated"
		if n < 12 {
			status = "unvalidated"
		}
		f.insert("v1:knowledge:documentChunk", f.id("v1:knowledge:documentChunk", fmt.Sprint(n)), map[string]any{"domainId": f.prefix, "text": fmt.Sprintf("%s passage %d", f.prefix, n), "validationStatus": status, "superseded": false})
	}
	f.insert("v1:library:file", f.id("v1:library:file", "foreign"), map[string]any{"ownerUserId": stranger, "name": "Foreign", "summary": "old NEEDLE%_ record", "archived": false})
	walk := func(call string, sizes []int) []map[string]any {
		t.Helper()
		var all []map[string]any
		cursor := ""
		for page, size := range sizes {
			result, err := eng.Execute(ContextWithCursor(ctx, cursor), call)
			require.NoError(t, err)
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
		return all
	}
	files := walk("query trainingFilesBrowse()", []int{10, 10, 5})
	for _, row := range files {
		require.NotContains(t, row, "blobUrl")
	}
	walk(`query trainingFilesBrowse(search: "needle%_")`, []int{10, 2})
	chunks := walk(fmt.Sprintf(`query documentChunksForDomain(domainId: %s)`, langparser.QuoteString(f.prefix)), []int{10, 10, 5})
	require.Len(t, chunks, 25)
	walk(fmt.Sprintf(`query documentChunksForDomain(domainId: %s, pending: true, search: %s)`, langparser.QuoteString(f.prefix), langparser.QuoteString(f.prefix)), []int{10, 2})
	runs, err := eng.Execute(ctx, fmt.Sprintf(`query trainingAnalysisRunsForFiles(fileIds: [%s,%s])`, langparser.QuoteString(fileIDs[0]), langparser.QuoteString(fileIDs[1])))
	require.NoError(t, err)
	rows := MaterializeRows(runs.OutputPayload())
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.NotContains(t, row, "variables")
	}
	reader := shareDirActor(context.Background(), owner, auth.RoleReader)
	_, err = eng.Execute(reader, `query documentChunksForDomain(pending: true)`)
	require.Error(t, err)
}

func TestTrainingEvidenceBrowseScopesAndBatchesDB(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	f := shareDirFixture{t: t, eng: eng, db: db, prefix: uniqueSuffix("training-evidence")}
	owner := f.person("owner", true)
	stranger := f.person("stranger", true)
	ctx := shareDirActor(context.Background(), owner, auth.RoleOwner)
	for n := 0; n < 25; n++ {
		scenario := "new-scenario"
		name := "Recent setup"
		if n < 12 {
			scenario = "old-scenario"
			name = "Older NEEDLE%_ setup"
		}
		f.insert("v1:work:observation", f.id("v1:work:observation", fmt.Sprintf("evaluation-%d", n)), map[string]any{"ownerUserId": owner, "data": map[string]any{"goalEvaluation": map[string]any{"schemaVersion": 1, "scenarioId": scenario, "setup": map[string]any{"name": name}}}})
		f.insert("v1:work:observation", f.id("v1:work:observation", fmt.Sprintf("selection-%d", n)), map[string]any{"ownerUserId": owner, "data": map[string]any{"trainingSelection": map[string]any{"schemaVersion": 1, "selected": []any{map[string]any{"lessonId": "lesson-one", "lessonVersion": n}}}}})
	}
	firstSelection := f.id("v1:work:observation", "selection-0")
	for n := 0; n < 2; n++ {
		f.insert("v1:work:observation", f.id("v1:work:observation", fmt.Sprintf("delivery-%d", n)), map[string]any{"ownerUserId": owner, "data": map[string]any{"execution": map[string]any{"call": map[string]any{"contextReferences": []string{firstSelection}, "model": fmt.Sprint(n)}}}})
	}
	f.insert("v1:work:observation", f.id("v1:work:observation", "foreign"), map[string]any{"ownerUserId": stranger, "data": map[string]any{"execution": map[string]any{"call": map[string]any{"contextReferences": []string{firstSelection}}}}})
	read := func(call string, sizes []int) {
		t.Helper()
		cursor := ""
		for n, size := range sizes {
			page, err := eng.Execute(ContextWithCursor(ctx, cursor), call)
			require.NoError(t, err)
			require.Len(t, MaterializeRows(page.OutputPayload()), size, call)
			cursor = page.GetMeta().Cursor
			if n == len(sizes)-1 {
				require.Empty(t, cursor)
			} else {
				require.NotEmpty(t, cursor)
			}
		}
	}
	read("query workGoalEvaluationsBrowse()", []int{10, 10, 5})
	read(`query workGoalEvaluationsBrowse(search: "needle%_")`, []int{10, 2})
	read(`query workGoalEvaluationsBrowse(scenarioId: "old-scenario")`, []int{10, 2})
	read(`query workTrainingSelectionsForLesson(lessonId: "lesson-one")`, []int{10, 10, 5})
	read(`query workTrainingSelectionsForLesson(lessonId: "missing")`, []int{0})
	read(fmt.Sprintf(`query workTrainingDeliveryForSelections(observationIds: [%s])`, langparser.QuoteString(firstSelection)), []int{2})
}

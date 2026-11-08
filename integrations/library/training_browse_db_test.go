package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
)

func TestTrainingLessonBrowseFoldsAllRevisionsBeforeSearchAndPagingDB(t *testing.T) {
	f := newRevisionDB(t)
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id LIKE $1`, "v1:work:observation:"+f.owner+"-browse-%")
	})
	fixture := make([]map[string]any, 0, 526)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for n := 0; n < 25; n++ {
		for sequence := 1; sequence <= 21; sequence++ {
			title := "Old needle"
			review := "reviewed"
			enabled := true
			if sequence == 21 {
				title = fmt.Sprintf("Current lesson %02d", n)
				review = "rejected"
				enabled = false
			}
			id := fmt.Sprintf("lesson-%02d", n)
			l := trainingLesson{SchemaVersion: 1, ID: id, Receipt: fmt.Sprintf("receipt-%02d-%02d", n, sequence), Sequence: sequence, Version: sequence, Digest: "fixture", Draft: lessonDraft{Title: title, Text: strings.Repeat("Guidance. ", 100), Scope: "owner"}, Source: map[string]any{"runId": "run"}, Review: review, Enabled: enabled, At: at.Add(time.Duration(sequence) * time.Second).Format(time.RFC3339Nano)}
			fixture = append(fixture, map[string]any{"id": fmt.Sprintf("v1:work:observation:%s-browse-%02d-%02d", f.owner, n, sequence), "createdAt": l.At, "payload": map[string]any{"ownerUserId": "v1:identity:user:" + f.owner, "data": map[string]any{"trainingLesson": l}}})
		}
	}
	fixture = append(fixture, map[string]any{"id": "v1:work:observation:" + f.owner + "-browse-foreign", "createdAt": at.Format(time.RFC3339Nano), "payload": map[string]any{"ownerUserId": "v1:identity:user:other-owner", "data": map[string]any{"trainingLesson": trainingLesson{SchemaVersion: 1, ID: "foreign", Sequence: 1, At: at.Format(time.RFC3339Nano)}}}})
	raw, err := json.Marshal(fixture)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,type,"createdBy","createdAt",payload,schema,metadata,provenance) SELECT x->>'id','v1:work:observation','object','test-training-browse',(x->>'createdAt')::timestamptz,x->'payload','{"$id":"v1:work:observation"}'::jsonb,'{}'::jsonb,'{}'::jsonb FROM jsonb_array_elements($1::jsonb) x`, string(raw))
	require.NoError(t, err)
	read := func(search, cursor string) ([]trainingLesson, string, int) {
		result, err := f.first.handleTrainingLessonsPage(f.ctx, map[string]any{"search": search, "cursor": cursor}, 0)
		require.NoError(t, err)
		var page struct {
			Lessons    []trainingLesson `json:"lessons"`
			NextCursor string           `json:"nextCursor"`
		}
		require.NoError(t, json.Unmarshal(result[0].Payload, &page))
		return page.Lessons, page.NextCursor, len(result[0].Payload)
	}
	first, cursor, firstBytes := read("", "")
	require.Len(t, first, 10)
	require.NotEmpty(t, cursor)
	second, cursor, _ := read("", cursor)
	require.Len(t, second, 10)
	require.NotEmpty(t, cursor)
	third, cursor, _ := read("", cursor)
	require.Len(t, third, 5)
	require.Empty(t, cursor)
	got := append(append(first, second...), third...)
	for n, l := range got {
		require.Equal(t, fmt.Sprintf("lesson-%02d", n), l.ID)
		require.Equal(t, 21, l.Sequence)
		require.Equal(t, "rejected", l.Review)
		require.False(t, l.Enabled)
	}
	search, _, _ := read("nEeDlE", "")
	require.Empty(t, search, "old matching enabled revisions must not resurrect rejected heads")
	search, _, _ = read("LESSON 24", "")
	require.Len(t, search, 1)
	require.Equal(t, "lesson-24", search[0].ID)
	_, err = f.first.handleTrainingLessonsPage(context.Background(), nil, 0)
	require.ErrorContains(t, err, "authenticated")
	_, err = f.first.handleTrainingLessonsPage(f.ctx, map[string]any{"cursor": "invalid"}, 0)
	require.ErrorContains(t, err, "cursor")
	// Measure the previous browser's complete authorized history on this fixture.
	oldBytes, oldRequests := 0, 0
	err = memql.WalkQueryPages(f.ctx, f.engine.Execute, "query workTrainingLessonsForOwner()", 1000, func(result *memql.ExecuteResult) error {
		rows := memql.MaterializeRows(result.OutputPayload())
		raw, e := json.Marshal(rows)
		oldBytes += len(raw)
		oldRequests++
		return e
	})
	require.NoError(t, err)
	require.Less(t, firstBytes, oldBytes/10)
	t.Logf("lessons fixture=525 owned revisions/25 heads+1 foreign old_browser_requests=%d old_wire_bytes=%d new_browser_requests=1 first_ten_head_bytes=%d; authorized server reducer still scans revision pages", oldRequests, oldBytes, firstBytes)
}

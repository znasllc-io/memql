package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The lesson source's SQL, against Postgres: the newest version of each lesson
// event, the head of each lesson, and an event that does not read as a lesson
// reported rather than dropped. The source pages every lesson event in the
// database, so the test reads after its own id prefix and keeps its own items.
func TestTheLessonSourceReadsItsEventsFromPostgres(t *testing.T) {
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "lesson memory source DB test", dbtest.DSN(), err)
	}
	prefix := fmt.Sprintf("v1:work:observation:lessonsrc%d", os.Getpid())
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id LIKE $1`, prefix+"%")
	})
	d := lessonDraft{Title: "T", Text: "Check the receipt", Applies: "when a step ends", Exceptions: "none"}
	head := trainingLesson{SchemaVersion: 1, ID: fmt.Sprintf("lesson-%d", os.Getpid()), Receipt: strings.TrimPrefix(prefix, "v1:work:observation:") + "-r2",
		Sequence: 2, Draft: d, Enabled: true, Review: "reviewed", Action: "approve"}
	older := head
	older.Receipt, older.Sequence = strings.TrimPrefix(prefix, "v1:work:observation:")+"-r1", 1
	at := time.Now().UTC().Add(-time.Minute)
	insert := func(id string, at time.Time, payload []byte) {
		t.Helper()
		row := []*memorynodes.MemoryNode{{ID: id, Concept: "v1:work:observation", CreatedBy: "lesson-source-test", CreatedAt: at,
			Payload: payload, Provenance: json.RawMessage(`{"kind":"direct","name":"lesson-source-test"}`)}}
		_, err := db.NewInsert().Model(&row).Exec(ctx)
		require.NoError(t, err)
	}
	insert(prefix+"-e1", at, lessonRow(t, "u1", older))
	insert(prefix+"-e2", at, lessonRow(t, "u1", head))
	// One event written twice: the newest version is what is read, so its
	// broken older version is not reported.
	insert(prefix+"-e3", at, []byte(`{"content":"Manual Training lesson","data":{"trainingLesson":{"schemaVersion":1}}}`))
	insert(prefix+"-e3", at.Add(time.Second), lessonRow(t, "u1", trainingLesson{SchemaVersion: 1, ID: "other-" + head.ID,
		Receipt: strings.TrimPrefix(prefix, "v1:work:observation:") + "-r3", Sequence: 1, Draft: d, Enabled: false, Review: "reviewed"}))
	// An event that does not read as a lesson is reported under its own id.
	insert(prefix+"-e4", at, []byte(`{"content":"Manual Training lesson","data":{"trainingLesson":{"schemaVersion":1}}}`))

	previous := activeTrainingReader.Load()
	activeTrainingReader.Store(&trainingReader{db: func() *sql.DB { return db.DB }})
	t.Cleanup(func() { activeTrainingReader.Store(previous) })

	mine := map[string]string{}
	after := prefix
	for page := 0; page < 50; page++ {
		items, next, err := trainingLessonItems(ctx, after, 2)
		require.NoError(t, err)
		for _, item := range items {
			if strings.HasPrefix(item.Id, prefix) {
				mine[item.Id] = item.Text + "|" + item.Failure
			}
		}
		if next == "" || !strings.HasPrefix(next, prefix) {
			break
		}
		after = next
	}
	require.Equal(t, map[string]string{
		prefix + "-r2": lessonPassage(head) + "|",
		prefix + "-e4": "|the lesson event does not read as a lesson: lesson receipt is incomplete",
	}, mine)
}

package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestCountReportsBoundedAndExhaustedSources(t *testing.T) {
	engine, db, _ := readMergeTestEngine(t)
	engine.config.MaxWindow = 2
	concept := "v1:campaigns:audience"
	prefix := fmt.Sprintf("count-probe-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", concept).Where("id LIKE ?", concept+":"+prefix+"-%").Exec(context.Background())
	})
	now := time.Now().UTC()
	write := func(id, state string, at time.Time) {
		payload, _ := json.Marshal(map[string]any{"name": prefix + "-" + state, "status": "active"})
		row := memorynodes.MemoryNode{ID: concept + ":" + prefix + "-" + id, Concept: concept, Payload: payload, CreatedAt: at}
		if _, err := db.NewInsert().Model(&row).Exec(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	write("one", "keep", now.Add(-5*time.Second))
	write("two", "keep", now.Add(-4*time.Second))
	write("three", "keep", now.Add(-3*time.Second))
	query := fmt.Sprintf(`count(concept == %q && payload.name == %q)`, concept, prefix+"-keep")
	read := func() map[string]any {
		res, err := engine.Execute(groupSeedCtx(), query)
		if err != nil {
			t.Fatal(err)
		}
		rows := MaterializeRows(res)
		if len(rows) != 1 {
			t.Fatalf("count response %v", rows)
		}
		return rows[0]
	}
	if row := read(); row["atLeast"] != true || fmt.Sprint(row["count"]) != "2" {
		t.Fatalf("truncated count %v", row)
	}
	// Latest versions no longer matching must not inflate either the count or
	// the completeness marker when SQL proves the remaining scan exhausted.
	write("two", "drop", now.Add(-time.Second))
	write("three", "drop", now)
	if row := read(); row["atLeast"] == true || fmt.Sprint(row["count"]) != "1" {
		t.Fatalf("latest versions changed the count %v", row)
	}
	engine.config.MaxWindow = 20
	if row := read(); row["atLeast"] == true || fmt.Sprint(row["count"]) != "1" {
		t.Fatalf("exhausted count %v", row)
	}
}

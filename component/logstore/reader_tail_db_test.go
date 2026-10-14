package logstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTailCursorPrunesOldChunksAndKeepsTimestampTies(t *testing.T) {
	admin := testDB(t)
	needTimescale(t, admin)
	db := scratchLogLine(t, admin, "1 day", true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.DB.ExecContext(ctx, `ALTER TABLE log_line ADD PRIMARY KEY (occurred_at, id)`); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	at := day.Add(12 * time.Hour)
	row := func(at time.Time, id string) Row {
		return Row{OccurredAt: at, ID: id, NodeType: "bff", Node: "tail-test", Level: "info", Component: "svc", Message: id}
	}
	fixture := []Row{
		row(at.AddDate(0, 0, -2), "old-two-days"),
		row(at.AddDate(0, 0, -1), "old-one-day"),
		row(at.Add(-time.Microsecond), "z-older"),
		row(at, "a"), row(at, "b"), row(at, "c"), row(at, "d"),
		row(at.Add(time.Microsecond), "0-later"),
		row(at.Add(time.Hour), "1-latest"),
	}
	if _, err := db.NewInsert().Model(&fixture).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	chunks := logLineChunks(t, db, day.AddDate(0, 0, -2), day.AddDate(0, 0, 1))
	if len(chunks) != 3 {
		t.Fatalf("fixture must span three chunks, got %s", chunkSpans(chunks))
	}
	for _, chunk := range chunks[:2] {
		compressLogLineChunk(t, db, chunk.Start)
	}

	// This is the default UI cursor poll: no node, component or time facets.
	// Page inside one timestamp, then cross to a later timestamp with a lower
	// ID. Neither an exclusive time bound nor an inclusive tuple is correct.
	q := Query{AfterAt: at, AfterId: "b", Limit: 2}
	for _, want := range []string{"c|d", "0-later|1-latest", ""} {
		rows, err := Tail(ctx, db, q)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(ids(rows), "|"); got != want {
			t.Fatalf("tail after (%s,%s) = %q, want %q", q.AfterAt, q.AfterId, got, want)
		}
		if len(rows) > 0 {
			last := rows[len(rows)-1]
			q.AfterAt, q.AfterId = last.OccurredAt, last.ID
		}
	}
	baseline, err := Tail(ctx, db, Query{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(baseline), "|"); got != "0-later|1-latest" {
		t.Fatalf("baseline must remain newest two ascending, got %q", got)
	}

	// Assert pruning against the real TimescaleDB planner, not a cost or scan
	// choice that changes with statistics. Only the current chunk can appear.
	var rows []Row
	query, _ := tailQuery(db, Query{AfterAt: at, AfterId: "b"}, &rows)
	var raw []byte
	if err := db.DB.QueryRowContext(ctx, "EXPLAIN (FORMAT JSON) "+query.String()).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan []struct{ Plan map[string]any }
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
		t.Fatalf("decode plan: %v, %s", err, raw)
	}
	currentSeen := false
	var inspect func(map[string]any)
	inspect = func(node map[string]any) {
		if relation, ok := node["Relation Name"].(string); ok {
			if relation == chunks[2].Name {
				currentSeen = true
			} else if relation != "log_line" {
				t.Errorf("tail planned an old relation %q; current chunk is %q", relation, chunks[2].Name)
			}
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				inspect(child.(map[string]any))
			}
		}
	}
	inspect(plan[0].Plan)
	if !currentSeen {
		t.Fatalf("tail plan did not scan the current chunk: %s", raw)
	}
}

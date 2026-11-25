package logstore

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/logger"
)

// The hold line is useless unless the administrator can find it by the
// reference the held person reads out. Inserted through InsertNow and
// searched back by text, the way the Logs app does.
func TestInsertNowLineIsFoundBySearchingItsReference(t *testing.T) {
	db := testDB(t)
	sfx := uniqueSuffix()
	node := "node-" + sfx
	cleanupNode(t, db, node)
	ref := "ERR-" + sfx

	s := NewSink(db, SinkOptions{NodeType: "bff", Node: node, Logger: discard()})
	line := logger.Line{At: time.Now(), Level: slog.LevelWarn, Component: "desktopAccess", Message: "held",
		Subject: "ana", SubjectConcept: "v1:identity:user", Attributes: map[string]any{"reference": ref}}
	if err := s.InsertNow(context.Background(), line, Stamp{NodeType: "bff", Node: node, UserId: "ana"}); err != nil {
		t.Fatalf("InsertNow: %v", err)
	}
	rows, err := Search(context.Background(), db, Query{
		WindowStart: time.Now().Add(-time.Minute), WindowEnd: time.Now().Add(time.Minute), Text: ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Subject != "ana" || rows[0].Node != node {
		t.Fatalf("search by reference = %+v, want the one line", rows)
	}
}

package logstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/logger"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func infoLine(msg string) logger.Line {
	return logger.Line{At: time.Now(), Level: slog.LevelInfo, Component: "test", Message: msg}
}

// captureInsert records every batch it is handed.
type captureInsert struct {
	mu   sync.Mutex
	rows []Row
}

func (c *captureInsert) insert(_ context.Context, rows []Row) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, rows...)
	return nil
}

func (c *captureInsert) all() []Row {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Row(nil), c.rows...)
}

func stopSink(t *testing.T, s *Sink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// The property the whole design rests on: a caller inside a log call never
// waits on the database. The insert here blocks forever until released; the
// caller writes 5000 lines and must return within the deadline, with the
// overflow counted under queue.
func TestSinkNeverBlocksAgainstAStalledInsert(t *testing.T) {
	release := make(chan struct{})
	stalled := func(_ context.Context, _ []Row) error {
		<-release
		return nil
	}
	s := NewSink(nil, SinkOptions{
		QueueSize: 64, MaxBatch: 8, FlushInterval: 5 * time.Millisecond,
		MaxLinesPerSecond: 1_000_000, NodeType: "bff", Node: "stalled-node",
		Insert: stalled, Logger: discard(),
	})
	s.Start()
	t.Cleanup(func() {
		close(release)
		stopSink(t, s)
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			s.Write(infoLine(fmt.Sprintf("line %d", i)))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked against a stalled insert; the caller must return")
	}
	st := s.Stats()
	if st.DroppedQueue == 0 {
		t.Fatalf("5000 writes into a 64-line queue behind a stalled insert dropped nothing: %+v", st)
	}
	if st.DroppedQueue+uint64(st.QueueDepth)+st.Written+8 < 5000-64 {
		t.Errorf("the accounting does not add up: %+v", st)
	}
}

func TestSinkRateBucketDropsBeyondNPerSecond(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	cap := &captureInsert{}
	s := NewSink(nil, SinkOptions{
		QueueSize: 1000, MaxLinesPerSecond: 10, NodeType: "bff", Node: "rate-node",
		Insert: cap.insert, Logger: discard(), Now: clock,
	})
	// Not started: the queue holds what the bucket lets through.
	for i := 0; i < 25; i++ {
		s.Write(infoLine("burst"))
	}
	st := s.Stats()
	if st.QueueDepth != 10 || st.DroppedRate != 15 {
		t.Fatalf("25 writes at 10/s: queued %d dropped{rate} %d, want 10 and 15", st.QueueDepth, st.DroppedRate)
	}
	// Half a second later the bucket has refilled five tokens.
	now = now.Add(500 * time.Millisecond)
	for i := 0; i < 10; i++ {
		s.Write(infoLine("after refill"))
	}
	st = s.Stats()
	if st.QueueDepth != 15 || st.DroppedRate != 20 {
		t.Fatalf("after 500ms: queued %d dropped{rate} %d, want 15 and 20", st.QueueDepth, st.DroppedRate)
	}
}

func TestTwoSinksStampTheirOwnNode(t *testing.T) {
	a, b := &captureInsert{}, &captureInsert{}
	opts := func(node string, ins InsertFunc) SinkOptions {
		return SinkOptions{QueueSize: 16, FlushInterval: 5 * time.Millisecond, MaxLinesPerSecond: 1000,
			NodeType: "bff", Node: node, Insert: ins, Logger: discard()}
	}
	s1 := NewSink(nil, opts("bff-a", a.insert))
	s2 := NewSink(nil, opts("bff-b", b.insert))
	s1.Start()
	s2.Start()
	s1.Write(infoLine("from a"))
	s2.Write(infoLine("from b"))
	stopSink(t, s1)
	stopSink(t, s2)
	ra, rb := a.all(), b.all()
	if len(ra) != 1 || len(rb) != 1 {
		t.Fatalf("each sink must accept its own line: a=%d b=%d", len(ra), len(rb))
	}
	if ra[0].Node != "bff-a" || ra[0].Message != "from a" {
		t.Errorf("sink a stamped %q on %q", ra[0].Node, ra[0].Message)
	}
	if rb[0].Node != "bff-b" || rb[0].Message != "from b" {
		t.Errorf("sink b stamped %q on %q", rb[0].Node, rb[0].Message)
	}
	if ra[0].NodeType != "bff" || ra[0].Level != "info" || ra[0].ID == "" {
		t.Errorf("row shape: %+v", ra[0])
	}
}

func TestSinkReportsItsOwnGapsUnderComponentLogs(t *testing.T) {
	var buf bytes.Buffer
	console := slog.New(slog.NewJSONHandler(&buf, nil))
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s := NewSink(nil, SinkOptions{
		QueueSize: 1000, MaxLinesPerSecond: 10, NodeType: "bff", Node: "gap-node",
		Insert: (&captureInsert{}).insert, Logger: console, Now: func() time.Time { return now },
	})
	for i := 0; i < 25; i++ {
		s.Write(infoLine("burst"))
	}
	s.ReportDropsNow()
	out := buf.String()
	if !strings.Contains(out, "logs: dropped 15 lines (queue 0, rate 15, db 0)") {
		t.Fatalf("gap line missing or wrong:\n%s", out)
	}
	if !strings.Contains(out, `"component":"logs"`) {
		t.Errorf("the gap line must carry component `logs` (not the guarded logs.store) so it is itself stored:\n%s", out)
	}
	// A clean interval says nothing.
	buf.Reset()
	s.ReportDropsNow()
	if buf.Len() != 0 {
		t.Errorf("a clean interval logged: %s", buf.String())
	}
}

func TestSinkCountsInsertFailuresAsDBDropsAndWarnsOncePerMinute(t *testing.T) {
	var buf bytes.Buffer
	console := slog.New(slog.NewJSONHandler(&buf, nil))
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	failing := func(_ context.Context, _ []Row) error { return errors.New("connection refused") }
	s := NewSink(nil, SinkOptions{
		QueueSize: 100, MaxBatch: 4, FlushInterval: 5 * time.Millisecond, MaxLinesPerSecond: 100000,
		NodeType: "bff", Node: "db-node", Insert: failing, Logger: console,
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
	})
	s.Start()
	for i := 0; i < 12; i++ {
		s.Write(infoLine("doomed"))
	}
	stopSink(t, s)
	st := s.Stats()
	if st.DroppedDB != 12 || st.Written != 0 {
		t.Fatalf("12 lines behind a failing insert: dropped{db} %d written %d", st.DroppedDB, st.Written)
	}
	if st.InsertErrors < 3 {
		t.Errorf("12 lines in batches of 4 should be at least 3 failed inserts, got %d", st.InsertErrors)
	}
	out := buf.String()
	if got := strings.Count(out, "insert failed"); got != 1 {
		t.Errorf("the insert warning must be logged at most once a minute; got %d:\n%s", got, out)
	}
	if !strings.Contains(out, `"component":"logs.store"`) {
		t.Errorf("the store's own failure must carry the guarded component logs.store:\n%s", out)
	}
}

func TestSinkAppliesTheFloorToLinesWithTheirOwnLevel(t *testing.T) {
	// The process floor in tests is the default, info: a debug line handed
	// straight to the sink (the OS write path) is dropped under level.
	s := NewSink(nil, SinkOptions{QueueSize: 10, MaxLinesPerSecond: 1000, NodeType: "bff", Node: "n",
		Insert: (&captureInsert{}).insert, Logger: discard()})
	ok := s.WriteStamped(logger.Line{At: time.Now(), Level: slog.LevelDebug, Message: "dbg"}, Stamp{NodeType: NodeTypeOS})
	if ok || s.Stats().DroppedLevel != 1 {
		t.Fatalf("a debug line under an info floor was accepted: %+v", s.Stats())
	}
}

func TestRowFromLineIsSafeForTheTable(t *testing.T) {
	long := strings.Repeat("é", 3000) // 6000 bytes, 3000 runes
	attrs := map[string]any{"big": strings.Repeat("x", 9000), "nul": "a\x00b", "n": int64(3)}
	l := logger.Line{
		At: time.Date(2026, 9, 3, 1, 2, 3, 0, time.UTC), Level: slog.LevelWarn + 1,
		Component: "svc\x00", Message: long + "\x00", Attributes: attrs,
	}
	r := rowFromLine(l, Stamp{}, "bff", "node-1")
	if len(r.Message) > MaxMessageBytes || !json.Valid([]byte(fmt.Sprintf("%q", r.Message))) {
		t.Errorf("message not truncated safely: %d bytes", len(r.Message))
	}
	if strings.ContainsRune(r.Message, 0) || strings.Contains(r.Component, "\x00") {
		t.Errorf("NUL bytes survived into a text column")
	}
	if r.Level != "warn" {
		t.Errorf("slog.LevelWarn+1 folded to %q, want warn", r.Level)
	}
	if len(r.Attributes) > MaxAttributesBytes || !json.Valid(r.Attributes) {
		t.Errorf("attributes are %d bytes / valid=%v; must fit %d as valid JSON", len(r.Attributes), json.Valid(r.Attributes), MaxAttributesBytes)
	}
	var decoded map[string]any
	if err := json.Unmarshal(r.Attributes, &decoded); err != nil {
		t.Fatalf("attributes: %v", err)
	}
	if decoded["_truncated"] != true {
		t.Errorf("oversized attributes must say so: %v", decoded)
	}
	if s, _ := decoded["nul"].(string); strings.ContainsRune(s, 0) {
		t.Errorf("NUL survived into a jsonb string")
	}
	if r.NodeType != "bff" || r.Node != "node-1" {
		t.Errorf("engine stamp: %q/%q", r.NodeType, r.Node)
	}
	// An OS line keeps its node blank rather than borrowing the bff's.
	os := rowFromLine(l, Stamp{NodeType: NodeTypeOS, Session: "os-1", UserId: "u1"}, "bff", "node-1")
	if os.NodeType != "os" || os.Node != "" || os.Session != "os-1" || os.UserId != "u1" {
		t.Errorf("OS stamp: %+v", os)
	}
	// No attributes -> NULL, not "null" or "{}".
	if r := rowFromLine(logger.Line{Level: slog.LevelInfo, Message: "m"}, Stamp{}, "bff", "n"); r.Attributes != nil {
		t.Errorf("empty attributes must be NULL, got %s", r.Attributes)
	}
	if r := rowFromLine(logger.Line{Level: slog.LevelInfo, Message: "m"}, Stamp{}, "bff", "n"); r.Component != "unknown" {
		t.Errorf("a line with no component must be stored as unknown, got %q", r.Component)
	}
}

func TestLevelName(t *testing.T) {
	cases := map[slog.Level]string{
		slog.LevelDebug: "debug", slog.LevelDebug + 3: "debug", slog.LevelInfo: "info",
		slog.LevelInfo + 2: "info", slog.LevelWarn: "warn", slog.LevelError: "error", slog.LevelError + 8: "error",
	}
	for l, want := range cases {
		if got := levelName(l); got != want {
			t.Errorf("levelName(%v) = %q, want %q", l, got, want)
		}
	}
}

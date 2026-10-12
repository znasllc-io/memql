package logstore

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// memArchiver is an in-memory Archiver that also records the ORDER of
// uploads and deletes on a shared journal, which is what "archives before it
// deletes" is asserted against.
type memArchiver struct {
	mu      sync.Mutex
	objects map[string][]byte
	journal *[]string
}

func newMemArchiver(journal *[]string) *memArchiver {
	return &memArchiver{objects: map[string][]byte{}, journal: journal}
}

func (m *memArchiver) Upload(_ context.Context, container, object string, data []byte, contentType string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if contentType != archiveContentType {
		return "", fmt.Errorf("content type %q", contentType)
	}
	m.objects[object] = append([]byte(nil), data...)
	*m.journal = append(*m.journal, "upload:"+object)
	return "mem://" + container + "/" + object, nil
}

func (m *memArchiver) Download(_ context.Context, _ string, object string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[object]
	if !ok {
		return nil, fmt.Errorf("no object %q", object)
	}
	return append([]byte(nil), b...), nil
}

func (m *memArchiver) ListPrefix(_ context.Context, _ string, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for name := range m.objects {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// memSource is an in-memory rowSource keyed (occurred_at, id).
type memSource struct {
	mu      sync.Mutex
	rows    []Row
	journal *[]string

	// chunkSpan is how much of a day dropDay can take whole, counted from the
	// day's start: 24h models a day that has a chunk of its own (production:
	// one-day chunks), 12h a chunk that covers only its first half, and zero a
	// table with no chunk lying wholly inside the day (a plain table, or a
	// chunk that straddles midnight). dropDay never touches a row outside the
	// span, which is the guarantee drop_chunks gives the real sweep.
	chunkSpan time.Duration
	// dropErr is what dropDay answers instead of dropping anything.
	dropErr error
}

func (m *memSource) sorted() []Row {
	out := append([]Row(nil), m.rows...)
	sort.Slice(out, func(i, j int) bool {
		if !out[i].OccurredAt.Equal(out[j].OccurredAt) {
			return out[i].OccurredAt.Before(out[j].OccurredAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (m *memSource) oldestBefore(_ context.Context, boundary time.Time) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var oldest time.Time
	found := false
	for _, r := range m.rows {
		if r.OccurredAt.Before(boundary) && (!found || r.OccurredAt.Before(oldest)) {
			oldest, found = r.OccurredAt, true
		}
	}
	return oldest, found, nil
}

func (m *memSource) nodeTypesForDay(_ context.Context, day time.Time) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	end := day.AddDate(0, 0, 1)
	for _, r := range m.rows {
		if !r.OccurredAt.Before(day) && r.OccurredAt.Before(end) {
			seen[r.NodeType] = true
		}
	}
	var out []string
	for nt := range seen {
		out = append(out, nt)
	}
	sort.Strings(out)
	return out, nil
}

func (m *memSource) page(_ context.Context, day time.Time, nodeType string, afterAt time.Time, afterId string, limit int) ([]Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	end := day.AddDate(0, 0, 1)
	var out []Row
	for _, r := range m.sorted() {
		if r.NodeType != nodeType || r.OccurredAt.Before(day) || !r.OccurredAt.Before(end) {
			continue
		}
		if !afterAt.IsZero() && !(r.OccurredAt.After(afterAt) || (r.OccurredAt.Equal(afterAt) && r.ID > afterId)) {
			continue
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memSource) deleteDay(_ context.Context, day time.Time, limit int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	end := day.AddDate(0, 0, 1)
	var kept []Row
	var n int64
	for _, r := range m.rows {
		if n < int64(limit) && !r.OccurredAt.Before(day) && r.OccurredAt.Before(end) {
			n++
			continue
		}
		kept = append(kept, r)
	}
	m.rows = kept
	*m.journal = append(*m.journal, fmt.Sprintf("delete:%s:%d", day.Format("2006-01-02"), n))
	return n, nil
}

func (m *memSource) dropDay(_ context.Context, day time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dropErr != nil {
		*m.journal = append(*m.journal, "drop:"+day.Format("2006-01-02")+":error")
		return 0, m.dropErr
	}
	end := day.Add(m.chunkSpan)
	var kept []Row
	var gone int
	for _, r := range m.rows {
		if m.chunkSpan > 0 && !r.OccurredAt.Before(day) && r.OccurredAt.Before(end) {
			gone++
			continue
		}
		kept = append(kept, r)
	}
	m.rows = kept
	var chunks int64
	if gone > 0 {
		chunks = 1
	}
	*m.journal = append(*m.journal, fmt.Sprintf("drop:%s:%d", day.Format("2006-01-02"), chunks))
	return chunks, nil
}

func (m *memSource) dayHasRows(_ context.Context, day time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	end := day.AddDate(0, 0, 1)
	for _, r := range m.rows {
		if !r.OccurredAt.Before(day) && r.OccurredAt.Before(end) {
			return true, nil
		}
	}
	return false, nil
}

func (m *memSource) insertIgnore(_ context.Context, rows []Row) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	have := map[string]bool{}
	for _, r := range m.rows {
		have[r.OccurredAt.UTC().Format(time.RFC3339Nano)+"|"+r.ID] = true
	}
	var n int64
	for _, r := range rows {
		k := r.OccurredAt.UTC().Format(time.RFC3339Nano) + "|" + r.ID
		if have[k] {
			continue
		}
		have[k] = true
		m.rows = append(m.rows, r)
		n++
	}
	return n, nil
}

type memLock struct{ acquired bool }

func (l memLock) tryLock(context.Context) (func(), bool, error) {
	if !l.acquired {
		return nil, false, nil
	}
	return func() {}, true, nil
}

func seedRow(at time.Time, nodeType, id string) Row {
	return Row{OccurredAt: at.UTC(), ID: id, NodeType: nodeType, Node: nodeType + "-1", Level: "info",
		Component: "svc", Message: "m " + id, Attributes: json.RawMessage(`{"k":"v"}`)}
}

func newSweeper(src *memSource, arch Archiver, container string, now time.Time) *Sweeper {
	return &Sweeper{Archive: arch, Container: container, RetentionDays: 30, Logger: discard(),
		Now: func() time.Time { return now }, source: src, locker: memLock{acquired: true}}
}

func TestSweepRefusesWithoutAContainerAndDeletesNothing(t *testing.T) {
	var journal []string
	now := time.Date(2026, 9, 3, 3, 20, 0, 0, time.UTC)
	src := &memSource{journal: &journal, rows: []Row{seedRow(now.AddDate(0, 0, -40), "bff", "old-1")}}
	s := newSweeper(src, newMemArchiver(&journal), "", now)
	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Refused == "" || !strings.Contains(rep.Refused, EnvArchiveContainer) {
		t.Errorf("no container must be a refusal naming the variable; got %+v", rep)
	}
	if rep.RowsDeleted != 0 || rep.DaysArchived != 0 || len(journal) != 0 || len(src.rows) != 1 {
		t.Errorf("a refused sweep must delete nothing: %+v journal=%v", rep, journal)
	}
	// And with a container but no client, the same refusal.
	s2 := newSweeper(src, nil, "c", now)
	rep2, _ := s2.Run(context.Background())
	if rep2.Refused == "" || len(src.rows) != 1 {
		t.Errorf("no archive client must refuse: %+v", rep2)
	}
}

func TestSweepIsANoOpOnAnEmptyStore(t *testing.T) {
	var journal []string
	now := time.Date(2026, 9, 3, 3, 20, 0, 0, time.UTC)
	src := &memSource{journal: &journal}
	rep, err := newSweeper(src, newMemArchiver(&journal), "c", now).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.DaysArchived != 0 || rep.RowsArchived != 0 || rep.RowsDeleted != 0 || len(journal) != 0 || rep.Refused != "" || rep.Skipped != "" {
		t.Errorf("empty store: %+v journal=%v", rep, journal)
	}
	if want := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC); !rep.Boundary.Equal(want) {
		t.Errorf("boundary %v, want UTC midnight 30 days ago %v", rep.Boundary, want)
	}
}

func TestSweepSkipsWhenAnotherSweepHoldsTheLock(t *testing.T) {
	var journal []string
	now := time.Date(2026, 9, 3, 3, 20, 0, 0, time.UTC)
	src := &memSource{journal: &journal, rows: []Row{seedRow(now.AddDate(0, 0, -40), "bff", "old-1")}}
	s := newSweeper(src, newMemArchiver(&journal), "c", now)
	s.locker = memLock{acquired: false}
	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Skipped != "another sweep is running" || len(src.rows) != 1 || len(journal) != 0 {
		t.Errorf("a held lock must skip and touch nothing: %+v", rep)
	}
}

func TestSweepArchivesEveryNodeTypeOfADayBeforeDeletingIt(t *testing.T) {
	var journal []string
	now := time.Date(2026, 9, 3, 3, 20, 0, 0, time.UTC)
	d1 := now.AddDate(0, 0, -40)
	d2 := now.AddDate(0, 0, -35)
	recent := now.AddDate(0, 0, -3)
	src := &memSource{journal: &journal, rows: []Row{
		seedRow(d1.Add(1*time.Hour), "bff", "d1-bff-a"),
		seedRow(d1.Add(2*time.Hour), "bff", "d1-bff-b"),
		seedRow(d1.Add(3*time.Hour), "agent", "d1-agent-a"),
		seedRow(d2.Add(1*time.Hour), "bff", "d2-bff-a"),
		seedRow(recent, "bff", "recent-a"),
	}}
	arch := newMemArchiver(&journal)
	rep, err := newSweeper(src, arch, "c", now).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.DaysArchived != 2 || rep.RowsArchived != 4 || rep.RowsDeleted != 4 {
		t.Fatalf("report: %+v", rep)
	}
	day1, day2 := utcDay(d1).Format("2006-01-02"), utcDay(d2).Format("2006-01-02")
	for _, obj := range []string{"logs/" + day1 + "/agent.ndjson.gz", "logs/" + day1 + "/bff.ndjson.gz", "logs/" + day2 + "/bff.ndjson.gz"} {
		if _, ok := arch.objects[obj]; !ok {
			t.Errorf("archive object %s missing; have %v", obj, journal)
		}
	}
	// The recent row survives.
	if len(src.rows) != 1 || src.rows[0].ID != "recent-a" {
		t.Errorf("rows after sweep: %+v", src.rows)
	}
	// ORDER: every upload of a day precedes that day's delete, and the delete
	// of day 1 precedes the uploads of day 2 (days are processed in order).
	idx := func(prefix string) int {
		for i, e := range journal {
			if strings.HasPrefix(e, prefix) {
				return i
			}
		}
		return -1
	}
	if idx("upload:logs/"+day1+"/agent") > idx("delete:"+day1) || idx("upload:logs/"+day1+"/bff") > idx("delete:"+day1) {
		t.Errorf("a delete ran before every node type of its day was uploaded: %v", journal)
	}
	if idx("delete:"+day1) > idx("upload:logs/"+day2+"/bff") {
		t.Errorf("days must be processed oldest first: %v", journal)
	}
	if idx("upload:logs/"+day2+"/bff") > idx("delete:"+day2) {
		t.Errorf("day 2 deleted before its upload: %v", journal)
	}

	// The object is gzip NDJSON in (occurred_at, id) order with the concept's
	// field names and the attributes inline.
	gz, err := gzip.NewReader(bytes.NewReader(arch.objects["logs/"+day1+"/bff.ndjson.gz"]))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	var records []map[string]any
	sc := bufio.NewScanner(gz)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("record: %v", err)
		}
		records = append(records, rec)
	}
	if len(records) != 2 || records[0]["id"] != "d1-bff-a" || records[1]["id"] != "d1-bff-b" {
		t.Fatalf("records: %v", records)
	}
	for _, key := range []string{"occurredAt", "id", "nodeType", "node", "level", "component", "app", "message", "attributes", "subject", "subjectConcept", "session", "userId"} {
		if _, ok := records[0][key]; !ok {
			t.Errorf("record lacks the concept field %q: %v", key, records[0])
		}
	}
	if attrs, _ := records[0]["attributes"].(map[string]any); attrs["k"] != "v" {
		t.Errorf("attributes must be inline JSON: %v", records[0]["attributes"])
	}
}

func TestSweepIsBoundedPerRun(t *testing.T) {
	var journal []string
	now := time.Date(2026, 9, 3, 3, 20, 0, 0, time.UTC)
	src := &memSource{journal: &journal, rows: []Row{
		seedRow(now.AddDate(0, 0, -50), "bff", "a"),
		seedRow(now.AddDate(0, 0, -45), "bff", "b"),
		seedRow(now.AddDate(0, 0, -40), "bff", "c"),
	}}
	s := newSweeper(src, newMemArchiver(&journal), "c", now)
	s.MaxDays = 2
	rep, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.DaysArchived != 2 || !rep.Truncated || len(src.rows) != 1 || src.rows[0].ID != "c" {
		t.Errorf("bounded run: %+v rows=%v", rep, src.rows)
	}
}

// journalIndex is the position of the first journal entry that starts with
// prefix, or -1. Callers assert on presence before they compare positions,
// because -1 sorts before everything and would pass an ordering check by
// accident.
func journalIndex(journal []string, prefix string) int {
	for i, e := range journal {
		if strings.HasPrefix(e, prefix) {
			return i
		}
	}
	return -1
}

// uploadFailsArchiver refuses one object and archives the rest.
type uploadFailsArchiver struct {
	*memArchiver
	failObject string
}

func (a uploadFailsArchiver) Upload(ctx context.Context, container, object string, data []byte, contentType string) (string, error) {
	if object == a.failObject {
		return "", errors.New("blob store unreachable")
	}
	return a.memArchiver.Upload(ctx, container, object, data, contentType)
}

// 2026-10-10 and 2026-10-11: the sweep archived 2026-09-09's 18,010 rows and
// its DELETE then hit the 8 s statement timeout (over a chunk the compression
// policy had long since compressed). The sweep now drops the archived day's
// chunk, and deletes nothing when the chunk was the whole day.
func TestSweepDropsArchivedDayChunk(t *testing.T) {
	var journal []string
	now := time.Date(2026, 9, 3, 3, 20, 0, 0, time.UTC)
	d1 := now.AddDate(0, 0, -40)
	d2 := now.AddDate(0, 0, -35)
	src := &memSource{journal: &journal, chunkSpan: 24 * time.Hour, rows: []Row{
		seedRow(d1.Add(1*time.Hour), "bff", "d1-bff-a"),
		seedRow(d1.Add(2*time.Hour), "bff", "d1-bff-b"),
		seedRow(d1.Add(3*time.Hour), "agent", "d1-agent-a"),
		seedRow(d2.Add(1*time.Hour), "bff", "d2-bff-a"),
		seedRow(now.AddDate(0, 0, -3), "bff", "recent-a"),
	}}
	rep, err := newSweeper(src, newMemArchiver(&journal), "c", now).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// RowsDeleted is the archived count: the rows left with their chunks, and
	// drop_chunks reports chunks, not rows.
	if rep.DaysArchived != 2 || rep.RowsArchived != 4 || rep.RowsDeleted != 4 || rep.ChunksDropped != 2 {
		t.Fatalf("report: %+v", rep)
	}
	for _, e := range journal {
		if strings.HasPrefix(e, "delete:") {
			t.Errorf("a day that left with its chunk must not be deleted row by row: %v", journal)
		}
	}
	if len(src.rows) != 1 || src.rows[0].ID != "recent-a" {
		t.Errorf("rows after sweep: %+v", src.rows)
	}

	// ORDER: every upload of a day precedes that day's drop, and the days go
	// oldest first.
	day1, day2 := utcDay(d1).Format("2006-01-02"), utcDay(d2).Format("2006-01-02")
	for _, prefix := range []string{
		"upload:logs/" + day1 + "/agent", "upload:logs/" + day1 + "/bff", "drop:" + day1 + ":1",
		"upload:logs/" + day2 + "/bff", "drop:" + day2 + ":1",
	} {
		if journalIndex(journal, prefix) < 0 {
			t.Fatalf("journal lacks %q: %v", prefix, journal)
		}
	}
	idx := func(prefix string) int { return journalIndex(journal, prefix) }
	if idx("upload:logs/"+day1+"/agent") > idx("drop:"+day1) || idx("upload:logs/"+day1+"/bff") > idx("drop:"+day1) {
		t.Errorf("a chunk was dropped before every node type of its day was uploaded: %v", journal)
	}
	if idx("drop:"+day1) > idx("upload:logs/"+day2+"/bff") {
		t.Errorf("days must be processed oldest first: %v", journal)
	}
	if idx("upload:logs/"+day2+"/bff") > idx("drop:"+day2) {
		t.Errorf("day 2 dropped before its upload: %v", journal)
	}

	// The reply a caller reads carries the count as chunks_dropped.
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var reply map[string]any
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if reply["chunks_dropped"] != float64(2) {
		t.Errorf("report JSON lacks chunks_dropped=2: %s", body)
	}
}

// A day whose chunk does not lie wholly inside it (a plain table, or a chunk
// that straddles midnight) is not dropped. The rows still go, in batches, until
// the day is empty.
func TestSweepFallsBackToDeleteWhenNoWholeChunk(t *testing.T) {
	var journal []string
	now := time.Date(2026, 9, 3, 3, 20, 0, 0, time.UTC)
	start := utcDay(now.AddDate(0, 0, -40))
	day := start.Format("2006-01-02")
	// One row more than a DELETE batch, so "until empty" takes two statements.
	n := SweepDeleteBatch + 1
	rows := make([]Row, 0, n+1)
	for i := 0; i < n; i++ {
		rows = append(rows, seedRow(start.Add(time.Duration(i)*time.Second), "bff", fmt.Sprintf("d-%05d", i)))
	}
	rows = append(rows, seedRow(now.AddDate(0, 0, -3), "bff", "recent-a"))
	src := &memSource{journal: &journal, rows: rows} // chunkSpan zero: no whole chunk

	rep, err := newSweeper(src, newMemArchiver(&journal), "c", now).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.DaysArchived != 1 || rep.RowsArchived != int64(n) || rep.RowsDeleted != int64(n) || rep.ChunksDropped != 0 {
		t.Fatalf("report: %+v", rep)
	}
	want := []string{"drop:" + day + ":0", fmt.Sprintf("delete:%s:%d", day, SweepDeleteBatch), "delete:" + day + ":1"}
	if len(journal) < len(want) || strings.Join(journal[len(journal)-len(want):], ",") != strings.Join(want, ",") {
		t.Errorf("after the uploads the journal must be the drop that found no chunk, then the DELETE batches until the day is empty: %v", journal)
	}
	if len(src.rows) != 1 || src.rows[0].ID != "recent-a" {
		t.Errorf("rows after sweep: %d, first %+v", len(src.rows), src.rows)
	}
}

// A chunk that covers only part of the day goes whole and the rest of the day
// goes by DELETE. Either way the day ends empty and is counted as the rows it
// archived.
func TestSweepEmptiesTheRestOfADayAfterAPartialDrop(t *testing.T) {
	var journal []string
	now := time.Date(2026, 9, 3, 3, 20, 0, 0, time.UTC)
	start := utcDay(now.AddDate(0, 0, -40))
	day := start.Format("2006-01-02")
	src := &memSource{journal: &journal, chunkSpan: 12 * time.Hour, rows: []Row{
		seedRow(start.Add(1*time.Hour), "bff", "early-a"),
		seedRow(start.Add(2*time.Hour), "bff", "early-b"),
		seedRow(start.Add(3*time.Hour), "bff", "early-c"),
		seedRow(start.Add(13*time.Hour), "bff", "late-a"),
		seedRow(start.Add(14*time.Hour), "bff", "late-b"),
		seedRow(now.AddDate(0, 0, -3), "bff", "recent-a"),
	}}
	rep, err := newSweeper(src, newMemArchiver(&journal), "c", now).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.DaysArchived != 1 || rep.RowsArchived != 5 || rep.RowsDeleted != 5 || rep.ChunksDropped != 1 {
		t.Fatalf("report: %+v", rep)
	}
	want := []string{"drop:" + day + ":1", "delete:" + day + ":2"}
	if len(journal) < len(want) || strings.Join(journal[len(journal)-len(want):], ",") != strings.Join(want, ",") {
		t.Errorf("the chunk goes first, then only the rows outside it: %v", journal)
	}
	if len(src.rows) != 1 || src.rows[0].ID != "recent-a" {
		t.Errorf("rows after sweep: %+v", src.rows)
	}
}

// NO ARCHIVE, NO DELETE holds for the chunk drop: an archive that fails for
// any node type of the day leaves the day, and its chunk, exactly as it was.
func TestSweepDoesNotDropBeforeArchive(t *testing.T) {
	var journal []string
	now := time.Date(2026, 9, 3, 3, 20, 0, 0, time.UTC)
	d1 := now.AddDate(0, 0, -40)
	d2 := now.AddDate(0, 0, -35)
	day1 := utcDay(d1).Format("2006-01-02")
	rows := []Row{
		seedRow(d1.Add(1*time.Hour), "bff", "d1-bff-a"),
		seedRow(d1.Add(2*time.Hour), "agent", "d1-agent-a"),
		seedRow(d2.Add(1*time.Hour), "bff", "d2-bff-a"),
	}
	src := &memSource{journal: &journal, chunkSpan: 24 * time.Hour, rows: append([]Row(nil), rows...)}
	// agent uploads first (node types go in order), bff then fails: the day is
	// half archived, which is not archived.
	arch := uploadFailsArchiver{memArchiver: newMemArchiver(&journal), failObject: "logs/" + day1 + "/bff.ndjson.gz"}

	rep, err := newSweeper(src, arch, "c", now).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "blob store unreachable") {
		t.Fatalf("an archive failure must end the run with its error: %v", err)
	}
	for _, e := range journal {
		if strings.HasPrefix(e, "drop:") || strings.HasPrefix(e, "delete:") {
			t.Errorf("the day left the store before it was archived: %v", journal)
		}
	}
	if len(src.rows) != len(rows) {
		t.Errorf("rows after a failed archive: %d, want all %d", len(src.rows), len(rows))
	}
	if rep.ChunksDropped != 0 || rep.RowsDeleted != 0 {
		t.Errorf("report: %+v", rep)
	}
}

// A drop that fails is the run's error, like a DELETE that fails: the day was
// archived, nothing is removed row by row behind the failure, and the next run
// archives the day again (same object names) and retries.
func TestSweepStopsWhenTheChunkDropFails(t *testing.T) {
	var journal []string
	now := time.Date(2026, 9, 3, 3, 20, 0, 0, time.UTC)
	d1 := now.AddDate(0, 0, -40)
	d2 := now.AddDate(0, 0, -35)
	src := &memSource{journal: &journal, chunkSpan: 24 * time.Hour,
		dropErr: errors.New("canceling statement due to statement timeout"),
		rows: []Row{
			seedRow(d1.Add(1*time.Hour), "bff", "d1-bff-a"),
			seedRow(d1.Add(2*time.Hour), "bff", "d1-bff-b"),
			seedRow(d2.Add(1*time.Hour), "bff", "d2-bff-a"),
		}}
	rep, err := newSweeper(src, newMemArchiver(&journal), "c", now).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("a failed drop must end the run with its error: %v", err)
	}
	if rep.DaysArchived != 1 || rep.RowsArchived != 2 || rep.RowsDeleted != 0 || rep.ChunksDropped != 0 {
		t.Errorf("report: %+v", rep)
	}
	for _, e := range journal {
		if strings.HasPrefix(e, "delete:") {
			t.Errorf("a failed drop must not be followed by a row DELETE: %v", journal)
		}
	}
	if len(src.rows) != 3 {
		t.Errorf("rows after a failed drop: %d, want all 3", len(src.rows))
	}
}

func TestRestoreIsIdempotentAgainstAnArchive(t *testing.T) {
	var journal []string
	now := time.Date(2026, 9, 3, 3, 20, 0, 0, time.UTC)
	d1 := now.AddDate(0, 0, -40)
	src := &memSource{journal: &journal, rows: []Row{
		seedRow(d1.Add(1*time.Hour), "bff", "r-1"),
		seedRow(d1.Add(2*time.Hour), "bff", "r-2"),
		seedRow(d1.Add(3*time.Hour), "agent", "r-3"),
	}}
	arch := newMemArchiver(&journal)
	s := newSweeper(src, arch, "c", now)
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(src.rows) != 0 {
		t.Fatalf("rows remain after the sweep: %v", src.rows)
	}
	day := utcDay(d1).Format("2006-01-02")

	// One node type back.
	rep, err := s.Restore(context.Background(), day, "bff")
	if err != nil {
		t.Fatalf("Restore bff: %v", err)
	}
	if rep.Restored != 2 || rep.Skipped != 0 || len(rep.Objects) != 1 {
		t.Errorf("first restore: %+v", rep)
	}
	// Everything back: the bff rows are already present and skipped.
	rep, err = s.Restore(context.Background(), day, "")
	if err != nil {
		t.Fatalf("Restore all: %v", err)
	}
	if rep.Restored != 1 || rep.Skipped != 2 || len(rep.Objects) != 2 {
		t.Errorf("second restore: %+v", rep)
	}
	// A third restores nothing.
	rep, err = s.Restore(context.Background(), day, "")
	if err != nil {
		t.Fatalf("Restore again: %v", err)
	}
	if rep.Restored != 0 || rep.Skipped != 3 {
		t.Errorf("third restore must restore 0: %+v", rep)
	}
	if !strings.Contains(rep.Note, "swept again") {
		t.Errorf("the reply must say a restored day is swept again: %q", rep.Note)
	}
	if len(src.rows) != 3 {
		t.Errorf("rows after restores: %d", len(src.rows))
	}
	// A day with nothing archived is an error that says so.
	if _, err := s.Restore(context.Background(), "2020-01-01", ""); err == nil || !strings.Contains(err.Error(), "nothing is archived") {
		t.Errorf("empty day: %v", err)
	}
	if _, err := s.Restore(context.Background(), "not-a-day", ""); err == nil {
		t.Errorf("a malformed day must be refused")
	}
}

func TestListArchiveParsesObjectsNewestDayFirst(t *testing.T) {
	var journal []string
	arch := newMemArchiver(&journal)
	for _, name := range []string{
		"logs/2026-08-01/bff.ndjson.gz", "logs/2026-08-02/agent.ndjson.gz", "logs/2026-08-02/bff.ndjson.gz",
		"logs/not-a-day/bff.ndjson.gz", "logs/2026-08-03/readme.txt", "other/2026-08-03/bff.ndjson.gz",
	} {
		arch.objects[name] = []byte("x")
	}
	s := &Sweeper{Archive: arch, Container: "c", Logger: discard()}
	got, err := s.ListArchive(context.Background())
	if err != nil {
		t.Fatalf("ListArchive: %v", err)
	}
	var names []string
	for _, o := range got {
		names = append(names, o.Day+"/"+o.NodeType)
		if o.SizeKnown {
			t.Errorf("an archiver without sizes must not claim one: %+v", o)
		}
	}
	want := []string{"2026-08-02/agent", "2026-08-02/bff", "2026-08-01/bff"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("ListArchive = %v, want %v", names, want)
	}
	// No archive configured: nil, nil -- the handler says which.
	none := &Sweeper{Logger: discard()}
	if got, err := none.ListArchive(context.Background()); err != nil || got != nil {
		t.Errorf("no archive: %v %v", got, err)
	}
}

func TestArchiveObjectNameRoundTrips(t *testing.T) {
	day := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)
	name := ArchiveObjectName(day, "voice")
	if name != "logs/2026-09-03/voice.ndjson.gz" {
		t.Fatalf("name %q", name)
	}
	obj, ok := parseArchiveObject(name)
	if !ok || obj.Day != "2026-09-03" || obj.NodeType != "voice" {
		t.Errorf("parse: %+v %v", obj, ok)
	}
}

package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLogFixture(t *testing.T, dir, content string, appendLog bool) {
	t.Helper()
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if appendLog {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := os.OpenFile(filepath.Join(dir, "worker.log"), flags, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

func logPageFixture(t *testing.T, dir, cursor string) localLogPage {
	t.Helper()
	page, err := readLocalWorkerLogPage(dir, cursor)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestLocalLogCursorRetainsDuplicateEventsAndDrainsBacklog(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, strings.Repeat("same event\n", 400), false)
	first := logPageFixture(t, dir, "")
	if len(first.Entries) != 300 || first.HasMore || first.Cursor == "" {
		t.Fatalf("initial page: %+v", first)
	}
	stable := logPageFixture(t, dir, "")
	seen := map[string]bool{}
	for i, e := range first.Entries {
		if e.ID == "" || seen[e.ID] || e.ID != stable.Entries[i].ID {
			t.Fatal("same text must retain distinct stable event IDs")
		}
		seen[e.ID] = true
	}
	idle := logPageFixture(t, dir, first.Cursor)
	if len(idle.Entries) != 0 || idle.Cursor != first.Cursor || idle.HasMore || idle.Reset != "" {
		t.Fatalf("unchanged read moved state: %+v", idle)
	}
	writeLogFixture(t, dir, "same event\n", true)
	shifted := logPageFixture(t, dir, "")
	if shifted.Entries[0].ID != first.Entries[1].ID || shifted.Entries[298].ID != first.Entries[299].ID {
		t.Fatal("appending shifted the identities of retained entries")
	}
	appended := logPageFixture(t, dir, first.Cursor)
	if len(appended.Entries) != 1 || seen[appended.Entries[0].ID] {
		t.Fatal("appended equal message lost its identity")
	}
	seen[appended.Entries[0].ID] = true
	writeLogFixture(t, dir, strings.Repeat("same event\n", 650), true)
	cursor, count := appended.Cursor, 0
	for {
		next := logPageFixture(t, dir, cursor)
		if next.Reset != "" || next.Cursor == cursor {
			t.Fatalf("backlog read: %+v", next)
		}
		for _, e := range next.Entries {
			if seen[e.ID] {
				t.Fatal("cursor repeated an event")
			}
			seen[e.ID] = true
		}
		count += len(next.Entries)
		cursor = next.Cursor
		if !next.HasMore {
			break
		}
		if count > 650 {
			t.Fatal("cursor did not drain")
		}
	}
	if count != 650 {
		t.Fatalf("read %d events, want 650", count)
	}
}

func TestLocalLogCursorHoldsPartialRecordsUntilComplete(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, "ready\n{\"msg\":\"part", false)
	first := logPageFixture(t, dir, "")
	if len(first.Entries) != 1 || first.Entries[0].Message != "ready" || first.HasMore {
		t.Fatalf("partial record rendered: %+v", first)
	}
	idle := logPageFixture(t, dir, first.Cursor)
	if len(idle.Entries) != 0 || idle.Cursor != first.Cursor || idle.HasMore {
		t.Fatalf("partial record advanced: %+v", idle)
	}
	writeLogFixture(t, dir, "ial\"}\n", true)
	next := logPageFixture(t, dir, first.Cursor)
	if len(next.Entries) != 1 || next.Entries[0].Message != "partial" || next.HasMore {
		t.Fatalf("completed record: %+v", next)
	}
}

func TestLocalLogCursorReportsRotationTruncationAndMissing(t *testing.T) {
	for _, kind := range []string{"rotated", "truncated", "regrown", "missing"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			writeLogFixture(t, dir, "original event\n", false)
			first := logPageFixture(t, dir, "")
			want := kind
			switch kind {
			case "rotated":
				if err := os.Rename(filepath.Join(dir, "worker.log"), filepath.Join(dir, "old.log")); err != nil {
					t.Fatal(err)
				}
				writeLogFixture(t, dir, "replacement\n", false)
			case "truncated":
				writeLogFixture(t, dir, "short\n", false)
			case "regrown":
				writeLogFixture(t, dir, "different and longer than original event\n", false)
				want = "truncated"
			case "missing":
				if err := os.Remove(filepath.Join(dir, "worker.log")); err != nil {
					t.Fatal(err)
				}
			}
			next := logPageFixture(t, dir, first.Cursor)
			if next.Reset != want || next.HasMore {
				t.Fatalf("reset %+v, want %s", next, want)
			}
			if kind == "missing" {
				if len(next.Entries) != 0 || next.Cursor != "" {
					t.Fatalf("missing log: %+v", next)
				}
			} else if len(next.Entries) != 1 || next.Entries[0].ID == first.Entries[0].ID {
				t.Fatalf("replacement event: %+v", next)
			}
		})
	}
}

func TestLocalLogCursorBoundsOutputAndReportsExpiredHistory(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, "before\n", false)
	first := logPageFixture(t, dir, "")
	// Control characters expand in JSON. Test the encoded budget, not only the
	// raw file size, while retaining the existing structured-field allowlist.
	line := strings.Repeat("\x01", 2000) + "\n"
	writeLogFixture(t, dir, strings.Repeat(line, 600)+`{"msg":"safe","token":"private-token","payload":"private prompt"}`+"\n", true)
	next := logPageFixture(t, dir, first.Cursor)
	if next.Reset != "expired" || len(next.Entries) == 0 {
		t.Fatalf("expired history: %+v", next)
	}
	response, err := localLogsControlResponse(dir, first.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(response)
	if len(encoded) > localLogOutputLimit+2048 || strings.Contains(string(encoded), "private-token") || strings.Contains(string(encoded), "private prompt") {
		t.Fatalf("unbounded or unredacted response (%d bytes)", len(encoded))
	}
}

func TestLocalLogCursorDrainsByteBoundedPagesAndOversizedRecords(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, "start\n", false)
	first := logPageFixture(t, dir, "")
	line := strings.Repeat("x", 4000) + "\n"
	writeLogFixture(t, dir, strings.Repeat(line, 150), true)
	cursor, count := first.Cursor, 0
	for pages := 0; pages < 10; pages++ {
		next := logPageFixture(t, dir, cursor)
		count += len(next.Entries)
		cursor = next.Cursor
		if !next.HasMore {
			break
		}
	}
	if count != 150 {
		t.Fatalf("byte-sized backlog lost records: %d", count)
	}
	writeLogFixture(t, dir, strings.Repeat("z", localLogReadLimit), true)
	huge := logPageFixture(t, dir, cursor)
	if huge.Reset != "oversized" || len(huge.Entries) != 0 || huge.HasMore {
		t.Fatalf("oversized trailing record: %+v", huge)
	}
	writeLogFixture(t, dir, "ignored remainder\nnext event\n", true)
	next := logPageFixture(t, dir, huge.Cursor)
	if len(next.Entries) != 1 || next.Entries[0].Message != "next event" || next.Reset != "" || next.HasMore {
		t.Fatalf("oversized record recovery: %+v", next)
	}
}

func TestLocalLogCursorRejectsMalformedOrPathBearingInput(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, "ready\n", false)
	first := logPageFixture(t, dir, "")
	raw, _ := base64.RawURLEncoding.DecodeString(first.Cursor)
	var object map[string]any
	json.Unmarshal(raw, &object)
	object["path"] = "/etc/passwd"
	injected, _ := json.Marshal(object)
	for _, bad := range []string{"/etc/passwd", strings.Repeat("a", 513), base64.RawURLEncoding.EncodeToString(injected), base64.RawURLEncoding.EncodeToString(append(raw, []byte(` {}`)...))} {
		response, err := localLogsControlResponse(dir, bad)
		if err == nil || response.OK || len(response.Logs) > 0 || response.LogCursor != "" {
			t.Fatalf("invalid cursor accepted: %+v %v", response, err)
		}
	}
}

func TestLocalLogCursorControlSocketPreservesLegacyAndIncrementalReads(t *testing.T) {
	fleet, _ := controlFixture(t)
	fleet.workers.StateDir = t.TempDir()
	writeLogFixture(t, fleet.workers.StateDir, "first\n", false)
	dir, err := os.MkdirTemp("/tmp", "mq-log-control-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(dir, "private", "worker.sock")
	listener, err := listenControl(ctx, socket, fleet)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	first, err := callControl(socket, controlRequest{Action: "logs"})
	if err != nil || !first.OK || len(first.Logs) != 1 || first.LogCursor == "" {
		t.Fatalf("legacy logs response: %+v %v", first, err)
	}
	writeLogFixture(t, fleet.workers.StateDir, "second\n", true)
	next, err := callControl(socket, controlRequest{Action: "logs", LogCursor: first.LogCursor})
	if err != nil || !next.OK || len(next.Logs) != 1 || next.Logs[0].Message != "second" || next.LogReset != "" {
		t.Fatalf("incremental socket response: %+v %v", next, err)
	}
}

func TestLocalLogCursorCannotSplitAStructuredRecord(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, `{"msg":"safe","payload":"private prompt"}`+"\n", false)
	first := logPageFixture(t, dir, "")
	cursor, err := decodeLocalLogCursor(first.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	cursor.Offset = 15
	file, err := os.Open(filepath.Join(dir, "worker.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cursor.Anchor, err = localLogAnchor(file, cursor.Offset)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cursor)
	if _, err = readLocalWorkerLogPage(dir, base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("cursor inside record accepted")
	}
}

func TestLocalLogCursorBindsAcknowledgementToDisplayedSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, "old record\n", false)
	first, err := readLocalWorkerLogPageWithRead(dir, "", func(file *os.File, data []byte, offset int64) (int, error) {
		n, err := file.ReadAt(data, offset)
		writeLogFixture(t, dir, "new record\n", false) // same inode, same size, after captured read
		return n, err
	})
	if err != nil || len(first.Entries) != 1 || first.Entries[0].Message != "old record" {
		t.Fatalf("captured snapshot: %+v %v", first, err)
	}
	next := logPageFixture(t, dir, first.Cursor)
	if next.Reset != "truncated" || len(next.Entries) != 1 || next.Entries[0].Message != "new record" {
		t.Fatalf("replacement was acknowledged without display: %+v", next)
	}
}

func TestLocalLogCursorRejectsReplacementBetweenValidationAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, "old record\n", false)
	first := logPageFixture(t, dir, "")
	writeLogFixture(t, dir, "appended\n", true)
	_, err := readLocalWorkerLogPageWithRead(dir, first.Cursor, func(file *os.File, data []byte, offset int64) (int, error) {
		writeLogFixture(t, dir, "new record\nappended\n", false)
		return file.ReadAt(data, offset)
	})
	if err == nil {
		t.Fatal("acknowledged a snapshot whose prefix changed after cursor validation")
	}
	next := logPageFixture(t, dir, first.Cursor)
	if next.Reset != "truncated" || len(next.Entries) != 2 || next.Entries[0].Message != "new record" {
		t.Fatalf("replacement read after retry: %+v", next)
	}
}

func TestLocalLogsDoNotExposeMalformedStructuredPayloads(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, `{"msg":"broken","payload":"private prompt","args_preview":"private args",`+"\nlegacy diagnostic\n", false)
	page := logPageFixture(t, dir, "")
	encoded, _ := json.Marshal(page.Entries)
	if len(page.Entries) != 2 || strings.Contains(string(encoded), "private") || page.Entries[1].Message != "legacy diagnostic" {
		t.Fatalf("malformed structured log bypassed whitelist: %s", encoded)
	}
}

func TestLocalLogCursorKeepsRepeatedEventsAfterTruncationDistinct(t *testing.T) {
	dir := t.TempDir()
	writeLogFixture(t, dir, "same event\nold second event\n", false)
	first := logPageFixture(t, dir, "")
	writeLogFixture(t, dir, "same event\nnew second event\n", false)
	next := logPageFixture(t, dir, first.Cursor)
	if next.Reset != "truncated" || len(next.Entries) != 2 || next.Entries[0].ID == first.Entries[0].ID {
		t.Fatalf("same-offset replacement event lost its generation: %+v", next)
	}
	idle := logPageFixture(t, dir, next.Cursor)
	if idle.Cursor != next.Cursor || len(idle.Entries) != 0 || idle.Reset != "" {
		t.Fatalf("replacement generation did not stay stable: %+v", idle)
	}
	writeLogFixture(t, dir, "later event\n", true)
	appended := logPageFixture(t, dir, next.Cursor)
	if len(appended.Entries) != 1 || appended.Reset != "" {
		t.Fatalf("replacement generation did not append: %+v", appended)
	}
}

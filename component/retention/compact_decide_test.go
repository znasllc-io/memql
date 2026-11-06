package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
)

// compact_decide_test.go -- every decision of the closed-chunk compaction, without
// a database (compact_decide.go). compact_db_test.go holds the same pass to
// TimescaleDB.

const mib = int64(1 << 20)

var decideNow = time.Date(2026, 11, 6, 4, 20, 0, 0, time.UTC)

// measured builds a closed chunk whose stored measurement is current: total bytes
// in all, index bytes of indexes, live bytes of live data.
func measured(name string, total, indexes, live int64) *chunk {
	return &chunk{
		Name: name, Hypertable: `public."MemoryNodes"`, TotalBytes: total, IndexBytes: indexes,
		RangeStart: decideNow.Add(-30 * 24 * time.Hour), RangeEnd: decideNow.Add(-23 * 24 * time.Hour),
		Changes: sql.NullInt64{Int64: 1000, Valid: true}, Owned: true,
		m: &measurement{At: decideNow.Add(-time.Hour), TotalBytes: total, IndexBytes: indexes, Changes: sql.NullInt64{Int64: 1000, Valid: true}, LiveBytes: live, LiveRows: 10},
	}
}

func TestStalenessSaysWhyAChunkIsMeasuredAgain(t *testing.T) {
	fresh := measured("c", 1000*mib, 100*mib, 300*mib)
	if got := staleness(fresh, decideNow); got != "" {
		t.Fatalf("an unchanged chunk measured an hour ago is %q, want current", got)
	}
	for _, tc := range []struct {
		name string
		edit func(*chunk)
		want string
	}{
		{"never measured", func(c *chunk) { c.m = nil }, "unmeasured"},
		{"measured past the backstop", func(c *chunk) { c.m.At = decideNow.Add(-measurementMaxAge - time.Hour) }, "aged"},
		// A row added or a vacuum that truncated the heap moves the size.
		{"size changed", func(c *chunk) { c.TotalBytes += 8192 }, "resized"},
		// A retention DELETE changes no size; it moves the change counter.
		{"rows deleted since", func(c *chunk) { c.Changes.Int64 += 500 }, "changed"},
		// A statistics reset restarts the counter below what was stored.
		{"counter reset", func(c *chunk) { c.Changes.Int64 = 0 }, "changed"},
		{"counter appeared", func(c *chunk) { c.m.Changes = sql.NullInt64{} }, "changed"},
		{"counter vanished", func(c *chunk) { c.Changes = sql.NullInt64{} }, "changed"},
	} {
		c := measured("c", 1000*mib, 100*mib, 300*mib)
		tc.edit(c)
		if got := staleness(c, decideNow); got != tc.want {
			t.Errorf("%s: staleness = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMeasureQueueTakesTheUnmeasuredThenTheOldestAndPutsRecentFailuresLast(t *testing.T) {
	small := measured("small", 100*mib, 10*mib, 50*mib)
	small.m = nil
	large := measured("large", 900*mib, 10*mib, 50*mib)
	large.m = nil
	failed := measured("failed", 5000*mib, 10*mib, 50*mib)
	failed.m = nil
	failed.MeasureFailedAt = sql.NullTime{Time: decideNow.Add(-24 * time.Hour), Valid: true}
	current := measured("current", 8000*mib, 10*mib, 50*mib) // measured, unchanged: not queued
	oldFailure := measured("oldFailure", 200*mib, 10*mib, 50*mib)
	oldFailure.m = nil
	oldFailure.MeasureFailedAt = sql.NullTime{Time: decideNow.Add(-measureFailureQuiet - time.Hour), Valid: true}
	// Two measured chunks retention changed since: the one measured longer ago
	// goes first, however small, so a busy week cannot starve it.
	changedLongAgo := measured("changedLongAgo", 300*mib, 10*mib, 50*mib)
	changedLongAgo.m.At = decideNow.Add(-10 * 24 * time.Hour)
	changedLongAgo.Changes.Int64++
	changedYesterday := measured("changedYesterday", 6000*mib, 10*mib, 50*mib)
	changedYesterday.m.At = decideNow.Add(-24 * time.Hour)
	changedYesterday.Changes.Int64++

	var names []string
	for _, c := range measureQueue([]*chunk{small, changedYesterday, failed, current, large, changedLongAgo, oldFailure}, decideNow) {
		names = append(names, c.Name)
	}
	if got, want := strings.Join(names, ","), "large,oldFailure,small,changedLongAgo,changedYesterday,failed"; got != want {
		t.Fatalf("measure queue = %s, want %s", got, want)
	}
}

func TestBackoffDoublesFromADayToSixteen(t *testing.T) {
	for failures, want := range map[int]time.Duration{0: 0, 1: 22 * time.Hour, 2: 46 * time.Hour, 3: 94 * time.Hour, 5: 382 * time.Hour, 9: 382 * time.Hour} {
		if got := backoff(failures); got != want {
			t.Errorf("backoff(%d) = %s, want %s", failures, got, want)
		}
	}
	c := measured("c", 1000*mib, 100*mib, 100*mib)
	c.Failures = 1
	c.AttemptedAt = sql.NullTime{Time: decideNow.Add(-23*time.Hour - 59*time.Minute), Valid: true}
	if inBackoff(c, decideNow) {
		t.Error("a chunk that failed last night is not retried tonight: the nightly schedule missed its slack")
	}
	c.AttemptedAt.Time = decideNow.Add(-time.Hour)
	if !inBackoff(c, decideNow) {
		t.Error("a chunk that failed an hour ago is retried at once")
	}
}

func TestEligibleNeedsBothThresholdsAndOrdersByReclaimable(t *testing.T) {
	opt := CompactOptions{MinReclaimBytes: 256 * mib, MinReclaimPercent: 30}
	// 7.7 GB with 3.3 GB live: production's measured case.
	big := measured("big", 7700*mib, 400*mib, 3300*mib)
	// 4 GB over threshold in bytes, but only 10% of a 40 GB chunk.
	sliver := measured("sliver", 40000*mib, 1000*mib, 35000*mib)
	// Mostly free, but under 256 MiB in all.
	tiny := measured("tiny", 200*mib, 20*mib, 30*mib)
	// Over both, less than big.
	medium := measured("medium", 2000*mib, 200*mib, 800*mib)
	stale := measured("stale", 9000*mib, 100*mib, 100*mib)
	stale.Changes.Int64++
	backingOff := measured("backingOff", 9000*mib, 100*mib, 100*mib)
	backingOff.Failures, backingOff.AttemptedAt = 1, sql.NullTime{Time: decideNow.Add(-time.Hour), Valid: true}

	var names []string
	for _, c := range eligible([]*chunk{tiny, sliver, medium, big, stale, backingOff}, opt, decideNow) {
		names = append(names, c.Name)
	}
	if got, want := strings.Join(names, ","), "big,medium"; got != want {
		t.Fatalf("eligible = %s, want %s", got, want)
	}
	if got := big.reclaimable(); got != 4000*mib {
		t.Fatalf("big gives back %d, want the data less the live bytes (%d)", got, 4000*mib)
	}
	// The thresholds are inclusive.
	edge := measured("edge", 1000*mib, 0, 700*mib) // exactly 300 MiB and 30%
	if len(eligible([]*chunk{edge}, opt, decideNow)) != 1 {
		t.Error("a chunk exactly at both thresholds is not eligible")
	}
}

func freshReport(capacity, data, walCapacity, wal int64, issues ...string) *StorageReading {
	return &StorageReading{ObservedAt: decideNow.Add(-4 * time.Minute), DataCapacityBytes: capacity, DataBytes: data, WALCapacityBytes: walCapacity, WALBytes: wal, Issues: issues}
}

func TestJudgeRoomUsesAFreshReportElseTheConfiguredVolumeElseRefuses(t *testing.T) {
	// Production on 2026-11-06: 78.9% of 33Gi.
	capacity := int64(33 << 30)
	t.Run("fresh report", func(t *testing.T) {
		room, reason, _ := judgeRoom(roomInputs{report: freshReport(capacity, 27_960_000_000, 32<<30, 1<<30), used: 27_000_000_000, now: decideNow})
		if reason != "" || room.Source != "databaseHealth" || room.CapacityBytes != capacity {
			t.Fatalf("room = %+v reason %q", room, reason)
		}
		if room.UsedBytes != 27_960_000_000 {
			t.Errorf("used %d: the larger of the live and reported figures is the report's here", room.UsedBytes)
		}
		if want := usable(capacity) - 27_960_000_000; room.UsableFreeBytes != want {
			t.Errorf("usable free %d, want capacity less the 5%% reserve less used = %d", room.UsableFreeBytes, want)
		}
		if room.WALFreeBytes == nil || *room.WALFreeBytes != usable(32<<30)-(1<<30) {
			t.Errorf("WAL free %v", room.WALFreeBytes)
		}
	})
	t.Run("the database grew since the report", func(t *testing.T) {
		room, _, _ := judgeRoom(roomInputs{report: freshReport(capacity, 27_000_000_000, 32<<30, 0), used: 28_000_000_000, now: decideNow})
		if room.UsedBytes != 28_000_000_000 {
			t.Errorf("used %d, want the live figure", room.UsedBytes)
		}
	})
	t.Run("stale report, configured volume", func(t *testing.T) {
		stale := freshReport(capacity, 1, 1, 1)
		stale.ObservedAt = decideNow.Add(-reportMaxAge - time.Minute)
		room, reason, _ := judgeRoom(roomInputs{report: stale, configured: 10 << 30, used: 2 << 30, now: decideNow})
		if reason != "" || room.Source != "configured" || room.CapacityBytes != 10<<30 || room.WALFreeBytes != nil {
			t.Fatalf("room = %+v reason %q", room, reason)
		}
	})
	t.Run("future report", func(t *testing.T) {
		future := freshReport(capacity, 1, 1, 1)
		future.ObservedAt = decideNow.Add(2 * time.Minute)
		if _, reason, _ := judgeRoom(roomInputs{report: future, now: decideNow}); reason != "room_unknown" {
			t.Fatalf("a report from the future was trusted: %q", reason)
		}
	})
	t.Run("configured but a database is unreadable", func(t *testing.T) {
		_, reason, detail := judgeRoom(roomInputs{configured: 10 << 30, used: 2 << 30, unreadable: 1, now: decideNow})
		if reason != "room_unknown" || !strings.Contains(detail, "not readable") {
			t.Fatalf("reason %q detail %q", reason, detail)
		}
	})
	t.Run("nothing to go on", func(t *testing.T) {
		_, reason, detail := judgeRoom(roomInputs{reportErr: "Storage monitoring is unavailable on this node.", used: 2 << 30, now: decideNow})
		if reason != "room_unknown" || !strings.Contains(detail, "MEMQL_CHUNK_COMPACTION_VOLUME_BYTES") || !strings.Contains(detail, "unavailable on this node") {
			t.Fatalf("reason %q detail %q", reason, detail)
		}
	})
	t.Run("full volume", func(t *testing.T) {
		room, reason, _ := judgeRoom(roomInputs{configured: 10 << 30, used: 11 << 30, now: decideNow})
		if reason != "" || room.UsableFreeBytes != 0 {
			t.Fatalf("a volume over capacity reads %+v", room)
		}
	})
}

func TestChooseTakesTheMostReclaimableThatFitsTwiceOver(t *testing.T) {
	big := measured("big", 7700*mib, 400*mib, 3300*mib)      // need 3.7 GiB, twice 7.4
	medium := measured("medium", 2000*mib, 200*mib, 800*mib) // need 1000 MiB, twice 2000
	candidates := []*chunk{big, medium}

	pick, _ := choose(candidates, Room{UsableFreeBytes: 8000 * mib})
	if pick != big {
		t.Fatalf("with room for both the pass took %v", pick)
	}
	// Production's position: room for the medium chunk, not for the big one.
	// The medium one goes first; what it gives back is room the big one gets.
	pick, smallest := choose(candidates, Room{UsableFreeBytes: 5000 * mib})
	if pick != medium || smallest != medium {
		t.Fatalf("pick %v smallest %v", pick, smallest)
	}
	// Exactly twice over fits.
	if pick, _ := choose([]*chunk{medium}, Room{UsableFreeBytes: 2000 * mib}); pick != medium {
		t.Fatal("room of exactly twice the need does not fit")
	}
	pick, smallest = choose(candidates, Room{UsableFreeBytes: 1999 * mib})
	if pick != nil || smallest != medium {
		t.Fatalf("with room for neither: pick %v smallest %v", pick, smallest)
	}
	// The WAL volume must hold the rewrite's WAL once.
	walFree := 900 * mib
	if pick, _ := choose([]*chunk{medium}, Room{UsableFreeBytes: 8000 * mib, WALFreeBytes: &walFree}); pick != nil {
		t.Fatal("a rewrite larger than the WAL volume's free space was chosen")
	}
}

func TestJudgeWALRefusesABacklogAFailureAStallAndTheUnknown(t *testing.T) {
	archived := func(name string, ago time.Duration) walState {
		return walState{ArchiveMode: "on", SegmentBytes: 16 << 20, CurrentWAL: "00000001000000020000000A",
			LastArchivedWAL: sql.NullString{String: name, Valid: true},
			LastArchivedAt:  sql.NullTime{Time: decideNow.Add(-ago), Valid: true}, DBNow: decideNow}
	}
	for _, tc := range []struct {
		name    string
		state   walState
		issues  []string
		reason  string
		pending int64
	}{
		{"archiving off", walState{ArchiveMode: "off", DBNow: decideNow}, nil, "", 0},
		// 0x20A is being written; 0x209 is the last archived: nothing waits.
		{"caught up", archived("000000010000000200000009", time.Minute), nil, "", 0},
		{"a few waiting", archived("000000010000000200000005", time.Minute), nil, "", 4},
		{"a backlog", archived("000000010000000200000004", time.Minute), nil, "wal_backlog", 5},
		// Across a log boundary: 0x1FF to 0x20A is eleven segments, ten waiting.
		{"across a log file", archived("0000000100000001000000FF", time.Minute), nil, "wal_backlog", 10},
		{"a backup history name", archived("000000010000000200000009.00000028.backup", time.Minute), nil, "", 0},
		{"a timeline history name", archived("00000002.history", time.Minute), nil, "wal_unknown", 0},
		{"nothing archived yet", walState{ArchiveMode: "on", SegmentBytes: 16 << 20, CurrentWAL: "00000001000000020000000A", DBNow: decideNow}, nil, "wal_unknown", 0},
		{"stalled", archived("000000010000000200000008", archiverStallAfter+time.Minute), nil, "wal_backlog", 1},
		{"the report saw it first", archived("000000010000000200000009", time.Minute), []string{"data-volume-over-80-percent", "archive-not-progressing"}, "wal_backlog", 0},
		{"a data warning is not a WAL refusal", archived("000000010000000200000009", time.Minute), []string{"data-volume-over-80-percent"}, "", 0},
	} {
		w, reason, detail := judgeWAL(tc.state, tc.issues)
		if reason != tc.reason || w.PendingSegments != tc.pending {
			t.Errorf("%s: reason %q (%s) pending %d, want %q pending %d", tc.name, reason, detail, w.PendingSegments, tc.reason, tc.pending)
		}
	}
	failed := archived("000000010000000200000009", time.Minute)
	failed.LastFailedAt = sql.NullTime{Time: decideNow.Add(-time.Second), Valid: true}
	if _, reason, detail := judgeWAL(failed, nil); reason != "wal_backlog" || !strings.Contains(detail, "latest attempt failed") {
		t.Errorf("a failed latest attempt: %q %q", reason, detail)
	}
	failed.LastFailedAt.Time = decideNow.Add(-time.Hour) // an old failure, archived since
	if _, reason, _ := judgeWAL(failed, nil); reason != "" {
		t.Errorf("an old failure the archiver has recovered from refused: %q", reason)
	}
}

func TestWALSegmentNumberFollowsTheSegmentSize(t *testing.T) {
	if n, ok := walSegmentNumber("000000010000000200000003", 16<<20); !ok || n != 2*256+3 {
		t.Errorf("16 MB segments: %d %v", n, ok)
	}
	if n, ok := walSegmentNumber("000000010000000200000003", 1<<30); !ok || n != 2*4+3 {
		t.Errorf("1 GB segments: %d %v", n, ok)
	}
	for _, bad := range []string{"", "00000002.history", "zz0000010000000200000003", "0000000100000002000000"} {
		if _, ok := walSegmentNumber(bad, 16<<20); ok {
			t.Errorf("%q read as a segment", bad)
		}
	}
}

func TestFailureKindNamesTheReasons(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{pgdriver.NewError(map[byte]string{'C': "55P03", 'M': "canceling statement due to lock timeout"}), "lock_timeout"},
		{fmt.Errorf("wrapped: %w", pgdriver.NewError(map[byte]string{'C': "55P03"})), "lock_timeout"},
		{pgdriver.NewError(map[byte]string{'C': "57014", 'M': "canceling statement due to statement timeout"}), "statement_timeout"},
		{pgdriver.NewError(map[byte]string{'C': "57014", 'M': "canceling statement due to user request"}), "cancelled"},
		{pgdriver.NewError(map[byte]string{'C': "53100", 'M': "could not extend file: No space left on device"}), "error"},
		{context.DeadlineExceeded, "cancelled"},
		{errors.New("read tcp: i/o timeout"), "error"},
	} {
		if got := failureKind(ctx, tc.err); got != tc.want {
			t.Errorf("failureKind(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got := failureKind(cancelled, errors.New("read tcp: i/o timeout")); got != "cancelled" {
		t.Errorf("an error after the pass was cancelled is %q", got)
	}
}

func TestCompactOptionsTakeTheCallersThresholdsAboveTheFloors(t *testing.T) {
	if got := compactOptionsFrom(nil); got != (CompactOptions{MinReclaimBytes: 256 << 20, MinReclaimPercent: 30, MeasureChunks: 4}) {
		t.Fatalf("defaults %+v", got)
	}
	got := compactOptionsFrom(map[string]any{"dryRun": true, "minReclaimBytes": float64(1 << 30), "minReclaimPercent": int64(50), "measureChunks": json.Number("8")})
	if got != (CompactOptions{DryRun: true, MinReclaimBytes: 1 << 30, MinReclaimPercent: 50, MeasureChunks: 8}) {
		t.Fatalf("explicit %+v", got)
	}
	// Below a floor is the floor; above a ceiling the ceiling.
	got = compactOptionsFrom(map[string]any{"minReclaimBytes": 1024, "minReclaimPercent": 1, "measureChunks": 500})
	if got.MinReclaimBytes != 16<<20 || got.MinReclaimPercent != 5 || got.MeasureChunks != 16 {
		t.Fatalf("clamped %+v", got)
	}
	got = compactOptionsFrom(map[string]any{"minReclaimPercent": 250})
	if got.MinReclaimPercent != 100 {
		t.Fatalf("a share above 100%% is %d", got.MinReclaimPercent)
	}
	// Not a threshold anybody meant: the default.
	for _, v := range []any{-5, float64(2.5), "lots", 1e300, nil} {
		if got := compactOptionsFrom(map[string]any{"minReclaimBytes": v}); got.MinReclaimBytes != 256<<20 {
			t.Errorf("minReclaimBytes %v read as %d", v, got.MinReclaimBytes)
		}
	}
}

func TestVolumeBytesFromEnvTakesOnlyAPositiveWholeNumber(t *testing.T) {
	for raw, want := range map[string]int64{"": 0, "35433480192": 35433480192, " 1024 ": 1024, "33Gi": 0, "-1": 0, "0": 0, "1.5e9": 0} {
		t.Setenv(compactionVolumeEnv, raw)
		if got := volumeBytesFromEnv(); got != want {
			t.Errorf("%s=%q read as %d, want %d", compactionVolumeEnv, raw, got, want)
		}
	}
}

// The operator's stop switch: off stops rewrites before a session is opened, a
// value nobody can parse stops them too, and a dry run is never stopped, so the
// effect of switching it back on can be seen first.
func TestTheStopSwitchStopsRewritesAndLeavesDryRuns(t *testing.T) {
	for raw, wantDisabled := range map[string]bool{"": false, "true": false, "1": false, "false": true, "0": true, "FALSE": true, "off": true, "nope": true} {
		t.Setenv(compactionEnabledEnv, raw)
		disabled, why := compactionDisabled()
		if disabled != wantDisabled || (disabled && !strings.Contains(why, compactionEnabledEnv)) {
			t.Errorf("%s=%q: disabled %v (%q), want %v", compactionEnabledEnv, raw, disabled, why, wantDisabled)
		}
	}

	opened := false
	c := &Compactor{Disabled: true, DisabledReason: "stopped", Session: func(context.Context) (*sql.Conn, func(), error) {
		opened = true
		return nil, nil, errors.New("no database in this test")
	}}
	report, err := c.Run(context.Background(), CompactOptions{MinReclaimBytes: 1, MinReclaimPercent: 1, MeasureChunks: 1})
	if err != nil || report.Outcome != "skipped" || report.Reason != "disabled" || report.Detail != "stopped" || opened {
		t.Fatalf("a stopped pass: report %+v err %v opened %v", report, err, opened)
	}
	_, err = c.Run(context.Background(), CompactOptions{DryRun: true, MinReclaimBytes: 1, MinReclaimPercent: 1, MeasureChunks: 1})
	if !opened || err == nil {
		t.Fatalf("a dry run was stopped by the switch: opened %v err %v", opened, err)
	}
}

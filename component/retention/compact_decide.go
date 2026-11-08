package retention

// compact_decide.go -- the closed-chunk compaction's decisions, as functions over
// values: which chunk to measure, which is worth rewriting, whether there is room
// for it, and whether the WAL archive is keeping up. compact.go gathers the values
// from the database and acts on the answer; nothing here reads or writes anything,
// which is what lets compact_decide_test.go hold every branch without a database.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
)

// The run's native limits. The DSL chooses the policy thresholds
// (closedChunkCompaction's arguments, clamped by the floors in plugin.go); these
// are the safety machinery a workflow cannot loosen.
const (
	// closedFor: a chunk is a candidate only once its range ended at least a day
	// ago. Inserts reach the newest chunk; a backdated write can still land in the
	// one that has just closed, and it would wait behind a rewrite's lock.
	closedFor = 24 * time.Hour

	// measurementMaxAge: a measurement is trusted for at most this long even when
	// the chunk's size and change counter are unchanged. A statistics reset
	// (crash recovery, pg_stat_reset) restarts the counter, and a restarted
	// counter could come back round to the stored value.
	measurementMaxAge = 30 * 24 * time.Hour

	// measureFailureQuiet: a chunk whose measurement failed goes to the back of
	// the measuring queue for this long, so one chunk that cannot be scanned in
	// time does not spend every run's measuring budget.
	measureFailureQuiet = 7 * 24 * time.Hour

	// tupleOverheadBytes is added per row to the summed pg_column_size of its
	// columns: tuple header, line pointer and alignment. Measured on TimescaleDB
	// 2.27 / PostgreSQL 16 against a freshly written chunk, the summed column
	// sizes plus 43.7 bytes a row matched the chunk's heap exactly; with TOAST the
	// estimate fell about 6% short of the rewritten size, because TOAST chunks
	// carry overhead of their own. Short is the safe direction for the room check,
	// which asks for twice the estimate, and costs the reclaim estimate a few per
	// cent.
	tupleOverheadBytes = 44

	// maxPendingWALSegments: more completed WAL segments than this waiting for the
	// archive is a backlog, and no rewrite starts on one. A rewrite writes WAL
	// roughly the size of what it rewrites, and the database-health monitor fails
	// the instance above 64 waiting segments; a healthy archiver keeps this at 0
	// or 1, so four (64 MB) is a backlog, not noise.
	maxPendingWALSegments = 4

	// archiverStallAfter: segments waiting while nothing has been archived for
	// this long is an archiver that has stopped, the monitor's own
	// archive-not-progressing rule.
	archiverStallAfter = 10 * time.Minute

	// reserveFraction: the share of a data volume PostgreSQL cannot use. A volume
	// formatted ext4 with its defaults (the Azure disk CSI driver's) reserves 5%
	// of its blocks for root, which the postgres user is not; the monitor's
	// capacity is the declared volume size, and its usage figure is the summed
	// database sizes, which leave out the few megabytes of cluster-wide files.
	reserveFraction = 0.05

	// reportMaxAge: a database-health report older than this, or more than a
	// minute in the future, is not a fresh reading -- the OS's own rule
	// (clients/os/src/cluster/storage/model.ts). The monitor publishes every five
	// minutes, so fifteen is three missed polls.
	reportMaxAge    = 15 * time.Minute
	reportMaxFuture = time.Minute
)

// CompactOptions is one pass's policy: the thresholds the DSL chose, and whether
// to rewrite at all.
type CompactOptions struct {
	// DryRun measures and reports what a pass would do, without rewriting a
	// chunk or writing the progress table.
	DryRun bool
	// MinReclaimBytes and MinReclaimPercent: a chunk is worth rewriting only when
	// it would give back at least this many bytes AND at least this share of its
	// total size. Both, so neither a huge chunk with a sliver of free space nor a
	// tiny chunk that is mostly free is rewritten.
	MinReclaimBytes   int64
	MinReclaimPercent int64
	// MeasureChunks bounds how many chunks one pass scans to (re)measure.
	MeasureChunks int
}

// chunk is one closed chunk as the catalog and the progress table describe it.
type chunk struct {
	Name       string // schema-qualified, quoted (format('%I.%I'))
	Hypertable string
	RangeStart time.Time
	RangeEnd   time.Time
	TotalBytes int64 // pg_total_relation_size: heap, TOAST, indexes
	IndexBytes int64 // pg_indexes_size
	Changes    sql.NullInt64
	Owned      bool // the session's role may act as the chunk's owner
	Owner      string

	m               *measurement
	MeasureFailedAt sql.NullTime
	AttemptedAt     sql.NullTime
	Outcome         string
	Failures        int
}

// measurement is what a heap scan found, and the size and change counter it is
// valid for.
type measurement struct {
	At         time.Time
	TotalBytes int64
	IndexBytes int64
	Changes    sql.NullInt64
	LiveBytes  int64 // what the chunk's heap and TOAST would be, rewritten
	LiveRows   int64
}

// dataBytes is the chunk's heap and TOAST, as it stands.
func (c *chunk) dataBytes() int64 { return c.TotalBytes - c.IndexBytes }

// reclaimable is what a rewrite would give back from the heap and TOAST. The
// rebuilt indexes give back their bloat too; that is reported after a rewrite,
// and left out of the decision, which it can only make more worthwhile.
func (c *chunk) reclaimable() int64 {
	if c.m == nil {
		return 0
	}
	return max(0, c.dataBytes()-c.m.LiveBytes)
}

// need is what a rewrite writes before the old copy goes: the live data, and the
// indexes rebuilt over it (no larger than they are now). The room check asks for
// twice this, the second copy covering a sort spilling to temporary files.
func (c *chunk) need() int64 {
	if c.m == nil {
		return 0
	}
	return c.m.LiveBytes + c.IndexBytes
}

// staleness answers why a chunk needs measuring, or "" when its measurement still
// describes it. Deletes change no size, but they move the change counter, which
// is how a retention sweep's deletes reach the next pass.
func staleness(c *chunk, now time.Time) string {
	switch {
	case c.m == nil:
		return "unmeasured"
	case now.Sub(c.m.At) > measurementMaxAge:
		return "aged"
	case c.m.TotalBytes != c.TotalBytes:
		return "resized"
	case c.m.Changes.Valid != c.Changes.Valid, c.m.Changes.Valid && c.m.Changes.Int64 != c.Changes.Int64:
		return "changed"
	}
	return ""
}

// measureQueue orders the chunks that need measuring. A chunk whose last
// measurement failed recently goes last. Before it, a chunk never measured comes
// first, then the one whose measurement is oldest -- so when retention keeps
// changing more chunks a night than a pass may scan, each still has its turn --
// and between equals the largest, since the space a pass can give back is
// bounded by the size of the chunk it picks.
func measureQueue(chunks []*chunk, now time.Time) []*chunk {
	var queue []*chunk
	for _, c := range chunks {
		if staleness(c, now) != "" {
			queue = append(queue, c)
		}
	}
	quiet := func(c *chunk) bool {
		return c.MeasureFailedAt.Valid && now.Sub(c.MeasureFailedAt.Time) < measureFailureQuiet
	}
	sort.SliceStable(queue, func(i, j int) bool {
		a, b := queue[i], queue[j]
		if quiet(a) != quiet(b) {
			return !quiet(a)
		}
		if (a.m == nil) != (b.m == nil) {
			return a.m == nil
		}
		if a.m != nil && !a.m.At.Equal(b.m.At) {
			return a.m.At.Before(b.m.At)
		}
		if a.TotalBytes != b.TotalBytes {
			return a.TotalBytes > b.TotalBytes
		}
		return a.Name < b.Name
	})
	return queue
}

// backoff is how long a chunk whose rewrite failed f times in a row waits before
// the next attempt: a day, doubling, at most sixteen days. A failed rewrite
// copies the whole chunk before it fails at the swap, so a chunk that keeps
// failing would otherwise cost a chunk's worth of I/O and WAL every night. Two
// hours of slack keep a nightly schedule from missing its next run by seconds.
func backoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	days := 1 << min(failures-1, 4)
	return time.Duration(days)*24*time.Hour - 2*time.Hour
}

// inBackoff reports whether c's last failed rewrite is too recent to try again.
func inBackoff(c *chunk, now time.Time) bool {
	return c.Failures > 0 && c.AttemptedAt.Valid && now.Sub(c.AttemptedAt.Time) < backoff(c.Failures)
}

// eligible is every chunk with a current measurement that is over both
// thresholds and not backing off a failure, most reclaimable first and, between
// equals, the oldest first.
func eligible(chunks []*chunk, opt CompactOptions, now time.Time) []*chunk {
	var out []*chunk
	for _, c := range chunks {
		if staleness(c, now) != "" || inBackoff(c, now) {
			continue
		}
		r := c.reclaimable()
		if r < opt.MinReclaimBytes || r*100 < opt.MinReclaimPercent*c.TotalBytes {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].reclaimable() != out[j].reclaimable() {
			return out[i].reclaimable() > out[j].reclaimable()
		}
		return out[i].RangeStart.Before(out[j].RangeStart)
	})
	return out
}

// Room is the data volume as the pass judged it.
type Room struct {
	// Source is where capacity came from: databaseHealth (the monitor's fresh
	// report) or configured (MEMQL_CHUNK_COMPACTION_VOLUME_BYTES).
	Source        string `json:"source"`
	CapacityBytes int64  `json:"capacityBytes"`
	UsedBytes     int64  `json:"usedBytes"`
	// UsableFreeBytes is capacity less the reserve PostgreSQL cannot use, less
	// what is used.
	UsableFreeBytes int64 `json:"usableFreeBytes"`
	// NeedBytes is twice the chosen (or, when none fits, the smallest) eligible
	// chunk's live data and indexes.
	NeedBytes int64 `json:"needBytes,omitempty"`
	// WALFreeBytes is the WAL volume's usable free space, when a fresh report
	// gave it.
	WALFreeBytes *int64     `json:"walFreeBytes,omitempty"`
	ObservedAt   *time.Time `json:"observedAt,omitempty"`
}

// StorageReading is the database-health monitor's report, as the room check
// uses it.
type StorageReading struct {
	ObservedAt        time.Time
	DataBytes         int64
	DataCapacityBytes int64
	WALBytes          int64
	WALCapacityBytes  int64
	Issues            []string
}

// roomInputs is everything the room decision reads.
type roomInputs struct {
	// report is the monitor's reading and reportErr why there is none.
	report    *StorageReading
	reportErr string
	// configured is MEMQL_CHUNK_COMPACTION_VOLUME_BYTES, 0 when unset.
	configured int64
	// used is the summed size of every database this role can read, and
	// unreadable how many it could not.
	used       int64
	unreadable int
	now        time.Time
}

// fresh reports whether a report is recent enough to stand for the volume now.
func fresh(r *StorageReading, now time.Time) bool {
	if r == nil {
		return false
	}
	age := now.Sub(r.ObservedAt)
	return age >= -reportMaxFuture && age <= reportMaxAge
}

// usable is capacity less the reserve PostgreSQL cannot use.
func usable(capacity int64) int64 {
	return capacity - int64(float64(capacity)*reserveFraction)
}

// judgeRoom decides the volume's usable free space, or the reason it cannot.
//
// The engine cannot run df. What it can know is the database-health monitor's
// report -- the declared volume size and the summed database sizes, read through
// the same verified path as clusterStorageStatus -- and the database sizes
// themselves, measured live. A FRESH report supplies capacity (the volume only
// grows, and a replaced installation's report is refused by its cluster
// identity) and usage is the larger of its figure and the live one, since the
// database grew since the report if it grew at all. Without a fresh report the
// pass uses the configured capacity, if an operator gave one, against the live
// figure, which must then cover every database. With neither it refuses: a
// rewrite needs room it cannot see.
func judgeRoom(in roomInputs) (Room, string, string) {
	var room Room
	switch {
	case fresh(in.report, in.now):
		observed := in.report.ObservedAt
		room = Room{Source: "databaseHealth", CapacityBytes: in.report.DataCapacityBytes, UsedBytes: max(in.used, in.report.DataBytes), ObservedAt: &observed}
		if in.report.WALCapacityBytes > 0 {
			wal := max(0, usable(in.report.WALCapacityBytes)-in.report.WALBytes)
			room.WALFreeBytes = &wal
		}
	case in.configured > 0:
		if in.unreadable > 0 {
			return Room{Source: "configured", CapacityBytes: in.configured}, "room_unknown", fmt.Sprintf(
				"%d database(s) are not readable by this role, so their size is unknown; with no fresh database-health report the volume's use cannot be measured", in.unreadable)
		}
		room = Room{Source: "configured", CapacityBytes: in.configured, UsedBytes: in.used}
	default:
		why := in.reportErr
		if why == "" && in.report != nil {
			why = fmt.Sprintf("the database-health report is from %s, not within %s", in.report.ObservedAt.UTC().Format(time.RFC3339), reportMaxAge)
		}
		return Room{}, "room_unknown", strings.TrimSpace("no fresh database-health report and no MEMQL_CHUNK_COMPACTION_VOLUME_BYTES: " + why)
	}
	room.UsableFreeBytes = max(0, usable(room.CapacityBytes)-room.UsedBytes)
	return room, "", ""
}

// fits reports whether the volume holds a rewrite of c: twice its live data and
// indexes on the data volume and, when a fresh report gave the WAL volume's free
// space, the WAL the rewrite writes. WAL beyond the archive's pace stays on that
// volume until it is archived, and a replication slot holding WAL back shows
// there and nowhere else.
func fits(c *chunk, room Room) bool {
	if 2*c.need() > room.UsableFreeBytes {
		return false
	}
	return room.WALFreeBytes == nil || c.need() <= *room.WALFreeBytes
}

// choose picks the most reclaimable eligible chunk that fits, or nil, with the
// eligible chunk needing least room for the report when none fits. A smaller
// chunk that fits is worth more than a larger one that does not: the space it
// gives back is room the larger one will have on a later night.
func choose(candidates []*chunk, room Room) (pick, smallest *chunk) {
	for _, c := range candidates {
		if pick == nil && fits(c, room) {
			pick = c
		}
		if smallest == nil || c.need() < smallest.need() {
			smallest = c
		}
	}
	return pick, smallest
}

// WAL is the archive as the pass judged it.
type WAL struct {
	ArchiveMode     string     `json:"archiveMode"`
	PendingSegments int64      `json:"pendingSegments"`
	LastArchivedAt  *time.Time `json:"lastArchivedAt,omitempty"`
}

// walState is what pg_stat_archiver and the WAL position say, read by any role:
// pg_ls_archive_statusdir(), the direct count of waiting segments, needs a
// superuser or pg_monitor, which the engine's role is not.
type walState struct {
	ArchiveMode     string
	SegmentBytes    int64
	CurrentWAL      string
	LastArchivedWAL sql.NullString
	LastArchivedAt  sql.NullTime
	LastFailedAt    sql.NullTime
	DBNow           time.Time
}

// archiveIssues are the database-health monitor's issues that mean the archive
// is behind or failing. A fresh report naming one refuses a rewrite even when the
// live reading looks clear: the two read the archive by different routes.
var archiveIssues = map[string]bool{
	"archive-backlog-over-64-segments": true,
	"archive-not-progressing":          true,
	"archive-latest-attempt-failed":    true,
}

// judgeWAL decides whether the archive is keeping up, from the live reading and
// a fresh report's issues. The waiting segments are those between the last one
// archived and the one being written, both named by any role; a backlog, a
// failed latest attempt or a stalled archiver each refuse, and a reading that
// cannot be made refuses too, because the cost of guessing wrong is a WAL volume
// that fills.
func judgeWAL(s walState, issues []string) (WAL, string, string) {
	w := WAL{ArchiveMode: s.ArchiveMode}
	if s.LastArchivedAt.Valid {
		at := s.LastArchivedAt.Time
		w.LastArchivedAt = &at
	}
	for _, issue := range issues {
		if archiveIssues[issue] {
			return w, "wal_backlog", "the database-health report says " + issue
		}
	}
	if s.ArchiveMode == "off" {
		// Nothing is archived, so nothing can wait: a checkpoint recycles WAL.
		return w, "", ""
	}
	if !s.LastArchivedWAL.Valid || s.LastArchivedWAL.String == "" {
		return w, "wal_unknown", "archive_mode is " + s.ArchiveMode + " and the archiver reports no archived segment, so the backlog cannot be read"
	}
	current, ok := walSegmentNumber(s.CurrentWAL, s.SegmentBytes)
	last, ok2 := walSegmentNumber(s.LastArchivedWAL.String, s.SegmentBytes)
	if !ok || !ok2 {
		return w, "wal_unknown", fmt.Sprintf("cannot compare WAL file names %q and %q", s.CurrentWAL, s.LastArchivedWAL.String)
	}
	// The segment being written is not complete, so it is not waiting.
	if current > last+1 {
		w.PendingSegments = int64(current - last - 1)
	}
	switch {
	case s.LastFailedAt.Valid && (!s.LastArchivedAt.Valid || s.LastFailedAt.Time.After(s.LastArchivedAt.Time)):
		return w, "wal_backlog", "the archiver's latest attempt failed"
	case w.PendingSegments > maxPendingWALSegments:
		return w, "wal_backlog", fmt.Sprintf("%d WAL segments await archiving (more than %d)", w.PendingSegments, maxPendingWALSegments)
	case w.PendingSegments > 0 && s.LastArchivedAt.Valid && s.DBNow.Sub(s.LastArchivedAt.Time) > archiverStallAfter:
		return w, "wal_backlog", fmt.Sprintf("%d WAL segments await archiving and nothing has been archived since %s", w.PendingSegments, s.LastArchivedAt.Time.UTC().Format(time.RFC3339))
	}
	return w, "", ""
}

// walSegmentNumber reads a WAL file name's position: 24 hex digits of timeline,
// log and segment, the first 24 characters of a backup-history name too
// (000000010000000000000003.00000028.backup). A timeline-history name has none.
// The timeline is ignored: segment numbers are positions in one WAL sequence.
func walSegmentNumber(name string, segmentBytes int64) (uint64, bool) {
	if len(name) < 24 || segmentBytes <= 0 || segmentBytes > 1<<30 {
		return 0, false
	}
	logID, err1 := strconv.ParseUint(name[8:16], 16, 32)
	seg, err2 := strconv.ParseUint(name[16:24], 16, 32)
	if _, err := strconv.ParseUint(name[0:8], 16, 32); err != nil || err1 != nil || err2 != nil {
		return 0, false
	}
	perLog := uint64(1<<32) / uint64(segmentBytes)
	return logID*perLog + seg, true
}

// failureKind classifies why a statement ended: the reasons a pass reports.
func failureKind(ctx context.Context, err error) string {
	var pg pgdriver.Error
	if errors.As(err, &pg) {
		switch pg.Field('C') {
		case "55P03": // lock_not_available: lock_timeout fired
			return "lock_timeout"
		case "57014": // query_canceled
			if strings.Contains(pg.Field('M'), "statement timeout") {
				return "statement_timeout"
			}
			return "cancelled"
		}
		return "error"
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "error"
}

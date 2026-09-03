package logstore

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// sweep.go -- retention and the archive (design F, L7, L8).
//
// NO ARCHIVE, NO DELETE. The sweep archives each expired UTC day per node
// type as logs/<day>/<nodeType>.ndjson.gz and deletes the day only once
// EVERY node type of that day is uploaded. A cluster with no archive
// container keeps its lines and the sweep says so in its own log line, every
// night. There is no TimescaleDB retention policy on log_line for exactly
// this reason: the archive must come first, and a policy cannot be told to
// wait for one.

// Archiver is the narrow slice of object storage the sweep and the restore
// need. azureblob.AzureBlobUploader is the production one; tests hold an
// in-memory map.
type Archiver interface {
	Upload(ctx context.Context, container, object string, data []byte, contentType string) (string, error)
	Download(ctx context.Context, container, object string) ([]byte, error)
	ListPrefix(ctx context.Context, container, prefix string) ([]string, error)
}

var _ Archiver = (*azureblob.AzureBlobUploader)(nil)

// sizedLister is the optional half of Archiver: a listing that carries
// sizes. The archive listing reports a size when its archiver has this and
// says so when it does not.
type sizedLister interface {
	ListPrefixWithSizes(ctx context.Context, container, prefix string) ([]azureblob.BlobInfo, error)
}

const (
	// SweepMaxDaysPerRun bounds one run. A store that fell far behind its
	// retention catches up over several nights rather than in one run that
	// holds the lock for hours.
	SweepMaxDaysPerRun = 60

	// SweepPageSize is how many rows one archive page reads;
	// SweepDeleteBatch how many one DELETE removes.
	SweepPageSize    = 5000
	SweepDeleteBatch = 5000

	// RestoreBatch is how many rows one restore INSERT carries.
	RestoreBatch = 1000

	archivePrefix      = "logs/"
	archiveSuffix      = ".ndjson.gz"
	archiveContentType = "application/gzip"

	// sweepAdvisoryLockKey keeps two sweeps from overlapping. Session-scoped,
	// taken on the DIRECT endpoint (PluginContext.DirectBunDB) because a
	// transaction pooler recycles the backend between statements and would
	// drop a held session lock. Distinct from every other key in the tree
	// (cron leader 7756010113207010561, reconciler ...10574, schema
	// ...25510, recovery mint ...31964).
	sweepAdvisoryLockKey int64 = 7756010113207040903
)

// Report is what one sweep answers.
type Report struct {
	Boundary     time.Time `json:"boundary"`
	DaysArchived int       `json:"daysArchived"`
	RowsArchived int64     `json:"rowsArchived"`
	RowsDeleted  int64     `json:"rowsDeleted"`
	Days         []string  `json:"days,omitempty"`
	Objects      []string  `json:"objects,omitempty"`
	Container    string    `json:"container,omitempty"`
	Skipped      string    `json:"skipped,omitempty"`
	Refused      string    `json:"refused,omitempty"`
	Truncated    bool      `json:"truncated,omitempty"` // more expired days remain
	Took         string    `json:"took"`
}

// RestoreReport is what one restore answers.
type RestoreReport struct {
	Day      string   `json:"day"`
	NodeType string   `json:"nodeType,omitempty"`
	Restored int64    `json:"restored"`
	Skipped  int64    `json:"skipped"`
	Objects  []string `json:"objects"`
	// Note says what a caller must know: a restored day is older than the
	// retention boundary by definition and is swept again at the next run.
	Note string `json:"note"`
}

// ArchiveObject is one row of logsArchiveList.
type ArchiveObject struct {
	Day      string `json:"day"`
	NodeType string `json:"nodeType"`
	Object   string `json:"object"`
	// Size is the object's bytes when the archiver reports sizes; SizeKnown
	// says whether it did, so an absent size is never read as zero.
	Size      int64 `json:"size,omitempty"`
	SizeKnown bool  `json:"sizeKnown"`
}

// Sweeper runs the retention sweep, the restore and the archive listing.
type Sweeper struct {
	// DB is the pooled handle for row traffic.
	DB *bun.DB
	// LockDB is the DIRECT handle for the advisory-lock session. Nil falls
	// back to DB, which is correct on a cluster with no pooler.
	LockDB *bun.DB

	Archive   Archiver
	Container string

	// RetentionDays is the window. Zero or negative uses the configured
	// value.
	RetentionDays int
	// MaxDays bounds one run. Zero uses SweepMaxDaysPerRun.
	MaxDays int

	Now    func() time.Time
	Logger *slog.Logger

	// source and locker are the storage seams. Nil resolves to Postgres over
	// DB / LockDB; tests inject memory.
	source rowSource
	locker lockSource
}

// rowSource is every statement the sweep and the restore run.
type rowSource interface {
	// oldestBefore returns the oldest occurred_at strictly before boundary.
	oldestBefore(ctx context.Context, boundary time.Time) (time.Time, bool, error)
	// nodeTypesForDay lists the node types with rows in [day, day+1).
	nodeTypesForDay(ctx context.Context, day time.Time) ([]string, error)
	// page reads rows of one (day, nodeType) after the keyset cursor, in
	// (occurred_at, id) order.
	page(ctx context.Context, day time.Time, nodeType string, afterAt time.Time, afterId string, limit int) ([]Row, error)
	// deleteDay removes up to limit rows of [day, day+1) and reports how many.
	deleteDay(ctx context.Context, day time.Time, limit int) (int64, error)
	// insertIgnore inserts rows with ON CONFLICT DO NOTHING and reports how
	// many landed.
	insertIgnore(ctx context.Context, rows []Row) (int64, error)
}

// lockSource takes the sweep lock. release is non-nil only when acquired.
type lockSource interface {
	tryLock(ctx context.Context) (release func(), acquired bool, err error)
}

// Run sweeps once. Refusals (no container) and skips (another sweep holds
// the lock) are answers, not errors: they land on the report and in the one
// log line, and the automation step succeeds.
func (s *Sweeper) Run(ctx context.Context) (Report, error) {
	started := s.now()
	rep := Report{Container: s.Container}
	rep.Boundary = s.boundary()

	if strings.TrimSpace(s.Container) == "" {
		rep.Refused = "no archive container configured (" + EnvArchiveContainer + " / " + envBlobContainer + "); nothing was deleted"
		rep.Took = s.now().Sub(started).String()
		s.logRun(rep, nil)
		return rep, nil
	}
	if s.Archive == nil {
		rep.Refused = "no archive client on this node (MEMQL_AZURE_STORAGE_CONNECTION_STRING is unset or invalid); nothing was deleted"
		rep.Took = s.now().Sub(started).String()
		s.logRun(rep, nil)
		return rep, nil
	}
	src, err := s.resolveSource()
	if err != nil {
		return rep, err
	}
	lock, err := s.resolveLocker()
	if err != nil {
		return rep, err
	}
	release, acquired, err := lock.tryLock(ctx)
	if err != nil {
		return rep, fmt.Errorf("logstore: sweep lock: %w", err)
	}
	if !acquired {
		rep.Skipped = "another sweep is running"
		rep.Took = s.now().Sub(started).String()
		s.logRun(rep, nil)
		return rep, nil
	}
	defer release()

	maxDays := s.MaxDays
	if maxDays <= 0 {
		maxDays = SweepMaxDaysPerRun
	}
	var lastDay time.Time
	for n := 0; ; n++ {
		if err := ctx.Err(); err != nil {
			return s.finish(rep, started, err)
		}
		oldest, ok, err := src.oldestBefore(ctx, rep.Boundary)
		if err != nil {
			return s.finish(rep, started, err)
		}
		if !ok {
			break
		}
		if n >= maxDays {
			rep.Truncated = true
			break
		}
		day := utcDay(oldest)
		if !lastDay.IsZero() && !day.After(lastDay) {
			return s.finish(rep, started, fmt.Errorf("logstore: sweep did not advance past %s; its rows were archived but not deleted", lastDay.Format("2006-01-02")))
		}
		lastDay = day

		archived, objects, err := s.archiveDay(ctx, src, day)
		if err != nil {
			return s.finish(rep, started, err)
		}
		deleted, err := s.deleteDay(ctx, src, day)
		rep.RowsArchived += archived
		rep.RowsDeleted += deleted
		rep.Objects = append(rep.Objects, objects...)
		rep.Days = append(rep.Days, day.Format("2006-01-02"))
		rep.DaysArchived++
		metrics.LogsArchived(archived)
		metrics.LogsDeleted(deleted)
		if err != nil {
			return s.finish(rep, started, err)
		}
	}
	return s.finish(rep, started, nil)
}

func (s *Sweeper) finish(rep Report, started time.Time, err error) (Report, error) {
	rep.Took = s.now().Sub(started).String()
	s.logRun(rep, err)
	return rep, err
}

// archiveDay uploads one object per node type present on the day and
// returns the rows archived plus the object names. Nothing is deleted here.
func (s *Sweeper) archiveDay(ctx context.Context, src rowSource, day time.Time) (int64, []string, error) {
	nodeTypes, err := src.nodeTypesForDay(ctx, day)
	if err != nil {
		return 0, nil, err
	}
	var total int64
	var objects []string
	for _, nt := range nodeTypes {
		body, n, err := s.encodeDay(ctx, src, day, nt)
		if err != nil {
			return total, objects, err
		}
		object := ArchiveObjectName(day, nt)
		if _, err := s.Archive.Upload(ctx, s.Container, object, body, archiveContentType); err != nil {
			return total, objects, fmt.Errorf("logstore: archive %s: %w", object, err)
		}
		total += n
		objects = append(objects, object)
	}
	return total, objects, nil
}

// encodeDay streams one (day, nodeType) into gzip NDJSON: one JSON object per
// row, the concept's field names, attributes inline, in (occurred_at, id)
// order, read in pages of SweepPageSize.
func (s *Sweeper) encodeDay(ctx context.Context, src rowSource, day time.Time, nodeType string) ([]byte, int64, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gz)
	var count int64
	var afterAt time.Time
	var afterId string
	for {
		rows, err := src.page(ctx, day, nodeType, afterAt, afterId, SweepPageSize)
		if err != nil {
			return nil, count, err
		}
		for i := range rows {
			if err := enc.Encode(&rows[i]); err != nil {
				return nil, count, fmt.Errorf("logstore: encode row %s: %w", rows[i].ID, err)
			}
			count++
		}
		if len(rows) < SweepPageSize {
			break
		}
		last := rows[len(rows)-1]
		afterAt, afterId = last.OccurredAt, last.ID
	}
	if err := gz.Close(); err != nil {
		return nil, count, fmt.Errorf("logstore: gzip: %w", err)
	}
	return buf.Bytes(), count, nil
}

// deleteDay removes the day in batches and returns how many rows went.
func (s *Sweeper) deleteDay(ctx context.Context, src rowSource, day time.Time) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.deleteDay(ctx, day, SweepDeleteBatch)
		total += n
		if err != nil {
			return total, err
		}
		if n < SweepDeleteBatch {
			return total, nil
		}
	}
}

// Restore brings an archived day back (design L8): list logs/<day>/, filter
// on nodeType when given, download, gunzip, parse, insert with ON CONFLICT
// DO NOTHING in batches. Idempotent on (occurred_at, id): a second restore
// restores nothing.
func (s *Sweeper) Restore(ctx context.Context, day string, nodeType string) (RestoreReport, error) {
	rep := RestoreReport{Day: day, NodeType: nodeType, Objects: []string{}}
	d, err := ParseDay(day)
	if err != nil {
		return rep, err
	}
	rep.Day = d.Format("2006-01-02")
	if s.Archive == nil || strings.TrimSpace(s.Container) == "" {
		return rep, errors.New("logstore: no archive is configured on this cluster, so there is nothing to restore from")
	}
	src, err := s.resolveSource()
	if err != nil {
		return rep, err
	}
	prefix := archivePrefix + rep.Day + "/"
	names, err := s.Archive.ListPrefix(ctx, s.Container, prefix)
	if err != nil {
		return rep, fmt.Errorf("logstore: list %s: %w", prefix, err)
	}
	sort.Strings(names)
	want := strings.TrimSpace(nodeType)
	for _, name := range names {
		obj, ok := parseArchiveObject(name)
		if !ok {
			continue
		}
		if want != "" && obj.NodeType != want {
			continue
		}
		body, err := s.Archive.Download(ctx, s.Container, name)
		if err != nil {
			return rep, fmt.Errorf("logstore: download %s: %w", name, err)
		}
		restored, skipped, err := s.restoreObject(ctx, src, body)
		rep.Restored += restored
		rep.Skipped += skipped
		if err != nil {
			return rep, fmt.Errorf("logstore: restore %s: %w", name, err)
		}
		rep.Objects = append(rep.Objects, name)
	}
	if len(rep.Objects) == 0 {
		if want != "" {
			return rep, fmt.Errorf("logstore: nothing is archived for %s and node type %q", rep.Day, want)
		}
		return rep, fmt.Errorf("logstore: nothing is archived for %s", rep.Day)
	}
	rep.Note = "A restored day is older than the retention boundary by definition and is swept again at the next nightly run; read it now."
	return rep, nil
}

// restoreObject inserts one archive object's rows and reports (restored,
// skipped) -- skipped being rows already present.
func (s *Sweeper) restoreObject(ctx context.Context, src rowSource, body []byte) (int64, int64, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return 0, 0, fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	var restored, skipped int64
	batch := make([]Row, 0, RestoreBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := src.insertIgnore(ctx, batch)
		restored += n
		skipped += int64(len(batch)) - n
		batch = batch[:0]
		return err
	}
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r Row
		if err := json.Unmarshal(line, &r); err != nil {
			return restored, skipped, fmt.Errorf("record %d: %w", lineNo, err)
		}
		if r.ID == "" || r.OccurredAt.IsZero() {
			return restored, skipped, fmt.Errorf("record %d carries no (occurredAt, id)", lineNo)
		}
		r.OccurredAt = r.OccurredAt.UTC()
		batch = append(batch, r)
		if len(batch) >= RestoreBatch {
			if err := flush(); err != nil {
				return restored, skipped, err
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return restored, skipped, fmt.Errorf("read archive: %w", err)
	}
	if err := flush(); err != nil {
		return restored, skipped, err
	}
	return restored, skipped, nil
}

// ListArchive answers logsArchiveList: every logs/<day>/<nodeType>.ndjson.gz
// object, newest day first. Nil archiver means no archive is configured; the
// caller says so.
func (s *Sweeper) ListArchive(ctx context.Context) ([]ArchiveObject, error) {
	if s.Archive == nil || strings.TrimSpace(s.Container) == "" {
		return nil, nil
	}
	var out []ArchiveObject
	if sized, ok := s.Archive.(sizedLister); ok {
		infos, err := sized.ListPrefixWithSizes(ctx, s.Container, archivePrefix)
		if err != nil {
			return nil, fmt.Errorf("logstore: list archive: %w", err)
		}
		for _, info := range infos {
			if obj, ok := parseArchiveObject(info.Name); ok {
				obj.Size, obj.SizeKnown = info.Size, true
				out = append(out, obj)
			}
		}
	} else {
		names, err := s.Archive.ListPrefix(ctx, s.Container, archivePrefix)
		if err != nil {
			return nil, fmt.Errorf("logstore: list archive: %w", err)
		}
		for _, name := range names {
			if obj, ok := parseArchiveObject(name); ok {
				out = append(out, obj)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Day != out[j].Day {
			return out[i].Day > out[j].Day
		}
		return out[i].NodeType < out[j].NodeType
	})
	return out, nil
}

// ArchiveObjectName is logs/<YYYY-MM-DD>/<nodeType>.ndjson.gz.
func ArchiveObjectName(day time.Time, nodeType string) string {
	return archivePrefix + day.UTC().Format("2006-01-02") + "/" + nodeType + archiveSuffix
}

// parseArchiveObject reads (day, nodeType) back out of an object name.
func parseArchiveObject(name string) (ArchiveObject, bool) {
	if !strings.HasPrefix(name, archivePrefix) || !strings.HasSuffix(name, archiveSuffix) {
		return ArchiveObject{}, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, archivePrefix), archiveSuffix)
	day, nodeType, ok := strings.Cut(rest, "/")
	if !ok || nodeType == "" || strings.Contains(nodeType, "/") {
		return ArchiveObject{}, false
	}
	if _, err := ParseDay(day); err != nil {
		return ArchiveObject{}, false
	}
	return ArchiveObject{Day: day, NodeType: nodeType, Object: name}, true
}

// ParseDay parses a UTC day, YYYY-MM-DD.
func ParseDay(day string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", strings.TrimSpace(day))
	if err != nil {
		return time.Time{}, fmt.Errorf("logstore: %q is not a day (YYYY-MM-DD)", day)
	}
	return d.UTC(), nil
}

// boundary is UTC midnight retentionDays ago: rows before it are expired.
func (s *Sweeper) boundary() time.Time {
	days := s.RetentionDays
	if days <= 0 {
		days = RetentionDays()
	}
	return utcDay(s.now()).AddDate(0, 0, -days)
}

func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (s *Sweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Sweeper) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// logRun writes the run's ONE line -- including the refused run, the skipped
// run and the zero-row run (the authactivity precedent): a sweep that logs
// only when it deletes is indistinguishable from one that is not running.
// Under component `logs`, so the line is itself stored.
func (s *Sweeper) logRun(rep Report, err error) {
	attrs := []any{
		"component", gapComponent,
		"boundary", rep.Boundary.Format(time.RFC3339),
		"days_archived", rep.DaysArchived,
		"rows_archived", rep.RowsArchived,
		"rows_deleted", rep.RowsDeleted,
		"took", rep.Took,
	}
	switch {
	case err != nil:
		s.log().Warn("logs retention sweep failed", append(attrs, "err", err.Error())...)
	case rep.Refused != "":
		s.log().Info("logs retention sweep refused: "+rep.Refused, attrs...)
	case rep.Skipped != "":
		s.log().Info("logs retention sweep skipped: "+rep.Skipped, attrs...)
	default:
		s.log().Info("logs retention sweep", append(attrs, "container", rep.Container, "truncated", rep.Truncated)...)
	}
}

func (s *Sweeper) resolveSource() (rowSource, error) {
	if s.source != nil {
		return s.source, nil
	}
	if s.DB == nil {
		return nil, ErrNoDatabase
	}
	return &pgSource{db: s.DB}, nil
}

func (s *Sweeper) resolveLocker() (lockSource, error) {
	if s.locker != nil {
		return s.locker, nil
	}
	db := s.LockDB
	if db == nil {
		db = s.DB
	}
	if db == nil {
		return nil, ErrNoDatabase
	}
	return &pgLocker{db: db}, nil
}

// ---------------------------------------------------------------------------
// Postgres
// ---------------------------------------------------------------------------

// pgSource runs the sweep's statements. The raw ones go through the EMBEDDED
// *sql.DB (db.DB) with $n placeholders: *bun.DB's own ExecContext formats
// bun's `?` placeholders into the text and sends no parameters, so a `$1`
// written against it is a literal the server has no value for.
type pgSource struct{ db *bun.DB }

func (p *pgSource) oldestBefore(ctx context.Context, boundary time.Time) (time.Time, bool, error) {
	var oldest sql.NullTime
	if err := p.db.DB.QueryRowContext(ctx, `SELECT min(occurred_at) FROM log_line WHERE occurred_at < $1`, boundary.UTC()).Scan(&oldest); err != nil {
		return time.Time{}, false, fmt.Errorf("logstore: oldest row: %w", err)
	}
	if !oldest.Valid {
		return time.Time{}, false, nil
	}
	return oldest.Time.UTC(), true, nil
}

func (p *pgSource) nodeTypesForDay(ctx context.Context, day time.Time) ([]string, error) {
	rows, err := p.db.DB.QueryContext(ctx,
		`SELECT DISTINCT node_type FROM log_line WHERE occurred_at >= $1 AND occurred_at < $2 ORDER BY node_type`,
		day.UTC(), day.UTC().AddDate(0, 0, 1))
	if err != nil {
		return nil, fmt.Errorf("logstore: node types for %s: %w", day.Format("2006-01-02"), err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var nt string
		if err := rows.Scan(&nt); err != nil {
			return nil, err
		}
		out = append(out, nt)
	}
	return out, rows.Err()
}

// staged-data: INDIFFERENT -- this reads the log_line hypertable, never MemoryNodes; no staged-data row can exist in it, so the gate has nothing to withhold.
func (p *pgSource) page(ctx context.Context, day time.Time, nodeType string, afterAt time.Time, afterId string, limit int) ([]Row, error) {
	rows := make([]Row, 0, limit)
	sel := p.db.NewSelect().Model(&rows).
		Where("occurred_at >= ?", day.UTC()).
		Where("occurred_at < ?", day.UTC().AddDate(0, 0, 1)).
		Where("node_type = ?", nodeType)
	if !afterAt.IsZero() {
		sel = sel.Where("(occurred_at, id) > (?, ?)", afterAt.UTC(), afterId)
	}
	if err := sel.OrderExpr("occurred_at ASC, id ASC").Limit(limit).Scan(ctx); err != nil {
		return nil, fmt.Errorf("logstore: page %s/%s: %w", day.Format("2006-01-02"), nodeType, err)
	}
	return rows, nil
}

func (p *pgSource) deleteDay(ctx context.Context, day time.Time, limit int) (int64, error) {
	res, err := p.db.DB.ExecContext(ctx,
		`DELETE FROM log_line WHERE (occurred_at, id) IN (
		   SELECT occurred_at, id FROM log_line
		    WHERE occurred_at >= $1 AND occurred_at < $2
		    ORDER BY occurred_at, id LIMIT $3)`,
		day.UTC(), day.UTC().AddDate(0, 0, 1), limit)
	if err != nil {
		return 0, fmt.Errorf("logstore: delete %s: %w", day.Format("2006-01-02"), err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (p *pgSource) insertIgnore(ctx context.Context, rows []Row) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	res, err := p.db.NewInsert().Model(&rows).On("CONFLICT (occurred_at, id) DO NOTHING").Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("logstore: restore insert: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// pgLocker takes the sweep lock on a dedicated connection and holds it until
// release, so the lock's session is the connection's own.
type pgLocker struct{ db *bun.DB }

func (l *pgLocker) tryLock(ctx context.Context) (func(), bool, error) {
	conn, err := l.db.DB.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, sweepAdvisoryLockKey).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	if !acquired {
		_ = conn.Close()
		return nil, false, nil
	}
	release := func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = conn.ExecContext(uctx, `SELECT pg_advisory_unlock($1)`, sweepAdvisoryLockKey)
		cancel()
		_ = conn.Close()
	}
	return release, true, nil
}

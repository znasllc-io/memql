package logstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

// The read bounds (design D). A page defaults to 200 rows and is capped at
// 500; a caller asking for more gets 500, never an unbounded scan.
const (
	DefaultLimit = 200
	MaxLimit     = 500
)

// Query is the facet set logsSearch and logsTail share.
//
// THE SCOPE RULE: Apps and SubjectConcepts together form ONE predicate, ORed
// -- an app's slice of the store is "lines tagged with me OR about the things
// I own" -- and every other facet ANDs. Both empty means no scope predicate.
type Query struct {
	// Window bounds: occurred_at >= WindowStart AND occurred_at < WindowEnd.
	// A zero bound is open. logsSearch requires both; logsTail has none.
	WindowStart time.Time
	WindowEnd   time.Time

	NodeTypes  []string
	Nodes      []string
	Components []string
	Levels     []string

	Apps            []string
	SubjectConcepts []string

	Subject        string
	SubjectConcept string
	Session        string
	UserId         string

	// Text is a case-insensitive substring of the message.
	Text string

	Limit int

	// Search cursor: rows strictly older than (BeforeAt, BeforeId).
	BeforeAt time.Time
	BeforeId string

	// Tail cursor: rows strictly newer than (AfterAt, AfterId).
	AfterAt time.Time
	AfterId string
}

// normalizeLimit applies the default and the cap.
func normalizeLimit(n int) int {
	if n <= 0 {
		return DefaultLimit
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}

// ilikePattern escapes the ILIKE metacharacters in text and wraps it in
// wildcards, so a search for "100%" finds the literal percent sign rather
// than everything.
func ilikePattern(text string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(text) + "%"
}

// applyFacets adds every AND-ed facet plus the one OR-ed scope predicate to
// q. Every value is a bound parameter; nothing is concatenated into the SQL.
func applyFacets(sel *bun.SelectQuery, q Query) *bun.SelectQuery {
	if !q.WindowStart.IsZero() {
		sel = sel.Where("occurred_at >= ?", q.WindowStart.UTC())
	}
	if !q.WindowEnd.IsZero() {
		sel = sel.Where("occurred_at < ?", q.WindowEnd.UTC())
	}
	if list := cleanList(q.NodeTypes); len(list) > 0 {
		sel = sel.Where("node_type = ANY(?::text[])", pgdialect.Array(list))
	}
	if list := cleanList(q.Nodes); len(list) > 0 {
		sel = sel.Where("node = ANY(?::text[])", pgdialect.Array(list))
	}
	if list := cleanList(q.Components); len(list) > 0 {
		sel = sel.Where("component = ANY(?::text[])", pgdialect.Array(list))
	}
	if list := cleanList(q.Levels); len(list) > 0 {
		sel = sel.Where("level = ANY(?::text[])", pgdialect.Array(list))
	}

	// The scope predicate: apps OR subjectConcepts, as one clause.
	apps, concepts := cleanList(q.Apps), cleanList(q.SubjectConcepts)
	switch {
	case len(apps) > 0 && len(concepts) > 0:
		sel = sel.Where("(app = ANY(?::text[]) OR subject_concept = ANY(?::text[]))",
			pgdialect.Array(apps), pgdialect.Array(concepts))
	case len(apps) > 0:
		sel = sel.Where("app = ANY(?::text[])", pgdialect.Array(apps))
	case len(concepts) > 0:
		sel = sel.Where("subject_concept = ANY(?::text[])", pgdialect.Array(concepts))
	}

	if v := strings.TrimSpace(q.Subject); v != "" {
		sel = sel.Where("subject = ?", v)
	}
	if v := strings.TrimSpace(q.SubjectConcept); v != "" {
		sel = sel.Where("subject_concept = ?", v)
	}
	if v := strings.TrimSpace(q.Session); v != "" {
		sel = sel.Where("session = ?", v)
	}
	if v := strings.TrimSpace(q.UserId); v != "" {
		sel = sel.Where("user_id = ?", v)
	}
	if v := strings.TrimSpace(q.Text); v != "" {
		sel = sel.Where(`message ILIKE ? ESCAPE '\'`, ilikePattern(v))
	}
	return sel
}

// staged-data: INDIFFERENT -- this reads the log_line hypertable, never MemoryNodes; no staged-data row can exist in it, so the gate has nothing to withhold.
// searchQuery builds logsSearch: newest first, keyset-paged older by
// (BeforeAt, BeforeId).
func searchQuery(db *bun.DB, q Query, rows *[]Row) *bun.SelectQuery {
	sel := applyFacets(db.NewSelect().Model(rows), q)
	if !q.BeforeAt.IsZero() && strings.TrimSpace(q.BeforeId) != "" {
		sel = sel.Where("(occurred_at, id) < (?, ?)", q.BeforeAt.UTC(), strings.TrimSpace(q.BeforeId))
	}
	return sel.OrderExpr("occurred_at DESC, id DESC").Limit(normalizeLimit(q.Limit))
}

// staged-data: INDIFFERENT -- this reads the log_line hypertable, never MemoryNodes; no staged-data row can exist in it, so the gate has nothing to withhold.
// tailQuery builds logsTail. With a cursor: rows newer than (AfterAt,
// AfterId), oldest first, so a client appends. Without one: the newest Limit
// rows, fetched newest-first and reversed by the caller into ascending order
// -- the baseline a stream starts from. The bool reports which.
func tailQuery(db *bun.DB, q Query, rows *[]Row) (*bun.SelectQuery, bool) {
	sel := applyFacets(db.NewSelect().Model(rows), q)
	limit := normalizeLimit(q.Limit)
	if !q.AfterAt.IsZero() && strings.TrimSpace(q.AfterId) != "" {
		sel = sel.Where("(occurred_at, id) > (?, ?)", q.AfterAt.UTC(), strings.TrimSpace(q.AfterId))
		return sel.OrderExpr("occurred_at ASC, id ASC").Limit(limit), false
	}
	return sel.OrderExpr("occurred_at DESC, id DESC").Limit(limit), true
}

// Search answers logsSearch.
func Search(ctx context.Context, db *bun.DB, q Query) ([]Row, error) {
	if db == nil {
		return nil, ErrNoDatabase
	}
	rows := make([]Row, 0, normalizeLimit(q.Limit))
	if err := searchQuery(db, q, &rows).Scan(ctx); err != nil {
		return nil, fmt.Errorf("logstore: search: %w", err)
	}
	return rows, nil
}

// Tail answers logsTail: always ascending.
func Tail(ctx context.Context, db *bun.DB, q Query) ([]Row, error) {
	if db == nil {
		return nil, ErrNoDatabase
	}
	rows := make([]Row, 0, normalizeLimit(q.Limit))
	sel, reverse := tailQuery(db, q, &rows)
	if err := sel.Scan(ctx); err != nil {
		return nil, fmt.Errorf("logstore: tail: %w", err)
	}
	if reverse {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	return rows, nil
}

// ErrNoDatabase is answered by every read on a node that has none.
var ErrNoDatabase = fmt.Errorf("logstore: this node has no database")

// Source is one row of logsSources: a distinct component, a (nodeType, node)
// pair, or an OS app, with its line count inside the window.
type Source struct {
	Kind     string `json:"kind"` // component | node | app
	Value    string `json:"value"`
	NodeType string `json:"nodeType,omitempty"`
	Count    int64  `json:"count"`
}

// The raw statements below run through the EMBEDDED *sql.DB (db.DB) with $n
// placeholders. *bun.DB's own QueryContext formats bun's `?` placeholders
// into the text and sends no parameters, so a `$1` written against it is a
// literal the server has no value for ("there is no parameter $1"). The
// builder queries above use `?` and bun; the raw ones use $n and database/sql.
const sourcesSQL = `
SELECT 'component' AS kind, component AS value, '' AS node_type, count(*) AS n
  FROM log_line WHERE occurred_at >= $1 AND occurred_at < $2 GROUP BY component
UNION ALL
SELECT 'node', node, node_type, count(*)
  FROM log_line WHERE occurred_at >= $1 AND occurred_at < $2 GROUP BY node_type, node
UNION ALL
SELECT 'app', app, '', count(*)
  FROM log_line WHERE occurred_at >= $1 AND occurred_at < $2 AND app <> '' GROUP BY app
ORDER BY 1, 3, 2`

// Sources answers logsSources for [from, to).
func Sources(ctx context.Context, db *bun.DB, from, to time.Time) ([]Source, error) {
	if db == nil {
		return nil, ErrNoDatabase
	}
	rows, err := db.DB.QueryContext(ctx, sourcesSQL, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("logstore: sources: %w", err)
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.Kind, &s.Value, &s.NodeType, &s.Count); err != nil {
			return nil, fmt.Errorf("logstore: sources scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("logstore: sources: %w", err)
	}
	return out, nil
}

// TableStatus is what the store holds: its bounds and an estimate of its
// size. RowEstimate is TimescaleDB's approximate_row_count where the
// extension is present and pg_class.reltuples otherwise -- an estimate
// either way, because a count(*) over thirty days of lines is exactly the
// scan a status call must not run.
type TableStatus struct {
	OldestAt    *time.Time `json:"oldestAt,omitempty"`
	NewestAt    *time.Time `json:"newestAt,omitempty"`
	RowEstimate int64      `json:"rowEstimate"`
	Timescale   bool       `json:"timescale"`
}

// Status answers the table half of logsStatus.
func Status(ctx context.Context, db *bun.DB) (TableStatus, error) {
	var st TableStatus
	if db == nil {
		return st, ErrNoDatabase
	}
	var oldest, newest sql.NullTime
	if err := db.DB.QueryRowContext(ctx, `SELECT min(occurred_at), max(occurred_at) FROM log_line`).Scan(&oldest, &newest); err != nil {
		return st, fmt.Errorf("logstore: status bounds: %w", err)
	}
	if oldest.Valid {
		t := oldest.Time.UTC()
		st.OldestAt = &t
	}
	if newest.Valid {
		t := newest.Time.UTC()
		st.NewestAt = &t
	}
	if err := db.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`).Scan(&st.Timescale); err != nil {
		return st, fmt.Errorf("logstore: status extension: %w", err)
	}
	var estimate sql.NullInt64
	if st.Timescale {
		if err := db.DB.QueryRowContext(ctx, `SELECT approximate_row_count('log_line')`).Scan(&estimate); err != nil {
			return st, fmt.Errorf("logstore: status estimate: %w", err)
		}
	} else {
		if err := db.DB.QueryRowContext(ctx, `SELECT COALESCE(reltuples, 0)::bigint FROM pg_class WHERE relname = 'log_line'`).Scan(&estimate); err != nil {
			return st, fmt.Errorf("logstore: status estimate: %w", err)
		}
	}
	if estimate.Valid && estimate.Int64 > 0 {
		st.RowEstimate = estimate.Int64
	}
	return st, nil
}

// cleanList trims and drops blanks, keeping order.
func cleanList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

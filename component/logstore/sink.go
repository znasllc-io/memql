package logstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/logger"
)

// The concept every row of the store is returned as.
const ConceptID = "v1:observability:logLine"

// The handler's bounds (design L5). Queue 4096 lines, batch 256, flush 1s.
// Overflow is dropped and counted by reason; a caller never waits on the
// database.
const (
	DefaultQueueSize     = 4096
	DefaultMaxBatch      = 256
	DefaultFlushInterval = time.Second

	// DefaultReportInterval is how often the sink says, in ONE warn line under
	// component `logs`, how many lines it dropped and why. That line is itself
	// stored, so the Logs app shows its own gaps.
	DefaultReportInterval = time.Minute

	// MaxMessageBytes caps a stored message; MaxAttributesBytes caps the
	// serialized attributes object. Both apply to engine lines and to OS lines
	// alike (design L9 states them for the OS write).
	MaxMessageBytes    = 4096
	MaxAttributesBytes = 8192

	// NodeTypeOS is the node type of a line written by the MemQL OS front end
	// through logsRecordClient. Such a line has no node.
	NodeTypeOS = "os"

	// storeComponent is the component the sink's OWN lines carry. It starts
	// with the recursion-guard prefix core/logger never forwards, so a failing
	// insert cannot produce a line that produces an insert that fails.
	storeComponent = "logs.store"

	// gapComponent is the component of the one-per-interval "dropped N lines"
	// warn line. Deliberately NOT the guarded prefix: the gap line is the one
	// line the store must keep about itself.
	gapComponent = "logs"
)

// Row is one row of log_line. The bun tags are the table; the json tags are
// the concept's field names, which is the shape a logLine node's payload, an
// archive's NDJSON record and a restore's input all share.
type Row struct {
	bun.BaseModel `bun:"table:log_line,alias:ll"`

	OccurredAt     time.Time       `bun:"occurred_at,pk" json:"occurredAt"`
	ID             string          `bun:"id,pk" json:"id"`
	NodeType       string          `bun:"node_type,notnull" json:"nodeType"`
	Node           string          `bun:"node,notnull" json:"node"`
	Level          string          `bun:"level,notnull" json:"level"`
	Component      string          `bun:"component,notnull" json:"component"`
	App            string          `bun:"app,notnull" json:"app"`
	Message        string          `bun:"message,notnull" json:"message"`
	Attributes     json.RawMessage `bun:"attributes,type:jsonb,nullzero" json:"attributes,omitempty"`
	Subject        string          `bun:"subject,notnull" json:"subject"`
	SubjectConcept string          `bun:"subject_concept,notnull" json:"subjectConcept"`
	Session        string          `bun:"session,notnull" json:"session"`
	UserId         string          `bun:"user_id,notnull" json:"userId"`
}

// Stamp is what a line carries about WHERE it came from. The engine path
// stamps the sink's own node type and node; the OS write path passes
// NodeTypeOS, a blank node, the tab session and the actor's user id.
type Stamp struct {
	NodeType string
	Node     string
	Session  string
	UserId   string
}

// InsertFunc writes one batch. The production one is a bun multi-row insert;
// tests inject one that records, blocks, or fails.
type InsertFunc func(ctx context.Context, rows []Row) error

// SinkOptions configures the sink. Zero values take the documented defaults.
type SinkOptions struct {
	QueueSize         int
	MaxBatch          int
	FlushInterval     time.Duration
	ReportInterval    time.Duration
	MaxLinesPerSecond int

	// NodeType and Node stamp every engine line. Resolved from the
	// environment when empty (MEMQL_NODE_TYPE; MEMQL_NODE_ID with the hostname
	// fallback), the way component/node resolves the node's identity.
	NodeType string
	Node     string

	// Logger is the CONSOLE logger the sink's own lines go through. Its lines
	// carry component `logs.store` and are never stored (the recursion guard);
	// the one exception, the periodic gap line, carries component `logs`.
	Logger *slog.Logger

	// Insert overrides the bun insert. Nil uses the database handed to
	// NewSink.
	Insert InsertFunc

	// Now is injectable for tests. Nil uses time.Now.
	Now func() time.Time
}

func (o SinkOptions) defaults() SinkOptions {
	if o.QueueSize <= 0 {
		o.QueueSize = DefaultQueueSize
	}
	if o.MaxBatch <= 0 {
		o.MaxBatch = DefaultMaxBatch
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = DefaultFlushInterval
	}
	if o.ReportInterval <= 0 {
		o.ReportInterval = DefaultReportInterval
	}
	if o.MaxLinesPerSecond <= 0 {
		o.MaxLinesPerSecond = MaxLinesPerSecond()
	}
	if o.NodeType == "" {
		o.NodeType = resolveNodeType()
	}
	if o.Node == "" {
		o.Node = resolveNodeId()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Stats is a snapshot of the sink's counters since the process started. The
// same numbers metrics export, kept in-process so logsStatus can answer them
// for the node that took the call.
type Stats struct {
	Written      uint64 `json:"written"`
	DroppedQueue uint64 `json:"droppedQueue"`
	DroppedRate  uint64 `json:"droppedRate"`
	DroppedLevel uint64 `json:"droppedLevel"`
	DroppedDB    uint64 `json:"droppedDb"`
	Flushes      uint64 `json:"flushes"`
	InsertErrors uint64 `json:"insertErrors"`
	QueueDepth   int    `json:"queueDepth"`
	QueueSize    int    `json:"queueSize"`
}

// Dropped is every drop reason summed.
func (s Stats) Dropped() uint64 {
	return s.DroppedQueue + s.DroppedRate + s.DroppedLevel + s.DroppedDB
}

// Sink is the batching writer over log_line, and the process's
// core/logger.Sink. Write is a per-second token bucket followed by a
// non-blocking channel send; one goroutine drains the queue into batched
// inserts. Lossy under pressure BY DESIGN: a full queue drops the line and
// counts it, because the alternative -- blocking the caller inside a log
// call -- turns logging into a latency hazard on every hot path in the
// process.
type Sink struct {
	opts   SinkOptions
	db     *bun.DB
	insert InsertFunc
	queue  chan Row
	bucket *tokenBucket

	floor slog.Level
	off   bool

	storeLog *slog.Logger // component logs.store: console only
	gapLog   *slog.Logger // component logs: stored

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}

	written      atomic.Uint64
	droppedQueue atomic.Uint64
	droppedRate  atomic.Uint64
	droppedLevel atomic.Uint64
	droppedDB    atomic.Uint64
	flushes      atomic.Uint64
	insertErrors atomic.Uint64

	// reported* are the counters at the last gap report, so the report says
	// what happened in the LAST interval rather than since boot.
	reportedQueue, reportedRate, reportedLevel, reportedDB uint64
	lastDBWarn                                             time.Time
}

// NewSink constructs the sink. It does not start the drain; call Start.
func NewSink(db *bun.DB, opts SinkOptions) *Sink {
	opts = opts.defaults()
	floor, off := logger.StoreFloor()
	s := &Sink{
		opts:     opts,
		db:       db,
		insert:   opts.Insert,
		queue:    make(chan Row, opts.QueueSize),
		bucket:   newTokenBucket(float64(opts.MaxLinesPerSecond), float64(opts.MaxLinesPerSecond), opts.Now()),
		floor:    floor,
		off:      off,
		storeLog: opts.Logger.With("component", storeComponent),
		gapLog:   opts.Logger.With("component", gapComponent),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	if s.insert == nil {
		s.insert = s.bunInsert
	}
	return s
}

// Write implements core/logger.Sink for the engine path: the line is stamped
// with this node's type and id.
func (s *Sink) Write(l logger.Line) {
	s.WriteStamped(l, Stamp{NodeType: s.opts.NodeType, Node: s.opts.Node})
}

// WriteStamped enqueues one line under an explicit stamp. Non-blocking. It
// reports whether the line was queued; a false is already counted under its
// reason.
func (s *Sink) WriteStamped(l logger.Line, st Stamp) bool {
	if s.off || l.Level < s.floor {
		s.droppedLevel.Add(1)
		metrics.LogsDropped(metrics.LogsDropLevel, 1)
		return false
	}
	if !s.bucket.take(s.opts.Now(), 1) {
		s.droppedRate.Add(1)
		metrics.LogsDropped(metrics.LogsDropRate, 1)
		return false
	}
	row := rowFromLine(l, st, s.opts.NodeType, s.opts.Node)
	select {
	case s.queue <- row:
		return true
	default:
		s.droppedQueue.Add(1)
		metrics.LogsDropped(metrics.LogsDropQueue, 1)
		return false
	}
}

// Start begins the background drain. Idempotent.
func (s *Sink) Start() {
	s.startOnce.Do(func() { go s.run() })
}

// Stop flushes what is queued and stops the drain. Blocks until done or ctx
// is cancelled. Idempotent.
func (s *Sink) Stop(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		select {
		case <-s.doneCh:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	return err
}

// Stats returns a snapshot of the counters.
func (s *Sink) Stats() Stats {
	return Stats{
		Written:      s.written.Load(),
		DroppedQueue: s.droppedQueue.Load(),
		DroppedRate:  s.droppedRate.Load(),
		DroppedLevel: s.droppedLevel.Load(),
		DroppedDB:    s.droppedDB.Load(),
		Flushes:      s.flushes.Load(),
		InsertErrors: s.insertErrors.Load(),
		QueueDepth:   len(s.queue),
		QueueSize:    cap(s.queue),
	}
}

// NodeType and Node are what this sink stamps engine lines with.
func (s *Sink) NodeType() string { return s.opts.NodeType }
func (s *Sink) Node() string     { return s.opts.Node }

// MaxLinesPerSecond is the bucket's rate on this node.
func (s *Sink) MaxLinesPerSecond() int { return s.opts.MaxLinesPerSecond }

func (s *Sink) run() {
	defer close(s.doneCh)
	ticker := time.NewTicker(s.opts.FlushInterval)
	defer ticker.Stop()
	report := time.NewTicker(s.opts.ReportInterval)
	defer report.Stop()

	batch := make([]Row, 0, s.opts.MaxBatch)

	drainTo := func(target *[]Row, max int) {
		for len(*target) < max {
			select {
			case r := <-s.queue:
				*target = append(*target, r)
			default:
				return
			}
		}
	}

	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.insert(ctx, batch)
		cancel()
		if err != nil {
			n := uint64(len(batch))
			s.insertErrors.Add(1)
			s.droppedDB.Add(n)
			metrics.LogsDropped(metrics.LogsDropDB, n)
			s.warnInsertFailure(len(batch), err)
		} else {
			s.written.Add(uint64(len(batch)))
			s.flushes.Add(1)
			metrics.LogsWritten(len(batch))
		}
		batch = batch[:0]
	}

	for {
		select {
		case r := <-s.queue:
			batch = append(batch, r)
			drainTo(&batch, s.opts.MaxBatch)
			if len(batch) >= s.opts.MaxBatch {
				flush()
			}
		case <-ticker.C:
			drainTo(&batch, s.opts.MaxBatch)
			flush()
		case <-report.C:
			s.reportDrops()
		case <-s.stopCh:
			drainTo(&batch, s.opts.MaxBatch)
			flush()
			for {
				select {
				case r := <-s.queue:
					batch = append(batch, r)
					if len(batch) >= s.opts.MaxBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// warnInsertFailure logs a failed insert to the CONSOLE ONLY (component
// logs.store, which the recursion guard never forwards), at most once a
// minute -- a database that is down fails every batch, and one line per
// second about it would be the store drowning the console it fell back to.
func (s *Sink) warnInsertFailure(count int, err error) {
	now := s.opts.Now()
	if !s.lastDBWarn.IsZero() && now.Sub(s.lastDBWarn) < time.Minute {
		return
	}
	s.lastDBWarn = now
	s.storeLog.Warn("log store: insert failed; batch dropped", "count", count, "err", err)
}

// reportDrops logs ONE warn line under component `logs` -- which IS stored --
// when anything was dropped since the last report, so the store shows its
// own gaps. Nothing is logged for a clean interval: a line saying "dropped 0"
// every minute on every node would itself be the noise.
func (s *Sink) reportDrops() {
	q, r, l, d := s.droppedQueue.Load(), s.droppedRate.Load(), s.droppedLevel.Load(), s.droppedDB.Load()
	dq, dr, dl, dd := q-s.reportedQueue, r-s.reportedRate, l-s.reportedLevel, d-s.reportedDB
	s.reportedQueue, s.reportedRate, s.reportedLevel, s.reportedDB = q, r, l, d
	total := dq + dr + dd
	if total == 0 {
		return
	}
	s.gapLog.Warn(fmt.Sprintf("logs: dropped %d lines (queue %d, rate %d, db %d)", total, dq, dr, dd),
		"dropped", total, "queue", dq, "rate", dr, "db", dd, "level", dl, "node", s.opts.Node)
}

// ReportDropsNow forces the gap report. For tests, so they do not wait a
// minute for the ticker.
func (s *Sink) ReportDropsNow() { s.reportDrops() }

func (s *Sink) bunInsert(ctx context.Context, rows []Row) error {
	if s.db == nil {
		return errors.New("nil bun.DB")
	}
	if _, err := s.db.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return fmt.Errorf("log_line batch insert: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Line -> Row
// ---------------------------------------------------------------------------

// rowFromLine renders a Line into the row the table takes. Every string is
// made safe for a Postgres text column (valid UTF-8, no NUL -- a single
// offending byte rejects the whole batch), the message is cut at
// MaxMessageBytes on a rune boundary, and the attributes are shrunk until
// they fit MaxAttributesBytes as VALID JSON, because a jsonb column refuses a
// truncated document and that refusal would take the other 255 rows of the
// batch with it.
func rowFromLine(l logger.Line, st Stamp, defaultNodeType, defaultNode string) Row {
	at := l.At
	if at.IsZero() {
		at = time.Now()
	}
	nodeType := st.NodeType
	if nodeType == "" {
		nodeType = defaultNodeType
	}
	node := st.Node
	if node == "" && nodeType != NodeTypeOS {
		node = defaultNode
	}
	component := safeText(l.Component)
	if component == "" {
		component = "unknown"
	}
	return Row{
		OccurredAt:     at.UTC(),
		ID:             id.NewShortId(),
		NodeType:       safeText(nodeType),
		Node:           safeText(node),
		Level:          levelName(l.Level),
		Component:      truncateUTF8(component, 256),
		App:            truncateUTF8(safeText(l.App), 64),
		Message:        truncateUTF8(safeText(l.Message), MaxMessageBytes),
		Attributes:     encodeAttributes(l.Attributes, MaxAttributesBytes),
		Subject:        truncateUTF8(safeText(l.Subject), 512),
		SubjectConcept: truncateUTF8(safeText(l.SubjectConcept), 256),
		Session:        truncateUTF8(safeText(st.Session), 64),
		UserId:         truncateUTF8(safeText(st.UserId), 256),
	}
}

// levelName maps an slog level onto the concept's closed enum. Levels between
// the named ones (slog.LevelWarn+1) fold onto the named level below them.
func levelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

// safeText makes a string storable in a Postgres text column: NUL bytes
// removed, invalid UTF-8 replaced.
func safeText(s string) string {
	if s == "" {
		return s
	}
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return s
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// encodeAttributes serializes attrs to at most limit bytes of VALID JSON, or
// nil when there are none. Three steps, each only when the previous did not
// fit: marshal as is; cut every string value to 512 bytes; replace the whole
// object with a marker saying how large it was. The marker keeps the row --
// a line whose attributes were too big is still a line that happened.
func encodeAttributes(attrs map[string]any, limit int) json.RawMessage {
	if len(attrs) == 0 {
		return nil
	}
	clean := make(map[string]any, len(attrs))
	for k, v := range attrs {
		if s, ok := v.(string); ok {
			v = safeText(s)
		}
		clean[safeText(k)] = v
	}
	b, err := json.Marshal(clean)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"_unserializable": err.Error()})
		return b
	}
	if len(b) <= limit {
		return b
	}
	for k, v := range clean {
		if s, ok := v.(string); ok && len(s) > 512 {
			clean[k] = truncateUTF8(s, 512) + "…"
		}
	}
	clean["_truncated"] = true
	if b2, err := json.Marshal(clean); err == nil && len(b2) <= limit {
		return b2
	}
	b3, _ := json.Marshal(map[string]any{"_truncated": true, "_bytes": len(b), "_keys": len(attrs)})
	return b3
}

// ---------------------------------------------------------------------------
// Token bucket
// ---------------------------------------------------------------------------

// tokenBucket is a capacity + refill-per-second bucket. The
// component/identity/abuse.IPRateLimiter shape, keyed on nothing: the store's
// bucket is per node, so one chatty replica cannot starve its siblings.
type tokenBucket struct {
	mu       sync.Mutex
	capacity float64
	rate     float64 // tokens per second
	tokens   float64
	updated  time.Time
}

func newTokenBucket(capacity, ratePerSecond float64, now time.Time) *tokenBucket {
	return &tokenBucket{capacity: capacity, rate: ratePerSecond, tokens: capacity, updated: now}
}

// take consumes n tokens when the bucket holds at least n after refill, and
// reports whether it did. A refusal consumes nothing.
func (b *tokenBucket) take(now time.Time, n float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if elapsed := now.Sub(b.updated); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.rate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.updated = now
	}
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// ---------------------------------------------------------------------------
// The process-global sink
// ---------------------------------------------------------------------------

var current atomic.Pointer[Sink]

// Current returns the sink the SinkComponent installed on this node, or nil
// when the node has no database (or has not finished booting). The plug-in
// reads stats and writes OS lines through it.
func Current() *Sink { return current.Load() }

func setCurrent(s *Sink) { current.Store(s) }

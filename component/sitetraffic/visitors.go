package sitetraffic

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/common"
)

// VisitorsComponentName is the bus-side name of the visitor-secret keeper.
const VisitorsComponentName = common.ComponentName("sitetraffic.visitors")

// daySecret is one UTC day's secret. It travels with its day so Visitor can
// refuse to hash with a secret that does not belong to the request's day.
type daySecret struct {
	day    time.Time // UTC midnight
	secret []byte
}

// visitorHash is HMAC-SHA256(secret, siteId|addressKey|userAgent), first 16
// bytes, hex. Keyed by a secret that is deleted after its day, so a visitor
// cannot be re-identified across days, and keyed by site so two sites cannot
// be joined on a visitor. The address and agent are inputs only, never stored.
func visitorHash(secret []byte, siteID, addressKey, userAgent string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(siteID + "|" + addressKey + "|" + userAgent))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// VisitorSecrets keeps the day's visitor secret on an edge replica.
//
// Every replica reads the SAME row (insert-if-absent, then select), so two
// replicas hash one visitor identically; that is what makes the daily
// unique-visitor count agree across a cluster.
//
// The request path only does an atomic load. All database work happens in
// Start, at each UTC midnight and on a periodic safety check, so a slow or
// absent database can never delay a response.
//
// FAIL CLOSED: a keeper that cannot read today's secret yields no visitor. It
// never falls back to an older secret, which would link a person across days
// and outlive the deletion that is the privacy promise.
type VisitorSecrets struct {
	logger   *slog.Logger
	provider func() *bun.DB
	now      func() time.Time

	current atomic.Pointer[daySecret]

	// deleteEarlier removes secrets of days before day. A field so a test can
	// make it fail once; production uses purgeSQL.
	deleteEarlier func(ctx context.Context, db *bun.DB, day time.Time) error
	running       atomic.Bool
	readyCh       chan struct{}

	mu     sync.Mutex // guards closed
	stop   chan struct{}
	closed bool
}

var _ common.Dependency = (*VisitorSecrets)(nil)

// NewVisitorSecrets constructs the keeper. now is injectable for tests.
func NewVisitorSecrets(logger *slog.Logger, provider func() *bun.DB, now func() time.Time) *VisitorSecrets {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &VisitorSecrets{
		logger:   logger.With("component", "sitetraffic.visitors"),
		provider: provider,
		now:      now,
		readyCh:  make(chan struct{}),
		stop:     make(chan struct{}),

		deleteEarlier: purgeSQL,
	}
}

func purgeSQL(ctx context.Context, db *bun.DB, day time.Time) error {
	_, err := db.NewRaw("DELETE FROM edge_visitor_secret WHERE day < ?", day).Exec(ctx)
	return err
}

// errPurge marks a failure of the delete half of a rotation, so the warning
// can say which half failed: visitors ARE being counted then, but an earlier
// day's secret is still stored.
var errPurge = errors.New("delete earlier secrets")

// Visitor returns the visitor id for a request at time at, or "" when the held
// secret is not that UTC day's (none held, rotation pending or failed).
func (v *VisitorSecrets) Visitor(siteID, addressKey, userAgent string, at time.Time) string {
	cur := v.current.Load()
	if cur == nil || !cur.day.Equal(utcMidnight(at)) {
		return ""
	}
	return visitorHash(cur.secret, siteID, addressKey, userAgent)
}

func utcMidnight(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// rotate makes day's secret current: insert one if no replica has yet, read
// back whichever row won, publish it, then delete earlier days.
func (v *VisitorSecrets) rotate(ctx context.Context, day time.Time) error {
	db := v.provider()
	if db == nil {
		return fmt.Errorf("no database handle")
	}
	day = utcMidnight(day)
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return fmt.Errorf("generate secret: %w", err)
	}
	if _, err := db.NewRaw("INSERT INTO edge_visitor_secret (day, secret) VALUES (?, ?) ON CONFLICT (day) DO NOTHING",
		day, fresh).Exec(ctx); err != nil {
		return fmt.Errorf("insert secret: %w", err)
	}
	var stored []byte
	if err := db.NewRaw("SELECT secret FROM edge_visitor_secret WHERE day = ?", day).Scan(ctx, &stored); err != nil {
		return fmt.Errorf("read secret: %w", err)
	}
	if len(stored) == 0 {
		return fmt.Errorf("stored secret for %s is empty", day.Format("2006-01-02"))
	}
	v.current.Store(&daySecret{day: day, secret: stored})
	// Published before the delete: a failed delete never leaves requests
	// without a secret. tick retries the delete on every pass, because the
	// held day alone says nothing about whether earlier rows are gone.
	return v.purge(ctx, db, day)
}

// purge deletes every secret older than day. On a table of at most a couple of
// rows it costs nothing, which is why tick runs it every time rather than
// tracking a "delete pending" flag that could itself be wrong.
func (v *VisitorSecrets) purge(ctx context.Context, db *bun.DB, day time.Time) error {
	if err := v.deleteEarlier(ctx, db, day); err != nil {
		return fmt.Errorf("%w: %w", errPurge, err)
	}
	return nil
}

func (v *VisitorSecrets) Start(ctx context.Context) {
	defer close(v.readyCh)
	for i := 0; i < 10 && v.provider() == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	v.tick(ctx)
	v.running.Store(true)
	go v.loop(ctx)
}

// loop rotates at each UTC midnight and re-checks hourly, so a replica whose
// midnight rotation or purge failed (database blip) recovers within the hour
// instead of going without visitors, or keeping a stale secret, for a day.
func (v *VisitorSecrets) loop(ctx context.Context) {
	for {
		now := v.now()
		next := utcMidnight(now).Add(24 * time.Hour)
		wait := next.Sub(now)
		if wait > time.Hour {
			wait = time.Hour
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
			v.tick(ctx)
		case <-v.stop:
			t.Stop()
			return
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

// tick rotates when the held secret is not today's, and otherwise re-runs the
// purge: the promise is that after midnight no stored secret outlives its day,
// and a delete that failed at rotation would otherwise wait a full day.
func (v *VisitorSecrets) tick(ctx context.Context) {
	day := utcMidnight(v.now())
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var err error
	if cur := v.current.Load(); cur != nil && cur.day.Equal(day) {
		db := v.provider()
		if db == nil {
			return
		}
		err = v.purge(rctx, db, day)
	} else {
		err = v.rotate(rctx, day)
	}
	switch {
	case err == nil:
	case errors.Is(err, errPurge):
		v.logger.Warn("could not delete earlier days' visitor secrets; visitors are still counted, but those secrets stay stored until a later hourly check succeeds", "err", err)
	default:
		v.logger.Warn("could not rotate the visitor secret; unique visitors are not counted until it succeeds", "err", err)
	}
}

func (v *VisitorSecrets) Stop(ctx context.Context) {
	v.mu.Lock()
	if !v.closed {
		v.closed = true
		close(v.stop)
	}
	v.mu.Unlock()
	v.running.Store(false)
}

func (v *VisitorSecrets) IsRunning() bool { return v.running.Load() }
func (v *VisitorSecrets) Order() int      { return 51 }
func (v *VisitorSecrets) ComponentName() common.ComponentName {
	return VisitorsComponentName
}
func (v *VisitorSecrets) Ready() <-chan struct{} { return v.readyCh }

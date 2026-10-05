package memql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// ErrRowVersionChanged means the read that authorized a write is no longer
// current. The caller must read and decide again, never blindly repeat effects.
var ErrRowVersionChanged = errors.New("row version changed before the write committed")

type rowVersionFence struct {
	concept, id string
	version     time.Time
}
type rowVersionFencesKey struct{}

// ContextWithRowVersionFence requires a row to retain its observed version
// until a mutation commits. A zero version requires the row to be absent.
// Fences compose, restrict writes only, and grant no read/write authority.
//
// The compare, advisory transaction lock and mutation use ONE connection.
// All writers of an ownership row must participate in this protocol; this is
// not a lock against arbitrary SQL or an old, unfenced engine process. The
// fence covers durable writes, not external calls or before-write effects.
func ContextWithRowVersionFence(ctx context.Context, concept, rowID string, version time.Time) context.Context {
	concept = strings.TrimSpace(concept)
	rowID = strings.TrimSpace(rowID)
	if rowID != "" && !strings.HasPrefix(rowID, concept+":") {
		rowID = concept + ":" + rowID
	}
	fences := append([]rowVersionFence(nil), rowVersionFences(ctx)...)
	fences = append(fences, rowVersionFence{concept: concept, id: rowID, version: version.UTC().Truncate(time.Microsecond)})
	return context.WithValue(ContextWithFreshRead(ctx), rowVersionFencesKey{}, fences)
}

// HasRowVersionFence reports whether the caller already supplied the witness
// for this row. Adapters must preserve it rather than silently refresh it.
func HasRowVersionFence(ctx context.Context, concept, rowID string) bool {
	concept = strings.TrimSpace(concept)
	rowID = strings.TrimSpace(rowID)
	if rowID != "" && !strings.HasPrefix(rowID, concept+":") {
		rowID = concept + ":" + rowID
	}
	for _, f := range rowVersionFences(ctx) {
		if f.concept == concept && f.id == rowID {
			return true
		}
	}
	return false
}
func rowVersionFences(ctx context.Context) []rowVersionFence {
	fences, _ := ctx.Value(rowVersionFencesKey{}).([]rowVersionFence)
	return fences
}

// checkRowVersionFences executes inside the mutation's own transaction. A
// killed database session rolls back the mutation along with its locks.
// staged-data: MUST-NOT-GATE -- an unpublished ownership version still
// supersedes the witness; hiding it would admit a stale writer.
func (e *MemQLEngine) checkRowVersionFences(ctx context.Context, tx bun.Tx) error {
	fences := append([]rowVersionFence(nil), rowVersionFences(ctx)...)
	sort.Slice(fences, func(i, j int) bool { return fences[i].id < fences[j].id })
	for _, f := range fences {
		if f.concept == "" || f.id == "" || f.id == f.concept+":" {
			return fmt.Errorf("row version fence requires a concept and row id")
		}
		if e.concepts == nil {
			return fmt.Errorf("row version fence requires a concept registry")
		}
		if _, err := e.concepts.Get(f.concept); err != nil {
			return fmt.Errorf("row version fence: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1296387415, hashtext(?))`, f.id); err != nil {
			return err
		}
		var at time.Time
		err := tx.NewSelect().Model((*memorynodes.MemoryNode)(nil)).Column("createdAt").Where("concept = ?", f.concept).Where("id = ?", f.id).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &at)
		if errors.Is(err, sql.ErrNoRows) {
			if f.version.IsZero() {
				continue
			}
			return ErrRowVersionChanged
		}
		if err != nil {
			return err
		}
		if f.version.IsZero() || !at.Equal(f.version) {
			return ErrRowVersionChanged
		}
	}
	return nil
}

// fenceWriteTarget records the version whose payload is about to be merged.
// A concurrent write invalidates the entire decision rather than losing fields.
// staged-data: MUST-NOT-GATE -- the witness must include unpublished target
// versions or a later merge could overwrite a write hidden by publication state.
func (e *MemQLEngine) fenceWriteTarget(ctx context.Context, concept, rowID string) (context.Context, error) {
	if HasRowVersionFence(ctx, concept, rowID) {
		return ctx, nil
	}
	canonical := rowID
	if !strings.HasPrefix(canonical, concept+":") {
		canonical = concept + ":" + canonical
	}
	if e.database() == nil {
		return nil, fmt.Errorf("row version fence requires a database")
	}
	var at time.Time
	err := e.database().NewSelect().Model((*memorynodes.MemoryNode)(nil)).Column("createdAt").Where("concept = ?", concept).Where("id = ?", canonical).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &at)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return ContextWithRowVersionFence(ctx, concept, rowID, at), nil
}

func fenceWriteClock(ctx context.Context, concept, rowID string, params *memorynodes.CreateParams) {
	if !strings.HasPrefix(rowID, concept+":") {
		rowID = concept + ":" + rowID
	}
	for _, f := range rowVersionFences(ctx) {
		if f.concept != concept || f.id != rowID || f.version.IsZero() {
			continue
		}
		now := time.Now()
		if params.Clock != nil {
			now = params.Clock()
		}
		at := VersionTimeAfter(f.version, now)
		params.Clock = func() time.Time { return at }
	}
}

package work

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

// Retention checkpoints survive a cron leader moving to another replica. A
// generation fence prevents a concurrent caller from moving the cursor back.
// Completing a traversal clears its checkpoint: a run/goal held by a child, a
// refused archive, a denied row or a backdated insertion must get another turn.
// The time index makes that recheck cheap; the cursor prevents an interrupted
// traversal from spending every night's share on the same protected history.
type retentionProgress struct {
	key        string
	at         sql.NullTime
	id         string
	generation int64
}

func (i *Integration) loadRetentionProgress(ctx context.Context, p operationalPolicy) (*retentionProgress, error) {
	db, err := i.gatedDB()
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%s/%s/%s", p.concept, p.env, id.NewUntracked().FromString(fmt.Sprintf("%d:%s", p.days, p.predicate)))
	if _, err := db.ExecContext(ctx, `INSERT INTO work_retention_progress (policy) VALUES (?) ON CONFLICT DO NOTHING`, key); err != nil {
		return nil, err
	}
	s := &retentionProgress{key: key}
	err = db.QueryRowContext(ctx, `SELECT cursor_at,cursor_id,generation FROM work_retention_progress WHERE policy=?`, key).Scan(&s.at, &s.id, &s.generation)
	return s, err
}

func (i *Integration) saveRetentionProgress(ctx context.Context, s *retentionProgress, at time.Time, id string) error {
	if s == nil {
		return nil
	} // dry runs neither consume nor move progress
	db, err := i.gatedDB()
	if err != nil {
		return err
	}
	var cursor any
	if !at.IsZero() {
		cursor = at
	}
	res, err := db.ExecContext(ctx, `UPDATE work_retention_progress SET cursor_at=?,cursor_id=?,generation=generation+1 WHERE policy=? AND generation=?`, cursor, id, s.key, s.generation)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("retention progress changed on another replica; resume on the next pass")
	}
	s.generation++
	return nil
}

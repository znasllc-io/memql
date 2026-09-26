package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"
)

// Refresh and read share one snapshot. An event committed while this reader is
// running stays queued for the next reader, just as a normal SELECT sees only
// commits before its snapshot. A backlog commits bounded progress and refuses
// to describe an incomplete projection as an empty set of waiting work.
func (i *Integration) runsInFlight(ctx context.Context) ([]map[string]any, error) {
	if i.admitRow == nil {
		return nil, fmt.Errorf("work: no row-admission gate is wired")
	}
	if i.bunDB == nil || i.bunDB() == nil {
		return nil, fmt.Errorf("work: recovery needs a database handle")
	}
	var progress []byte
	for attempt := 0; attempt < 4; attempt++ {
		var rows []map[string]any
		var state struct {
			Ready bool `json:"ready"`
		}
		err := i.bunDB().RunInTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(ctx context.Context, tx bun.Tx) error {
			if err := tx.QueryRowContext(ctx, `SELECT refresh_work_run_heads(1000)`).Scan(&progress); err != nil {
				return err
			}
			if err := json.Unmarshal(progress, &state); err != nil {
				return err
			}
			if !state.Ready {
				return nil // Commit the batch before reporting that recovery is not ready.
			}
			var err error
			rows, err = i.selectAdmittedFrom(ctx, tx, runConcept, runsInFlightSQL, sweepPageSize, runConcept)
			return err
		})
		if err != nil {
			var pgerr pgdriver.Error
			if errors.As(err, &pgerr) && pgerr.Field('C') == "40001" {
				continue // A fresh transaction, never a statement in the aborted one.
			}
			return nil, fmt.Errorf("work: current-run recovery projection: %w", err)
		}
		if state.Ready {
			return rows, nil
		}
	}
	return nil, fmt.Errorf("work: current-run recovery projection is catching up; retry the sweep: %s", progress)
}

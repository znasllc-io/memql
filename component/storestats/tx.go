package storestats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

// tx.go -- the one way this package writes.
//
// # One lock
//
// Every transaction that writes begins by taking one lock, lockWrites: LOCK
// TABLE store_sales_store IN SHARE ROW EXCLUSIVE MODE, the lock the website
// statistics rollup takes on its own table (component/sitetraffic,
// rollup.go). The mode conflicts with itself and with every writer of the
// table, and with no reader, so two nodes' writers -- two runs when the cron
// lead moves, a run and the check -- take turns, and the statistics read is
// never held up by one. It is taken first, whatever is written after, so no
// two orders of locks exist to deadlock on.
//
// # One generation
//
// The lock serialises writes; it cannot say that what a write carries is
// still right. A page of a store's mirror read before a rebuild and written
// after it would put old facts over a store whose watermarks the rebuild had
// set back, and nothing would read the rows between again. So a store's row
// holds a generation, raised by a rebuild and a removal, and a write made for
// a page names the generation the page was read under: writeTx reads the
// stored one under the lock and, when they differ, runs nothing and answers
// errRebuilt. A write that is right whatever the generation -- a store's own
// row, a rebuild, a batch of a delete -- names anyGeneration.
//
// # Nothing of the engine inside
//
// No engine call is made inside writeTx. The engine reads through its own
// pool, and a page read while this transaction holds the lock would hold the
// lock for as long as the read takes, every other node's writer waiting
// behind it. What a transaction needs from the engine is read before it
// begins.
//
// TestEveryWritingTransactionBeginsWithTheLock holds all of this to the
// source: a transaction is opened here and nowhere else, writeTx's first
// statement is the lock, and every function that writes is named there and
// writes through the bun.Tx it is handed.

// anyGeneration is the generation a write names when it is right whatever the
// store's generation: its own row, a rebuild, a batch of a delete.
const anyGeneration int64 = -1

// errRebuilt answers a write made for a page read under an older generation
// of its store, or for a store with no row: the write was not made, and the
// store is left for the next run, which reads it under the new one. Not a
// failure.
var errRebuilt = errors.New("storestats: the store was rebuilt after this page was read")

// lockWrites takes the package's one lock inside tx. See "One lock".
func lockWrites(ctx context.Context, tx bun.Tx) error {
	if _, err := tx.ExecContext(ctx, "LOCK TABLE store_sales_store IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return fmt.Errorf("take the store statistics lock: %w", err)
	}
	return nil
}

// storeGeneration reads the store's generation. A store with no row answers
// an error that wraps sql.ErrNoRows.
func storeGeneration(ctx context.Context, db bun.IDB, storeID string) (int64, error) {
	var generation int64
	err := db.NewSelect().Table("store_sales_store").Column("generation").
		Where("store_id = ?", storeID).Scan(ctx, &generation)
	if err != nil {
		return 0, fmt.Errorf("read the generation of store %s: %w", storeID, err)
	}
	return generation, nil
}

// writeTx runs fn in one transaction that begins with lockWrites and then,
// unless generation is anyGeneration, reads the store's generation under the
// lock. When that is not the one named, the store has no row, or the store
// is purging, fn is not run, nothing is written, and errRebuilt is answered.
func writeTx(ctx context.Context, db *bun.DB, storeID string, generation int64, fn func(ctx context.Context, tx bun.Tx) error) error {
	if db == nil {
		return errors.New("this node has no database handle, so the store statistics cannot be written")
	}
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := lockWrites(ctx, tx); err != nil {
			return err
		}
		if generation != anyGeneration {
			stored, err := storeGeneration(ctx, tx, storeID)
			if errors.Is(err, sql.ErrNoRows) {
				return errRebuilt
			}
			if err != nil {
				return err
			}
			if stored != generation {
				return errRebuilt
			}
			// Nothing but the purge's own batches (anyGeneration) is written
			// to a store whose facts are being deleted, read under the same
			// lock: a page written now would be deleted by the next batch
			// and its watermark kept.
			purging, err := storePurging(ctx, tx, storeID)
			if err != nil {
				return err
			}
			if purging {
				return errRebuilt
			}
		}
		return fn(ctx, tx)
	})
}

// readTx runs fn in one read-only transaction. It takes no lock, so it never
// waits for a writer. It reads at REPEATABLE READ, one snapshot for every
// statement fn makes, so a read that sums several tables sums one moment of
// them; a read-only transaction at that level is never refused for a
// conflict.
func readTx(ctx context.Context, db *bun.DB, fn func(ctx context.Context, tx bun.Tx) error) error {
	if db == nil {
		return errors.New("this node has no database handle, so the store statistics cannot be read")
	}
	return db.RunInTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}, fn)
}

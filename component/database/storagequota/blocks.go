package storagequota

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

var ErrUnreservedBlock = errors.New("storage block has no reservation")

type block struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
}
type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// One database round trip for a 15,000-block backup, not one per block.
func (g *Guard) selected(ctx context.Context, q queryer, ids []string) ([]block, int64, error) {
	if len(ids) > 50000 {
		return nil, 0, errors.New("too many storage blocks")
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			return nil, 0, errors.New("invalid or duplicate storage block")
		}
		seen[id] = true
	}
	data, _ := json.Marshal(ids)
	if ids == nil {
		data = []byte("[]")
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT ON (block_id) block_id,size_bytes FROM storage_capacity_blocks WHERE object_key=$1 AND block_id IN (SELECT jsonb_array_elements_text($2::jsonb)) ORDER BY block_id,committed ASC`, g.Key, string(data))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	sizes := make(map[string]int64, len(ids))
	for rows.Next() {
		var b block
		if err = rows.Scan(&b.ID, &b.Size); err != nil {
			return nil, 0, err
		}
		sizes[b.ID] = b.Size
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	blocks := make([]block, len(ids))
	var total int64
	for i, id := range ids {
		n, ok := sizes[id]
		if !ok {
			return nil, 0, ErrUnreservedBlock
		}
		if n > ClusterBytes-total {
			return nil, 0, ErrLimit
		}
		total += n
		blocks[i] = block{ID: id, Size: n}
	}
	return blocks, total, nil
}
func (g *Guard) BlockListSize(ctx context.Context, ids []string) (int64, error) {
	db, err := g.Store.Database()
	if err != nil {
		return 0, err
	}
	_, size, err := g.selected(ctx, db, ids)
	return size, err
}
func (g *Guard) BlocksCommitted(ctx context.Context, ids []string, final bool) error {
	return g.Store.transaction(ctx, func(tx *sql.Tx) error {
		blocks, size, err := g.selected(ctx, tx, ids)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM storage_capacity_blocks WHERE object_key=$1`, g.Key); err != nil {
			return err
		}
		data, _ := json.Marshal(blocks)
		if _, err = tx.ExecContext(ctx, `INSERT INTO storage_capacity_blocks(object_key,block_id,size_bytes,committed) SELECT $1,id,size,true FROM jsonb_to_recordset($2::jsonb) AS b(id text,size bigint)`, g.Key, string(data)); err != nil {
			return err
		}
		return g.settled(ctx, tx, size, final)
	})
}

// ImportBlocks adopts a provider block catalog left by a pre-ledger upload.
// Its physical bytes were already imported by account inventory. Capacity never
// shrinks here; unknown writes remain reserved until a fenced commit receipt.
func (g *Guard) ImportBlocks(ctx context.Context, committed, staged map[string]int64) error {
	return g.Store.transaction(ctx, func(tx *sql.Tx) error {
		var owner string
		var used, capacity int64
		if err := tx.QueryRowContext(ctx, `SELECT owner_id,used_bytes,capacity_bytes FROM storage_capacity_objects WHERE object_key=$1 AND NOT retired`, g.Key).Scan(&owner, &used, &capacity); err != nil {
			return err
		}
		if owner != g.Owner {
			return errors.New("storage destination belongs to another owner")
		}
		var current, total int64
		for _, set := range []map[string]int64{committed, staged} {
			for id, n := range set {
				if id == "" || len(id) > 128 || n <= 0 || n > ClusterBytes-total {
					return ErrLimit
				}
				total += n
			}
		}
		for _, n := range committed {
			current += n
		}
		target := max(capacity, total+ObjectOverhead)
		if target > capacity {
			u, err := usage(ctx, tx, g.Owner)
			if err != nil {
				return err
			}
			if target-capacity > u.Available {
				return ErrLimit
			}
		}
		for _, entry := range []struct {
			blocks    map[string]int64
			committed bool
		}{{committed, true}, {staged, false}} {
			for id, size := range entry.blocks {
				var previous int64
				err := tx.QueryRowContext(ctx, `SELECT size_bytes FROM storage_capacity_blocks WHERE object_key=$1 AND block_id=$2 AND committed=$3`, g.Key, id, entry.committed).Scan(&previous)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if err == nil && previous != size {
					return errors.New("stored block changed outside its reservation")
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO storage_capacity_blocks(object_key,block_id,size_bytes,committed) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, g.Key, id, size, entry.committed); err != nil {
					return err
				}
			}
		}
		newUsed := current + ObjectOverhead
		if _, err := tx.ExecContext(ctx, `UPDATE storage_capacity_objects SET used_bytes=$2,capacity_bytes=$3 WHERE object_key=$1`, g.Key, newUsed, target); err != nil {
			return err
		}
		return totals(ctx, tx, g.Owner, newUsed-used, (target-newUsed)-(capacity-used))
	})
}

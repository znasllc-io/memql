package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/events"
)

// pgOutboxStore is the Postgres/bun-backed outboxStore. It speaks plain SQL
// against the mesh_outbox / mesh_key_seq / mesh_cursor tables created by the
// 20260610000000_mesh_delivery_substrate migration -- no replication slot, no
// NOTIFY, so it runs byte-identical on local Docker Postgres and managed Tiger
// Cloud (memql#1263, ADR section 5.1).
//
// dbGetter is resolved lazily (mirroring ClusterExecutionGuard) so the store
// can be constructed before the database component is up. A nil DB yields a
// typed error rather than a panic.
type pgOutboxStore struct {
	dbGetter func() *bun.DB
}

// newPGOutboxStore wires a durable outbox store over the cluster database.
func newPGOutboxStore(dbGetter func() *bun.DB) *pgOutboxStore {
	return &pgOutboxStore{dbGetter: dbGetter}
}

// errNoDB is returned when the database handle is not yet available.
var errNoDB = errors.New("delivery substrate: database not available")

func (s *pgOutboxStore) db() (*sql.DB, error) {
	if s.dbGetter == nil {
		return nil, errNoDB
	}
	bunDB := s.dbGetter()
	if bunDB == nil || bunDB.DB == nil {
		return nil, errNoDB
	}
	return bunDB.DB, nil
}

// Append idempotently inserts a deliverable, allocating the next per-key seq in
// the same transaction. Idempotency is enforced by the uq_mesh_outbox_event_id
// unique index: a duplicate EventID does not insert and we return the existing
// row's seq with duplicate=true (ADR 4.2). The seq allocator upserts
// mesh_key_seq under the row's per-key lock so seq is strictly increasing per
// routing_key without scanning the outbox.
func (s *pgOutboxStore) Append(ctx context.Context, d Deliverable) (int64, bool, error) {
	db, err := s.db()
	if err != nil {
		return 0, false, err
	}

	payload, err := json.Marshal(d.Payload)
	if err != nil {
		return 0, false, fmt.Errorf("delivery substrate: marshal payload: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit; best-effort on the error path

	// Fast idempotency check: if this EventID already landed, return its seq.
	var existing int64
	err = tx.QueryRowContext(ctx,
		`SELECT seq FROM mesh_outbox WHERE event_id = $1`, d.EventID).Scan(&existing)
	switch {
	case err == nil:
		// Already produced; idempotent no-op.
		if cErr := tx.Commit(); cErr != nil {
			return 0, false, cErr
		}
		return existing, true, nil
	case errors.Is(err, sql.ErrNoRows):
		// New event; fall through to allocate + insert.
	default:
		return 0, false, err
	}

	// Allocate the next per-key seq. The upsert takes the per-key row lock, so
	// concurrent producers on the SAME key serialize here and each gets a
	// distinct, strictly-increasing seq.
	key := d.Key.String()
	var seq int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO mesh_key_seq (routing_key, next_seq)
		   VALUES ($1, 1)
		 ON CONFLICT (routing_key)
		   DO UPDATE SET next_seq = mesh_key_seq.next_seq + 1
		 RETURNING next_seq`,
		key).Scan(&seq); err != nil {
		return 0, false, fmt.Errorf("delivery substrate: allocate seq for %s: %w", key, err)
	}

	// Insert the durable row. ON CONFLICT (event_id) DO NOTHING guards against
	// a racing duplicate produce that slipped past the read above; in that case
	// we re-read the winner's seq below.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO mesh_outbox
		   (routing_key, seq, event_id, topic, kind, payload, origin_node)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (event_id) DO NOTHING`,
		key, seq, d.EventID, d.Topic, int(d.Kind), payload, d.OriginNode)
	if err != nil {
		return 0, false, fmt.Errorf("delivery substrate: insert outbox row for %s: %w", key, err)
	}

	if n, _ := res.RowsAffected(); n == 0 {
		// A concurrent producer won the EventID race; the seq we allocated is
		// unused (a harmless gap -- the contract only requires strictly
		// increasing, gap-tolerant seq). Return the winner's seq.
		if err := tx.QueryRowContext(ctx,
			`SELECT seq FROM mesh_outbox WHERE event_id = $1`, d.EventID).Scan(&existing); err != nil {
			return 0, false, err
		}
		if cErr := tx.Commit(); cErr != nil {
			return 0, false, cErr
		}
		return existing, true, nil
	}

	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return seq, false, nil
}

// ReadAfter returns rows for key with seq > afterSeq in ascending order (the
// catch-up/replay read, ADR 4.4), up to limit rows.
func (s *pgOutboxStore) ReadAfter(ctx context.Context, key RoutingKey, afterSeq int64, limit int) ([]Deliverable, error) {
	db, err := s.db()
	if err != nil {
		return nil, err
	}

	query := `SELECT seq, event_id, topic, kind, payload, origin_node
		        FROM mesh_outbox
		       WHERE routing_key = $1 AND seq > $2
		       ORDER BY seq ASC`
	args := []any{key.String(), afterSeq}
	if limit > 0 {
		query += ` LIMIT $3`
		args = append(args, limit)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("delivery substrate: read after for %s: %w", key.String(), err)
	}
	defer rows.Close()

	var out []Deliverable
	for rows.Next() {
		var (
			seq        int64
			eventID    string
			topic      string
			kind       int
			payloadRaw []byte
			originNode string
		)
		if err := rows.Scan(&seq, &eventID, &topic, &kind, &payloadRaw, &originNode); err != nil {
			return nil, err
		}
		var payload map[string]any
		if len(payloadRaw) > 0 {
			if err := json.Unmarshal(payloadRaw, &payload); err != nil {
				return nil, fmt.Errorf("delivery substrate: unmarshal payload (seq %d): %w", seq, err)
			}
		}
		out = append(out, Deliverable{
			EventID:    eventID,
			Key:        key,
			Seq:        seq,
			Topic:      topic,
			Kind:       events.Kind(kind),
			Payload:    payload,
			OriginNode: originNode,
		})
	}
	return out, rows.Err()
}

// LoadCursor returns the persisted cursor seq for (key, consumerID), or 0 if
// none is stored (a brand-new consumer replays from the watermark, ADR 4.4).
func (s *pgOutboxStore) LoadCursor(ctx context.Context, key RoutingKey, consumerID string) (int64, error) {
	db, err := s.db()
	if err != nil {
		return 0, err
	}
	var seq int64
	err = db.QueryRowContext(ctx,
		`SELECT cursor_seq FROM mesh_cursor WHERE routing_key = $1 AND consumer_id = $2`,
		key.String(), consumerID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("delivery substrate: load cursor for %s/%s: %w", key.String(), consumerID, err)
	}
	return seq, nil
}

// SaveCursor advances the cursor for (key, consumerID) to seq, monotonically:
// the GREATEST() guard means a stale or out-of-order Ack never moves the cursor
// backwards (ADR 4.4).
func (s *pgOutboxStore) SaveCursor(ctx context.Context, key RoutingKey, consumerID string, seq int64) error {
	db, err := s.db()
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO mesh_cursor (routing_key, consumer_id, cursor_seq, updated_at)
		   VALUES ($1, $2, $3, now())
		 ON CONFLICT (routing_key, consumer_id)
		   DO UPDATE SET cursor_seq = GREATEST(mesh_cursor.cursor_seq, EXCLUDED.cursor_seq),
		                 updated_at = now()`,
		key.String(), consumerID, seq)
	if err != nil {
		return fmt.Errorf("delivery substrate: save cursor for %s/%s: %w", key.String(), consumerID, err)
	}
	return nil
}

// Ensure pgOutboxStore satisfies the seam.
var _ outboxStore = (*pgOutboxStore)(nil)

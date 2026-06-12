package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

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

// NewSubstrateWithStore builds a production DeliverySubstrate over the cluster
// Postgres outbox (delivery_store_pg.go), durable-only (no mesh fast-path).
// This is the wiring entry point used by app/cluster.go for the chat-reply
// migration (memql#1264): the durable outbox alone is the cross-replica
// delivery guarantee. dbGetter is resolved lazily so the substrate can be
// constructed before the database component is up. dedupTTL mirrors the mesh's
// eventDedup window (defaultDedupTTL) so a per-subscription cross-path dedup
// behaves consistently with the rest of the mesh.
func NewSubstrateWithStore(dbGetter func() *bun.DB, logger *slog.Logger) *Substrate {
	return NewSubstrate(newPGOutboxStore(dbGetter), defaultDedupTTL, nil, logger)
}

// NewSubstrateWithMeshFastPath builds a production DeliverySubstrate over the
// cluster Postgres outbox WITH the mesh fast-path wired (memql#1289). The durable
// outbox is still the delivery guarantee; fastPath (the EventBridge) is the
// best-effort low-latency hint surface that wakes a cross-node subscriber
// instantly instead of waiting for the durable-poll floor. The hint carries the
// same EventID as the durable row, so the per-subscription dedup collapses the
// two paths to exactly one delivery and the cursor advances on the durable path
// only (ADR 4.5). This is the wiring entry point app/cluster.go uses on every
// mesh node; the matching SetFastPathSink(substrate.HandleFastPath) on the same
// EventBridge feeds inbound hints back into this node's subscriptions.
func NewSubstrateWithMeshFastPath(dbGetter func() *bun.DB, fastPath meshFastPath, logger *slog.Logger) *Substrate {
	return NewSubstrate(newPGOutboxStore(dbGetter), defaultDedupTTL, fastPath, logger)
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
		key, seq, d.EventID, d.Topic, int(d.Kind), string(payload), d.OriginNode)
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

// SweepOlderThan deletes mesh_outbox rows whose created_at is older than
// cutoff, and mesh_cursor rows whose updated_at is older than cutoff (dead
// per-pod consumer ids from rollouts otherwise accumulate forever). It returns
// the per-table delete counts. This is the retention sweep documented as
// intent in the 20260610000000_mesh_delivery_substrate migration header
// (memql#1328); chat-reply delivery is a live-stream concern and the graph
// rows remain the source of truth, so age-based deletion is safe. Deletes are
// idempotent, so concurrent sweeps across replicas are harmless (the same
// multi-node posture as the automation_execution_claims prune, memql#561).
//
// mesh_key_seq is DELIBERATELY NOT swept (correctness beats completeness --
// the rows are tiny). Deleting a key's allocator row would reset its next_seq
// to 1 on the key's next produce, breaking the per-key strictly-increasing seq
// invariant the replay contract leans on: any consumer still holding a
// position from the key's earlier life (an in-memory tail position, or a
// cursor recreated before the restart) would silently skip every restarted
// seq <= that position. There is also a narrower producer race (Append's
// allocator upsert vs. the sweep's delete). Both are eliminated by simply
// keeping the allocator rows.
func (s *pgOutboxStore) SweepOlderThan(ctx context.Context, cutoff time.Time) (outboxDeleted, cursorsDeleted int64, err error) {
	db, err := s.db()
	if err != nil {
		return 0, 0, err
	}

	res, err := db.ExecContext(ctx,
		`DELETE FROM mesh_outbox WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("delivery substrate: sweep mesh_outbox: %w", err)
	}
	outboxDeleted, _ = res.RowsAffected()

	res, err = db.ExecContext(ctx,
		`DELETE FROM mesh_cursor WHERE updated_at < $1`, cutoff)
	if err != nil {
		return outboxDeleted, 0, fmt.Errorf("delivery substrate: sweep mesh_cursor: %w", err)
	}
	cursorsDeleted, _ = res.RowsAffected()

	return outboxDeleted, cursorsDeleted, nil
}

// EnsureCursor resolves the starting cursor for (key, consumerID) (ADR 4.4,
// amended by memql#1328). An existing cursor row is returned as-is (the
// consumer resumes exactly). When no row exists and fromStart is true, it
// returns 0 without creating a row -- the opt-in backlog replay; the row
// appears on the first Ack, exactly the pre-#1328 shape. Otherwise it
// atomically initializes AND persists the cursor at the key's current high
// watermark, so a brand-new consumer receives only deliverables produced
// after it first subscribed.
//
// The watermark is mesh_key_seq.next_seq (the seq allocator's committed
// value), not max(mesh_outbox.seq): the allocator row is never retention-swept
// (see SweepOlderThan), so the watermark stays correct even when the outbox
// tip rows have been pruned. Race-safety against concurrent producers: Append
// bumps the allocator and inserts the outbox row in ONE transaction, and the
// allocator row lock serializes per-key appends, so per-key commit order
// equals seq order and this read sees only committed appends. Any append that
// commits after the snapshot therefore has seq > watermark and is returned by
// the subscriber's ReadAfter; an append committed before it is skipped by
// design (it predates the consumer). A key that has never produced has no
// allocator row; COALESCE pins the cursor at 0, which is equivalent.
func (s *pgOutboxStore) EnsureCursor(ctx context.Context, key RoutingKey, consumerID string, fromStart bool) (int64, bool, error) {
	db, err := s.db()
	if err != nil {
		return 0, false, err
	}
	k := key.String()

	var seq int64
	err = db.QueryRowContext(ctx,
		`SELECT cursor_seq FROM mesh_cursor WHERE routing_key = $1 AND consumer_id = $2`,
		k, consumerID).Scan(&seq)
	switch {
	case err == nil:
		return seq, false, nil // existing consumer: resume exactly (ADR 4.4)
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, fmt.Errorf("delivery substrate: load cursor for %s/%s: %w", k, consumerID, err)
	}

	if fromStart {
		return 0, false, nil
	}

	// High-watermark init. ON CONFLICT DO NOTHING guards a concurrent
	// subscriber initializing the same (key, consumer); the loser falls
	// through and resumes from the winner's row.
	err = db.QueryRowContext(ctx,
		`INSERT INTO mesh_cursor (routing_key, consumer_id, cursor_seq, updated_at)
		   VALUES ($1, $2,
		           COALESCE((SELECT next_seq FROM mesh_key_seq WHERE routing_key = $1), 0),
		           now())
		 ON CONFLICT (routing_key, consumer_id) DO NOTHING
		 RETURNING cursor_seq`,
		k, consumerID).Scan(&seq)
	switch {
	case err == nil:
		return seq, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, fmt.Errorf("delivery substrate: init cursor at high watermark for %s/%s: %w", k, consumerID, err)
	}

	// Lost the init race (or the row appeared between the SELECT and the
	// INSERT): read the winner's position and resume from it.
	if err := db.QueryRowContext(ctx,
		`SELECT cursor_seq FROM mesh_cursor WHERE routing_key = $1 AND consumer_id = $2`,
		k, consumerID).Scan(&seq); err != nil {
		return 0, false, fmt.Errorf("delivery substrate: reread cursor for %s/%s: %w", k, consumerID, err)
	}
	return seq, false, nil
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

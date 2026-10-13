package memoryNodes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/component"
)

const schemaCompactionBatch = 500
const schemaCompactionInterval = 5 * time.Second

// SchemaCompactor converts old inline schemas after startup. Each transaction
// owns the shared cursor row, so a second replica skips the work and a crash
// rolls back both the batch and its cursor. Serving and liveness never wait on
// this maintenance loop; every batch has its own five-second deadline.
type SchemaCompactor struct {
	*component.Component
	provider func() *bun.DB
	nodeID   func() string
	logger   *slog.Logger
}

func NewSchemaCompactor(provider func() *bun.DB, nodeID func() string, logger *slog.Logger) *SchemaCompactor {
	comp, _ := component.New(common.ComponentName("schemaCompactor"))
	if logger == nil {
		logger = slog.Default()
	}
	c := &SchemaCompactor{Component: comp, provider: provider, nodeID: nodeID, logger: logger}
	_ = c.ConfigureLifecycle(component.WithRunHook(c.run))
	return c
}

func (*SchemaCompactor) Order() int { return 50 }

func (c *SchemaCompactor) run(ctx context.Context, markStarted func()) error {
	markStarted()
	ticker := time.NewTicker(schemaCompactionInterval)
	defer ticker.Stop()
	var retryAfter time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if now.Before(retryAfter) {
				continue
			}
			db := c.provider()
			if db == nil {
				continue
			}
			batchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			enabled, err := EnableSchemaReferencesWhenReady(batchCtx, db, c.nodeID())
			if err == nil && enabled {
				_, err = CompactSchemaBatch(batchCtx, db)
			}
			cancel()
			if err != nil && ctx.Err() == nil {
				c.logger.Warn("row schema conversion paused; retrying in one minute", "error", err)
				retryAfter = now.Add(time.Minute)
			}
		}
	}
}

// CompactSchemaBatch preserves payload, provenance and version timestamps.
// The primary-key walk bounds each read even when all earlier rows were
// already converted. Inline rows inserted after the walk starts are normalized
// by the trigger, including backdated restores behind the cursor.
func CompactSchemaBatch(ctx context.Context, db *bun.DB) (int, error) {
	converted := 0
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var enabled bool
		if err := tx.QueryRowContext(ctx, `SELECT references_enabled FROM memory_schema_storage WHERE singleton`).Scan(&enabled); err != nil {
			return err
		}
		if !enabled {
			return nil
		}
		var table string
		var cursorID sql.NullString
		var cursorAt sql.NullTime
		err := tx.QueryRowContext(ctx, `SELECT table_name,cursor_id,cursor_at FROM memory_schema_compaction WHERE NOT completed AND next_at<=clock_timestamp() ORDER BY next_at,table_name LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&table, &cursorID, &cursorAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if table != MemoryNodesTable && table != SecretMemoryNodesTable {
			return fmt.Errorf("unknown schema conversion table %q", table)
		}
		type rowKey struct {
			ID string    `json:"id"`
			At time.Time `json:"at"`
		}
		var rows []struct {
			ID     string
			At     time.Time
			Schema json.RawMessage
		}
		q := tx.NewSelect().Table(table).Column("id", "schema").ColumnExpr(`"createdAt" AS at`).
			OrderExpr(`id, "createdAt"`).Limit(schemaCompactionBatch)
		if cursorID.Valid && cursorAt.Valid {
			q = q.Where(`(id,"createdAt") > (?,?)`, cursorID.String, cursorAt.Time)
		}
		if err := q.Scan(ctx, &rows); err != nil {
			return err
		}
		var keys []rowKey
		for _, row := range rows {
			var doc struct {
				Ref string `json:"$ref"`
			}
			if err := json.Unmarshal(row.Schema, &doc); err != nil {
				return err
			}
			if !strings.HasPrefix(doc.Ref, schemaReferencePrefix) {
				keys = append(keys, rowKey{row.ID, row.At})
			}
		}
		if len(keys) > 0 {
			raw, err := json.Marshal(keys)
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE ? AS n SET schema=n.schema FROM jsonb_to_recordset(?::jsonb) AS k(id text,at timestamptz) WHERE n.id=k.id AND n."createdAt"=k.at`, bun.Ident(table), string(raw))
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			converted = int(n) // bounded by schemaCompactionBatch
		}
		if len(rows) != 0 {
			last := rows[len(rows)-1]
			cursorID, cursorAt = sql.NullString{String: last.ID, Valid: true}, sql.NullTime{Time: last.At, Valid: true}
		}
		_, err = tx.ExecContext(ctx, `UPDATE memory_schema_compaction SET cursor_id=?,cursor_at=?,completed=?,next_at=clock_timestamp()+interval '5 seconds' WHERE table_name=?`, cursorID, cursorAt, len(rows) < schemaCompactionBatch, table)
		return err
	})
	return converted, err
}

// EnableSchemaReferencesWhenReady waits for every non-retired engine node to
// advertise support. It deliberately does not infer support from a stale
// heartbeat: an old process could still serve reads. The topology's normal
// stopped transitions retire old pod identities. Offline alone is insufficient:
// a disconnected process could still serve reads. Once enabled, a
// downgrade requires the documented full-schema restore.
func EnableSchemaReferencesWhenReady(ctx context.Context, db *bun.DB, nodeID string) (bool, error) {
	var enabled bool
	if err := db.QueryRowContext(ctx, `SELECT references_enabled FROM memory_schema_storage WHERE singleton`).Scan(&enabled); err != nil || enabled {
		return enabled, err
	}
	if strings.TrimSpace(nodeID) == "" {
		return false, nil
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO memory_schema_readers (node_id,seen_at) VALUES (?,clock_timestamp()) ON CONFLICT (node_id) DO UPDATE SET seen_at=excluded.seen_at`, nodeID); err != nil {
		return false, err
	}
	// staged-data: MUST-NOT-GATE -- storage rollout must include all engine
	// readers, irrespective of authored concept publication or caller identity.
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// One topology scan per cluster per interval, not one per replica.
		var singleton bool
		err := tx.QueryRowContext(ctx, `SELECT singleton FROM memory_schema_storage WHERE singleton AND NOT references_enabled AND next_check_at<=clock_timestamp() FOR UPDATE SKIP LOCKED`).Scan(&singleton)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_schema_storage SET next_check_at=clock_timestamp()+interval '5 seconds' WHERE singleton`); err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `WITH latest AS MATERIALIZED (
		SELECT DISTINCT ON (id) id,payload FROM "MemoryNodes"
		WHERE concept='v1:cluster:node' ORDER BY id,"createdAt" DESC
	), active AS (
		SELECT regexp_replace(id,'^v1:cluster:node:','') AS id FROM latest
		WHERE COALESCE(payload->>'health','') <> 'stopped'
	)
	UPDATE memory_schema_storage SET references_enabled=true
	WHERE singleton AND NOT references_enabled
	AND EXISTS(SELECT 1 FROM active WHERE id=?) AND NOT EXISTS(
		SELECT 1 FROM active a WHERE NOT EXISTS(
			SELECT 1 FROM memory_schema_readers r WHERE r.node_id=a.id AND r.seen_at>clock_timestamp()-interval '30 seconds'
		)
	) RETURNING references_enabled`, nodeID).Scan(&enabled)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return enabled, err
}

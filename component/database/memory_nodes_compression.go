package database

// memory_nodes_compression.go -- the compression "MemoryNodes" was meant to
// have and has never had (memql#5421).
//
// # What was intended
//
// 20260609000000_document_version_history.up.sql enables a LONG-WINDOW
// compression policy on "MemoryNodes": segment by concept, order by "createdAt"
// DESC, compress a chunk once it is 90 days cold. Append-only version history
// is the canonical compressible workload, and its cold tail is read rarely.
//
// # Why no cluster has it -- three reasons, any one enough
//
// All measured against TimescaleDB 2.29.0 with the extension in `public`:
//
//   - A FRESH INSTALL runs the whole migration set before "MemoryNodes" is a
//     hypertable. 20260324000000_initial_setup converts it only when
//     create_hypertable lives in a schema named `timescaledb`, and CREATE
//     EXTENSION installs into `public`; timescaleExtensionPostHook converts it
//     afterwards. 20260609 skips a table that is not yet a hypertable.
//   - A TABLE THAT ALREADY IS ONE fails the block anyway.
//     add_compression_policy('MemoryNodes', ...) casts an unquoted name to
//     regclass, which folds it to `memorynodes` -- "relation does not exist" --
//     and the block's EXCEPTION WHEN OTHERS rolls back the ALTER TABLE before
//     it, leaving one NOTICE. So an install from before 20260609 did not get it
//     either.
//   - SINCE 20260926020000_work_run_heads THE TABLE CANNOT HAVE IT AT ALL.
//     That migration captures deletes with a statement-level trigger carrying
//     a transition table (work_run_head_delete, REFERENCING OLD TABLE), and
//     TimescaleDB refuses compression on a hypertable with a DELETE trigger of
//     that kind -- "DELETE triggers with transition tables not supported" -- in
//     both directions: compression cannot be enabled while the trigger exists,
//     and the trigger cannot be created on a table with compression. INSERT and
//     UPDATE transition triggers are accepted, and a ROW-level DELETE trigger
//     is accepted and fires for rows deleted out of a compressed chunk.
//
// An applied migration never runs again, and a new one would run while a fresh
// install's table is still plain. So the settings are applied where the
// conversion happens: in the post-migration hook, after it, on every boot. While
// the trigger conflict stands, every boot REPORTS it -- on the status node and
// in the log, naming the trigger -- and changes nothing; the boot after it is
// resolved compresses every cluster, fresh or old, with no further change here.
//
// # What it does, and what it leaves alone
//
//   - It acts only when "MemoryNodes" IS a hypertable, has NO compression
//     settings, and carries no DELETE trigger with a transition table. A table
//     with settings is left exactly as it is, whoever set them: an operator's
//     own segmentby survives, and so does a decision to remove the POLICY,
//     which is the lever for stopping automatic compression (removing the
//     settings would have this hook put them back).
//   - The settings and the policy commit TOGETHER, so a boot that dies between
//     them cannot leave settings with no policy -- a state the rule above would
//     never repair.
//   - It waits for its lock at most memoryNodesCompressionLockTimeout. Enabling
//     compression takes a lock that conflicts with an ordinary read (measured:
//     it waits behind an open SELECT), and a DDL waiting in the lock queue holds
//     every later query on the table behind it. A boot that cannot get the lock
//     quickly gives up, says so, and the next boot tries again.
//   - The blocking trigger is looked for BEFORE anything is attempted.
//     TimescaleDB would refuse the ALTER anyway (measured: at once, even behind
//     an open read), but with an error that reads like a transient failure the
//     next boot might get past; the answer here names the trigger instead.
//   - "SecretMemoryNodes" gets nothing. No migration intends compression for
//     it, and a post-migration hook is not the place to invent a storage policy
//     for the table that holds secrets.
//
// # What changes on a running cluster, once it can
//
// The policy job runs as soon as the scheduler sees it -- within seconds, not
// at its 12-hour interval (measured) -- and compresses every chunk more than 90
// days cold, so on a cluster with history that first boot starts a background
// job over the whole cold tail. Reads keep their meaning (a compressed chunk is
// decompressed transparently) and are slower on that tail, which is the trade
// the migration chose. DML into a compressed chunk decompresses per segment,
// and TimescaleDB caps that per transaction; migrationConnParams lifts the cap
// for the migration runner, because a concept-scoped repair migration is
// exactly that shape. The runtime paths that delete or rewrite old rows keep
// it, and a single statement of theirs over a large compressed set would be
// refused -- the operator doc names them. The operator's half -- how to check
// an instance, and how to stop it -- is docs/public/operate/database-platform.md,
// "MemoryNodes compression".

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/uptrace/bun"
)

// The settings 20260609000000_document_version_history.up.sql states, verbatim.
const (
	memoryNodesCompressSegmentBy = "concept"
	memoryNodesCompressOrderBy   = `"createdAt" DESC`
	memoryNodesCompressAfter     = "90 days"
)

// memoryNodesCompressionLockTimeout bounds how long enabling compression may
// wait for the table lock, and therefore how long it can hold later queries
// behind it in the lock queue.
const memoryNodesCompressionLockTimeout = "2s"

// memoryNodesCompressionAdvisoryLockKey serialises two nodes enabling it at
// once, so the second re-reads the settings the first committed rather than
// altering the table again. ASCII "mncomprs"; only its stability across nodes
// matters. Distinct from timescaleExtensionAdvisoryLockKey and from the
// latest-row index keys (latestRowIndexLockKey hashes a table name).
const memoryNodesCompressionAdvisoryLockKey int64 = 0x6d6e636f6d707273

// What one boot made of it, as the status node records it.
const (
	compressionEnabledThisBoot = "enabled"
	compressionAlreadyEnabled  = "already enabled"
	compressionNotAHypertable  = "skipped: not a hypertable"
	compressionNoTimescaleDB   = "skipped: TimescaleDB is not installed"
	// compressionBlockedPrefix is followed by the blocking triggers' names.
	compressionBlockedPrefix    = "blocked: DELETE trigger with a transition table: "
	compressionFailedOutcomePfx = "failed: "
)

// hypertableCompression reports whether table is a hypertable and whether it
// carries compression settings.
//
// The table is matched by the relation its quoted name RESOLVES to on this
// connection -- the one every query means -- and never by bare name:
// timescaledb_information lists every schema, and a same-named hypertable
// somewhere else must not stand in for this one.
func hypertableCompression(ctx context.Context, q bun.IConn, table string) (hypertable, enabled bool, err error) {
	err = q.QueryRowContext(ctx, `
		SELECT h.compression_enabled
		  FROM timescaledb_information.hypertables AS h
		  JOIN pg_class AS c ON c.oid = to_regclass(?)
		  JOIN pg_namespace AS n ON n.oid = c.relnamespace
		 WHERE h.hypertable_schema = n.nspname
		   AND h.hypertable_name = c.relname`, quoteIdentifier(table)).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, enabled, nil
}

// deleteTransitionTriggers names the triggers on table that TimescaleDB will not
// compress beside: AFTER DELETE with a transition table (pg_trigger's tgtype
// DELETE bit is 8; tgoldtable names the OLD TABLE relation). Sorted.
func deleteTransitionTriggers(ctx context.Context, q bun.IConn, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT tgname FROM pg_trigger
		 WHERE tgrelid = to_regclass(?) AND NOT tgisinternal
		   AND (tgtype & 8) <> 0 AND tgoldtable IS NOT NULL
		 ORDER BY tgname`, quoteIdentifier(table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// ensureMemoryNodesCompression applies the intended compression to
// "MemoryNodes" when it is a hypertable without any and nothing forbids it, and
// reports what it did. It never fails the boot: compression is an optimisation,
// and a refusal here is logged, recorded on the status node, and retried by the
// next boot.
func ensureMemoryNodesCompression(ctx context.Context, db *bun.DB, logger *slog.Logger) string {
	outcome, jobID, err := enableMemoryNodesCompression(ctx, db)
	if logger != nil {
		switch {
		case err != nil:
			logger.Warn("MemoryNodes compression not enabled on this boot; the next boot tries again",
				"table", memoryNodesTableName, "error", err)
		case strings.HasPrefix(outcome, compressionBlockedPrefix):
			logger.Warn("MemoryNodes compression is blocked: TimescaleDB refuses compression on a hypertable "+
				"carrying a DELETE trigger with a transition table, so the 90-day policy the migrations intend "+
				"cannot be enabled; nothing was changed (memql#5421)",
				"table", memoryNodesTableName, "triggers", strings.TrimPrefix(outcome, compressionBlockedPrefix))
		case outcome == compressionEnabledThisBoot:
			logger.Info("MemoryNodes compression enabled",
				"table", memoryNodesTableName,
				"segmentby", memoryNodesCompressSegmentBy,
				"orderby", memoryNodesCompressOrderBy,
				"compressAfter", memoryNodesCompressAfter,
				"policyJob", jobID)
		}
	}
	if err != nil {
		return compressionFailedOutcomePfx + err.Error()
	}
	return outcome
}

// memoryNodesCompressionState answers every question that decides whether to
// act, in the order that makes each answer meaningful. An empty outcome means
// "enable it".
func memoryNodesCompressionState(ctx context.Context, q bun.IConn) (string, error) {
	hypertable, enabled, err := hypertableCompression(ctx, q, memoryNodesTableName)
	if err != nil {
		return "", fmt.Errorf("read the hypertable catalog: %w", err)
	}
	switch {
	case !hypertable:
		return compressionNotAHypertable, nil
	case enabled:
		return compressionAlreadyEnabled, nil
	}
	blocking, err := deleteTransitionTriggers(ctx, q, memoryNodesTableName)
	if err != nil {
		return "", fmt.Errorf("read the table's triggers: %w", err)
	}
	if len(blocking) > 0 {
		return compressionBlockedPrefix + strings.Join(blocking, ", "), nil
	}
	return "", nil
}

func enableMemoryNodesCompression(ctx context.Context, db *bun.DB) (outcome string, jobID int64, err error) {
	if db == nil {
		return "", 0, errors.New("no database handle")
	}
	var installed bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`).Scan(&installed); err != nil {
		return "", 0, fmt.Errorf("check for the timescaledb extension: %w", err)
	}
	if !installed {
		return compressionNoTimescaleDB, 0, nil
	}
	if outcome, err := memoryNodesCompressionState(ctx, db); err != nil || outcome != "" {
		return outcome, 0, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit succeeds
	if _, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout = '"+memoryNodesCompressionLockTimeout+"'"); err != nil {
		return "", 0, err
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", memoryNodesCompressionAdvisoryLockKey); err != nil {
		return "", 0, fmt.Errorf("serialise with another node: %w", err)
	}
	// Again, under the lock: another node may have committed it since.
	if outcome, err := memoryNodesCompressionState(ctx, tx); err != nil || outcome != "" {
		return outcome, 0, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		"ALTER TABLE %s SET (timescaledb.compress, timescaledb.compress_segmentby = '%s', timescaledb.compress_orderby = '%s')",
		quoteIdentifier(memoryNodesTableName), memoryNodesCompressSegmentBy, memoryNodesCompressOrderBy)); err != nil {
		return "", 0, fmt.Errorf("enable compression: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT add_compression_policy(?::regclass, INTERVAL '`+memoryNodesCompressAfter+`', if_not_exists => TRUE)`,
		quoteIdentifier(memoryNodesTableName)).Scan(&jobID); err != nil {
		return "", 0, fmt.Errorf("add the compression policy: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	return compressionEnabledThisBoot, jobID, nil
}

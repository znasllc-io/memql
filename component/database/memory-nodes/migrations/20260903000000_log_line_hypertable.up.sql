-- The log store: log_line hypertable (epic memql#4893, design record
-- docs/superpowers/specs/2026-09-03-logs-design.md section A).
--
-- Every node's log lines, persisted beside the observability rows by
-- component/logstore.Sink and read back by the v1:observability:logLine
-- builtins. A dedicated table, never a graph row: the loudest table in the
-- system does not belong in MemoryNodes.
--
-- NO TIMESCALE RETENTION POLICY, on purpose. Retention is owned by the
-- nightly logsRetentionSweep automation (builtin logsSweep), because the
-- archive must come first -- each expired day is uploaded per node type as
-- logs/<day>/<nodeType>.ndjson.gz and only then deleted -- and a policy
-- cannot be told to wait for an upload. Do not add add_retention_policy here.
--
-- Cross-references:
--   * Concept surface:  dsl/observability/concepts.memql (logLine)
--   * Builtins:         dsl/observability/builtins.memql
--   * Writer + reader:  component/logstore/

-- PK is (occurred_at, id): the time column first so chunk exclusion does its
-- job, the short id second so a keyset cursor (occurred_at, id) is unique.
CREATE TABLE IF NOT EXISTS "log_line" (
    occurred_at      TIMESTAMPTZ NOT NULL,
    id               TEXT NOT NULL,
    node_type        TEXT NOT NULL,
    node             TEXT NOT NULL DEFAULT '',
    level            TEXT NOT NULL,
    component        TEXT NOT NULL,
    app              TEXT NOT NULL DEFAULT '',
    message          TEXT NOT NULL,
    attributes       JSONB,
    subject          TEXT NOT NULL DEFAULT '',
    subject_concept  TEXT NOT NULL DEFAULT '',
    session          TEXT NOT NULL DEFAULT '',
    user_id          TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (occurred_at, id)
);

--bun:split

-- Promote to a hypertable with one-day chunks where TimescaleDB is present;
-- a plain Postgres box still migrates and keeps a plain table.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        PERFORM create_hypertable(
            'log_line',
            'occurred_at',
            chunk_time_interval => INTERVAL '1 day',
            if_not_exists       => TRUE
        );
    ELSE
        RAISE NOTICE 'timescaledb not installed -- log_line stays a plain table';
    END IF;
END
$$;

--bun:split

-- Compress everything older than a day, segmented by (node_type, component)
-- so the common read -- one component's lines inside a window -- decompresses
-- one segment rather than scanning across them.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        ALTER TABLE "log_line"
            SET (
                timescaledb.compress,
                timescaledb.compress_segmentby = 'node_type, component',
                timescaledb.compress_orderby   = 'occurred_at DESC'
            );
        PERFORM add_compression_policy('log_line', INTERVAL '1 day', if_not_exists => TRUE);
    END IF;
END
$$;

--bun:split

-- The facets. Partial where the empty value is the common case, so the
-- index holds only the rows a facet can select.
CREATE INDEX IF NOT EXISTS log_line_subject_idx
    ON "log_line" (subject, occurred_at DESC)
    WHERE subject <> '';

--bun:split

CREATE INDEX IF NOT EXISTS log_line_component_idx
    ON "log_line" (component, occurred_at DESC);

--bun:split

CREATE INDEX IF NOT EXISTS log_line_app_idx
    ON "log_line" (app, occurred_at DESC)
    WHERE app <> '';

--bun:split

CREATE INDEX IF NOT EXISTS log_line_level_idx
    ON "log_line" (level, occurred_at DESC)
    WHERE level IN ('warn', 'error');

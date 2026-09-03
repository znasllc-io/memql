-- The edge's request log, and the traffic aggregate folded from it
-- (epic memql#4906, the Run epic of the Deployables program).
--
-- The edge emitted no per-site metrics at all, so "is anybody using this
-- deployable, and is it healthy" had no answer to surface. This is the
-- measurement: component/edge records one row per served request, and
-- TimescaleDB folds those rows into the per-minute and per-hour figures the
-- Live stop reads. One write, folded by the database -- so the raw log and
-- the figure cannot disagree, which is the whole reason the aggregate is a
-- continuous aggregate rather than a counter somebody increments.
--
-- WHY A DEDICATED TABLE RATHER THAN THE LOG STORE. The program record names
-- the log store as the request log's home. Three facts moved it here, all
-- recorded in docs/superpowers/specs/2026-09-03-deployables-run-design.md:
-- the store's writer is rate-limited and drops on pressure, so a figure
-- folded from it would under-count silently and read as a healthy dip; a
-- continuous aggregate needs typed columns and the store carries its
-- attributes as jsonb; and the store is a sibling epic's table, so an
-- aggregate over it would refuse boot wherever that epic has not landed.
--
-- Cross-references:
--   * The writer:     component/edge/requestlog.go + component/sitetraffic
--   * The read:       dsl/platform/builtins.memql (siteTrafficInWindow)
--   * The row shape:  dsl/observability/concepts.memql (siteTraffic)
--   * The precedent:  20260515000000_observability_hypertable.up.sql

-- Raw per-request rows. PK is (served_at, id) -- the time column first so
-- TimescaleDB's chunk exclusion does its job, and a minted short id second
-- because two requests to one site can land in the same microsecond and a
-- (served_at, site_id) key would silently drop one of them.
CREATE TABLE IF NOT EXISTS "edge_request" (
    served_at    TIMESTAMPTZ NOT NULL,
    id           TEXT NOT NULL,
    site_id      TEXT NOT NULL,
    node         TEXT NOT NULL DEFAULT '',
    status       INTEGER NOT NULL,
    path_class   TEXT NOT NULL,
    bytes        BIGINT NOT NULL DEFAULT 0,
    duration_ns  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (served_at, id)
);

--bun:split

-- Promote to a hypertable. 1-day chunks, matching the compression policy
-- below and the code_invocation precedent.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        PERFORM create_hypertable(
            'edge_request',
            'served_at',
            chunk_time_interval => INTERVAL '1 day',
            if_not_exists       => TRUE
        );
    ELSE
        RAISE NOTICE 'timescaledb not installed -- edge_request stays a plain table';
    END IF;
END
$$;

--bun:split

-- The one index the raw log needs. Every read of the raw rows is "this
-- deployable, over this window" -- the same question the aggregate answers,
-- asked of the forensic detail behind a figure.
CREATE INDEX IF NOT EXISTS edge_request_site_served_idx
    ON "edge_request" (site_id, served_at DESC);

--bun:split

-- Compress after a day, segmented by site so the common access pattern reads
-- a contiguous segment rather than scanning across them.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        ALTER TABLE "edge_request"
            SET (
                timescaledb.compress,
                timescaledb.compress_segmentby = 'site_id',
                timescaledb.compress_orderby   = 'served_at DESC'
            );
        PERFORM add_compression_policy('edge_request', INTERVAL '1 day', if_not_exists => TRUE);
    END IF;
END
$$;

--bun:split

-- Retention: thirty days by default on the raw rows AND on both aggregates,
-- which is the log store's window and the one the program's Run epic scopes
-- itself to. component/sitetraffic re-applies these from
-- MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS at boot, so an operator changes the
-- window by changing the variable rather than by writing SQL.
--
-- THE AGGREGATES ARE DROPPED ON THE SAME SCHEDULE AS THE RAW ROWS, which is
-- the opposite of what code_invocation does, and deliberately: there the
-- aggregate is a trendline worth keeping after the forensic detail is gone,
-- while here the aggregate IS the product and a figure with no rows behind it
-- is a figure nobody can check. Keeping them in step means "unmeasured" means
-- the same thing at every horizon.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        PERFORM add_retention_policy('edge_request', INTERVAL '30 days', if_not_exists => TRUE);
    END IF;
END
$$;

--bun:split

-- The per-minute fold.
--
-- THE RELATION EXISTS EITHER WAY. On TimescaleDB it is a continuous
-- aggregate with a refresh policy; on a plain Postgres box it is an ordinary
-- view over the same expression. The reader therefore has ONE query rather
-- than a branch, and a cluster without the extension answers honestly (from
-- the raw rows, live) instead of erroring on a missing relation -- which the
-- reader would have had to translate into "unmeasured", the one answer that
-- must mean "nothing measured this" and nothing else.
--
-- `error_count` is 5xx and `client_error_count` is 4xx, counted apart because
-- they are different situations: a 500 is the deployable failing and a 404 is
-- somebody asking for a page it does not have. Folding them together would
-- make a healthy site with a broken inbound link look unhealthy.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        EXECUTE $cq$
            CREATE MATERIALIZED VIEW IF NOT EXISTS edge_request_1m
            WITH (timescaledb.continuous) AS
            SELECT
                site_id,
                time_bucket(INTERVAL '1 minute', served_at) AS window_start,
                COUNT(*)                                     AS request_count,
                COUNT(*) FILTER (WHERE status >= 500)        AS error_count,
                COUNT(*) FILTER (WHERE status >= 400 AND status < 500) AS client_error_count,
                SUM(bytes)                                   AS bytes_total,
                MAX(served_at)                               AS last_served_at
            FROM edge_request
            GROUP BY site_id, window_start
            WITH NO DATA;
        $cq$;
        PERFORM add_continuous_aggregate_policy(
            'edge_request_1m',
            start_offset      => INTERVAL '1 hour',
            end_offset        => INTERVAL '1 minute',
            schedule_interval => INTERVAL '1 minute',
            if_not_exists     => TRUE
        );
        PERFORM add_retention_policy('edge_request_1m', INTERVAL '30 days', if_not_exists => TRUE);
    ELSE
        EXECUTE $vq$
            CREATE OR REPLACE VIEW edge_request_1m AS
            SELECT
                site_id,
                date_trunc('minute', served_at) AS window_start,
                COUNT(*)                        AS request_count,
                COUNT(*) FILTER (WHERE status >= 500) AS error_count,
                COUNT(*) FILTER (WHERE status >= 400 AND status < 500) AS client_error_count,
                SUM(bytes)                      AS bytes_total,
                MAX(served_at)                  AS last_served_at
            FROM edge_request
            GROUP BY site_id, date_trunc('minute', served_at);
        $vq$;
    END IF;
END
$$;

--bun:split

-- The per-hour fold: the same shape over a longer horizon, which is what a
-- day-long or week-long window on the Live stop reads. A window picker that
-- asked for a week of minute buckets would ask for ten thousand rows to draw
-- one line.
--
-- end_offset is one minute for the minute view and five for this one: a
-- bucket is only materialized once it can no longer change, and a partial
-- bucket that churned would make the newest figure move under somebody
-- reading it. `materialized_only` is left at its default (false), so the
-- most recent, not-yet-materialized bucket is answered live from the raw
-- rows -- which is what makes "last served at" say seconds-ago rather than
-- minutes-ago.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        EXECUTE $cq$
            CREATE MATERIALIZED VIEW IF NOT EXISTS edge_request_1h
            WITH (timescaledb.continuous) AS
            SELECT
                site_id,
                time_bucket(INTERVAL '1 hour', served_at) AS window_start,
                COUNT(*)                                   AS request_count,
                COUNT(*) FILTER (WHERE status >= 500)      AS error_count,
                COUNT(*) FILTER (WHERE status >= 400 AND status < 500) AS client_error_count,
                SUM(bytes)                                 AS bytes_total,
                MAX(served_at)                             AS last_served_at
            FROM edge_request
            GROUP BY site_id, window_start
            WITH NO DATA;
        $cq$;
        PERFORM add_continuous_aggregate_policy(
            'edge_request_1h',
            start_offset      => INTERVAL '1 day',
            end_offset        => INTERVAL '5 minutes',
            schedule_interval => INTERVAL '5 minutes',
            if_not_exists     => TRUE
        );
        PERFORM add_retention_policy('edge_request_1h', INTERVAL '30 days', if_not_exists => TRUE);
    ELSE
        EXECUTE $vq$
            CREATE OR REPLACE VIEW edge_request_1h AS
            SELECT
                site_id,
                date_trunc('hour', served_at) AS window_start,
                COUNT(*)                      AS request_count,
                COUNT(*) FILTER (WHERE status >= 500) AS error_count,
                COUNT(*) FILTER (WHERE status >= 400 AND status < 500) AS client_error_count,
                SUM(bytes)                    AS bytes_total,
                MAX(served_at)                AS last_served_at
            FROM edge_request
            GROUP BY site_id, date_trunc('hour', served_at);
        $vq$;
    END IF;
END
$$;

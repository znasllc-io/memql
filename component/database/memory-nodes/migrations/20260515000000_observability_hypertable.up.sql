-- Observability: code_invocation hypertable + continuous aggregates.
--
-- The runtime (component/observe) emits one row per captured Method
-- or Func invocation. TimescaleDB compresses the chunks after a day
-- and the per-FQN retention policy (driven by codeProfile.retentionDays
-- via the codeProfileEnforcement automation -- forthcoming) drops raw
-- rows on schedule. Continuous aggregates persist past retention so
-- the trendline (callCount, p50/p95, errorRate) stays visible after
-- the forensic detail is gone.
--
-- Cross-references:
--   * Concept surface:  dsl/observability/concepts.memql
--   * Runtime emitter:  component/observe/
--   * Static map:       component/architecture/embedded/topology.model.json

-- Raw per-invocation hypertable. PK is intentionally (occurred_at,
-- code_reference) -- the time column first so TimescaleDB's chunk
-- exclusion does its job, code_reference second so per-FQN queries
-- hit a btree leaf rather than a scan.
CREATE TABLE IF NOT EXISTS "code_invocation" (
    occurred_at     TIMESTAMPTZ NOT NULL,
    code_reference  TEXT NOT NULL,
    duration_ns     BIGINT NOT NULL,
    level           TEXT NOT NULL,
    error_message   TEXT,
    args            JSONB,
    result          JSONB,
    trace_id        TEXT,
    span_id         TEXT,
    PRIMARY KEY (occurred_at, code_reference)
);

--bun:split

-- Promote to a hypertable. 1-day chunk interval matches the
-- compression policy below (compress everything older than the
-- current chunk).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        PERFORM create_hypertable(
            'code_invocation',
            'occurred_at',
            chunk_time_interval => INTERVAL '1 day',
            if_not_exists       => TRUE
        );
    ELSE
        RAISE NOTICE 'timescaledb not installed -- code_invocation stays a plain table';
    END IF;
END
$$;

--bun:split

-- Compress everything older than a day. Segment by FQN so the
-- common access pattern ("all invocations of method X in window W")
-- reads a contiguous compressed segment instead of scanning across
-- segments. Order by occurred_at DESC so the most-recent rows
-- decompress first when a forensic query lands.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        ALTER TABLE "code_invocation"
            SET (
                timescaledb.compress,
                timescaledb.compress_segmentby = 'code_reference',
                timescaledb.compress_orderby   = 'occurred_at DESC'
            );
        PERFORM add_compression_policy('code_invocation', INTERVAL '1 day', if_not_exists => TRUE);
    END IF;
END
$$;

--bun:split

-- Default retention: 7 days. Per-FQN overrides driven by
-- v1:observability:codeProfile.retentionDays are applied by the
-- codeProfileEnforcement automation, which calls drop_chunks /
-- alter_job to keep this policy aligned with the operator's intent.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        PERFORM add_retention_policy('code_invocation', INTERVAL '7 days', if_not_exists => TRUE);
    END IF;
END
$$;

--bun:split

-- 1-minute continuous aggregate: callCount, p50/p95/p99, error rate.
-- Refresh policy below materializes new buckets every minute with a
-- 5-minute lag so partial buckets don't churn.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        EXECUTE $cq$
            CREATE MATERIALIZED VIEW IF NOT EXISTS code_invocation_1m
            WITH (timescaledb.continuous) AS
            SELECT
                code_reference,
                time_bucket(INTERVAL '1 minute', occurred_at) AS window_start,
                COUNT(*)                                       AS call_count,
                COUNT(*) FILTER (WHERE error_message IS NOT NULL AND error_message <> '') AS error_count,
                percentile_cont(0.5)  WITHIN GROUP (ORDER BY duration_ns) AS p50_duration_ns,
                percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ns) AS p95_duration_ns,
                percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ns) AS p99_duration_ns,
                SUM(duration_ns)                                AS total_duration_ns
            FROM code_invocation
            GROUP BY code_reference, window_start
            WITH NO DATA;
        $cq$;
        PERFORM add_continuous_aggregate_policy(
            'code_invocation_1m',
            start_offset      => INTERVAL '1 hour',
            end_offset        => INTERVAL '5 minutes',
            schedule_interval => INTERVAL '1 minute',
            if_not_exists     => TRUE
        );
    END IF;
END
$$;

--bun:split

-- 1-hour continuous aggregate: same shape, longer horizon, drives
-- the cockpit's drill-down sparkline default zoom.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        EXECUTE $cq$
            CREATE MATERIALIZED VIEW IF NOT EXISTS code_invocation_1h
            WITH (timescaledb.continuous) AS
            SELECT
                code_reference,
                time_bucket(INTERVAL '1 hour', occurred_at) AS window_start,
                COUNT(*)                                     AS call_count,
                COUNT(*) FILTER (WHERE error_message IS NOT NULL AND error_message <> '') AS error_count,
                percentile_cont(0.5)  WITHIN GROUP (ORDER BY duration_ns) AS p50_duration_ns,
                percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ns) AS p95_duration_ns,
                percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ns) AS p99_duration_ns,
                SUM(duration_ns)                              AS total_duration_ns
            FROM code_invocation
            GROUP BY code_reference, window_start
            WITH NO DATA;
        $cq$;
        PERFORM add_continuous_aggregate_policy(
            'code_invocation_1h',
            start_offset      => INTERVAL '1 day',
            end_offset        => INTERVAL '1 hour',
            schedule_interval => INTERVAL '5 minutes',
            if_not_exists     => TRUE
        );
    END IF;
END
$$;

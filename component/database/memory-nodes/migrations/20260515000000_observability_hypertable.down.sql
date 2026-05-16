-- Reverse the observability hypertable. Order matters: continuous
-- aggregates first (they hold a dependency on the hypertable),
-- then the policies, then the table.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        EXECUTE 'DROP MATERIALIZED VIEW IF EXISTS code_invocation_1h CASCADE';
        EXECUTE 'DROP MATERIALIZED VIEW IF EXISTS code_invocation_1m CASCADE';
    END IF;
END
$$;

--bun:split

DROP TABLE IF EXISTS "code_invocation";

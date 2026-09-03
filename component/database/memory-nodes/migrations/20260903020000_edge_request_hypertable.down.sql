-- Reverse the edge request log. Order matters: the aggregates first (they
-- hold a dependency on the hypertable), then the table.
--
-- Both spellings are dropped whichever extension is present, because the up
-- migration creates a continuous aggregate on TimescaleDB and an ordinary
-- view without it, under the same two names -- so a down that dropped only
-- the shape it expected would leave the other standing and make the next up
-- fail on a name that is already taken.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        EXECUTE 'DROP MATERIALIZED VIEW IF EXISTS edge_request_1h CASCADE';
        EXECUTE 'DROP MATERIALIZED VIEW IF EXISTS edge_request_1m CASCADE';
    END IF;
END
$$;

--bun:split

DROP VIEW IF EXISTS edge_request_1h CASCADE;

--bun:split

DROP VIEW IF EXISTS edge_request_1m CASCADE;

--bun:split

DROP TABLE IF EXISTS "edge_request";

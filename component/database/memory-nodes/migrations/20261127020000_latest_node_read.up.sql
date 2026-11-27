-- A point read should plan a payload lookup only in the chunk that holds it.
-- The key query has a reusable generic plan; EXECUTE then plans with the actual
-- timestamp, allowing Timescale to exclude old chunks before planning/locking
-- all their payload indexes. EXECUTE binds values, never interpolates them.
--
-- STABLE is required for correctness: both reads use the calling statement's
-- snapshot, including when a heartbeat replaces a memory_liveness row between
-- the key and payload reads. There is no cross-request data cache. This is an
-- invoker function, and row admission remains the engine caller's responsibility.
-- https://www.postgresql.org/docs/16/xfunc-volatility.html
CREATE FUNCTION memql_latest_node(wanted_id text, as_of timestamptz DEFAULT NULL)
RETURNS SETOF "MemoryNodes"
LANGUAGE plpgsql STABLE
SET plan_cache_mode = force_generic_plan
AS $$
DECLARE
    newest_at timestamptz;
BEGIN
    SELECT observed."createdAt" INTO newest_at
    FROM (
        SELECT id, "createdAt" FROM "MemoryNodes"
        UNION ALL SELECT id, "createdAt" FROM memory_liveness
    ) observed
    WHERE observed.id = wanted_id
      AND observed."createdAt" <= coalesce(as_of, 'infinity'::timestamptz)
    ORDER BY observed."createdAt" DESC
    LIMIT 1;
    IF NOT FOUND THEN RETURN; END IF;

    RETURN QUERY EXECUTE
        'SELECT * FROM (
            SELECT * FROM "MemoryNodes"
            UNION ALL SELECT * FROM memory_liveness
        ) observed WHERE id = $1 AND "createdAt" = $2 LIMIT 1'
        USING wanted_id, newest_at;
END
$$;

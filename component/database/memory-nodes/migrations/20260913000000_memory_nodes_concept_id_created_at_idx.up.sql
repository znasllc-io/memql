-- Every concept read the executor emits is "the latest row per id WITHIN one
-- concept":
--
--     SELECT DISTINCT ON (id) ... FROM "MemoryNodes"
--     WHERE concept = $1 ORDER BY id ASC, "createdAt" DESC
--
-- With only (id, "createdAt" DESC) and (concept) to choose from, TimescaleDB
-- plans that as a SkipScan over the id index and filters concept per row, so a
-- read of one small concept walks EVERY id in the table. On memql.znas.io
-- (2026-09-13) the 274-row staleClusterNodes read touched 1,006,847 rows and
-- 6.9 GB of buffers per call, took 178 s, was issued on every node heartbeat by
-- every pod, and exhausted max_connections -- which the edge reported as
-- "internal error" on every cold host resolution.
--
-- The composite index lets the same plan skip within one concept's ids.
-- transaction_per_chunk keeps the build's lock to one chunk at a time on a
-- live hypertable; this file is NOT transactional (no .tx. suffix), which is
-- what that option requires.
CREATE INDEX IF NOT EXISTS memory_nodes_concept_id_created_at_desc_idx
    ON "MemoryNodes" (concept, id, "createdAt" DESC)
    WITH (timescaledb.transaction_per_chunk);

-- Access paths for v1:identity:authActivity (memql#4328, memql#4330).
--
-- The concept is the routine authentication-mechanics log split out of
-- v1:identity:auditEvent: one row per refresh-token rotation and one per
-- PAT-authenticated request. It is written on the hot auth path and will be
-- among the largest concepts in the table, and it is read on two paths that
-- are both latency-sensitive in their own way.
--
-- No new table. Like every other concept its rows live in the "MemoryNodes"
-- TimescaleDB hypertable, already created and partitioned on createdAt in
-- 20260324000000_initial_setup. Both indexes below are PARTIAL, scoped to the
-- concept, so neither carries every other concept's rows.

-- 1. THE REUSE LOOKUP (memql#4329).
--
-- authActivityByRetiredHash asks "did any rotation retire this hash", and it
-- asks on the /auth/refresh path for every presented token that resolves to no
-- live session. Without an index that is a sequential scan of the largest
-- concept in the table, on a request that is already a cache miss -- so the
-- cost lands exactly where a user is waiting.
--
-- Partial on the concept AND on a non-empty hash: only rotations record one,
-- and the blocked / grace / PAT rows are the majority. Excluding them keeps
-- the index roughly the size of the rotation history rather than of the whole
-- log.
CREATE INDEX IF NOT EXISTS memory_nodes_auth_activity_retired_hash_idx
    ON "MemoryNodes" ((payload->>'retiredTokenHash'))
    WHERE concept = 'v1:identity:authActivity'
      AND payload->>'retiredTokenHash' <> '';

--bun:split

-- 2. THE RETENTION SWEEP (memql#4330).
--
-- component/identity/authactivity selects expired ids by (concept, createdAt)
-- before deleting them. createdAt is the hypertable's partition key, so the
-- range predicate already prunes chunks; this index makes the per-chunk work
-- an ordered scan of just this concept's rows rather than a filter over every
-- concept's.
--
-- The sweep's second predicate (the CASE over payload->>'occurredAt') is
-- deliberately NOT indexed. It is a re-check, not a selector -- occurredAt is
-- stamped within milliseconds of createdAt on an append-only log -- so
-- indexing it would double the write cost of the hottest concept in the table
-- to speed up a daily job that is already chunk-pruned.
CREATE INDEX IF NOT EXISTS memory_nodes_auth_activity_created_at_idx
    ON "MemoryNodes" ("createdAt")
    WHERE concept = 'v1:identity:authActivity';

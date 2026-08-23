-- Reverse the v1:identity:authActivity access paths (memql#4328, memql#4330).
--
-- The rows themselves live in the shared "MemoryNodes" hypertable and are NOT
-- dropped here: the up migration created no table, and dropping a concept's
-- history to undo an index would destroy the evidence refresh-token reuse
-- detection runs on.

DROP INDEX IF EXISTS memory_nodes_auth_activity_created_at_idx;

--bun:split

DROP INDEX IF EXISTS memory_nodes_auth_activity_retired_hash_idx;

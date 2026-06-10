-- Reverse the mesh delivery substrate (znasllc-io/memql#1263).
DROP TABLE IF EXISTS mesh_cursor;

--bun:split

DROP TABLE IF EXISTS mesh_key_seq;

--bun:split

DROP INDEX IF EXISTS uq_mesh_outbox_event_id;

--bun:split

DROP INDEX IF EXISTS idx_mesh_outbox_key_seq;

--bun:split

DROP TABLE IF EXISTS mesh_outbox;

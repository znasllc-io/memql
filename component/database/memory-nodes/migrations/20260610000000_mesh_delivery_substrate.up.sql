-- Durable mesh delivery substrate: outbox + cursor (znasllc-io/memql#1263).
--
-- Phase 1 of the resilient multi-node mesh epic (memql#1259). Implements the
-- "outbox + cursor" mechanism decided in the spike ADR
-- (docs/internal/design/mesh-delivery-substrate-adr.md, memql#1262).
--
-- The durable graph (Postgres) is the delivery GUARANTEE: producers append a
-- row to mesh_outbox addressed by a LOGICAL routing key (space:/session:/user:,
-- never a physical node-id); consumers subscribe by that logical key, replay
-- from a per-(key, consumer) cursor in mesh_cursor on every (re)connect, and
-- advance the cursor as they acknowledge. The existing mesh push becomes a
-- best-effort fast-path deduped against this path by event_id.
--
-- Plain tables + plain SQL on purpose: no replication slot, no NOTIFY
-- dependency -- runs byte-identical on local Docker Postgres and managed Tiger
-- Cloud Postgres (env-agnostic). Retention mirrors the
-- automation_execution_claims precedent (memql#561): rows are prunable once
-- every active subscriber's cursor is past them, or past a max-age watermark.
--
-- NOTE: no `--bun:split` before the first statement. A leading comment-only
-- segment makes bun emit an empty query ("pgdriver: query is empty"), aborting
-- the migration (memql#570). The header rides in the first statement's segment.
-- All statements are idempotent (IF NOT EXISTS) so the migration is
-- forward-safe on the 2-replica cluster.

CREATE TABLE IF NOT EXISTS mesh_outbox (
  routing_key  text        NOT NULL,                 -- logical addr, e.g. 'space:<id>'
  seq          bigint      NOT NULL,                 -- per-key monotonic sequence
  event_id     text        NOT NULL,                 -- stable id; the dedup key
  topic        text        NOT NULL,                 -- e.g. graph.node.created....
  kind         int         NOT NULL,                 -- events.Kind
  payload      jsonb       NOT NULL,
  origin_node  text        NOT NULL DEFAULT '',      -- producer node id (audit only)
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (routing_key, seq)
);

--bun:split

-- Covering read path for catch-up: WHERE routing_key=$1 AND seq>$cursor.
CREATE INDEX IF NOT EXISTS idx_mesh_outbox_key_seq
  ON mesh_outbox (routing_key, seq);

--bun:split

-- Idempotent-produce guard: a duplicate event_id must not double-insert, so
-- the mesh fast-path and a retried Publish both collapse to one durable row.
CREATE UNIQUE INDEX IF NOT EXISTS uq_mesh_outbox_event_id
  ON mesh_outbox (event_id);

--bun:split

-- Per-(routing_key) sequence allocator. The producing transaction upserts the
-- counter and returns next_seq, so seq is strictly increasing per key without
-- a table scan or an advisory lock. A small prunable coordination table, like
-- automation_execution_claims (memql#561).
CREATE TABLE IF NOT EXISTS mesh_key_seq (
  routing_key  text   NOT NULL,
  next_seq     bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (routing_key)
);

--bun:split

-- Per-(routing_key, consumer) cursor. consumer_id is a LOGICAL consumer (the
-- key's owner), never a node-id; survives the consumer's restart and defines
-- the replay start point on (re)connect.
CREATE TABLE IF NOT EXISTS mesh_cursor (
  routing_key  text        NOT NULL,
  consumer_id  text        NOT NULL,
  cursor_seq   bigint      NOT NULL DEFAULT 0,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (routing_key, consumer_id)
);

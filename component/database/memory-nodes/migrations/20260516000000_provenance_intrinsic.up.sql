-- Adds the `provenance` JSONB intrinsic column to MemoryNodes +
-- SecretMemoryNodes. Engine-stamped at insert time; required (NOT
-- NULL) for every new row. See component/provenance for the shape.
--
-- The DEFAULT is a sentinel that only catches rows inserted by
-- non-migration paths racing with this migration during cluster
-- bootstrap (e.g. the concept seeder writing v1:cluster:node rows
-- on each node's startup before migrations finish on the first node
-- to win the migration lock). The engine's Go-side validator
-- enforces real provenance on every NEW write -- the default never
-- fires for application-issued inserts.

ALTER TABLE "MemoryNodes"
  ADD COLUMN provenance JSONB NOT NULL
  DEFAULT '{"kind":"system","name":"migration-bootstrap"}'::jsonb;

--bun:split

ALTER TABLE "SecretMemoryNodes"
  ADD COLUMN provenance JSONB NOT NULL
  DEFAULT '{"kind":"system","name":"migration-bootstrap"}'::jsonb;

--bun:split

-- GIN index lets `provenance @> '{"kind":"seed"}'` and similar
-- containment queries push down to the index. Used by the future
-- "show me every row created by automation X" queries.
CREATE INDEX IF NOT EXISTS idx_memorynodes_provenance
  ON "MemoryNodes" USING GIN (provenance);

--bun:split

CREATE INDEX IF NOT EXISTS idx_secretmemorynodes_provenance
  ON "SecretMemoryNodes" USING GIN (provenance);

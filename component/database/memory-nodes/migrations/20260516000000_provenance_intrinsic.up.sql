-- Adds the `provenance` JSONB intrinsic column to MemoryNodes +
-- SecretMemoryNodes. Engine-stamped at insert time; required (NOT
-- NULL) for every new row. See component/provenance for the shape.
--
-- Genesis-mode migration: no backfill for legacy rows because there
-- are none (dev clusters are wiped on every test cycle). The
-- engine's mutation executor enforces provenance presence on inserts
-- via the Go-side validator -- this column is enforced at the
-- database layer as a belt-and-suspenders second check.

ALTER TABLE "MemoryNodes"
  ADD COLUMN provenance JSONB NOT NULL;

ALTER TABLE "SecretMemoryNodes"
  ADD COLUMN provenance JSONB NOT NULL;

-- GIN index lets `provenance @> '{"kind":"seed"}'` and similar
-- containment queries push down to the index. Used by the future
-- "show me every row created by automation X" queries.
CREATE INDEX IF NOT EXISTS idx_memorynodes_provenance
  ON "MemoryNodes" USING GIN (provenance);

CREATE INDEX IF NOT EXISTS idx_secretmemorynodes_provenance
  ON "SecretMemoryNodes" USING GIN (provenance);

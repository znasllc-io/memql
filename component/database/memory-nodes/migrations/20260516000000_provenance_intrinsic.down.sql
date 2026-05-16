DROP INDEX IF EXISTS idx_secretmemorynodes_provenance;

--bun:split

DROP INDEX IF EXISTS idx_memorynodes_provenance;

--bun:split

ALTER TABLE "SecretMemoryNodes" DROP COLUMN IF EXISTS provenance;

--bun:split

ALTER TABLE "MemoryNodes" DROP COLUMN IF EXISTS provenance;

DROP INDEX IF EXISTS idx_secretmemorynodes_provenance;
DROP INDEX IF EXISTS idx_memorynodes_provenance;

ALTER TABLE "SecretMemoryNodes" DROP COLUMN IF EXISTS provenance;
ALTER TABLE "MemoryNodes" DROP COLUMN IF EXISTS provenance;

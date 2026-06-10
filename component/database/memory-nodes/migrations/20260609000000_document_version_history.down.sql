-- Reverse the document version history storage tuning (memql#1228).
--
-- documentVersion rows themselves live in the shared "MemoryNodes"
-- hypertable and are NOT dropped here (the up migration created no
-- table). We only undo the two storage optimizations the up migration
-- added: the version-history index, and -- only if THIS migration was
-- the one that turned MemoryNodes compression on -- the compression
-- policy + settings.

-- Remove the compression policy + settings, guarded so this is a no-op
-- when TimescaleDB is absent or compression was never enabled. NOTE:
-- this drops MemoryNodes compression unconditionally on down; if a
-- later migration takes ownership of MemoryNodes compression it should
-- not depend on this one being applied.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        IF EXISTS (
            SELECT 1 FROM timescaledb_information.compression_settings
            WHERE hypertable_name = 'MemoryNodes'
        ) THEN
            PERFORM remove_compression_policy('MemoryNodes', if_exists => TRUE);
            -- Decompress every compressed chunk before disabling, else
            -- the ALTER ... SET (compress = false) is rejected.
            PERFORM decompress_chunk(format('%I.%I', chunk_schema, chunk_name)::regclass, true)
            FROM timescaledb_information.chunks
            WHERE hypertable_name = 'MemoryNodes' AND is_compressed;
            ALTER TABLE "MemoryNodes" SET (timescaledb.compress = false);
        END IF;
    END IF;
EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'skipping MemoryNodes compression teardown: %', SQLERRM;
END
$$;

--bun:split

DROP INDEX IF EXISTS memory_nodes_document_version_history_idx;

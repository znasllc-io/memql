DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_available_extensions
        WHERE name = 'timescaledb'
    ) THEN
        IF NOT EXISTS (
            SELECT 1
            FROM pg_extension
            WHERE extname = 'timescaledb'
        ) THEN
            CREATE EXTENSION timescaledb;
        ELSE
            RAISE NOTICE 'timescaledb extension already installed; skipping create';
        END IF;
    ELSE
        RAISE NOTICE 'timescaledb extension is not available on this server';
    END IF;
END
$$;
--bun:split

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_available_extensions
        WHERE name = 'timescaledb_toolkit'
    ) THEN
        IF NOT EXISTS (
            SELECT 1
            FROM pg_extension
            WHERE extname = 'timescaledb_toolkit'
        ) THEN
            CREATE EXTENSION timescaledb_toolkit;
        ELSE
            RAISE NOTICE 'timescaledb_toolkit extension already installed; skipping create';
        END IF;
    ELSE
        RAISE NOTICE 'timescaledb_toolkit extension is not available on this server';
    END IF;
END
$$;
--bun:split

-- Partitions metadata table
CREATE TABLE IF NOT EXISTS "Partitions" (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    type TEXT NOT NULL DEFAULT 'standard',
    config JSONB DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
--bun:split

-- Seed the default partition
INSERT INTO "Partitions" (id, name, type)
VALUES ('default', 'default', 'standard')
ON CONFLICT (id) DO NOTHING;
--bun:split

-- Memory nodes with partition isolation
CREATE TABLE IF NOT EXISTS "MemoryNodes" (
    partition TEXT NOT NULL DEFAULT 'default',
    id TEXT NOT NULL,
    "createdAt" TIMESTAMPTZ NOT NULL,
    "createdBy" TEXT NOT NULL,
    schema JSONB NOT NULL,
    payload JSONB NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}',
    "type" TEXT NOT NULL DEFAULT 'object',
    concept TEXT NOT NULL,
    PRIMARY KEY (partition, id, "createdAt")
);
--bun:split

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE p.proname = 'create_hypertable'
          AND n.nspname = 'timescaledb'
    ) THEN
        PERFORM create_hypertable('MemoryNodes', 'createdAt', if_not_exists => TRUE);
    ELSE
        RAISE NOTICE 'create_hypertable function not available; MemoryNodes remains a regular table';
    END IF;
END
$$;
--bun:split

CREATE INDEX IF NOT EXISTS memory_nodes_partition_id_created_at_desc_idx
    ON "MemoryNodes" (partition, id, "createdAt" DESC);
--bun:split

CREATE INDEX IF NOT EXISTS memory_nodes_partition_concept_idx
    ON "MemoryNodes" (partition, concept);
--bun:split

CREATE INDEX IF NOT EXISTS memory_nodes_metadata_gin_idx
    ON "MemoryNodes" USING GIN (metadata);
--bun:split

-- Secret memory nodes with partition isolation
CREATE TABLE IF NOT EXISTS "SecretMemoryNodes" (
    partition TEXT NOT NULL DEFAULT 'default',
    id TEXT NOT NULL,
    "createdAt" TIMESTAMPTZ NOT NULL,
    "createdBy" TEXT NOT NULL,
    schema JSONB NOT NULL,
    payload JSONB NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}',
    "type" TEXT NOT NULL DEFAULT 'object',
    concept TEXT NOT NULL,
    PRIMARY KEY (partition, id, "createdAt")
);
--bun:split

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE p.proname = 'create_hypertable'
          AND n.nspname = 'timescaledb'
    ) THEN
        PERFORM create_hypertable('SecretMemoryNodes', 'createdAt', if_not_exists => TRUE);
    ELSE
        RAISE NOTICE 'create_hypertable function not available; SecretMemoryNodes remains a regular table';
    END IF;
END
$$;
--bun:split

CREATE INDEX IF NOT EXISTS secret_memory_nodes_partition_id_created_at_desc_idx
    ON "SecretMemoryNodes" (partition, id, "createdAt" DESC);
--bun:split

CREATE INDEX IF NOT EXISTS secret_memory_nodes_partition_concept_idx
    ON "SecretMemoryNodes" (partition, concept);
--bun:split

CREATE INDEX IF NOT EXISTS secret_memory_nodes_metadata_gin_idx
    ON "SecretMemoryNodes" USING GIN (metadata);
--bun:split

-- TimescaleDB status node
DO $$
DECLARE
    installed_version TEXT;
    installed_schema  TEXT;
    status_payload    JSONB;
    status_schema     JSONB := jsonb_build_object(
        'type', 'timescaledb_extension_status',
        'source', 'migration'
    );
    status_id TEXT := 'system.timescaledb.status';
    status_created_at TIMESTAMPTZ := TIMESTAMPTZ '1970-01-01 00:00:00+00';
BEGIN
    SELECT e.extversion, ns.nspname
    INTO installed_version, installed_schema
    FROM pg_extension e
    JOIN pg_namespace ns ON ns.oid = e.extnamespace
    WHERE e.extname = 'timescaledb';

    status_payload := jsonb_build_object(
        'installed', installed_version IS NOT NULL,
        'version', installed_version,
        'schema', installed_schema,
        'checked_at', now()
    );

    INSERT INTO "MemoryNodes" (partition, id, "createdAt", "createdBy", schema, payload, "type", concept)
    VALUES (
        'default',
        status_id,
        status_created_at,
        'system',
        status_schema,
        status_payload,
        'object',
        'system.migration'
    )
    ON CONFLICT (partition, id, "createdAt") DO UPDATE
    SET payload = EXCLUDED.payload,
        schema = EXCLUDED.schema;

    IF installed_version IS NOT NULL THEN
        RAISE NOTICE 'TimescaleDB extension verified (version %, schema %)', installed_version, installed_schema;
    ELSE
        RAISE WARNING 'TimescaleDB extension is not installed; MemoryNodes recorded failure status';
    END IF;
END
$$;

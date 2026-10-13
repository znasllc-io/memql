-- A schema version is immutable content, shared by every row that used it.
-- Keep $id beside the reference so tombstone discrimination remains local.
CREATE TABLE memory_node_schemas (
    fingerprint text PRIMARY KEY,
    document jsonb NOT NULL
);

-- Readers arrive before the physical representation changes during a rolling
-- upgrade. No operator-machine switch and no read of an in-process peer cache.
CREATE TABLE memory_schema_storage (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    references_enabled boolean NOT NULL DEFAULT false,
    next_check_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO memory_schema_storage DEFAULT VALUES;
CREATE TABLE memory_schema_readers (
    node_id text PRIMARY KEY,
    seen_at timestamptz NOT NULL
);

CREATE FUNCTION memory_node_compact_schema() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
    schema_fingerprint text;
    stored jsonb;
BEGIN
    IF NOT (SELECT references_enabled FROM memory_schema_storage WHERE singleton) THEN
        RETURN NEW;
    END IF;
    IF left(NEW.schema->>'$ref', 13) = 'memql-schema:' THEN
        schema_fingerprint := substr(NEW.schema->>'$ref', 14);
        SELECT document INTO stored FROM memory_node_schemas s WHERE s.fingerprint = schema_fingerprint;
        IF stored IS NULL OR NEW.schema->>'$id' IS DISTINCT FROM stored->>'$id' THEN
            RAISE EXCEPTION 'missing or mismatched memory-node schema reference %', schema_fingerprint;
        END IF;
        RETURN NEW;
    END IF;
    schema_fingerprint := encode(sha256(convert_to(NEW.schema::text, 'UTF8')), 'hex');
    INSERT INTO memory_node_schemas VALUES (schema_fingerprint, NEW.schema)
        ON CONFLICT DO NOTHING;
    SELECT document INTO stored FROM memory_node_schemas s WHERE s.fingerprint = schema_fingerprint;
    IF stored IS DISTINCT FROM NEW.schema THEN
        RAISE EXCEPTION 'memory-node schema fingerprint collision';
    END IF;
    NEW.schema := jsonb_build_object('$ref', 'memql-schema:' || schema_fingerprint);
    IF stored ? '$id' THEN
        NEW.schema := NEW.schema || jsonb_build_object('$id', stored->'$id');
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER memory_node_shared_schema BEFORE INSERT OR UPDATE OF schema
    ON "MemoryNodes" FOR EACH ROW EXECUTE FUNCTION memory_node_compact_schema();
CREATE TRIGGER secret_memory_node_shared_schema BEFORE INSERT OR UPDATE OF schema
    ON "SecretMemoryNodes" FOR EACH ROW EXECUTE FUNCTION memory_node_compact_schema();

-- Existing history is converted by bounded background batches after startup.
-- The shared cursor and due time serialize all replicas without a leader lease.
CREATE TABLE memory_schema_compaction (
    table_name text PRIMARY KEY CHECK (table_name IN ('MemoryNodes', 'SecretMemoryNodes')),
    cursor_id text,
    cursor_at timestamptz,
    completed boolean NOT NULL DEFAULT false,
    next_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO memory_schema_compaction (table_name) VALUES ('MemoryNodes'), ('SecretMemoryNodes');

-- Re-encoding schema storage changes no recovery head. Do not turn the
-- historical conversion into a backlog of work-run recovery invalidations.
CREATE OR REPLACE FUNCTION enqueue_work_run_head() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO work_run_head_events (id)
        SELECT DISTINCT id FROM new_rows WHERE concept='v1:work:run';
    ELSIF TG_OP = 'DELETE' THEN
        INSERT INTO work_run_head_events (id)
        SELECT DISTINCT id FROM old_rows WHERE concept='v1:work:run';
    ELSE
        INSERT INTO work_run_head_events (id)
        SELECT DISTINCT coalesce(n.id,o.id)
        FROM old_rows o FULL JOIN new_rows n
          ON n.id=o.id AND n."createdAt"=o."createdAt" AND n.concept=o.concept
        WHERE (o.concept='v1:work:run' OR n.concept='v1:work:run')
          AND (o.id IS NULL OR n.id IS NULL OR o.payload->>'status' IS DISTINCT FROM n.payload->>'status');
    END IF;
    RETURN NULL;
END
$$;

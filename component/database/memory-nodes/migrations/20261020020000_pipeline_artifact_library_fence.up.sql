-- A Library edit and retirement must agree on which won, across replicas.
-- This separate metadata lock must NOT be the upload owner's lock: a stream
-- finalizer holds that owner lock while the engine writes on another connection.
-- Keep this guard after retirement: a delayed promotion must not recreate a
-- Library entry whose object was permanently replaced by a leased tombstone.
ALTER TABLE pipeline_artifact_uploads ADD COLUMN IF NOT EXISTS library_purged_at timestamptz;
CREATE OR REPLACE FUNCTION guard_retired_pipeline_library_file() RETURNS trigger AS $$
DECLARE
    target_file text;
    target_state text;
BEGIN
    CASE NEW.concept
        WHEN 'v1:library:file' THEN
            target_file := regexp_replace(NEW.id, '^v1:library:file:', '');
        WHEN 'v1:library:artifact' THEN
            IF NEW.payload->>'sourceConceptRef' NOT LIKE 'v1:library:file:%' THEN
                RETURN NEW;
            END IF;
            target_file := regexp_replace(NEW.payload->>'sourceConceptRef', '^v1:library:file:', '');
        WHEN 'v1:library:fileVersion' THEN
            target_file := regexp_replace(NEW.payload->>'fileId', '^v1:library:file:', '');
        ELSE
            RETURN NEW;
    END CASE;
    PERFORM pg_advisory_xact_lock(('x' || substr(encode(sha256(convert_to(
        'pipeline-artifact-library:' || target_file, 'UTF8')), 'hex'), 1, 16))::bit(64)::bigint);
    -- Also lock a file without an intent: legacy adoption may be committing
    -- its first receipt concurrently with an owner's edit.
    -- Read again after the lock; the first read might have raced retirement.
    SELECT state INTO target_state FROM pipeline_artifact_uploads WHERE file_id=target_file;
    IF target_state IN ('retiring','retired') THEN
        RAISE EXCEPTION 'pipeline artifact bytes were permanently retired';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE TRIGGER pipeline_library_retirement_fence
    BEFORE INSERT OR UPDATE OF payload ON "MemoryNodes"
    FOR EACH ROW WHEN (NEW.concept IN ('v1:library:file','v1:library:artifact','v1:library:fileVersion'))
    EXECUTE FUNCTION guard_retired_pipeline_library_file();

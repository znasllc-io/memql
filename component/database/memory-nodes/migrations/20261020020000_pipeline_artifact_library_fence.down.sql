-- Retirement is permanent. Removing its write fence would admit delayed
-- Library promotions and edits for objects whose bytes are gone.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pipeline_artifact_uploads WHERE state IN ('retiring','retired')) THEN
        RAISE EXCEPTION 'cannot remove the Library fence for retired pipeline artifacts';
    END IF;
END $$;
DROP TRIGGER IF EXISTS pipeline_library_retirement_fence ON "MemoryNodes";
DROP FUNCTION IF EXISTS guard_retired_pipeline_library_file();

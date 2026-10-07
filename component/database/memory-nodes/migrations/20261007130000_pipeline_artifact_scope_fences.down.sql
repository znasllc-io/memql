DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pipeline_artifact_scope_fences) THEN
        RAISE EXCEPTION 'cannot remove artifact producer fences: delayed uploads could resume';
    END IF;
END $$;
DROP TRIGGER IF EXISTS pipeline_artifact_scope_admission ON pipeline_artifact_uploads;
DROP FUNCTION IF EXISTS guard_pipeline_artifact_scope();
DROP INDEX IF EXISTS pipeline_artifact_uploads_scope;
DROP TABLE IF EXISTS pipeline_artifact_scope_fences;

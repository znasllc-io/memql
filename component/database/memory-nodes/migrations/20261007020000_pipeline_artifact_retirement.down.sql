DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pipeline_artifact_references)
       OR EXISTS (SELECT 1 FROM pipeline_artifact_uploads WHERE state IN ('retiring','retired')) THEN
        RAISE EXCEPTION 'artifact reference and retirement fences cannot be discarded';
    END IF;
END $$;
DROP TABLE pipeline_artifact_references;
ALTER TABLE pipeline_artifact_uploads
    DROP COLUMN retirement_lease_id,
    DROP COLUMN retired_etag,
    DROP CONSTRAINT pipeline_artifact_uploads_state_check;
ALTER TABLE pipeline_artifact_uploads
    ADD CONSTRAINT pipeline_artifact_uploads_state_check CHECK (state IN ('reserved','blob_verified','ready'));

-- Upload intents are durable receipts and may name committed objects without
-- Library rows. A schema rollback must not discard their reservations.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pipeline_artifact_uploads) THEN
        RAISE EXCEPTION 'pipeline artifact upload journal is not empty; refusing to discard durable storage receipts';
    END IF;
END $$;
DROP TABLE pipeline_artifact_uploads;

-- Dropping the columns while an upload runs would remove its fence.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM release_draft_intents WHERE upload_lease_until>clock_timestamp()) THEN
        RAISE EXCEPTION 'a release upload is still running; wait for its lease to end';
    END IF;
END $$;
ALTER TABLE release_draft_intents
    DROP COLUMN IF EXISTS upload_lease_id,
    DROP COLUMN IF EXISTS upload_lease_until;

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM installation_preparations WHERE application_continuity <> '{}'::jsonb) THEN
        RAISE EXCEPTION 'retained Application continuity cannot be discarded';
    END IF;
END $$;
ALTER TABLE installation_preparations DROP COLUMN application_continuity;

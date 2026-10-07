-- Never discard the only record of an uncertain external update.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM installation_revision_attempts) THEN
        RAISE EXCEPTION 'installation revision history is not empty; preserve its effect and cancellation records';
    END IF;
END $$;
DROP TABLE installation_revision_heads;
DROP TABLE installation_revision_attempts;

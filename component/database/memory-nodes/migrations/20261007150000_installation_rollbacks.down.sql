DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM installation_revision_rollbacks) THEN
        RAISE EXCEPTION 'installation rollback history must be retained';
    END IF;
END $$;
DROP TABLE installation_revision_rollbacks;

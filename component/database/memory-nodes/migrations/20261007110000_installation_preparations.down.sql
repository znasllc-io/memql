-- Never erase the only source-dispatch or artifact-pin recovery record.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM installation_preparations) THEN
        RAISE EXCEPTION 'installation preparation history is not empty; preserve its source and cleanup records';
    END IF;
END $$;
ALTER TABLE installation_revision_heads DROP CONSTRAINT installation_one_active_operation;
ALTER TABLE installation_revision_heads DROP COLUMN active_preparation_id;
DROP TABLE installation_preparations;

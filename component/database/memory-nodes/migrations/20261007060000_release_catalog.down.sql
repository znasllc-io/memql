DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM release_catalog) OR EXISTS (SELECT 1 FROM release_publication_intents WHERE target_snapshot IS NOT NULL) THEN
        RAISE EXCEPTION 'release catalog evidence is not empty; preserve publication history';
    END IF;
END $$;
DROP TABLE IF EXISTS release_catalog;
ALTER TABLE release_publication_intents DROP COLUMN IF EXISTS target_snapshot;

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pipeline_sarif_publications) THEN
        RAISE EXCEPTION 'cannot discard SARIF publication identities or uncertain external effects';
    END IF;
END $$;
DROP TABLE pipeline_sarif_publications;

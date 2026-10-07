-- A started-effect fence cannot be discarded by schema rollback.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM release_draft_intents) THEN
        RAISE EXCEPTION 'release draft journal is not empty; preserve its effect fences and publication history';
    END IF;
END $$;
DROP TABLE IF EXISTS release_draft_intents;

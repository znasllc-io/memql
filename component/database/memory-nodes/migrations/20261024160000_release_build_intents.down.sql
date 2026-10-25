DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM release_build_intents) THEN
        RAISE EXCEPTION 'release build intents retain irreversible effect identities; refusing to drop their journal';
    END IF;
END $$;
DROP TABLE IF EXISTS release_build_intents;

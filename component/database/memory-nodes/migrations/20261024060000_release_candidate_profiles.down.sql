DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM release_candidate_profile_bindings) THEN
        RAISE EXCEPTION 'Cannot remove durable release profile bindings';
    END IF;
END $$;
DROP TABLE IF EXISTS release_candidate_profile_bindings;

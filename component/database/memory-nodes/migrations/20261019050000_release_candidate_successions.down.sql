DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM release_candidate_successions) THEN
        RAISE EXCEPTION 'cannot remove release succession authority and history after use';
    END IF;
END $$;
DROP TABLE IF EXISTS release_candidate_successions;

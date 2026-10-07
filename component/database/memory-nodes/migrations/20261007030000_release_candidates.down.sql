-- Approval and retirement history cannot be discarded by a rollback.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM release_candidates) THEN
        RAISE EXCEPTION 'release candidate journal is not empty; preserve its approval and retirement history';
    END IF;
END $$;
DROP TABLE release_publication_intents;
DROP TABLE release_candidate_approvals;
DROP TABLE release_candidates;

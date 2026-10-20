DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM release_candidate_archives)
       OR EXISTS (SELECT 1 FROM release_candidates WHERE state='archived') THEN
        RAISE EXCEPTION 'release archive evidence is not empty';
    END IF;
END $$;
DROP TABLE release_candidate_archives;
ALTER TABLE release_candidates DROP CONSTRAINT release_candidates_state_check;
ALTER TABLE release_candidates ADD CONSTRAINT release_candidates_state_check
    CHECK (state IN ('preparing','ready','approved','retired'));

-- A supersession row is the only record that a candidate's build inputs were
-- released without a catalog. Rolling back would leave an archived candidate
-- whose evidence the older schema cannot hold, so refuse while one exists.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM release_candidate_archives WHERE basis <> 'catalog') THEN
        RAISE EXCEPTION 'release archive supersession evidence is not empty';
    END IF;
END $$;
ALTER TABLE release_candidate_archives DROP CONSTRAINT release_candidate_archives_basis_check;
ALTER TABLE release_candidate_archives DROP COLUMN basis;
ALTER TABLE release_candidate_archives ALTER COLUMN catalog_digest SET NOT NULL;

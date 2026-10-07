DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM installation_preparations WHERE promoted_plan_id IS NOT NULL) THEN
        RAISE EXCEPTION 'installation promotion history is not empty; preserve its source and revision bindings';
    END IF;
END $$;
DROP INDEX installation_preparation_promoted_plan_idx;
ALTER TABLE installation_preparations
    DROP CONSTRAINT installation_preparation_promotion_state,
    DROP COLUMN promoted_plan_id,
    DROP CONSTRAINT installation_preparations_state_check,
    ADD CONSTRAINT installation_preparations_state_check CHECK (state IN ('preparing','cancelled'));

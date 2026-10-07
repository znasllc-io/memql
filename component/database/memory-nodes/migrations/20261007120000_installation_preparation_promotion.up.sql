-- The preparation and revision use one head throughout their atomic handoff.
ALTER TABLE installation_preparations
    DROP CONSTRAINT installation_preparations_state_check,
    ADD CONSTRAINT installation_preparations_state_check
        CHECK (state IN ('preparing','cancelled','promoted')),
    ADD COLUMN promoted_plan_id text REFERENCES installation_revision_attempts(plan_id),
    ADD CONSTRAINT installation_preparation_promotion_state
        CHECK ((state='promoted') = (promoted_plan_id IS NOT NULL));
CREATE UNIQUE INDEX installation_preparation_promoted_plan_idx
    ON installation_preparations(promoted_plan_id) WHERE promoted_plan_id IS NOT NULL;

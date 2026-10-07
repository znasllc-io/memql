DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM installation_preparations WHERE retirement <> 'null'::jsonb OR state='retiring') THEN
        RAISE EXCEPTION 'cannot remove installation preparation retirement history';
    END IF;
END $$;
ALTER TABLE installation_preparations DROP CONSTRAINT installation_preparations_state_check;
ALTER TABLE installation_preparations ADD CONSTRAINT installation_preparations_state_check
    CHECK (state IN ('preparing','cancelled','promoted'));
ALTER TABLE installation_preparations DROP COLUMN retirement;

-- Retirement keeps the installation head until producer and artifact cleanup
-- receipts are complete. The original dispatch intent/history remains intact.
ALTER TABLE installation_preparations
    ADD COLUMN IF NOT EXISTS retirement jsonb NOT NULL DEFAULT 'null'::jsonb;
ALTER TABLE installation_preparations DROP CONSTRAINT installation_preparations_state_check;
ALTER TABLE installation_preparations ADD CONSTRAINT installation_preparations_state_check
    CHECK (state IN ('preparing','retiring','cancelled','promoted'));

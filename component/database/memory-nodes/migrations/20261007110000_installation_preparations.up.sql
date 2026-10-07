-- Preparation owns the installation slot before its first source Job. The
-- scope and each observed receipt survive the process that dispatched work.
CREATE TABLE IF NOT EXISTS installation_preparations (
    preparation_id text PRIMARY KEY CHECK (preparation_id ~ '^memql-id:[0-9a-f]{64}$'),
    installation_id text NOT NULL REFERENCES installation_revision_heads(installation_id),
    request_id text NOT NULL,
    requested_by text NOT NULL,
    source_run_id text NOT NULL UNIQUE,
    slot_epoch bigint NOT NULL CHECK (slot_epoch > 0),
    scope jsonb NOT NULL,
    captures jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('preparing','cancelled')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (installation_id, request_id)
);
ALTER TABLE installation_revision_heads
    ADD COLUMN IF NOT EXISTS active_preparation_id text REFERENCES installation_preparations(preparation_id);
ALTER TABLE installation_revision_heads
    ADD CONSTRAINT installation_one_active_operation
    CHECK (active_plan_id IS NULL OR active_preparation_id IS NULL);
CREATE INDEX IF NOT EXISTS installation_preparation_history_idx
    ON installation_preparations(installation_id, created_at, preparation_id);

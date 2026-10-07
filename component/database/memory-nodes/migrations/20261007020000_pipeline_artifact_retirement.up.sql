-- Pins and retirement share the upload-intent lock. Released references and
-- retired destinations remain as permanent fences against delayed producers.
ALTER TABLE pipeline_artifact_uploads
    DROP CONSTRAINT IF EXISTS pipeline_artifact_uploads_state_check;
ALTER TABLE pipeline_artifact_uploads
    ADD CONSTRAINT pipeline_artifact_uploads_state_check
    CHECK (state IN ('reserved', 'blob_verified', 'ready', 'retiring', 'retired'));
ALTER TABLE pipeline_artifact_uploads
    ADD COLUMN IF NOT EXISTS retirement_lease_id text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS retired_etag text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS pipeline_artifact_references (
    reference_key text PRIMARY KEY,
    owner_user_id text NOT NULL,
    reference_id text NOT NULL,
    scope jsonb NOT NULL,
    intent_ids jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('active', 'released')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS pipeline_artifact_references_owner
    ON pipeline_artifact_references (owner_user_id) WHERE state='active';

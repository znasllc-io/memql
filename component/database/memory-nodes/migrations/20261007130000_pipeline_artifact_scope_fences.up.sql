-- Close producer admissions even when no artifact receipt was returned.
-- These tombstones never expire: a delayed producer may resume on any node.
CREATE TABLE IF NOT EXISTS pipeline_artifact_scope_fences (
    scope_key text PRIMARY KEY,
    owner_user_id text NOT NULL,
    scope jsonb NOT NULL CHECK (jsonb_typeof(scope) = 'object'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS pipeline_artifact_uploads_scope
    ON pipeline_artifact_uploads (owner_user_id, (identity->>'WorkRunID'),
        (identity->>'StepKey'), (identity->>'Attempt'), intent_id COLLATE "C");
CREATE INDEX IF NOT EXISTS pipeline_artifact_scope_fences_owner
    ON pipeline_artifact_scope_fences (owner_user_id);

-- An older writer may still be running during a rolling update. Enforce the
-- admission fence at the journal as well as in the new native adapter. This
-- lock is the exact signed big-endian first eight SHA-256 bytes used by
-- lockPipelineUploadOwner; it also serializes a pre-fence delayed INSERT.
CREATE OR REPLACE FUNCTION guard_pipeline_artifact_scope() RETURNS trigger AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(('x' || substr(encode(sha256(convert_to(
        'pipeline-artifact-quota:' || NEW.owner_user_id, 'UTF8')), 'hex'), 1, 16))::bit(64)::bigint);
    IF (TG_OP = 'INSERT' OR NEW.state NOT IN ('retiring','retired'))
       AND EXISTS (SELECT 1 FROM pipeline_artifact_scope_fences
           WHERE owner_user_id=NEW.owner_user_id
             AND scope->>'WorkRunID'=NEW.identity->>'WorkRunID'
             AND scope->>'StepKey'=NEW.identity->>'StepKey'
             AND scope->>'Attempt'=NEW.identity->>'Attempt') THEN
        RAISE EXCEPTION 'artifact producer scope was permanently fenced';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE OR REPLACE TRIGGER pipeline_artifact_scope_admission
    BEFORE INSERT OR UPDATE OF generation ON pipeline_artifact_uploads
    FOR EACH ROW EXECUTE FUNCTION guard_pipeline_artifact_scope();

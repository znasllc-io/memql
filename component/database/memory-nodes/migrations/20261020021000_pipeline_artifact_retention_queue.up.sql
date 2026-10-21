-- A small durable cursor per producer provides fair, bounded cleanup across
-- replicas. It is not an authorization source: every pass rereads the owner,
-- pipeline and work journals before a provider effect.
CREATE TABLE IF NOT EXISTS pipeline_artifact_retention_queue (
    owner_user_id text NOT NULL,
    work_run_id text NOT NULL,
    after_intent_id text NOT NULL DEFAULT '',
    visited_at timestamptz NOT NULL DEFAULT 'epoch',
    lease_id text NOT NULL DEFAULT '',
    lease_until timestamptz NOT NULL DEFAULT 'epoch',
    PRIMARY KEY (owner_user_id, work_run_id)
);
CREATE INDEX IF NOT EXISTS pipeline_artifact_retention_queue_visit
    ON pipeline_artifact_retention_queue (visited_at, owner_user_id, work_run_id);
CREATE INDEX IF NOT EXISTS pipeline_artifact_uploads_retention_scope
    ON pipeline_artifact_uploads (owner_user_id, (identity->>'WorkRunID'), intent_id COLLATE "C")
    WHERE state<>'retired' OR library_purged_at IS NULL;

CREATE OR REPLACE FUNCTION enqueue_pipeline_artifact_retention() RETURNS trigger AS $$
BEGIN
    INSERT INTO pipeline_artifact_retention_queue(owner_user_id, work_run_id)
    VALUES(NEW.owner_user_id, NEW.identity->>'WorkRunID') ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE OR REPLACE TRIGGER pipeline_artifact_retention_enqueue
    AFTER INSERT ON pipeline_artifact_uploads
    FOR EACH ROW EXECUTE FUNCTION enqueue_pipeline_artifact_retention();

INSERT INTO pipeline_artifact_retention_queue(owner_user_id, work_run_id)
SELECT DISTINCT owner_user_id, identity->>'WorkRunID' FROM pipeline_artifact_uploads
WHERE state<>'retired' OR library_purged_at IS NULL
ON CONFLICT DO NOTHING;

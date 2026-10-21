DROP TRIGGER IF EXISTS pipeline_artifact_retention_enqueue ON pipeline_artifact_uploads;
DROP FUNCTION IF EXISTS enqueue_pipeline_artifact_retention();
DROP INDEX IF EXISTS pipeline_artifact_uploads_retention_scope;
DROP TABLE IF EXISTS pipeline_artifact_retention_queue;

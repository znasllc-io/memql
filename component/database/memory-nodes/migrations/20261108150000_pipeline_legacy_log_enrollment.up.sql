-- Step logs written before logs had upload receipts (on the production
-- installation 2026-10-16..10-22, engines before 0.30.5) have no row in
-- pipeline_artifact_uploads, so sweepPipelineArtifacts never sees them and
-- their bytes and Library rows never expire. This durable list names each such
-- file once; enrollLegacyPipelineLogs verifies its bytes and writes its
-- receipt, after which the ordinary retention policy retires it like any other.
--
-- THE SET IS CLOSED. Every generated file is now written through the verified
-- stream, which writes its receipt first, and the receipt-less writer has no
-- caller, so this seed is complete when it runs. A row is deleted once its
-- receipt exists; one the enrollment could not verify stays as 'refused' with
-- its reason, and so does its file. Retention keeps a producer's journals
-- while any of its rows is still pending (integrations/work
-- retentionHeldBackSQL): the enrollment needs them to prove the step.
CREATE TABLE IF NOT EXISTS pipeline_legacy_log_enrollment (
    file_id text PRIMARY KEY,
    owner_user_id text NOT NULL,
    work_run_id text NOT NULL,
    step_key text NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'refused')),
    tries integer NOT NULL DEFAULT 0 CHECK (tries >= 0),
    reason text NOT NULL DEFAULT '',
    visited_at timestamptz NOT NULL DEFAULT 'epoch',
    lease_id text NOT NULL DEFAULT '',
    lease_until timestamptz NOT NULL DEFAULT 'epoch'
);
CREATE INDEX IF NOT EXISTS pipeline_legacy_log_enrollment_next
    ON pipeline_legacy_log_enrollment (visited_at, owner_user_id, work_run_id)
    WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS pipeline_legacy_log_enrollment_producer
    ON pipeline_legacy_log_enrollment (owner_user_id, work_run_id, file_id)
    WHERE state = 'pending';

-- staged-data: MUST-NOT-GATE -- the seed must see every receipt-less pipeline
-- file. Hiding a staged one would leave its bytes and rows unretired forever;
-- the enrollment rereads each file under its owner before any effect.
-- The newest version decides: a file whose current row is no longer a
-- pipeline file is not this process's to retire. Ids are stored bare, as the
-- receipts and the retention queue store them. Rerunning this statement adds
-- nothing twice.
INSERT INTO pipeline_legacy_log_enrollment (file_id, owner_user_id, work_run_id, step_key)
SELECT regexp_replace(f.id, '^v1:library:file:', ''),
       regexp_replace(f.payload->>'ownerUserId', '^v1:identity:user:', ''),
       regexp_replace(f.payload->>'producedByRunId', '^v1:work:run:', ''),
       f.payload->>'producedByStepKey'
  FROM (SELECT DISTINCT ON (id) id, payload FROM "MemoryNodes"
         WHERE concept = 'v1:library:file' ORDER BY id, "createdAt" DESC) f
 WHERE f.payload->>'source' = 'pipeline'
   AND coalesce(f.payload->>'ownerUserId', '') <> ''
   AND coalesce(f.payload->>'producedByRunId', '') <> ''
   AND coalesce(f.payload->>'producedByStepKey', '') <> ''
   AND NOT EXISTS (SELECT 1 FROM pipeline_artifact_uploads u
                    WHERE u.file_id = regexp_replace(f.id, '^v1:library:file:', ''))
ON CONFLICT (file_id) DO NOTHING;

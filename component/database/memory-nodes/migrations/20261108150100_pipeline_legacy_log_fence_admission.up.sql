-- A FENCED SCOPE STILL ADMITS THE ADOPTION OF ITS OWN PRE-RECEIPT LOG.
--
-- The scope fence (20261007130000) closes a producer's (owner, run, step,
-- attempt) to further uploads once cleanup begins, so a delayed producer on any
-- replica cannot add bytes behind a sealed inventory. Adoption is not an upload.
-- It writes the receipt for bytes a finished producer stored before logs had
-- receipts, after verifying them, and the receipt then enters retirement like
-- the scope's other files. Without this exception, the log of every legacy step
-- whose artifacts retention had already retired, and so fenced, could never be
-- enrolled (pipeline_legacy_log_enrollment, 20261108150000): its bytes and
-- Library rows would stay for good.
--
-- The exception is exactly adoption's insert, which no producer can make:
-- only adoption inserts a receipt as 'ready' (a producer's journal, older ones
-- included, inserts 'reserved' and advances it by update), and only adoption
-- writes the '.memql-legacy-log/<file>/' path, bound here to the row's own
-- file_id. Every other admission and recovery is refused exactly as before; the
-- COALESCE keeps a row with no path from reading as NULL, which an IF treats as
-- false. Covered by TestPipelineScopeDBFenceAdmitsOnlyALegacyLogsAdoption and
-- the fenced-scope case of TestPipelineLegacyLogDB.
CREATE OR REPLACE FUNCTION guard_pipeline_artifact_scope() RETURNS trigger AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(('x' || substr(encode(sha256(convert_to(
        'pipeline-artifact-quota:' || NEW.owner_user_id, 'UTF8')), 'hex'), 1, 16))::bit(64)::bigint);
    IF (TG_OP = 'INSERT' OR NEW.state NOT IN ('retiring','retired'))
       AND NOT (TG_OP = 'INSERT' AND NEW.state = 'ready'
                AND starts_with(coalesce(NEW.identity->>'Path', ''), '.memql-legacy-log/' || NEW.file_id || '/'))
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

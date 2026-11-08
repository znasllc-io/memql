-- Restore the scope guard as 20261007130000 defined it: no exception for
-- legacy log adoption.
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

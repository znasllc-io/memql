DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM installation_revision_attempts WHERE state='complete') THEN
  RAISE EXCEPTION 'completed installation history cannot be discarded';
 END IF;
END $$;
ALTER TABLE installation_revision_attempts
 DROP CONSTRAINT installation_revision_state,
 DROP CONSTRAINT installation_revision_started,
 DROP CONSTRAINT installation_revision_observation,
 DROP CONSTRAINT installation_revision_completion,
 DROP COLUMN completion_receipt,
 DROP COLUMN completed_at,
 ADD CHECK (state IN ('prepared','applying','cancelled')),
 ADD CHECK ((state='applying' AND started_at IS NOT NULL) OR (state IN ('prepared','cancelled') AND started_at IS NULL)),
 ADD CHECK ((observation_version=0 AND observation IS NULL) OR (observation_version>0 AND observation IS NOT NULL AND state='applying'));

DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM installation_revision_attempts WHERE state='closed') THEN
  RAISE EXCEPTION 'closed installation history cannot be discarded';
 END IF;
END $$;
ALTER TABLE installation_revision_attempts
 DROP CONSTRAINT installation_revision_close,
 DROP CONSTRAINT installation_revision_state,
 DROP CONSTRAINT installation_revision_started,
 DROP CONSTRAINT installation_revision_observation,
 DROP CONSTRAINT installation_revision_completion,
 DROP COLUMN closed_at,
 DROP COLUMN closed_by,
 DROP COLUMN close_reason,
 ADD CONSTRAINT installation_revision_state CHECK(state IN ('prepared','applying','reverting','cancelled','complete','rolled_back')),
 ADD CONSTRAINT installation_revision_started CHECK((state IN ('applying','reverting','complete','rolled_back') AND started_at IS NOT NULL) OR (state IN ('prepared','cancelled') AND started_at IS NULL)),
 ADD CONSTRAINT installation_revision_observation CHECK((observation_version=0 AND observation IS NULL) OR (observation_version>0 AND observation IS NOT NULL AND state IN ('applying','reverting','complete','rolled_back'))),
 ADD CONSTRAINT installation_revision_completion CHECK(
   (state IN ('complete','reverting') AND completed_at IS NOT NULL AND completion_receipt IS NOT NULL)
   OR (state='rolled_back' AND ((completed_at IS NOT NULL AND completion_receipt IS NOT NULL) OR (completed_at IS NULL AND completion_receipt IS NULL)))
   OR (state IN ('prepared','applying','cancelled') AND completed_at IS NULL AND completion_receipt IS NULL));

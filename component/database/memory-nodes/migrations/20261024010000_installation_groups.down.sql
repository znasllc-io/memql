DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM installation_application_claims) OR EXISTS(SELECT 1 FROM installation_revision_attempts WHERE plan->>'formatVersion'='2' OR state IN ('reverting','rolled_back')) OR EXISTS(SELECT 1 FROM installation_revision_rollbacks WHERE application_observations IS NOT NULL OR plan->>'baselineFence'='true') THEN
  RAISE EXCEPTION 'installation group authority and history cannot be discarded';
 END IF;
END $$;
DROP TABLE installation_application_claims;
ALTER TABLE installation_revision_rollbacks DROP COLUMN application_observations;
ALTER TABLE installation_revision_attempts
 DROP CONSTRAINT installation_revision_rollback_completion,
 DROP COLUMN application_observations,
 DROP COLUMN rollback_completion_receipt,
 DROP COLUMN rollback_completed_at,
 DROP CONSTRAINT installation_revision_state,
 DROP CONSTRAINT installation_revision_started,
 DROP CONSTRAINT installation_revision_observation,
 DROP CONSTRAINT installation_revision_completion,
 ADD CONSTRAINT installation_revision_state CHECK(state IN ('prepared','applying','cancelled','complete')),
 ADD CONSTRAINT installation_revision_started CHECK((state IN ('applying','complete') AND started_at IS NOT NULL) OR (state IN ('prepared','cancelled') AND started_at IS NULL)),
 ADD CONSTRAINT installation_revision_observation CHECK((observation_version=0 AND observation IS NULL) OR (observation_version>0 AND observation IS NOT NULL AND state IN ('applying','complete'))),
 ADD CONSTRAINT installation_revision_completion CHECK((state='complete' AND completed_at IS NOT NULL AND completion_receipt IS NOT NULL) OR (state<>'complete' AND completed_at IS NULL AND completion_receipt IS NULL));

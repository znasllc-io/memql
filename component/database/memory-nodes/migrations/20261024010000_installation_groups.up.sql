-- One installation may coordinate several existing Applications. Claims remain
-- until cancellation or verified terminal completion; elapsed time is no grant.
CREATE TABLE installation_application_claims (
 namespace text NOT NULL,
 name text NOT NULL,
 uid text NOT NULL,
 installation_id text NOT NULL REFERENCES installation_revision_heads(installation_id),
 preparation_id text NOT NULL REFERENCES installation_preparations(preparation_id),
 PRIMARY KEY(namespace,name)
);
-- Existing active single-Application preparations acquire the same exclusion.
-- Conflicting historical ownership refuses migration instead of picking a winner.
INSERT INTO installation_application_claims(namespace,name,uid,installation_id,preparation_id)
SELECT p.scope #>> '{intent,target,namespace}',p.scope #>> '{intent,target,name}',
 p.scope #>> '{intent,target,uid}',p.installation_id,p.preparation_id
FROM installation_preparations p
JOIN installation_revision_heads h ON h.installation_id=p.installation_id
WHERE h.active_preparation_id=p.preparation_id OR
 EXISTS(SELECT 1 FROM installation_revision_attempts r WHERE r.plan_id=h.active_plan_id AND r.plan #>> '{preparation,id}'=p.preparation_id);
ALTER TABLE installation_revision_attempts ADD COLUMN application_observations jsonb;
-- Forward completion remains immutable when its operator later selects rollback.
ALTER TABLE installation_revision_attempts ADD COLUMN rollback_completion_receipt jsonb;
ALTER TABLE installation_revision_attempts ADD COLUMN rollback_completed_at timestamptz;
ALTER TABLE installation_revision_rollbacks ADD COLUMN application_observations jsonb;
ALTER TABLE installation_revision_attempts
 DROP CONSTRAINT installation_revision_state,
 DROP CONSTRAINT installation_revision_started,
 DROP CONSTRAINT installation_revision_observation,
 DROP CONSTRAINT installation_revision_completion,
 ADD CONSTRAINT installation_revision_state CHECK(state IN ('prepared','applying','reverting','cancelled','complete','rolled_back')),
 ADD CONSTRAINT installation_revision_started CHECK((state IN ('applying','reverting','complete','rolled_back') AND started_at IS NOT NULL) OR (state IN ('prepared','cancelled') AND started_at IS NULL)),
 ADD CONSTRAINT installation_revision_observation CHECK((observation_version=0 AND observation IS NULL) OR (observation_version>0 AND observation IS NOT NULL AND state IN ('applying','reverting','complete','rolled_back'))),
 ADD CONSTRAINT installation_revision_completion CHECK(
   (state IN ('complete','reverting') AND completed_at IS NOT NULL AND completion_receipt IS NOT NULL)
   OR (state='rolled_back' AND ((completed_at IS NOT NULL AND completion_receipt IS NOT NULL) OR (completed_at IS NULL AND completion_receipt IS NULL)))
   OR (state IN ('prepared','applying','cancelled') AND completed_at IS NULL AND completion_receipt IS NULL)),
 ADD CONSTRAINT installation_revision_rollback_completion CHECK(
   (state='rolled_back' AND rollback_completed_at IS NOT NULL AND rollback_completion_receipt IS NOT NULL)
   OR (state<>'rolled_back' AND rollback_completed_at IS NULL AND rollback_completion_receipt IS NULL));

-- A started update whose completion evidence can no longer match (its saved
-- protected-resource digest cannot be reproduced) may be closed by the operator
-- who started it. Closing writes no completion receipt and claims no
-- verification: it records who closed it, when and why, and releases the
-- installation head. Only an applying attempt can be closed, so a closed row was
-- started and never completed; the closing operator is always its requester.
ALTER TABLE installation_revision_attempts ADD COLUMN closed_at timestamptz;
ALTER TABLE installation_revision_attempts ADD COLUMN closed_by text;
ALTER TABLE installation_revision_attempts ADD COLUMN close_reason text;
ALTER TABLE installation_revision_attempts
 DROP CONSTRAINT installation_revision_state,
 DROP CONSTRAINT installation_revision_started,
 DROP CONSTRAINT installation_revision_observation,
 DROP CONSTRAINT installation_revision_completion,
 ADD CONSTRAINT installation_revision_state CHECK(state IN ('prepared','applying','reverting','cancelled','complete','rolled_back','closed')),
 ADD CONSTRAINT installation_revision_started CHECK((state IN ('applying','reverting','complete','rolled_back','closed') AND started_at IS NOT NULL) OR (state IN ('prepared','cancelled') AND started_at IS NULL)),
 ADD CONSTRAINT installation_revision_observation CHECK((observation_version=0 AND observation IS NULL) OR (observation_version>0 AND observation IS NOT NULL AND state IN ('applying','reverting','complete','rolled_back','closed'))),
 ADD CONSTRAINT installation_revision_completion CHECK(
   (state IN ('complete','reverting') AND completed_at IS NOT NULL AND completion_receipt IS NOT NULL)
   OR (state='rolled_back' AND ((completed_at IS NOT NULL AND completion_receipt IS NOT NULL) OR (completed_at IS NULL AND completion_receipt IS NULL)))
   OR (state IN ('prepared','applying','cancelled','closed') AND completed_at IS NULL AND completion_receipt IS NULL)),
 ADD CONSTRAINT installation_revision_close CHECK(
   (state='closed' AND closed_at IS NOT NULL AND closed_by IS NOT NULL AND closed_by=requested_by
    AND close_reason IS NOT NULL AND char_length(close_reason) BETWEEN 1 AND 500)
   OR (state<>'closed' AND closed_at IS NULL AND closed_by IS NULL AND close_reason IS NULL));

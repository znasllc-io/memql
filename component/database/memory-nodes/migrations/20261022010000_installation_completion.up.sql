-- A completed update has native fresh workload, continuity and preservation proof.
ALTER TABLE installation_revision_attempts ADD COLUMN completion_receipt jsonb;
ALTER TABLE installation_revision_attempts ADD COLUMN completed_at timestamptz;
DO $$ DECLARE item record; BEGIN
  FOR item IN SELECT conname FROM pg_constraint
    WHERE conrelid='installation_revision_attempts'::regclass AND contype='c'
      AND pg_get_constraintdef(oid) LIKE '%state%'
  LOOP EXECUTE format('ALTER TABLE installation_revision_attempts DROP CONSTRAINT %I', item.conname); END LOOP;
END $$;
ALTER TABLE installation_revision_attempts
 ADD CONSTRAINT installation_revision_state CHECK (state IN ('prepared','applying','cancelled','complete')),
 ADD CONSTRAINT installation_revision_started CHECK ((state IN ('applying','complete') AND started_at IS NOT NULL) OR (state IN ('prepared','cancelled') AND started_at IS NULL)),
 ADD CONSTRAINT installation_revision_observation CHECK ((observation_version=0 AND observation IS NULL) OR (observation_version>0 AND observation IS NOT NULL AND state IN ('applying','complete'))),
 ADD CONSTRAINT installation_revision_completion CHECK ((state='complete' AND completed_at IS NOT NULL AND completion_receipt IS NOT NULL) OR (state<>'complete' AND completed_at IS NULL AND completion_receipt IS NULL));

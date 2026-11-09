DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM installation_revision_attempts WHERE owned_successes IS NOT NULL OR withdrawn_restorations IS NOT NULL)
    OR EXISTS (SELECT 1 FROM installation_revision_rollbacks WHERE replanned_intents IS NOT NULL) THEN
  RAISE EXCEPTION 'installation owned-success, withdrawn-restoration and replanned-reversal history cannot be discarded';
 END IF;
END $$;
ALTER TABLE installation_revision_rollbacks
 DROP CONSTRAINT installation_rollback_replanned_intents,
 DROP COLUMN replanned_intents;
ALTER TABLE installation_revision_attempts
 DROP CONSTRAINT installation_revision_owned_successes,
 DROP CONSTRAINT installation_revision_withdrawn_restorations,
 DROP COLUMN owned_successes,
 DROP COLUMN withdrawn_restorations;

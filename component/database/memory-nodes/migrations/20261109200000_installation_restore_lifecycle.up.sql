-- Records that keep a healthy installation from stranding a plan.
--
-- owned_successes: the journal's record that a plan's own Argo operation for
-- one intent was observed finishing Succeeded, keyed by intent digest, with the
-- revision and source it synced. Argo keeps only the last operation in
-- status.operationState, so one later sync (a hand sync of a manual-sync
-- Application, or self-heal) otherwise erased the only evidence completion and
-- restoration accept. It is written only while the plan is active and kept for
-- every later state.
--
-- withdrawn_restorations: restorations of a completed update that wrote no Argo
-- intent and were returned to the completed plan, either automatically after a
-- check refused them or by their operator's close with a reason. Each entry
-- keeps who, when and why. The reserved reversal itself is deleted at that
-- point, because a completed plan carries no reversal.
--
-- installation_revision_rollbacks.replanned_intents: a member's reversal planned
-- again, keyed by member, when its saved intent can no longer be admitted (its
-- Kubernetes history is gone, or its strict generation fence can never pass)
-- and nothing of it can still reach the cluster. A reserved reversal used to be
-- planned once, so such a member held the installation for good.
--
-- All are new columns rather than fields in existing JSON columns: every
-- existing column is decoded strictly, and an engine that predates these
-- fields must still read the rows (a release rolled back by hand).
ALTER TABLE installation_revision_attempts ADD COLUMN owned_successes jsonb;
ALTER TABLE installation_revision_attempts ADD COLUMN withdrawn_restorations jsonb;
ALTER TABLE installation_revision_attempts
 ADD CONSTRAINT installation_revision_owned_successes CHECK(
   owned_successes IS NULL
   OR (jsonb_typeof(owned_successes)='object' AND state IN ('applying','reverting','complete','rolled_back','closed'))),
 ADD CONSTRAINT installation_revision_withdrawn_restorations CHECK(
   withdrawn_restorations IS NULL
   OR (jsonb_typeof(withdrawn_restorations)='array' AND state IN ('complete','reverting','rolled_back')));
ALTER TABLE installation_revision_rollbacks ADD COLUMN replanned_intents jsonb;
ALTER TABLE installation_revision_rollbacks
 ADD CONSTRAINT installation_rollback_replanned_intents CHECK(
   replanned_intents IS NULL OR (jsonb_typeof(replanned_intents)='object' AND started_at IS NOT NULL));

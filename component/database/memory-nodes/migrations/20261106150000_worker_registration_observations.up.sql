-- A machine's live reports are current observations, not business versions.
--
-- v1:worker:registration's observation list was written before three fields
-- that are re-reported on the same cadence as lastSeenAt: sharingActivity (the
-- stream-holding replica republishes it every 15 s with a fresh observedAt,
-- component/worker/sharing_activity.go), and capacity and setup (both carry a
-- reportedAt from the agent's clock and ride every heartbeat flush,
-- component/worker/server.go). A write changing only those still appended a
-- whole new version of the registration: about 1,400 a day for five machines
-- from the sharing publisher alone on production (2026-11-06).
--
-- This re-creates the trigger function with those three fields added to the
-- worker list. Every other branch, lock, comparison and list is unchanged from
-- 20261013060000_reconcile_observations. A business change (rename, sharing,
-- permissions, labels, revocation) still appends a version and clears the
-- observation in the same transaction.
--
-- NO READER HANDSHAKE IS NEEDED, unlike the two earlier liveness migrations.
-- Their handshakes exist because a reader must know which CONCEPTS have rows in
-- memory_liveness. The concept set is unchanged here: readers already merge
-- memory_liveness into every v1:worker:registration read
-- (memorynodes.HasLiveness and ObservedRowsSQL in
-- component/database/memory-nodes/liveness.go), so a row observed under the
-- longer field list is visible to every engine that can read the shorter one.
-- The switch stays memory_liveness_storage.enabled: while it is off this
-- function returns NEW for the concept, exactly as before.
--
-- INSTANT: a function replace. No table is read or rewritten and the trigger
-- keeps the function's identity, so the migration lock is held for no longer
-- than the catalog update.
CREATE OR REPLACE FUNCTION memory_node_store_liveness() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
 prior record;
 fields text[];
 prior_schema jsonb;
 next_schema jsonb;
BEGIN
 IF NEW.concept IN ('v1:platform:customDomain','v1:platform:syncState') THEN
   IF NOT (SELECT enabled FROM memory_reconcile_observation_storage WHERE singleton) THEN RETURN NEW; END IF;
 ELSIF NEW.concept IN ('v1:worker:registration','v1:cluster:node','v1:platform:moduleReadiness') THEN
   IF NOT (SELECT enabled FROM memory_liveness_storage WHERE singleton) THEN RETURN NEW; END IF;
 ELSE RETURN NEW;
 END IF;
 -- Same transaction lock as the engine's observed-version fence. It covers
 -- the storage decision too, including direct maintenance inserts.
 PERFORM pg_advisory_xact_lock(1296387415,hashtext(NEW.id));
 SELECT * INTO prior FROM "MemoryNodesObserved" WHERE id=NEW.id ORDER BY "createdAt" DESC LIMIT 1;
 IF NOT FOUND THEN RETURN NEW; END IF;
 -- Backdated restores remain history and must not clear a newer observation.
 IF NEW."createdAt" <= prior."createdAt" THEN RETURN NEW; END IF;
 fields := CASE NEW.concept
  WHEN 'v1:worker:registration' THEN ARRAY['lastSeenAt','lastConnectedFromIP','connectedNodeId','activeCount','hardware','rttMs','rttAt','clockSkewMs','credentialExpiresAt','sharingActivity','capacity','setup']
  WHEN 'v1:cluster:node' THEN ARRAY['health','lastSeen','mesh']
  WHEN 'v1:platform:customDomain' THEN ARRAY['lastCheckedAt']
  WHEN 'v1:platform:syncState' THEN ARRAY['lastReconcileAt','lastAttemptAt']
  ELSE ARRAY['state','reason','core','optional','dismissable','lanes','reportedAt'] END;
 prior_schema := COALESCE((SELECT document FROM memory_node_schemas WHERE fingerprint=substr(prior.schema->>'$ref',14)),prior.schema);
 next_schema := COALESCE((SELECT document FROM memory_node_schemas WHERE fingerprint=substr(NEW.schema->>'$ref',14)),NEW.schema);
 IF NEW.concept=prior.concept AND NEW.type=prior.type AND next_schema=prior_schema
    AND (NEW.payload-fields)=(prior.payload-fields) THEN
   INSERT INTO memory_liveness (id,"createdAt","createdBy",concept,type,schema,payload,metadata,provenance)
   VALUES (NEW.id,NEW."createdAt",NEW."createdBy",NEW.concept,NEW.type,NEW.schema,NEW.payload,NEW.metadata,NEW.provenance)
   ON CONFLICT (id) DO UPDATE SET "createdAt"=excluded."createdAt","createdBy"=excluded."createdBy",
     concept=excluded.concept,type=excluded.type,schema=excluded.schema,payload=excluded.payload,
     metadata=excluded.metadata,provenance=excluded.provenance;
   RETURN NULL;
 END IF;
 -- A business edit or tombstone supersedes the observation atomically.
 DELETE FROM memory_liveness WHERE id=NEW.id;
 RETURN NEW;
END
$$;

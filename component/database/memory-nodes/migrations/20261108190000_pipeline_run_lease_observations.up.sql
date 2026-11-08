-- A pipeline run's lease renewal is a current observation, not a new version.
--
-- The pipelines driver renews its lease on the v1:pipelines:run row every 30 s
-- (renew in component/pipelinerun/lease.go writes driverHeartbeatAt and nothing
-- else). Each renewal appended a whole version of the row, the run's ~14 KB
-- policySnapshot included: about three quarters of the 3,831 versions of 30
-- runs written in one day on production (2026-11-06), about 17 KB each. Since
-- 0.31.18 superseded versions expire after a day (@retention on the concept),
-- so what the renewals cost is write volume and the last day of versions in
-- each chunk that closes.
--
-- With this migration a write that changes only driverHeartbeatAt replaces the
-- run's one row in memory_liveness, as worker, cluster-node, readiness,
-- custom-domain and sync-state observations already do. Every other change --
-- the open, a claim (a new driverNodeId and driverLeaseId), the stage table,
-- the work run, a cancel request, the conclusion -- still appends a version,
-- carries the observed heartbeat into it through the read-merge, and clears the
-- observation in the same transaction. The lease stays the authority: every
-- engine read merges memory_liveness, and the row-version fence compares the
-- newest createdAt over MemoryNodes and memory_liveness together
-- (ObservedRowsSQL in component/database/memory-nodes/liveness.go), so a
-- decision made before an observed renewal is refused.
--
-- ITS OWN READER HANDSHAKE, like 20261013060000_reconcile_observations: a
-- reader must list the concept in memorynodes.HasLiveness, or it enumerates the
-- run's latest keys from MemoryNodes alone and reports every renewal to a held
-- journal as a business write. memory_pipeline_run_observation_storage stays
-- off until every non-stopped node has advertised in
-- memory_pipeline_run_observation_readers within 30 s
-- (EnablePipelineRunObservationsWhenReady); a reader of the two earlier
-- handshakes does not count. While it is off this function returns NEW for the
-- concept, exactly as before. Once on it stays on.
--
-- RETENTION is unchanged in effect. The declared keep-latest sweep
-- (component/retention) judges the newest version within MemoryNodes, so it
-- keeps the newest business version; the prune trigger below deletes an
-- observation only when no version of its run remains, which whole-run
-- operational retention (integrations/work) and nothing else does.
--
-- LOCKS. Replacing the prune trigger takes ACCESS EXCLUSIVE on "MemoryNodes" and
-- its chunks, and the concept CHECK takes it on memory_liveness; every engine
-- read touches both. Either queued behind a long read would queue every later
-- read and write behind itself, so the whole file runs under a 5 s
-- lock_timeout, as the latest-row index's DROP does
-- (component/database/latest_row_index.go): a busy moment fails the migration,
-- nothing is applied, and the next attempt tries again. The trigger goes
-- first: readers lock "MemoryNodes" before memory_liveness (the union's
-- order) and writers do too (the insert, then this trigger's upsert), so
-- taking the tables in that order cannot deadlock with either, and once
-- "MemoryNodes" is held no reader is left holding memory_liveness. Once held,
-- every statement here is a catalog change or a scan of the small observation
-- table.
SET LOCAL lock_timeout = '5s';

CREATE TABLE memory_pipeline_run_observation_storage (
 singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
 enabled boolean NOT NULL DEFAULT false,
 next_check_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO memory_pipeline_run_observation_storage DEFAULT VALUES;
CREATE TABLE memory_pipeline_run_observation_readers (node_id text PRIMARY KEY,seen_at timestamptz NOT NULL);

-- Every branch, lock, comparison and list other than the pipeline run's is
-- unchanged from 20261106150000_worker_registration_observations.
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
 ELSIF NEW.concept='v1:pipelines:run' THEN
   IF NOT (SELECT enabled FROM memory_pipeline_run_observation_storage WHERE singleton) THEN RETURN NEW; END IF;
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
  WHEN 'v1:pipelines:run' THEN ARRAY['driverHeartbeatAt']
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

-- Extend the compression-compatible cleanup trigger to the run's observation.
DROP TRIGGER memory_node_prune_liveness ON "MemoryNodes";
CREATE TRIGGER memory_node_prune_liveness AFTER DELETE ON "MemoryNodes"
 FOR EACH ROW WHEN (OLD.concept IN
   ('v1:worker:registration','v1:cluster:node','v1:platform:moduleReadiness','v1:platform:customDomain','v1:platform:syncState','v1:pipelines:run'))
 EXECUTE FUNCTION memory_node_prune_liveness();

ALTER TABLE memory_liveness DROP CONSTRAINT memory_liveness_concept_check;
ALTER TABLE memory_liveness ADD CONSTRAINT memory_liveness_concept_check CHECK
 (concept IN ('v1:worker:registration','v1:cluster:node','v1:platform:moduleReadiness','v1:platform:customDomain','v1:platform:syncState','v1:pipelines:run'));

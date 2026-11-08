-- Restores 20261106150000_worker_registration_observations: every lease
-- renewal appends a version of the v1:pipelines:run row again.
--
-- Nothing is lost. Each run's current observation is folded back first, as the
-- run's newest version exactly as stored (same createdAt, payload and schema
-- reference), then removed from memory_liveness. A reader of any release then
-- reads the heartbeat it last saw, and a running driver's row-version fence,
-- taken at that createdAt, still matches.
--
-- LOCKS, in the order every engine reader and writer takes them: dropping the
-- prune trigger takes ACCESS EXCLUSIVE on "MemoryNodes" first, so no renewal can
-- write an observation between the fold and the delete; memory_liveness second.
-- Under a 5 s lock_timeout, as the up migration: a busy moment fails this file
-- and changes nothing. An engine that still advertises the dropped handshake
-- logs a paused schema conversion each minute until it is replaced by one
-- that does not (SchemaCompactor.run), so roll the engines back with it.
SET LOCAL lock_timeout = '5s';

DROP TRIGGER memory_node_prune_liveness ON "MemoryNodes";

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

INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",concept,type,schema,payload,metadata,provenance)
SELECT id,"createdAt","createdBy",concept,type,schema,payload,metadata,provenance
FROM memory_liveness WHERE concept='v1:pipelines:run'
ON CONFLICT DO NOTHING;
DELETE FROM memory_liveness WHERE concept='v1:pipelines:run';

ALTER TABLE memory_liveness DROP CONSTRAINT memory_liveness_concept_check;
ALTER TABLE memory_liveness ADD CONSTRAINT memory_liveness_concept_check CHECK
 (concept IN ('v1:worker:registration','v1:cluster:node','v1:platform:moduleReadiness','v1:platform:customDomain','v1:platform:syncState'));

CREATE TRIGGER memory_node_prune_liveness AFTER DELETE ON "MemoryNodes"
 FOR EACH ROW WHEN (OLD.concept IN
   ('v1:worker:registration','v1:cluster:node','v1:platform:moduleReadiness','v1:platform:customDomain','v1:platform:syncState'))
 EXECUTE FUNCTION memory_node_prune_liveness();

DROP TABLE memory_pipeline_run_observation_readers;
DROP TABLE memory_pipeline_run_observation_storage;

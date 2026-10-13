-- Checking again is a current observation, not another business version.
-- This feature has its own reader handshake: an older liveness-capable engine
-- must not enable it before all readers recognize these two additional concepts.
CREATE TABLE memory_reconcile_observation_storage (
 singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
 enabled boolean NOT NULL DEFAULT false,
 next_check_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO memory_reconcile_observation_storage DEFAULT VALUES;
CREATE TABLE memory_reconcile_observation_readers (node_id text PRIMARY KEY,seen_at timestamptz NOT NULL);
ALTER TABLE memory_liveness DROP CONSTRAINT memory_liveness_concept_check;
ALTER TABLE memory_liveness ADD CONSTRAINT memory_liveness_concept_check CHECK
 (concept IN ('v1:worker:registration','v1:cluster:node','v1:platform:moduleReadiness','v1:platform:customDomain','v1:platform:syncState'));

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
  WHEN 'v1:worker:registration' THEN ARRAY['lastSeenAt','lastConnectedFromIP','connectedNodeId','activeCount','hardware','rttMs','rttAt','clockSkewMs','credentialExpiresAt']
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

CREATE OR REPLACE FUNCTION memory_node_prune_liveness() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
 DELETE FROM memory_liveness l USING (
   SELECT DISTINCT id FROM old_rows WHERE concept IN
     ('v1:worker:registration','v1:cluster:node','v1:platform:moduleReadiness','v1:platform:customDomain','v1:platform:syncState')
 ) gone WHERE l.id=gone.id AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" n WHERE n.id=l.id);
 RETURN NULL;
END
$$;

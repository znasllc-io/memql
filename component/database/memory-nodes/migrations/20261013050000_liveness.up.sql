-- Current observations have one mutable row per record. Business changes still
-- append to MemoryNodes; the current observation participates in normal reads.
CREATE TABLE memory_liveness (LIKE "MemoryNodes" INCLUDING DEFAULTS);
ALTER TABLE memory_liveness ADD PRIMARY KEY (id);
ALTER TABLE memory_liveness ADD CHECK (concept IN ('v1:worker:registration','v1:cluster:node','v1:platform:moduleReadiness'));
CREATE INDEX memory_liveness_concept_id_at ON memory_liveness (concept,id,"createdAt" DESC);
CREATE TRIGGER memory_liveness_shared_schema BEFORE INSERT OR UPDATE OF schema
 ON memory_liveness FOR EACH ROW EXECUTE FUNCTION memory_node_compact_schema();

CREATE VIEW "MemoryNodesObserved" AS
 SELECT * FROM "MemoryNodes"
 UNION ALL SELECT * FROM memory_liveness;

CREATE TABLE memory_liveness_storage (
 singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
 enabled boolean NOT NULL DEFAULT false,
 next_check_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO memory_liveness_storage DEFAULT VALUES;
CREATE TABLE memory_liveness_readers (node_id text PRIMARY KEY,seen_at timestamptz NOT NULL);

CREATE FUNCTION memory_node_store_liveness() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
DECLARE
 prior record;
 fields text[];
 prior_schema jsonb;
 next_schema jsonb;
BEGIN
 IF NEW.concept NOT IN ('v1:worker:registration','v1:cluster:node','v1:platform:moduleReadiness')
    OR NOT (SELECT enabled FROM memory_liveness_storage WHERE singleton) THEN
   RETURN NEW;
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
CREATE TRIGGER memory_node_liveness BEFORE INSERT ON "MemoryNodes"
 FOR EACH ROW EXECUTE FUNCTION memory_node_store_liveness();

-- Retiring a node removes its current observation together with its history.
-- Statement transition tables deduplicate ids before probing remaining rows.
CREATE FUNCTION memory_node_prune_liveness() RETURNS trigger
LANGUAGE plpgsql SET search_path FROM CURRENT AS $$
BEGIN
 DELETE FROM memory_liveness l USING (
   SELECT DISTINCT id FROM old_rows WHERE concept IN
     ('v1:worker:registration','v1:cluster:node','v1:platform:moduleReadiness')
 ) gone WHERE l.id=gone.id AND NOT EXISTS (SELECT 1 FROM "MemoryNodes" n WHERE n.id=l.id);
 RETURN NULL;
END
$$;
CREATE TRIGGER memory_node_prune_liveness AFTER DELETE ON "MemoryNodes"
 REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION memory_node_prune_liveness();

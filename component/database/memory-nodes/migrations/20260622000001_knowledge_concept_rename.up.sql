-- Knowledge concept namespace fix (memql#1960): v1:common:* -> v1:knowledge:*
--
-- Three knowledge concepts were mis-tagged @namespace("common") while their
-- source lives in dsl/knowledge/, so they emitted v1:common:* ids:
--   v1:common:documentChunk    -> v1:knowledge:documentChunk
--   v1:common:knowledgeBridge  -> v1:knowledge:knowledgeBridge
--   v1:common:knowledgeDomain  -> v1:knowledge:knowledgeDomain
--
-- The DSL @namespace flip changes the canonical id at the source; this migration
-- rewrites EXISTING rows to match: (1) the three concepts' own rows (concept +
-- id prefix), and (2) every foreign-key reference to those ids carried in OTHER
-- rows' payloads (agent.capabilities skill domainIds[], documentChunk.domainId,
-- domainEntitySchema.domainId, entityIndex.domainId/sourceItemId,
-- knowledgeBridge.domainIds[], imageRegion.embeddedAsChunkId, ...). A blanket
-- payload text-replace catches every FK field regardless of path.
--
-- Idempotent: re-running finds no remaining v1:common:{documentChunk,
-- knowledgeBridge,knowledgeDomain} and is a no-op. Pre-release / no-compat: the
-- old ids are not retained.
--
-- NOTE: no leading `--bun:split` before this first statement -- bun runs the
-- pre-first-split segment as its own query, and a comment-only segment makes the
-- pgdriver log `query is empty` on every migrate (memql#2275). The header shares
-- this first statement's segment.

-- (1) Own rows: concept column + the id prefix, for each of the three concepts.
UPDATE "MemoryNodes" SET
  concept = 'v1:knowledge:documentChunk',
  id      = replace(id, 'v1:common:documentChunk', 'v1:knowledge:documentChunk')
WHERE concept = 'v1:common:documentChunk';

--bun:split

UPDATE "MemoryNodes" SET
  concept = 'v1:knowledge:knowledgeBridge',
  id      = replace(id, 'v1:common:knowledgeBridge', 'v1:knowledge:knowledgeBridge')
WHERE concept = 'v1:common:knowledgeBridge';

--bun:split

UPDATE "MemoryNodes" SET
  concept = 'v1:knowledge:knowledgeDomain',
  id      = replace(id, 'v1:common:knowledgeDomain', 'v1:knowledge:knowledgeDomain')
WHERE concept = 'v1:common:knowledgeDomain';

--bun:split

-- (2) Foreign-key references in any row's payload. The id strings are specific
-- (full concept prefix), so a text replace rewrites the prefix wherever it
-- appears (scalars + JSONB arrays) without touching unrelated content.
UPDATE "MemoryNodes" SET
  payload = replace(payload::text, 'v1:common:documentChunk', 'v1:knowledge:documentChunk')::jsonb
WHERE payload::text LIKE '%v1:common:documentChunk%';

--bun:split

UPDATE "MemoryNodes" SET
  payload = replace(payload::text, 'v1:common:knowledgeBridge', 'v1:knowledge:knowledgeBridge')::jsonb
WHERE payload::text LIKE '%v1:common:knowledgeBridge%';

--bun:split

UPDATE "MemoryNodes" SET
  payload = replace(payload::text, 'v1:common:knowledgeDomain', 'v1:knowledge:knowledgeDomain')::jsonb
WHERE payload::text LIKE '%v1:common:knowledgeDomain%';

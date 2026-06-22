-- Reverse of the knowledge concept namespace fix (memql#1960):
-- v1:knowledge:* -> v1:common:* for the three migrated concepts. Idempotent.

--bun:split

UPDATE "MemoryNodes" SET
  concept = 'v1:common:documentChunk',
  id      = replace(id, 'v1:knowledge:documentChunk', 'v1:common:documentChunk')
WHERE concept = 'v1:knowledge:documentChunk';

--bun:split

UPDATE "MemoryNodes" SET
  concept = 'v1:common:knowledgeBridge',
  id      = replace(id, 'v1:knowledge:knowledgeBridge', 'v1:common:knowledgeBridge')
WHERE concept = 'v1:knowledge:knowledgeBridge';

--bun:split

UPDATE "MemoryNodes" SET
  concept = 'v1:common:knowledgeDomain',
  id      = replace(id, 'v1:knowledge:knowledgeDomain', 'v1:common:knowledgeDomain')
WHERE concept = 'v1:knowledge:knowledgeDomain';

--bun:split

UPDATE "MemoryNodes" SET
  payload = replace(payload::text, 'v1:knowledge:documentChunk', 'v1:common:documentChunk')::jsonb
WHERE payload::text LIKE '%v1:knowledge:documentChunk%';

--bun:split

UPDATE "MemoryNodes" SET
  payload = replace(payload::text, 'v1:knowledge:knowledgeBridge', 'v1:common:knowledgeBridge')::jsonb
WHERE payload::text LIKE '%v1:knowledge:knowledgeBridge%';

--bun:split

UPDATE "MemoryNodes" SET
  payload = replace(payload::text, 'v1:knowledge:knowledgeDomain', 'v1:common:knowledgeDomain')::jsonb
WHERE payload::text LIKE '%v1:knowledge:knowledgeDomain%';

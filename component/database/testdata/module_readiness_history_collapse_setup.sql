-- A transaction-local table shadows the live one, so fixture ids matching real
-- row ids never touch the cluster's data.
CREATE TEMP TABLE "MemoryNodes" (id text, concept text, "createdAt" timestamptz, payload jsonb) ON COMMIT DROP;

INSERT INTO "MemoryNodes" (id, concept, "createdAt", payload) VALUES
-- Three restatements of one verdict that never changed: the churn this
-- collapses. Deliberately out of insertion order, so a migration that kept
-- "the last row inserted" rather than the newest createdAt fails here.
('v1:platform:moduleReadiness:ai--bff-a', 'v1:platform:moduleReadiness', '2026-09-13T10:00:00Z', '{"module":"ai","nodeId":"bff-a","state":"configured"}'),
('v1:platform:moduleReadiness:ai--bff-a', 'v1:platform:moduleReadiness', '2026-09-13T10:30:00Z', '{"module":"ai","nodeId":"bff-a","state":"configured"}'),
('v1:platform:moduleReadiness:ai--bff-a', 'v1:platform:moduleReadiness', '2026-09-13T10:15:00Z', '{"module":"ai","nodeId":"bff-a","state":"configured"}'),

-- The same node's OTHER module keeps its own newest: the collapse is per row,
-- not per node.
('v1:platform:moduleReadiness:storage--bff-a', 'v1:platform:moduleReadiness', '2026-09-13T09:00:00Z', '{"module":"storage","nodeId":"bff-a","state":"unconfigured"}'),
('v1:platform:moduleReadiness:storage--bff-a', 'v1:platform:moduleReadiness', '2026-09-13T09:05:00Z', '{"module":"storage","nodeId":"bff-a","state":"configured"}'),

-- A row with exactly one version is already collapsed and must not move.
('v1:platform:moduleReadiness:ai--edge-b', 'v1:platform:moduleReadiness', '2026-09-13T11:00:00Z', '{"module":"ai","nodeId":"edge-b","state":"unknown","reason":"fleetReadFailed"}'),

-- SCOPED BY CONCEPT. Another concept with a versioned history, under an id
-- shaped like a readiness row's, keeps every version: `module` and `nodeId`
-- are names a product bundle may declare on a concept this repo never saw.
('v1:platform:moduleReadiness:ai--bff-a', 'v1:product:moduleReadiness', '2026-09-13T10:00:00Z', '{"module":"ai","nodeId":"bff-a"}'),
('v1:platform:moduleReadiness:ai--bff-a', 'v1:product:moduleReadiness', '2026-09-13T10:30:00Z', '{"module":"ai","nodeId":"bff-a"}'),
('v1:cluster:node:bff-a', 'v1:cluster:node', '2026-09-13T10:00:00Z', '{"health":"healthy"}'),
('v1:cluster:node:bff-a', 'v1:cluster:node', '2026-09-13T10:30:00Z', '{"health":"stopped"}');

-- The expected result, computed independently of the migration's own clause:
-- every row of every other concept, plus the newest version of each readiness
-- row. Restating the WHERE would only prove the statement equals itself.
CREATE TEMP TABLE expected_rows ON COMMIT DROP AS
SELECT * FROM "MemoryNodes" WHERE concept <> 'v1:platform:moduleReadiness'
UNION ALL
SELECT m.* FROM "MemoryNodes" m
 WHERE m.concept = 'v1:platform:moduleReadiness'
   AND m."createdAt" = (
     SELECT MAX(x."createdAt") FROM "MemoryNodes" x
      WHERE x.concept = 'v1:platform:moduleReadiness' AND x.id = m.id
   );

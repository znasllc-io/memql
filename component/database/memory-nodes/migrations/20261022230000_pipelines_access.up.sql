-- Moving a control must move its saved allow/deny decisions as well. Copy the
-- latest version (including an inactive decision), never resurrect old grants.
-- Existing decisions at the destination win, including explicit revocations.
WITH mapping(old_resource, new_resource) AS (VALUES
  ('app:cluster/releases', 'app:pipelines/releases'),
  ('app:deployables/sources', 'app:pipelines/repositories'),
  ('app:deployables/connect', 'app:pipelines/connect'),
  ('app:deployables/rerun', 'app:pipelines/rerun'),
  ('app:deployables/cancel', 'app:pipelines/cancel'),
  ('app:deployables/channels', 'app:pipelines/channels'),
  ('app:deployables/runs', 'app:pipelines/runs'),
  ('app:deployables', 'app:pipelines')
), latest AS (
  SELECT DISTINCT ON (id) * FROM "MemoryNodes"
  WHERE concept IN ('v1:rbac:grant', 'v1:rbac:capability')
  ORDER BY id, "createdAt" DESC
), moved AS (
  SELECT src.*, mapping.new_resource,
    CASE WHEN src.concept = 'v1:rbac:grant' THEN
      'v1:rbac:grant:' || encode(sha256(convert_to(
        encode(sha256(convert_to(trim(src.payload->>'subjectKind'), 'UTF8')), 'hex') ||
        encode(sha256(convert_to(trim(src.payload->>'subjectId'), 'UTF8')), 'hex') ||
        encode(sha256(convert_to(trim(src.payload->>'verb'), 'UTF8')), 'hex') ||
        encode(sha256(convert_to(mapping.new_resource, 'UTF8')), 'hex'), 'UTF8')), 'hex')
    ELSE 'v1:rbac:capability:pipelines-' || encode(sha256(convert_to(src.id || ':' || mapping.new_resource, 'UTF8')), 'hex') END AS destination_id
  FROM latest src JOIN mapping ON src.payload->>'resourceType' = mapping.old_resource
  WHERE src.payload->>'predefined' IS DISTINCT FROM 'true'
    -- The old app also contains hosted products. Its explicit denials carry
    -- across, but broad product visibility alone does not grant delivery access.
    AND (mapping.old_resource <> 'app:deployables' OR src.payload->>'effect' = 'deny')
)
INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, metadata, "type", concept, provenance)
SELECT destination_id, "createdAt", "createdBy", schema,
  jsonb_set(payload, '{resourceType}', to_jsonb(new_resource)), metadata, "type", concept, provenance
FROM moved
WHERE NOT EXISTS (SELECT 1 FROM latest dest WHERE dest.id = moved.destination_id)
ON CONFLICT DO NOTHING;
--bun:split

-- These parts have moved entirely. Keep the original records for audit, but
-- stop obsolete predefined seeds from granting a second control surface.
UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{active}', 'false'::jsonb)
WHERE concept = 'v1:rbac:capability'
  AND payload->>'predefined' = 'true'
  AND payload->>'resourceType' IN (
    'app:cluster/releases', 'app:deployables/sources', 'app:deployables/connect', 'app:deployables/rerun',
    'app:deployables/cancel', 'app:deployables/channels', 'app:deployables/runs'
  );

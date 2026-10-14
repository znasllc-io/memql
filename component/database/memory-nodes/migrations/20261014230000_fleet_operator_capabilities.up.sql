-- Removed seeds must also stop granting access on existing installations.
-- Keep their history, deactivating only the retired predefined Fleet grants.
UPDATE "MemoryNodes"
SET payload = jsonb_set(payload, '{active}', 'false'::jsonb)
WHERE concept = 'v1:rbac:capability'
  AND payload->>'predefined' = 'true'
  AND payload->>'roleSlug' IN ('admin', 'user', 'viewer')
  AND payload->>'resourceType' IN ('app:fleet', 'app:fleet/logs')
  AND payload->>'verb' = 'read'
  AND payload->>'active' IS DISTINCT FROM 'false';

-- Removing the seed does not remove previously materialized site versions.
-- Retire only the platform's obsolete bundled site, across its entire history.
-- Keep customer sites (including ones called portal), other bundles and owners.
DELETE FROM "MemoryNodes"
WHERE concept = 'v1:platform:site'
  AND id = 'v1:platform:site:portal'
  AND payload->>'systemOwned' = 'true'
  AND payload->>'bundleRef' = 'file:///app/portal'
  AND COALESCE(payload->>'ownerUserId', '') = '';

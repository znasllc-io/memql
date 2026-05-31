-- Reverse of 20260520000000_slug_kebab_case.up.sql -- rewrites
-- catalog slug fields from kebab-case back to snake_case. Symmetric
-- because both forms are distinguishable from `:` separators and
-- free-form prose.
--
-- NOTE: no `--bun:split` before the first statement -- a leading
-- comment-only segment makes bun emit an empty query. See the .up.sql
-- header + memql#570.

CREATE OR REPLACE FUNCTION snake_slug(s text) RETURNS text AS $$
  SELECT CASE
    WHEN s IS NULL THEN NULL
    ELSE REPLACE(s, '-', '_')
  END;
$$ LANGUAGE SQL IMMUTABLE;

--bun:split

CREATE OR REPLACE FUNCTION snake_slug_array(arr jsonb) RETURNS jsonb AS $$
  SELECT CASE
    WHEN arr IS NULL OR jsonb_typeof(arr) <> 'array' THEN arr
    ELSE (
      SELECT jsonb_agg(snake_slug(elem #>> '{}'))
      FROM jsonb_array_elements(arr) AS elem
    )
  END;
$$ LANGUAGE SQL IMMUTABLE;

--bun:split

UPDATE "MemoryNodes" SET
  id = snake_slug(id),
  payload = jsonb_set(
    jsonb_set(
      jsonb_set(
        jsonb_set(payload,
          '{slug}', to_jsonb(snake_slug(payload->>'slug'))),
        '{lockedDomainIds}', snake_slug_array(payload->'lockedDomainIds')),
      '{defaultDomainIds}', snake_slug_array(payload->'defaultDomainIds')),
    '{lockedToolSlugs}', snake_slug_array(payload->'lockedToolSlugs'))
WHERE concept = 'v1:agents:agentRole';

--bun:split

UPDATE "MemoryNodes" SET
  payload = jsonb_set(
    jsonb_set(
      jsonb_set(payload,
        '{roleSlug}', to_jsonb(snake_slug(payload->>'roleSlug'))),
      '{capabilities, tools}', snake_slug_array(payload#>'{capabilities, tools}')),
    '{capabilities, domains}', snake_slug_array(payload#>'{capabilities, domains}'))
WHERE concept = 'v1:agents:agent';

--bun:split

UPDATE "MemoryNodes" SET
  id = regexp_replace(id, '^(.*:v1:common:knowledgeDomain:)(.*)$',
                      '\1' || snake_slug(substring(id from '[^:]+$'))),
  payload = jsonb_set(
    jsonb_set(
      jsonb_set(
        jsonb_set(payload,
          '{id}', to_jsonb(snake_slug(payload->>'id'))),
        '{relevantForRoles}', snake_slug_array(payload->'relevantForRoles')),
      '{requiredByToolSlugs}', snake_slug_array(payload->'requiredByToolSlugs')),
    '{lockedForRoles}', snake_slug_array(payload->'lockedForRoles'))
WHERE concept = 'v1:common:knowledgeDomain';

--bun:split

UPDATE "MemoryNodes" SET
  payload = jsonb_set(
    jsonb_set(payload,
      '{lockedDomainIds}', snake_slug_array(payload->'lockedDomainIds')),
    '{lockedToolSlugs}', snake_slug_array(payload->'lockedToolSlugs'))
WHERE concept = 'v1:agents:agentAuthorization';

--bun:split

UPDATE "MemoryNodes" SET
  payload = jsonb_set(payload, '{domainId}',
                      to_jsonb(snake_slug(payload->>'domainId')))
WHERE concept = 'v1:common:documentChunk';

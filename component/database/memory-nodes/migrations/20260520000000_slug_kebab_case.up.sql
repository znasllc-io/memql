-- Slug catalog: underscore -> dash.
--
-- The two-pattern id rule (see core/id/) now mandates kebab-case
-- shortIds for catalog concepts. Pre-cutover row sets carry the
-- legacy snake_case form ("row_crop_farmer", "human_resources",
-- "business_administration", etc.). This migration rewrites every
-- affected slug in place across the four concept families that
-- carry catalog identifiers:
--
--   v1:agents:agentRole           -- row id + payload.slug
--                                 -- payload.lockedDomainIds[]
--                                 -- payload.defaultDomainIds[]
--                                 -- payload.lockedToolSlugs[]
--   v1:agents:agent               -- payload.roleSlug
--                                 -- payload.capabilities.tools[]
--                                 -- payload.capabilities.domains[]
--   v1:common:knowledgeDomain     -- row id (the `:` -prefixed tail)
--                                 -- payload.id
--                                 -- payload.relevantForRoles[]
--                                 -- payload.requiredByToolSlugs[]
--                                 -- payload.lockedForRoles[]
--   v1:agents:agentAuthorization  -- payload.lockedDomainIds[] (if any)
--                                 -- payload.lockedToolSlugs[] (if any)
--
-- Strategy: a Postgres helper function `kebab_slug(text)` performs
-- the same `_` -> `-` replacement on string scalars. JSONB array
-- fields are rewritten by extracting elements, mapping through the
-- helper, and reassembling. Idempotent: re-running is a no-op
-- because already-kebab strings have no `_` to replace.
--
-- The down migration (.down.sql) reverses by mapping `-` -> `_`
-- in the same positions. Symmetric because both forms are
-- distinguishable from `: ` separators and free-form prose.

--bun:split

CREATE OR REPLACE FUNCTION kebab_slug(s text) RETURNS text AS $$
  SELECT CASE
    WHEN s IS NULL THEN NULL
    ELSE REPLACE(s, '_', '-')
  END;
$$ LANGUAGE SQL IMMUTABLE;

--bun:split

CREATE OR REPLACE FUNCTION kebab_slug_array(arr jsonb) RETURNS jsonb AS $$
  SELECT CASE
    WHEN arr IS NULL OR jsonb_typeof(arr) <> 'array' THEN arr
    ELSE (
      SELECT jsonb_agg(kebab_slug(elem #>> '{}'))
      FROM jsonb_array_elements(arr) AS elem
    )
  END;
$$ LANGUAGE SQL IMMUTABLE;

--bun:split

-- v1:agents:agentRole: id is the slug; payload.slug, lockedDomainIds,
-- defaultDomainIds, lockedToolSlugs are all kebab targets.
UPDATE "MemoryNodes" SET
  id = kebab_slug(id),
  payload = jsonb_set(
    jsonb_set(
      jsonb_set(
        jsonb_set(payload,
          '{slug}', to_jsonb(kebab_slug(payload->>'slug'))),
        '{lockedDomainIds}', kebab_slug_array(payload->'lockedDomainIds')),
      '{defaultDomainIds}', kebab_slug_array(payload->'defaultDomainIds')),
    '{lockedToolSlugs}', kebab_slug_array(payload->'lockedToolSlugs'))
WHERE concept = 'v1:agents:agentRole'
  AND (
    id LIKE '%\_%' ESCAPE '\'
    OR payload->>'slug' LIKE '%\_%' ESCAPE '\'
    OR payload::text LIKE '%\_%' ESCAPE '\'
  );

--bun:split

-- v1:agents:agent: payload.roleSlug + capability tool / domain arrays.
UPDATE "MemoryNodes" SET
  payload = jsonb_set(
    jsonb_set(
      jsonb_set(payload,
        '{roleSlug}', to_jsonb(kebab_slug(payload->>'roleSlug'))),
      '{capabilities, tools}', kebab_slug_array(payload#>'{capabilities, tools}')),
    '{capabilities, domains}', kebab_slug_array(payload#>'{capabilities, domains}'))
WHERE concept = 'v1:agents:agent'
  AND (
    payload->>'roleSlug' LIKE '%\_%' ESCAPE '\'
    OR payload#>>'{capabilities, tools}' LIKE '%\_%' ESCAPE '\'
    OR payload#>>'{capabilities, domains}' LIKE '%\_%' ESCAPE '\'
  );

--bun:split

-- v1:common:knowledgeDomain: id (after the concept prefix), payload.id,
-- relevantForRoles, requiredByToolSlugs, lockedForRoles.
UPDATE "MemoryNodes" SET
  id = regexp_replace(id, '^(.*:v1:common:knowledgeDomain:)(.*)$',
                      '\1' || kebab_slug(substring(id from '[^:]+$'))),
  payload = jsonb_set(
    jsonb_set(
      jsonb_set(
        jsonb_set(payload,
          '{id}', to_jsonb(kebab_slug(payload->>'id'))),
        '{relevantForRoles}', kebab_slug_array(payload->'relevantForRoles')),
      '{requiredByToolSlugs}', kebab_slug_array(payload->'requiredByToolSlugs')),
    '{lockedForRoles}', kebab_slug_array(payload->'lockedForRoles'))
WHERE concept = 'v1:common:knowledgeDomain'
  AND (
    id LIKE '%\_%' ESCAPE '\'
    OR payload::text LIKE '%\_%' ESCAPE '\'
  );

--bun:split

-- v1:agents:agentAuthorization: any locked* arrays carrying slugs.
UPDATE "MemoryNodes" SET
  payload = jsonb_set(
    jsonb_set(payload,
      '{lockedDomainIds}', kebab_slug_array(payload->'lockedDomainIds')),
    '{lockedToolSlugs}', kebab_slug_array(payload->'lockedToolSlugs'))
WHERE concept = 'v1:agents:agentAuthorization'
  AND (
    payload#>>'{lockedDomainIds}' LIKE '%\_%' ESCAPE '\'
    OR payload#>>'{lockedToolSlugs}' LIKE '%\_%' ESCAPE '\'
  );

--bun:split

-- v1:common:documentChunk: payload.domainId references a knowledgeDomain
-- shortId; rewrite the underscore form to kebab so the chunk -> domain
-- foreign-key (via payload field, not a real FK) stays valid.
UPDATE "MemoryNodes" SET
  payload = jsonb_set(payload, '{domainId}',
                      to_jsonb(kebab_slug(payload->>'domainId')))
WHERE concept = 'v1:common:documentChunk'
  AND payload->>'domainId' LIKE '%\_%' ESCAPE '\';

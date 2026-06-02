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
-- NULL-SAFETY (memql#624): `jsonb_set` is STRICT -- if the new value
-- is SQL NULL it returns NULL, which would null the NOT-NULL payload
-- column (SQLSTATE 23502) for any matched row that lacks one of the
-- rewritten keys. The row-level WHERE matches on id / payload::text,
-- so a matched row need not carry every key (e.g. an agentRole with no
-- `slug`, or an agent with no `capabilities.tools`). The set helpers
-- `kebab_set_scalar` / `kebab_set_array` below therefore no-op when the
-- target key/path is absent or not the expected json type, so a matched
-- row missing a key is left untouched instead of crashing the migration.
--
-- The down migration (.down.sql) reverses by mapping `-` -> `_`
-- in the same positions, using the symmetric snake_* helpers.
--
-- NOTE: no `--bun:split` between this header comment and the first
-- statement. A leading comment-only segment makes bun emit an empty
-- query ("pgdriver: query is empty"), which aborts the migration and
-- leaves later tables (e.g. automation_execution_claims) uncreated.
-- memql#570. The header therefore rides in the first statement's
-- segment (SQL ignores leading comments).

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
    -- COALESCE to '[]' because jsonb_agg over zero rows returns NULL: an
    -- EMPTY input array would otherwise map to SQL NULL, and feeding that to
    -- the strict jsonb_set in kebab_set_array nulls the NOT-NULL payload
    -- column (SQLSTATE 23502) -- the live crash that blocked all migrations
    -- after this one (claims table never created). memql#624.
    ELSE COALESCE((
      SELECT jsonb_agg(kebab_slug(elem #>> '{}'))
      FROM jsonb_array_elements(arr) AS elem
    ), '[]'::jsonb)
  END;
$$ LANGUAGE SQL IMMUTABLE;

--bun:split

-- Rewrite a top-level scalar string field in place, but ONLY when the
-- key is present and holds a string. jsonb_set is strict, so feeding it
-- a NULL value (absent key) would null the whole payload; this guard
-- makes the rewrite a no-op for rows that don't carry the key.
CREATE OR REPLACE FUNCTION kebab_set_scalar(p jsonb, key text) RETURNS jsonb AS $$
  SELECT CASE
    WHEN p ? key AND jsonb_typeof(p -> key) = 'string'
      THEN jsonb_set(p, ARRAY[key], to_jsonb(kebab_slug(p ->> key)))
    ELSE p
  END;
$$ LANGUAGE SQL IMMUTABLE;

--bun:split

-- Rewrite an array-of-strings field at `path`, but ONLY when the path
-- resolves to a json array. No-op (and crucially, no payload-nulling)
-- when the path is absent or not an array.
CREATE OR REPLACE FUNCTION kebab_set_array(p jsonb, path text[]) RETURNS jsonb AS $$
  SELECT CASE
    WHEN p #> path IS NOT NULL AND jsonb_typeof(p #> path) = 'array'
      THEN jsonb_set(p, path, kebab_slug_array(p #> path))
    ELSE p
  END;
$$ LANGUAGE SQL IMMUTABLE;

--bun:split

-- v1:agents:agentRole: id is the slug; payload.slug, lockedDomainIds,
-- defaultDomainIds, lockedToolSlugs are all kebab targets.
UPDATE "MemoryNodes" SET
  id = kebab_slug(id),
  payload = kebab_set_array(
              kebab_set_array(
                kebab_set_array(
                  kebab_set_scalar(payload, 'slug'),
                  '{lockedDomainIds}'),
                '{defaultDomainIds}'),
              '{lockedToolSlugs}')
WHERE concept = 'v1:agents:agentRole'
  AND (
    id LIKE '%\_%' ESCAPE '\'
    OR payload->>'slug' LIKE '%\_%' ESCAPE '\'
    OR payload::text LIKE '%\_%' ESCAPE '\'
  );

--bun:split

-- v1:agents:agent: payload.roleSlug + capability tool / domain arrays.
UPDATE "MemoryNodes" SET
  payload = kebab_set_array(
              kebab_set_array(
                kebab_set_scalar(payload, 'roleSlug'),
                '{capabilities, tools}'),
              '{capabilities, domains}')
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
  payload = kebab_set_array(
              kebab_set_array(
                kebab_set_array(
                  kebab_set_scalar(payload, 'id'),
                  '{relevantForRoles}'),
                '{requiredByToolSlugs}'),
              '{lockedForRoles}')
WHERE concept = 'v1:common:knowledgeDomain'
  AND (
    id LIKE '%\_%' ESCAPE '\'
    OR payload::text LIKE '%\_%' ESCAPE '\'
  );

--bun:split

-- v1:agents:agentAuthorization: any locked* arrays carrying slugs.
UPDATE "MemoryNodes" SET
  payload = kebab_set_array(
              kebab_set_array(payload, '{lockedDomainIds}'),
              '{lockedToolSlugs}')
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
  payload = kebab_set_scalar(payload, 'domainId')
WHERE concept = 'v1:common:documentChunk'
  AND payload->>'domainId' LIKE '%\_%' ESCAPE '\';

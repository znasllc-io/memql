-- Reverse of 20260520000000_slug_kebab_case.up.sql -- rewrites
-- catalog slug fields from kebab-case back to snake_case. Symmetric
-- because both forms are distinguishable from `:` separators and
-- free-form prose.
--
-- NULL-SAFETY (memql#624): like the up migration, the set helpers
-- (`snake_set_scalar` / `snake_set_array`) no-op when the target key /
-- path is absent or not the expected json type. jsonb_set is strict, so
-- feeding it a NULL new-value would null the NOT-NULL payload column for
-- any row missing a rewritten key.
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
    -- COALESCE to '[]' so an EMPTY array doesn't become SQL NULL (jsonb_agg
    -- over zero rows) and null the NOT-NULL payload via strict jsonb_set. memql#624.
    ELSE COALESCE((
      SELECT jsonb_agg(snake_slug(elem #>> '{}'))
      FROM jsonb_array_elements(arr) AS elem
    ), '[]'::jsonb)
  END;
$$ LANGUAGE SQL IMMUTABLE;

--bun:split

CREATE OR REPLACE FUNCTION snake_set_scalar(p jsonb, key text) RETURNS jsonb AS $$
  SELECT CASE
    WHEN p ? key AND jsonb_typeof(p -> key) = 'string'
      THEN jsonb_set(p, ARRAY[key], to_jsonb(snake_slug(p ->> key)))
    ELSE p
  END;
$$ LANGUAGE SQL IMMUTABLE;

--bun:split

CREATE OR REPLACE FUNCTION snake_set_array(p jsonb, path text[]) RETURNS jsonb AS $$
  SELECT CASE
    WHEN p #> path IS NOT NULL AND jsonb_typeof(p #> path) = 'array'
      THEN jsonb_set(p, path, snake_slug_array(p #> path))
    ELSE p
  END;
$$ LANGUAGE SQL IMMUTABLE;

--bun:split

UPDATE "MemoryNodes" SET
  id = snake_slug(id),
  payload = snake_set_array(
              snake_set_array(
                snake_set_array(
                  snake_set_scalar(payload, 'slug'),
                  '{lockedDomainIds}'),
                '{defaultDomainIds}'),
              '{lockedToolSlugs}')
WHERE concept = 'v1:agents:agentRole';

--bun:split

UPDATE "MemoryNodes" SET
  payload = snake_set_array(
              snake_set_array(
                snake_set_scalar(payload, 'roleSlug'),
                '{capabilities, tools}'),
              '{capabilities, domains}')
WHERE concept = 'v1:agents:agent';

--bun:split

UPDATE "MemoryNodes" SET
  id = regexp_replace(id, '^(.*:v1:common:knowledgeDomain:)(.*)$',
                      '\1' || snake_slug(substring(id from '[^:]+$'))),
  payload = snake_set_array(
              snake_set_array(
                snake_set_array(
                  snake_set_scalar(payload, 'id'),
                  '{relevantForRoles}'),
                '{requiredByToolSlugs}'),
              '{lockedForRoles}')
WHERE concept = 'v1:common:knowledgeDomain';

--bun:split

UPDATE "MemoryNodes" SET
  payload = snake_set_array(
              snake_set_array(payload, '{lockedDomainIds}'),
              '{lockedToolSlugs}')
WHERE concept = 'v1:agents:agentAuthorization';

--bun:split

UPDATE "MemoryNodes" SET
  payload = snake_set_scalar(payload, 'domainId')
WHERE concept = 'v1:common:documentChunk';

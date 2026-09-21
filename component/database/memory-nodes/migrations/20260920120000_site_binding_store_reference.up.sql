-- v1:platform:site.binding stops COPYING the store and starts NAMING it
-- (epic memql#5530, issue memql#5538).
--
-- WHAT CHANGES. A shopify_storefront's binding was {storeDomain,
-- storefrontTokenRef} -- the same two values v1:shopify:store already held,
-- edited in two places at two authorization tiers (gap G4). It becomes
-- {storeId}, and component/edge resolves the domain and the token reference
-- through the store row at serve time.
--
-- WHY A MIGRATION AND NOT A FALLBACK READ. Pre-release means no shim
-- (CLAUDE.md): the edge reads storeId and nothing else, so a row still
-- carrying the old shape would resolve no store, serve no storefront block
-- and name no store in its Content-Security-Policy -- a live storefront going
-- dark, silently, at the moment the engine rolled. Two writers produced the
-- old shape: createSite through the OS compose form, and the deploy pipeline
-- from memql-package.yaml. Both are converted in the same change.
--
-- SCOPED BY CONCEPT, NEVER BY KEY NAME. `binding` is v1:platform:site's, and
-- `storeDomain` is exactly the kind of key a product bundle mounted at
-- MEMQL_DSL_PATH may declare on a concept this repository has never seen.
--
-- THE JOIN IS ON THE ONE IDENTIFIER SHOPIFY NEVER CHANGES. A store row's
-- `domain` is the myshopify.com host, which is what the old binding copied.
-- A site whose domain matches no store row is left ALONE rather than cleared:
-- an unconverted binding is visible (the storefront stops resolving a store
-- and the OS says so), while a cleared one is indistinguishable from a
-- storefront nobody ever bound. The operator repairs it by attaching the
-- store on the deployable.
--
-- APPEND-ONLY ROWS, EVERY VERSION. Idempotent: the guard matches nothing on a
-- cluster whose site rows were all written after this change.

UPDATE "MemoryNodes" AS s
SET payload = jsonb_set(
      s.payload,
      '{binding}',
      jsonb_build_object('storeId', split_part(st.id, ':', 4))
    )
FROM (
  SELECT DISTINCT ON (payload->>'domain') id, payload->>'domain' AS domain
  FROM "MemoryNodes"
  WHERE concept = 'v1:shopify:store'
    AND payload->>'domain' IS NOT NULL
    AND payload->>'domain' <> ''
  ORDER BY payload->>'domain', "createdAt" DESC
) AS st
WHERE s.concept = 'v1:platform:site'
  AND s.payload->>'kind' = 'shopify_storefront'
  AND s.payload->'binding' ? 'storeDomain'
  AND s.payload->'binding'->>'storeDomain' = st.domain;

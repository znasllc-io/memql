# Knowledge-domain seeder

**Status:** Pipeline shipped (not yet run against the catalog). Expansion + first seed run pending API spend approval.
**Related:** [llm-driven-decisions.md](./llm-driven-decisions.md) (uses the same caching infra), `integrations/knowledge/seed.go` (existing domain definitions + the `copresent_ui` corpus this seeder generalises from).

## What this is

The catalog ships ~250 knowledge domains as concept rows but only one (`copresent_ui`) has actual retrievable content. This adds a pipeline that LLM-generates seed chunks for the other domains so an agent assigned to, say, `quantum_mechanics` actually has retrievable specialist context to ground its answers, not just a label slapped on a general LLM.

Three principles:

1. **Tier-based safety.** Not every domain should be auto-seeded. Medicine and law need authoritative sources or "consult a professional" caveats; general knowledge can be LLM-generated. Each domain carries a tier (A / B / C) that drives the seeding strategy.

2. **Idempotent + re-runnable.** Chunk ids derive from `(domain, recipeVersion, chunkIndex)` so re-running the pipeline against the same domain at the same recipe version is a no-op. Bumping the recipe version invalidates and re-generates. Lets us improve the recipe later without orphaning old chunks.

3. **Provenance on every chunk.** Each chunk records `seedSource` (`'llm-generated'` / `'curated'` / `'wikipedia:Article'` / `'openstax:Book'`), `seedTier` (A/B/C), `recipeVersion`. Lets the agent caveat or cite when it matters; lets us audit / re-generate selectively.

## Tier system

| Tier | Stakes | Seeding strategy |
|---|---|---|
| **A** | General knowledge — history, sports, hobbies, intro science, business basics, productivity | LLM-generated + validated. Ship. |
| **B** | Safety-relevant — personal finance, basic legal, basic health, parenting | LLM-generated + validated + explicit "general info, not professional advice" disclaimer chunk added at index 0. |
| **C** | High-stakes specialist — clinical medicine, surgical technique, securities advice, legal practice | **Don't auto-seed.** Domain exists as a label so users can attach their own authoritative content. Ships with a single "this domain doesn't auto-seed; consult a licensed professional or upload your own authoritative sources" chunk at index 0. |

Tier C is the load-bearing call. We're explicitly choosing not to ship malpractice-shaped content even with disclaimers. If an agent needs real surgical technique, the user uploads textbook content themselves.

## Pipeline overview

```
┌──────────────────────────────────────────────────────────────────┐
│ seedDomainContent(domainId, recipeVersion?)                      │
│                                                                  │
│ 1. Look up domain meta -> tier, name, description, category      │
│ 2. Switch on tier:                                               │
│      A: generate -> validate -> store                            │
│      B: generate -> validate -> prepend disclaimer -> store      │
│      C: store single "user-curated" placeholder chunk            │
│                                                                  │
│ 3. Generation phase (tier A/B):                                  │
│      seedDomain prompt (chat54Mini, structured output)           │
│      Returns: { chunks: [{title, body, kind}, ...] }             │
│      Three chunk kinds: principle, decisionRule, factExample     │
│      Recipe targets ~30 chunks per domain                        │
│                                                                  │
│ 4. Validation phase (tier A/B):                                  │
│      seedDomainValidate prompt -- per-chunk gate                 │
│      Returns: { accept: bool, reason: string }                   │
│      Accepted chunks proceed; rejected chunks logged + dropped   │
│                                                                  │
│ 5. Storage phase:                                                │
│      Each accepted chunk -> mutationCreateDocumentChunk          │
│      chunkId = sha256(domainId + recipeVersion + chunkIndex)     │
│      Carries seedSource, seedTier, recipeVersion in payload      │
│      Vector embedded into node_vectors at insert time            │
└──────────────────────────────────────────────────────────────────┘
```

## Recipe shape

The `seedDomain` prompt produces a structured output with the following per-chunk schema:

```json
{
  "chunks": [
    {
      "kind": "principle" | "decisionRule" | "factExample",
      "title": "Heisenberg uncertainty principle",
      "body": "Position and momentum cannot both be measured to arbitrary precision...",
      "keyTerms": ["uncertainty", "complementarity", "wave function"]
    }
  ]
}
```

Per domain target: 8 principle + 12 decisionRule + 10 factExample = ~30 chunks. Validator drops obviously bad ones (vague, factually wrong, too short, generic filler). Rare misses are tolerable since the agent's pretraining still fills gaps.

## Cost estimate

Generation (~30 chunks × 200 words × 250 domains):
- ~$5 per million tokens (chat54Mini class)
- ~$200-300 total one-time

Validation (per-chunk gate):
- Cheaper, smaller prompt
- ~$50-100 total

Embedding (per chunk, embedding3Small):
- ~$0.02 per 1k tokens
- ~$10-20 total

**Total: ~$300-450 one-time** to seed the entire catalog. Re-runs of individual domains for recipe improvements cost a fraction of that.

## Idempotency

`chunkIdFor` (already exists in the knowledge integration) hashes `domainId + sourceRef + seq + text` for legacy chunks. The seeder uses a different scheme:

```
seedChunkId = sha256("seed:" + recipeVersion + ":" + domainId + ":" + chunkIndex)
```

So:
- Same `(recipeVersion, domain, index)` → same id → re-insert is a no-op (memQL latest-wins with identical content writes the same row).
- Bump `recipeVersion` → all ids change → next run re-generates and writes new chunks. Old chunks get superseded on read by the latest version of the new chunks. (memQL is time-series; the old rows still exist but reads return the latest.)

This means iteration on the recipe is cheap: improve the prompt, bump `recipeVersion` from `"v1"` to `"v2"`, run the seeder. New content lands; old content stays as historical versions.

## Catalog expansion

Existing 96 domains get tiered (most are A; finance/legal/health basics are B; clinical medicine and surgical technique are C).

New ~150 domains added in this commit, organised:

- **Science & engineering** (~50): physics, chemistry, biology, earth science, math, CS, engineering disciplines
- **Medicine & health** (~20): mostly tier C — clinical specialties don't auto-seed; foundational ones (anatomy, pharmacology basics) are tier B
- **Humanities & social sciences** (~25): history, philosophy, lit, linguistics, anthro/socio, psych, econ, polisci, religious studies
- **Arts & design** (~15): visual arts deep-dive, music theory, performing arts, architecture, design disciplines
- **Specialized fields & hobbies** (~20): law subdomains, education theory, journalism, sports per-sport, board/video games, outdoor pursuits, collecting

Final catalog: ~250 domains, all classified by tier.

## What's NOT in this commit

- **The first seed run.** Building the pipeline is one cost (engineering time); running it costs LLM API tokens. We ship the pipeline, document how to invoke it, and the user kicks the run when they're ready to spend the ~$400.

- **UI categorization for the picker.** With 250 domains the existing single-pane picker becomes unbrowseable. Domain categories already exist (`category` field on the concept) and the picker just needs grouping. Separate frontend commit; not blocking the backend pipeline.

- **Authoritative-source ingest for tier C.** If/when we want real clinical / legal content we build a separate ingest path (Wikipedia/OpenStax/NIH parsers) that respects each source's license. Tier-C domains stay user-curated until then.

- **Re-generation cron.** Automated periodic re-seeding (when models improve) is a Phase-2 feature. Manual `seedDomainContent({domainId, recipeVersion})` calls suffice for now.

## How to run

Once approved for spend:

```bash
# Seed one domain (test/iterate the recipe):
docker exec memql-bff /app/memql query 'seedDomainContent({domainId: "quantum_mechanics", recipeVersion: "v1"})'

# Seed the full catalog (no domainId arg = all tier-A and tier-B):
docker exec memql-bff /app/memql query 'seedAllDomainContent({recipeVersion: "v1"})'
```

Watch the BFF logs for `knowledge.seedDomain: completed` lines per domain. Failed validations log `knowledge.seedDomain: chunk rejected` with reason.

After the run, query a domain to confirm chunks landed:

```sql
SELECT COUNT(*) FROM "MemoryNodes"
 WHERE concept = 'v1:common:documentChunk'
   AND payload->>'domainId' = 'quantum_mechanics'
   AND payload->>'seedSource' = 'llm-generated';
```

Should return ~25-30.

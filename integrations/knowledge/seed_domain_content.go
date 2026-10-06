package knowledge

// LLM-driven knowledge seeder. Generates baseline retrievable content
// for empty domain shells via the seedDomainContent prompt + embeds
// each chunk + writes idempotent rows.
//
// See docs/internal/planning/knowledge-seeder.md for the strategy. Two
// capabilities exposed:
//
//   seedDomainContent({domainId, recipeVersion?})
//     Single-domain run. Useful for iterating on the recipe before
//     spending API tokens on the full catalog.
//
//   seedAllDomainContent({recipeVersion?, tierFilter?})
//     Full-catalog run. Loops every active KnowledgeDomain row,
//     skips Tier C (writes a single placeholder), generates Tier A/B
//     chunks via the prompt, and writes them.
//
// Idempotency: chunk ids are sha256("seed:" + recipeVersion + ":" +
// domainId + ":" + index). Re-running with the same recipeVersion is
// a no-op (MemQL latest-wins with identical content). Bumping
// recipeVersion invalidates all old chunks for that domain (MemQL
// time-series; old rows still exist as historical versions but reads
// return the latest).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// seedDomainContentSchemaJSON is the structured-output schema the LLM
// is constrained to. Returns an array of chunks; each chunk carries
// kind / title / body / keyTerms.
//
// Schema description embeds the version "v1" so a schema bump is a
// natural cache-invalidation trigger via the AI cache (the cached LLM
// response keys on the rendered prompt + provider; the prompt embeds
// the schema description which embeds the version).
const seedDomainContentSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "description": "seedDomainContent.v1",
  "properties": {
    "chunks": {
      "type": "array",
      "minItems": 1,
      "maxItems": 50,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "kind": {
            "type": "string",
            "enum": ["principle", "decisionRule", "factExample"],
            "description": "principle = core concept / definition; decisionRule = when-X-do-Y heuristic; factExample = specific named fact or example."
          },
          "title": {
            "type": "string",
            "description": "Short noun-phrase title, 3-10 words. Used as the chunk's retrieval anchor."
          },
          "body": {
            "type": "string",
            "description": "100-300 words, declarative, dense, self-contained. The actual chunk content that gets embedded + retrieved."
          },
          "keyTerms": {
            "type": "array",
            "items": {"type": "string"},
            "description": "3-8 short terms agents can use to recognise relevance. Surfaced in the chunk's retrieval metadata."
          }
        },
        "required": ["kind", "title", "body", "keyTerms"]
      }
    }
  },
  "required": ["chunks"]
}`

// seedChunkPayload mirrors the JSON shape the prompt returns.
type seedChunkPayload struct {
	Chunks []seedChunk `json:"chunks"`
}

type seedChunk struct {
	Kind     string   `json:"kind"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	KeyTerms []string `json:"keyTerms"`
}

// defaultRecipeVersion bumps when the prompt template or schema
// changes in a way we want to invalidate prior cached generations
// for. Re-runs at the same version are no-ops; bumping forces fresh
// LLM calls + new chunk ids.
//
// v2 (2026-05-03): tightened seedDomainContent prompt to require
// named-anchor coverage on factExample chunks (NAMED event /
// person / work / place + specific date or threshold) and bumped
// targetCount to 60 for BroadSurvey domains (history, world
// civilizations) so multi-millennium scopes get real per-era depth
// instead of one generic chunk per civilization. Bump invalidates
// prior generations -- next training touch on a domain re-seeds
// against the new prompt.
const defaultRecipeVersion = "v2"

// seedDomainContentHandler runs the seeder for a single domain. Looks
// up the domain (so it knows the name + tier), branches on tier, and
// either generates + stores chunks (A/B) or writes the Tier-C
// placeholder.
func (i *Integration) seedDomainContentHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	domainId, _ := args["domainId"].(string)
	if strings.TrimSpace(domainId) == "" {
		return nil, fmt.Errorf("knowledge.seedDomainContent: domainId is required")
	}
	recipeVersion, _ := args["recipeVersion"].(string)
	if recipeVersion == "" {
		recipeVersion = defaultRecipeVersion
	}

	domain, err := i.lookupSeededDomain(ctx, domainId)
	if err != nil {
		return nil, err
	}

	chunks, err := i.runSeederForDomain(ctx, domain, recipeVersion)
	if err != nil {
		return nil, err
	}

	i.Logger.Info("knowledge.seedDomainContent: completed",
		"domainId", domainId,
		"tier", domain.Tier,
		"recipeVersion", recipeVersion,
		"chunksWritten", chunks,
	)
	return nil, nil
}

// seedAllDomainContentHandler loops every standardDomain and runs the
// seeder per domain. Best-effort -- one domain failing doesn't abort
// the rest. Logs a summary at the end.
//
// Optional args:
//   - recipeVersion: as above
//   - tierFilter: "A" / "B" / "C" -- only seed domains matching this
//     tier (lets us run an A-only pass first to validate quality
//     before paying for B + C). Empty means all tiers.
//   - domainIdPrefix: only seed domains whose id starts with this
//     prefix (e.g. "physics_" to test the physics block in isolation).
func (i *Integration) seedAllDomainContentHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	recipeVersion, _ := args["recipeVersion"].(string)
	if recipeVersion == "" {
		recipeVersion = defaultRecipeVersion
	}
	tierFilter, _ := args["tierFilter"].(string)
	domainIdPrefix, _ := args["domainIdPrefix"].(string)

	domains := map[string]StandardDomain{}
	facts := []any{}
	for _, d := range allSeedDomains() {
		d.Tier = effectiveTier(d)
		domains[d.ID] = d
		facts = append(facts, map[string]any{"id": d.ID, "tier": d.Tier})
	}
	totalDomains, totalChunks := 0, 0
	_, err := workflowhost.Run(ctx, "knowledgeSeedAllWorkflow", map[string]any{"domains": facts, "tierFilter": strings.ToUpper(strings.TrimSpace(tierFilter)), "domainIdPrefix": domainIdPrefix}, workflowhost.Options{Logger: i.Logger, Operations: map[string]workflowhost.Operation{
		"knowledgeSeedSelectedDomain": func(ctx context.Context, a map[string]any) (any, error) {
			d, ok := domains[stringArg(a, "domainId")]
			if !ok {
				return nil, fmt.Errorf("domain is outside the seed catalog")
			}
			written, err := i.runSeederForDomain(ctx, d, recipeVersion)
			if err == nil {
				totalDomains++
				totalChunks += written
			}
			return nil, err
		},
	}})
	i.Logger.Info("knowledge.seedAllDomainContent: complete", "recipeVersion", recipeVersion, "domainsSeeded", totalDomains, "chunksWritten", totalChunks, "error", err)
	return nil, err
}

// runSeederForDomain is the per-domain pipeline body. Branches on tier:
//   - A: generate via prompt -> validate -> store
//   - B: prepend disclaimer chunk -> generate -> validate -> store
//   - C: fetch authoritative Wikipedia content if WikipediaArticles
//     is set on the StandardDomain; otherwise store the
//     placeholder chunk only.
//
// On success, stamps lastSeededAt + seederRecipeVersion on the
// domain row via markKnowledgeDomainSeeded. The training
// pipeline reads those fields to decide whether to re-seed a stale
// domain on retrain (per docs/internal/planning/knowledge-seeder.md, Phase 2).
//
// Returns the number of chunks written (so the caller can summarise).
func (i *Integration) runSeederForDomain(ctx context.Context, d StandardDomain, recipeVersion string) (int, error) {
	if i.engine == nil || i.embeddingProvider == nil {
		return 0, fmt.Errorf("knowledge.runSeederForDomain: integration not fully wired")
	}
	tier := strings.TrimSpace(d.Tier)
	if tier == "" {
		tier = effectiveTier(d)
	}

	scope := &seedScope{i: i, domain: d, recipe: recipeVersion}
	result, err := workflowhost.Run(ctx, "knowledgeSeedDomainWorkflow", map[string]any{
		"domainId": d.ID, "domainName": d.Name, "domainDescription": d.Description, "category": coalesceCategory(d.Category),
		"tier": strings.ToUpper(tier), "broadSurvey": d.BroadSurvey, "articles": wikipediaArticlesFor(d.ID),
	}, workflowhost.Options{Logger: i.Logger, Operations: scope.operations()})
	if err != nil {
		return scope.written, err
	}
	_ = result
	return scope.written, nil

}

// stampDomainSeeded calls markKnowledgeDomainSeeded to
// record that the domain just successfully seeded at the given
// recipe version. Used by both runSeederForDomain (above) and
// any future per-domain refresher.
func (i *Integration) stampDomainSeeded(ctx context.Context, domainId, recipeVersion string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	q := fmt.Sprintf(
		`mutation markKnowledgeDomainSeeded(domainId: %s, lastSeededAt: %s, seederRecipeVersion: %s)`,
		quoteString(domainId),
		quoteString(now),
		quoteString(recipeVersion),
	)
	_, err := i.engine.Execute(ctx, q)
	return err
}

// writeTierABChunks runs the generation prompt + writes chunks for
// Tier-A and Tier-B domains. Prepends the disclaimer chunk for B.
// storeSeedChunk persists one chunk: idempotent id, embed, write the
// chunk row + node_vectors row. Carries seedSource / seedTier /
// recipeVersion / kind / title / keyTerms in payload metadata so we
// can later filter (e.g. "show me only LLM-generated chunks for
// re-validation").
func (i *Integration) storeSeedChunk(
	ctx context.Context,
	d StandardDomain,
	recipeVersion string,
	chunkIndex int,
	c seedChunk,
	seedSource string,
	chunkSource string, // chunk-level provenance class (llmSeeded / crossDomainBridge / ...). Required by the chunk concept.
	providerName string,
	provider interface {
		Embed(ctx context.Context, text string) ([]float32, error)
	},
) error {
	chunkId := seedChunkId(recipeVersion, d.ID, chunkIndex)
	sourceRef := fmt.Sprintf("seed:%s:%s:%d", recipeVersion, seedSource, chunkIndex)

	// Body is the retrievable text. We embed body alone (title is
	// retrieval metadata, not part of the chunk content the agent
	// reads).
	vec, err := provider.Embed(ctx, c.Body)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}

	// The createDocumentChunk mutation only takes the core
	// fields. To carry our seedSource / seedTier / recipeVersion /
	// kind / title / keyTerms we encode them into sourceRef + a
	// separate metadata stamp via a follow-up mutation? Or stuff
	// them into the body? The lightest path: prepend a small JSON
	// header to the body so retrieval still works on the body text
	// AND a future filter / display path can parse the metadata
	// out. We keep the body text human-readable by putting the
	// metadata behind a marker the chunker / displayer can strip.
	//
	// Format:
	//   <!--seed:{json}-->\n\n{title}\n\n{body}
	// Marker is HTML-comment-shaped so most retrieval paths pass it
	// through cleanly; downstream displays can strip it via regex.
	// Sanitize the title before indexing so role markers / markdown
	// headers in seed content don't ride into the retrieval pool.
	// Defense-in-depth on top of the prompt-render-time framing
	// (pack PR #25); see SanitizeChunkTitle's doc-comment
	// for the full rule set and pack#29 for the rationale.
	cleanTitle := SanitizeChunkTitle(c.Title)
	metadata := map[string]any{
		"seedSource":    seedSource,
		"seedTier":      d.Tier,
		"recipeVersion": recipeVersion,
		"chunkKind":     c.Kind,
		"chunkTitle":    cleanTitle,
		"keyTerms":      c.KeyTerms,
	}
	metadataJSON, _ := json.Marshal(metadata)
	enrichedBody := fmt.Sprintf("<!--seed:%s-->\n\n## %s\n\n%s", string(metadataJSON), cleanTitle, c.Body)

	insertQuery := fmt.Sprintf(
		`mutation createDocumentChunk(chunkId: %s, domainId: %s, text: %s, source: %s, sourceRef: %s, seq: %d, tokenCount: %d)`,
		quoteString(chunkId),
		quoteString(d.ID),
		quoteString(enrichedBody),
		quoteString(chunkSource),
		quoteString(sourceRef),
		chunkIndex,
		approxTokens(enrichedBody),
	)
	if _, err := i.engine.Execute(ctx, insertQuery); err != nil {
		return fmt.Errorf("insert chunk: %w", err)
	}
	if err := i.storeVector(ctx, providerName, chunkId, "v1:knowledge:documentChunk", vec); err != nil {
		return fmt.Errorf("persist vector: %w", err)
	}
	return nil
}

// seedChunkId derives a deterministic chunk id from
// (recipeVersion, domainId, chunkIndex) so re-runs are no-ops at the
// same recipe version and bumping the version invalidates the prior
// run. The seed-vs-augment origin is captured in row provenance, not
// in the id string.
func seedChunkId(recipeVersion, domainId string, chunkIndex int) string {
	return string(id.New().MustFromMap(map[string]any{
		"kind":          "seed-chunk",
		"recipeVersion": recipeVersion,
		"domainId":      domainId,
		"chunkIndex":    chunkIndex,
	}))
}

// lookupSeededDomain resolves a StandardDomain from the standardDomains
// slice by id. Used by seedDomainContentHandler so we know the tier
// + name + description without querying the DB.
//
// Returns an error if the id isn't in the catalog (user-created
// domains aren't in standardDomains; the seeder only supports the
// shipped catalog -- user-uploaded content goes through the existing
// `ingest` capability).
func (i *Integration) lookupSeededDomain(_ context.Context, domainId string) (StandardDomain, error) {
	domainId = strings.TrimSpace(domainId)
	for _, d := range allSeedDomains() {
		if d.ID == domainId {
			d.Tier = effectiveTier(d)
			return d, nil
		}
	}
	return StandardDomain{}, fmt.Errorf("domain %q is not in the shipped catalog (user-created domains use the `ingest` capability instead)", domainId)
}

// seedScope binds a recipe to one domain. Index assignment and receipts stay
// with the writer; content selection, prompts, iteration and continuation are DSL.
type seedScope struct {
	i                  *Integration
	domain             StandardDomain
	recipe             string
	nextIndex, written int
	providerName       string
	provider           memql.EmbeddingAIProvider
}

func (s *seedScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{
		"knowledgeGenerateSeedChunks": func(ctx context.Context, a map[string]any) (any, error) {
			data, _ := a["data"].(map[string]any)
			schemaName := stringArg(a, "schema")
			var schema string
			switch schemaName {
			case "seedDomainContent":
				schema = seedDomainContentSchemaJSON
			case "seedDomainBridge":
				schema = bridgeChunkSchemaJSON
			default:
				return nil, fmt.Errorf("unknown seed response contract %q", schemaName)
			}
			raw, err := s.i.engine.InvokeAIStructured(ctx, stringArg(a, "prompt"), data, schemaName, json.RawMessage(schema), true)
			if err != nil {
				return nil, err
			}
			var payload seedChunkPayload
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				return nil, fmt.Errorf("seed response: %w", err)
			}
			out := []any{}
			for idx, c := range payload.Chunks {
				out = append(out, map[string]any{"index": idx, "kind": c.Kind, "title": strings.TrimSpace(c.Title), "body": strings.TrimSpace(c.Body), "keyTerms": c.KeyTerms})
			}
			return out, nil
		},
		"knowledgeResolveSeedEmbedder": func(ctx context.Context, _ map[string]any) (any, error) { return nil, s.resolveEmbedder(ctx) },
		"knowledgeStoreSeedChunk":      s.store,
		"knowledgeStampSeed": func(ctx context.Context, _ map[string]any) (any, error) {
			return nil, s.i.stampDomainSeeded(ctx, s.domain.ID, s.recipe)
		},
		"knowledgeFetchSeedArticle": func(ctx context.Context, a map[string]any) (any, error) {
			title, body, url, err := fetchWikipediaArticle(ctx, stringArg(a, "article"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"title": title, "body": body, "url": url}, nil
		},
		"knowledgeSplitSeedText": func(_ context.Context, a map[string]any) (any, error) {
			return Chunk(stringArg(a, "text"), defaultChunkSize, defaultOverlap), nil
		},
	}
}

func (s *seedScope) store(ctx context.Context, a map[string]any) (any, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("seed embedder must be resolved before storing a chunk")
	}
	index := s.nextIndex
	if _, explicit := a["index"]; explicit {
		index = intArg(a, "index", index)
	}
	s.nextIndex++
	err := s.i.storeSeedChunk(ctx, s.domain, s.recipe, index, seedChunk{Kind: stringArg(a, "kind"), Title: stringArg(a, "title"), Body: stringArg(a, "body"), KeyTerms: toStringSlice(a["keyTerms"])}, stringArg(a, "seedSource"), stringArg(a, "source"), s.providerName, s.provider)
	if err != nil {
		s.i.Logger.Warn("knowledge: seed chunk write failed", "domainId", s.domain.ID, "index", index, "error", err)
		return nil, err
	}
	s.written++
	return nil, nil
}

func (s *seedScope) resolveEmbedder(ctx context.Context) error {
	if s.provider == nil {
		bound, err := memql.ResolveEmbedderProvider(ctx)
		if err != nil {
			return err
		}
		provider, err := s.i.embeddingProvider(ctx, bound)
		if err != nil {
			return err
		}
		s.providerName, s.provider = bound, provider
	}
	return nil
}

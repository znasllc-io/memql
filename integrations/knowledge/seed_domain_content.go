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
// a no-op (memQL latest-wins with identical content). Bumping
// recipeVersion invalidates all old chunks for that domain (memQL
// time-series; old rows still exist as historical versions but reads
// return the latest).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

// seedDomainContentSchemaJSON is the structured-output schema the LLM
// is constrained to. Returns an array of chunks; each chunk carries
// kind / title / body / keyTerms.
//
// Schema description embeds the version "v1" so a schema bump is a
// natural cache-invalidation trigger via the SI cache (the cached LLM
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

// disclaimerChunkText is prepended (as a chunk at index 0) to every
// Tier-B domain's seeded content. Tells the agent to caveat its
// answers; tells the user the generated content is not professional
// advice.
const disclaimerChunkText = `**General information only -- not professional advice.** Content in this domain is generated for educational reference. For specific decisions involving health, legal, financial, or safety matters, consult a licensed professional. The agent should: (1) frame answers as general information, not personal advice; (2) name what kind of professional could help with the specific situation; (3) decline to give actionable specifics where they could cause harm if applied without expert oversight.`

// tierCPlaceholderText is the single chunk Tier-C domains get instead
// of LLM-generated content. Tells the user the domain doesn't
// auto-seed and points at the upload path. Lets the domain still
// exist + be selectable; the agent just has nothing retrievable.
const tierCPlaceholderText = `**This domain doesn't auto-seed content.** Specialist domains in this tier (clinical medicine, surgical technique, securities advice, legal practice, etc.) carry too much downstream risk for LLM-generated baseline content. Upload your own authoritative materials -- textbooks, professional society guidelines, peer-reviewed articles -- via the Knowledge panel's document-attach flow. Until then, an agent assigned this domain answers from its general LLM pretraining alone.`

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

	startedAt := time.Now()
	totalDomains := 0
	totalChunks := 0
	skipped := 0
	failed := 0

	for _, d := range standardDomains {
		tier := effectiveTier(d)
		if tierFilter != "" && tier != strings.ToUpper(strings.TrimSpace(tierFilter)) {
			skipped++
			continue
		}
		if domainIdPrefix != "" && !strings.HasPrefix(d.ID, domainIdPrefix) {
			skipped++
			continue
		}
		// Stamp tier on the local copy so runSeederForDomain doesn't
		// have to re-resolve via effectiveTier.
		d.Tier = tier

		written, err := i.runSeederForDomain(ctx, d, recipeVersion)
		if err != nil {
			i.Logger.Warn("knowledge.seedAllDomainContent: domain failed",
				"domainId", d.ID, "tier", tier, "err", err)
			failed++
			continue
		}
		totalDomains++
		totalChunks += written
	}

	i.Logger.Info("knowledge.seedAllDomainContent: complete",
		"recipeVersion", recipeVersion,
		"tierFilter", tierFilter,
		"domainIdPrefix", domainIdPrefix,
		"domainsSeeded", totalDomains,
		"chunksWritten", totalChunks,
		"skipped", skipped,
		"failed", failed,
		"elapsedMs", time.Since(startedAt).Milliseconds(),
	)
	return nil, nil
}

// runSeederForDomain is the per-domain pipeline body. Branches on tier:
//   - A: generate via prompt -> validate -> store
//   - B: prepend disclaimer chunk -> generate -> validate -> store
//   - C: fetch authoritative Wikipedia content if WikipediaArticles
//     is set on the StandardDomain; otherwise store the
//     placeholder chunk only.
//
// On success, stamps lastSeededAt + seederRecipeVersion on the
// domain row via mutationMarkKnowledgeDomainSeeded. The training
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

	var (
		written int
		err     error
	)
	switch strings.ToUpper(tier) {
	case "C":
		// Tier C: prefer Wikipedia content when an article mapping
		// exists for this domain id (see tierCWikipediaArticles in
		// seed.go); fall back to the placeholder chunk when no
		// mapping is configured.
		if articles := wikipediaArticlesFor(d.ID); len(articles) > 0 {
			written, err = i.writeTierCWikipediaChunks(ctx, d, articles, recipeVersion)
		} else {
			written, err = i.writeTierCPlaceholder(ctx, d, recipeVersion)
		}
	case "B":
		written, err = i.writeTierABChunks(ctx, d, "B", recipeVersion)
	default:
		// "A" or anything else falls through here.
		written, err = i.writeTierABChunks(ctx, d, "A", recipeVersion)
	}

	if err != nil {
		return written, err
	}

	// Stamp freshness on success. Best-effort -- if the mark fails we
	// log but don't roll back the chunks (they're written either way;
	// freshness is observability, not correctness).
	if stampErr := i.stampDomainSeeded(ctx, d.ID, recipeVersion); stampErr != nil {
		i.Logger.Warn("knowledge.runSeederForDomain: stampDomainSeeded failed",
			"domainId", d.ID, "err", stampErr)
	}
	return written, nil
}

// stampDomainSeeded calls mutationMarkKnowledgeDomainSeeded to
// record that the domain just successfully seeded at the given
// recipe version. Used by both runSeederForDomain (above) and
// any future per-domain refresher.
func (i *Integration) stampDomainSeeded(ctx context.Context, domainId, recipeVersion string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	q := fmt.Sprintf(
		`mutationMarkKnowledgeDomainSeeded({domainId: %s, lastSeededAt: %s, seederRecipeVersion: %s})`,
		quoteString(domainId),
		quoteString(now),
		quoteString(recipeVersion),
	)
	_, err := i.engine.Execute(ctx, q)
	return err
}

// writeTierABChunks runs the generation prompt + writes chunks for
// Tier-A and Tier-B domains. Prepends the disclaimer chunk for B.
func (i *Integration) writeTierABChunks(ctx context.Context, d StandardDomain, tier string, recipeVersion string) (int, error) {
	// Render + invoke the prompt. cacheSeconds isn't meaningful here
	// (each domain is a unique input) so we don't pass it; the SI
	// runtime's default kicks in.
	//
	// BroadSurvey domains (multi-millennium history, world civs, etc.)
	// get a 60-chunk target + a flag that flips the prompt into
	// named-anchor coverage mode. Narrow domains keep the 30 default
	// from agentReply.tmpl's existing instructions.
	targetCount := 30
	if d.BroadSurvey {
		targetCount = 60
	}
	data := map[string]any{
		"domainId":          d.ID,
		"domainName":        d.Name,
		"domainDescription": d.Description,
		"category":          coalesceCategory(d.Category),
		"targetCount":       targetCount,
		"tier":              tier,
		"broadSurvey":       d.BroadSurvey,
	}
	raw, err := i.engine.InvokeAIStructured(
		ctx,
		"seedDomainContent",
		data,
		"seedDomainContent",
		json.RawMessage(seedDomainContentSchemaJSON),
		true,
	)
	if err != nil {
		return 0, fmt.Errorf("seedDomainContent SI call: %w", err)
	}
	var payload seedChunkPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return 0, fmt.Errorf("seedDomainContent JSON parse: %w", err)
	}

	// Provider for embedding the chunk text.
	provider, err := i.embeddingProvider(defaultProvider)
	if err != nil {
		return 0, fmt.Errorf("resolve embedding provider %q: %w", defaultProvider, err)
	}
	partition := i.resolvePartition(ctx)

	written := 0
	chunkIndex := 0

	// Tier-B: prepend the disclaimer chunk at index 0 so it always
	// retrieves with high similarity to safety-relevant queries.
	if tier == "B" {
		if err := i.storeSeedChunk(ctx, partition, d, recipeVersion, chunkIndex, seedChunk{
			Kind:     "principle",
			Title:    "Disclaimer: general information only",
			Body:     disclaimerChunkText,
			KeyTerms: []string{"disclaimer", "professional advice", "safety", "consult"},
		}, "seed-disclaimer", "llmSeeded", provider); err != nil {
			i.Logger.Warn("seedDomainContent: disclaimer write failed",
				"domainId", d.ID, "err", err)
		} else {
			written++
		}
		chunkIndex++
	}

	for _, c := range payload.Chunks {
		c.Title = strings.TrimSpace(c.Title)
		c.Body = strings.TrimSpace(c.Body)
		if c.Title == "" || c.Body == "" {
			continue
		}
		if err := i.storeSeedChunk(ctx, partition, d, recipeVersion, chunkIndex, c, "llm-generated", "llmSeeded", provider); err != nil {
			i.Logger.Warn("seedDomainContent: chunk write failed",
				"domainId", d.ID, "title", c.Title, "err", err)
			chunkIndex++
			continue
		}
		written++
		chunkIndex++
	}
	return written, nil
}

// writeTierCPlaceholder writes the single "this domain doesn't
// auto-seed" placeholder chunk. Lets Tier-C domains still exist as
// selectable in the UI without shipping LLM-generated content for
// high-stakes specialist topics.
func (i *Integration) writeTierCPlaceholder(ctx context.Context, d StandardDomain, recipeVersion string) (int, error) {
	provider, err := i.embeddingProvider(defaultProvider)
	if err != nil {
		return 0, fmt.Errorf("resolve embedding provider %q: %w", defaultProvider, err)
	}
	partition := i.resolvePartition(ctx)

	chunk := seedChunk{
		Kind:     "principle",
		Title:    fmt.Sprintf("Tier-C placeholder for %s", d.Name),
		Body:     tierCPlaceholderText,
		KeyTerms: []string{"placeholder", "specialist", "upload", "authoritative"},
	}
	if err := i.storeSeedChunk(ctx, partition, d, recipeVersion, 0, chunk, "tier-c-placeholder", "llmSeeded", provider); err != nil {
		return 0, err
	}
	return 1, nil
}

// storeSeedChunk persists one chunk: idempotent id, embed, write the
// chunk row + node_vectors row. Carries seedSource / seedTier /
// recipeVersion / kind / title / keyTerms in payload metadata so we
// can later filter (e.g. "show me only LLM-generated chunks for
// re-validation").
func (i *Integration) storeSeedChunk(
	ctx context.Context,
	partition string,
	d StandardDomain,
	recipeVersion string,
	chunkIndex int,
	c seedChunk,
	seedSource string,
	chunkSource string, // chunk-level provenance class (llmSeeded / crossDomainBridge / ...). Required by the chunk concept.
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

	// The mutationCreateDocumentChunk mutation only takes the core
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
	// (bff-copresent PR #25); see SanitizeChunkTitle's doc-comment
	// for the full rule set and bff-copresent#29 for the rationale.
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
		`mutationCreateDocumentChunk({chunkId: %s, domainId: %s, text: %s, source: %s, sourceRef: %s, seq: %d, tokenCount: %d})`,
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
	if err := i.storeVector(ctx, chunkId, "v1:common:documentChunk", vec); err != nil {
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
	for _, d := range standardDomains {
		if d.ID == domainId {
			d.Tier = effectiveTier(d)
			return d, nil
		}
	}
	return StandardDomain{}, fmt.Errorf("domain %q is not in the shipped catalog (user-created domains use the `ingest` capability instead)", domainId)
}

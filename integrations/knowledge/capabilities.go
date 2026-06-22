// Package knowledge provides retrieval-augmented generation primitives for
// agents: chunking + embedding text into v1:common:documentChunk rows
// scoped to a v1:common:knowledgeDomain, and cosine similarity retrieval
// filtered by a set of domain IDs.
//
// Capabilities exposed to the DSL:
//
//	integration.knowledge.ingest  -- split a document into chunks, embed each,
//	                                 and persist both the chunk rows and their
//	                                 vectors. Safe to re-run for the same
//	                                 sourceRef -- duplicate chunks are replaced
//	                                 (content-hashed id + ON CONFLICT in
//	                                 node_vectors).
//
// Retrieval used to live here too (knowledge.lookup + knowledgeLookup
// builtin) but has moved to integrations/similarity (the generic
// `similarTo` DSL operator). This integration now owns only the
// write-side pipeline.
//
// The integration deliberately does NOT re-implement the embed/store path
// that integrations/embedding already owns -- it reuses the embedding
// provider for raw compute and writes vectors into the same node_vectors
// table via the same pgvector literal format. The only thing knowledge
// adds on top is the chunk-level concept, the domain-scoped filter, and
// the text-aware chunker.
package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// Integration exposes knowledge-base capabilities to the DSL.
type Integration struct {
	Logger            *slog.Logger
	engine            memql.IntegrationEngineAccess
	dbGetter          func() *sql.DB
	embeddingProvider func(name string) (memql.EmbeddingAIProvider, error)
	partitionFunc     func(ctx context.Context) string
}

// New constructs a knowledge integration. Dependencies are wired by the
// plug-in factory at startup; capability handlers resolve them at call
// time so the database handle can come online after the factory fires.
func New(logger *slog.Logger) *Integration {
	return &Integration{Logger: logger}
}

// SetEngine wires the MemQL engine so the ingest handler can execute
// the createDocumentChunk mutation via the normal DSL path (which
// handles partition stamping, id generation, and row insertion).
func (i *Integration) SetEngine(e memql.IntegrationEngineAccess) { i.engine = e }

// SetDBGetter wires the lazy database handle used for the cosine
// similarity query in lookup and the vector insert in ingest.
func (i *Integration) SetDBGetter(fn func() *sql.DB) { i.dbGetter = fn }

// SetEmbeddingProvider wires the embedding provider resolver. The knowledge
// integration embeds chunks at ingest time and the query at lookup time;
// both go through the caller-named provider (default: embedding3Small).
func (i *Integration) SetEmbeddingProvider(fn func(name string) (memql.EmbeddingAIProvider, error)) {
	i.embeddingProvider = fn
}

// SetPartitionFunc wires the request-scoped partition resolver.
func (i *Integration) SetPartitionFunc(fn func(ctx context.Context) string) { i.partitionFunc = fn }

// IntegrationName returns the stable identifier for this integration.
func (i *Integration) IntegrationName() string { return "knowledge" }

// Capabilities lists the DSL-callable functions. The agent/cognition
// node types both embed this integration so builtins in both binaries
// can dispatch to the same handlers.
//
// Retrieval lives in integrations/similarity now (the generic
// `similarTo` DSL operator). This integration retains only the
// write-side pipeline -- chunker + embed + idempotent chunk-row
// write -- and the one-shot seed helper.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "ingest",
			Description: "Split a document into chunks, embed each, and persist chunk rows linked to a knowledge domain.",
			Handler:     i.ingestHandler,
			ArgsSchema: map[string]string{
				"domainId":  "string (required) - ID of the parent knowledge domain",
				"text":      "string (required) - the full source text to ingest",
				"sourceRef": "string (optional) - origin tag, e.g. 'opid:agents.new' or 'doc:README.md'",
				"chunkSize": "int (optional) - approx chars per chunk (default 1800)",
				"overlap":   "int (optional) - char overlap between chunks (default 180)",
				"provider":  "string (optional) - embedding provider name (default embedding3Small)",
			},
		},
		{
			Name:        "seedStandardDomains",
			Description: "Seed the shipped knowledge-domain catalog and the initial CoPresent UI corpus. Idempotent: skips domains that already exist and chunk sources that were previously ingested.",
			Handler:     i.seedStandardDomainsHandler,
			ArgsSchema: map[string]string{
				"forceIngest": "bool (optional) - if true, re-ingest the CoPresent UI corpus even if chunks already exist (new-content bump rewrites vectors via ON CONFLICT).",
			},
		},
		{
			// Single-domain LLM-driven seeder. See
			// docs/internal/planning/knowledge-seeder.md for the full pipeline.
			// Useful for iterating on the recipe before paying for the
			// full-catalog run.
			Name:        "seedDomainContent",
			Description: "Generate ~30 retrievable chunks for a single domain via the seedDomainContent prompt + embed + write. Tier-A domains seed normally; Tier-B prepends a disclaimer chunk; Tier-C writes a placeholder ('upload your own authoritative content'). Idempotent at a given recipeVersion -- re-runs are no-ops; bump the version to invalidate.",
			Handler:     i.seedDomainContentHandler,
			ArgsSchema: map[string]string{
				"domainId":      "string (required) - shipped-catalog domain id, e.g. 'physics_quantum_mechanics'.",
				"recipeVersion": "string (optional) - bumps invalidate prior chunks; default 'v1'.",
			},
		},
		{
			// Full-catalog LLM-driven seeder. Loops every shipped
			// domain and runs seedDomainContent per entry. Best-effort
			// per-domain (one failure doesn't abort the rest).
			Name:        "seedAllDomainContent",
			Description: "Run seedDomainContent across every shipped domain. Tier-A + Tier-B generate via LLM; Tier-C gets the placeholder OR Wikipedia content (when an article mapping exists in tierCWikipediaArticles). Optional tierFilter / domainIdPrefix narrow the run. Costs ~$300-500 for the full ~250-domain catalog at recipe v1; partial runs are proportional.",
			Handler:     i.seedAllDomainContentHandler,
			ArgsSchema: map[string]string{
				"recipeVersion":  "string (optional) - default 'v1'.",
				"tierFilter":     "string (optional) - 'A' / 'B' / 'C' to seed only that tier. Empty = all.",
				"domainIdPrefix": "string (optional) - only seed domains whose id starts with this (e.g. 'physics_'). Useful for testing a category in isolation.",
			},
		},
		{
			// Cross-domain bridge generation (v2 of the seeder plan).
			// See integrations/knowledge/seed_bridge.go.
			Name:        "ensureKnowledgeBridge",
			Description: "For an agent's (roleSlug, domainIds) combination, ensures a cross-domain bridge corpus exists -- generating it via the seedDomainBridge prompt if missing. Hash-keyed by (roleSlug, sortedDomainIds) so identical combinations across agents share the same bridge id + chunks. Returns a node carrying the bridge id (or empty when no bridge is needed -- e.g. fewer than 2 domains).",
			Handler:     i.ensureKnowledgeBridgeHandler,
			ArgsSchema: map[string]string{
				"roleSlug":  "string (required) - the agent's role slug.",
				"domainIds": "array of string (required) - the agent's full domain set (gets sorted + deduped before hashing).",
			},
		},
		{
			// Preflight analyser for the chat 'Analyze for training'
			// action. Decides whether a chat exchange's topic warrants
			// augmenting one of the agent's domains with new chunks.
			// Synchronous + cheap (~1-2k token prompt). Frontend uses
			// the structured outcome to decide whether to surface a
			// Confirm step before paying for the heavier generate call.
			Name:        "augmentDomainAnalyze",
			Description: "Analyse a chat exchange + an agent's attached domains and return one of {addable, alreadyCovered, outOfScope} with a best-fit domainId + topic + reasoning. Drives the chat 'Analyze for training' action's preflight check; the heavier augmentDomainGenerate runs only when this returns addable + the user confirms.",
			Handler:     i.augmentDomainAnalyzeHandler,
			ArgsSchema: map[string]string{
				"userQuestion":  "string (required) - the user message that prompted the agent's reply.",
				"agentResponse": "string (required) - the agent's reply text. Used as the SIGNAL of a coverage gap, never as content to train on.",
				"domains":       "array of object (required) - the agent's attached domains. Each: {id, name, description}.",
				"retrieved":     "array of object (optional) - the RAG pool the agent saw before replying. Each: {domainId, similarity, titleHint}.",
			},
		},
		{
			// Topic-focused chunk generator for the same action. Runs
			// the augmentDomainContent prompt to produce ~10 dense
			// chunks targeted at one topic, embeds + writes them, and
			// inserts a Plan row for audit + Tasks-panel visibility.
			// Synchronous in v1 (~30s for the full AI + embed + write
			// path); the frontend shows a "training queued" state
			// until it returns. Pulls in a follow-up Plan-completed
			// canvas card via the existing Plan transition flow.
			Name:        "augmentDomainGenerate",
			Description: "Generate ~10 topic-focused chunks for a knowledge domain, embed + write each with source='augment' + provenance back-pointers, insert a Plan row for audit. Synchronous in v1 (~30s). Returns {planId, chunksAdded, domainId, domainName, topic}.",
			Handler:     i.augmentDomainGenerateHandler,
			ArgsSchema: map[string]string{
				"domainId":          "string (required) - target domain (must be one the agent has attached).",
				"topic":             "string (required) - narrow topic the chunks should focus on.",
				"sourceUtteranceId": "string (optional) - bare id of the utterance that triggered the augment; persisted on each chunk for provenance.",
				"sourceAgentId":     "string (optional) - bare id of the agent whose retrieval gap surfaced this; provenance.",
				"spaceId":           "string (required) - space the Plan row scopes to.",
				"requestedBy":       "string (required) - user id requesting the augment; Plan ownership + audit.",
			},
		},
		{
			// Trainer Agent tool (Q3 / Q9). STUB: no web-search
			// provider wired in-repo; returns an empty result set.
			// See integrations/knowledge/trainer_tools.go.
			Name:        capWebSearch,
			Description: "Issue a web search for the Trainer Agent. Returns []{url, title, snippet}. STUB until a search provider is wired -- returns empty + a note.",
			Handler:     i.webSearchHandler,
			ArgsSchema: map[string]string{
				"query":       "string (required) - the search query.",
				"num_results": "int (optional) - max results (default 5).",
			},
		},
		{
			// Trainer Agent tool (Q3 / Q9). Real bounded HTTP GET +
			// readable-text extraction. The Trainer's grounding primitive.
			Name:        capFetchURL,
			Description: "Fetch + extract readable text from a URL for the Trainer Agent. Bounded http.Client (dial/response/size caps). Returns {url, status, contentType, text, truncated}.",
			Handler:     i.fetchUrlHandler,
			ArgsSchema: map[string]string{
				"url": "string (required) - absolute http/https URL to fetch.",
			},
		},
		{
			// Trainer Agent tool (Q3 / Q9). Real embed-and-store path
			// (#645): resolves the chunk text, embeds it, writes the
			// vector to node_vectors so the chunk becomes retrievable.
			// Idempotent -- a chunk that already has a vector is skipped.
			Name:        capEmbedChunk,
			Description: "Embed one knowledge chunk so it becomes retrievable via similarTo. Resolves the chunk text, computes a vector, writes it to node_vectors keyed by the chunk id. Idempotent: returns {chunkId, embedded, alreadyEmbedded}.",
			Handler:     i.embedChunkHandler,
			ArgsSchema: map[string]string{
				"chunkId":  "string (required) - the chunk to embed.",
				"provider": "string (optional) - embedding provider name (default embedding3Small).",
			},
		},
		{
			// Bulk domain-warm path (#645). Embeds every unembedded
			// retrievable chunk in a domain (optionally scoped to one
			// Document) and drives the parent Document's embeddingStatus
			// none -> partial -> complete. Driven by the planner's
			// EmbedDomainItemsDispatcher on a kind='embedDomainItems' Plan
			// (after a domain is seeded or a user uploads a file).
			Name:        capEmbedDomainItems,
			Description: "Embed every unembedded validated chunk attached to a knowledge domain (optionally scoped to one Document), then recompute the parent Document's embeddingStatus (none -> partial -> complete). Idempotent. Returns {domainId, documentId, total, embedded, already, failed, documentsRolledUp}.",
			Handler:     i.embedDomainItemsHandler,
			ArgsSchema: map[string]string{
				"domainId":   "string (required) - the knowledge domain whose chunks to embed.",
				"documentId": "string (optional) - scope the run to one Document's chunks. Empty = the whole domain.",
				"provider":   "string (optional) - embedding provider name (default embedding3Small).",
			},
		},
	}
}

// chunkDefaults -- tuned for GPT-class context budgets. 1800 chars is
// roughly 450 tokens; 180-char overlap keeps paragraph boundaries
// stitchable without blowing up the chunk count.
const (
	defaultChunkSize = 1800
	defaultOverlap   = 180
	defaultLimit     = 5
	defaultProvider  = "embedding3Small"
)

// Chunk splits text into overlapping windows. It prefers paragraph and
// sentence boundaries within the target window so chunks read as coherent
// snippets rather than mid-sentence slices. Exported for tests and for
// reuse by any caller that just wants chunks without persisting.
func Chunk(text string, size, overlap int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if size <= 0 {
		size = defaultChunkSize
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 4
	}
	if len(text) <= size {
		return []string{text}
	}

	runes := []rune(text)
	var chunks []string
	i := 0
	for i < len(runes) {
		end := i + size
		if end >= len(runes) {
			chunks = append(chunks, strings.TrimSpace(string(runes[i:])))
			break
		}
		// Prefer a paragraph break within the last 25% of the window.
		window := string(runes[i:end])
		cut := end
		if idx := strings.LastIndex(window, "\n\n"); idx > size*3/4 {
			cut = i + idx + 2
		} else if idx := strings.LastIndex(window, ". "); idx > size*3/4 {
			cut = i + idx + 2
		} else if idx := strings.LastIndex(window, "\n"); idx > size*3/4 {
			cut = i + idx + 1
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[i:cut])))
		next := cut - overlap
		if next <= i {
			next = i + 1
		}
		i = next
	}
	return chunks
}

// ingestHandler chunks, embeds, and persists a document. Each chunk
// becomes a v1:common:documentChunk row with a deterministic id derived
// from domainId + sourceRef + seq so re-ingesting the same source is
// idempotent (the engine treats same-id inserts as new time-series
// versions; the latest wins and the vector is replaced via ON CONFLICT).
func (i *Integration) ingestHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	handlerStart := time.Now()

	domainId, _ := args["domainId"].(string)
	text, _ := args["text"].(string)
	sourceRef, _ := args["sourceRef"].(string)
	source, _ := args["source"].(string)
	if domainId == "" || text == "" {
		return nil, fmt.Errorf("knowledge.ingest: domainId and text are required")
	}
	if source == "" {
		return nil, fmt.Errorf("knowledge.ingest: source is required (one of llmSeeded / augment / crossDomainBridge / appStructure / fileUpload)")
	}
	providerName, _ := args["provider"].(string)
	if providerName == "" {
		providerName = defaultProvider
	}
	chunkSize := intArg(args, "chunkSize", defaultChunkSize)
	overlap := intArg(args, "overlap", defaultOverlap)
	i.Logger.Info("knowledge.ingest: handler start",
		"domainId", domainId,
		"sourceRef", sourceRef,
		"text_bytes", len(text),
		"provider", providerName,
	)

	if i.engine == nil {
		return nil, fmt.Errorf("knowledge.ingest: engine not configured")
	}
	if i.db() == nil {
		return nil, fmt.Errorf("knowledge.ingest: database not configured")
	}
	if i.embeddingProvider == nil {
		return nil, fmt.Errorf("knowledge.ingest: embedding provider not configured")
	}

	chunks := Chunk(text, chunkSize, overlap)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("knowledge.ingest: chunker produced no chunks (empty text?)")
	}

	provider, err := i.embeddingProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("knowledge.ingest: resolve provider %q: %w", providerName, err)
	}

	stored := 0
	for seq, chunkText := range chunks {
		chunkId := chunkIdFor(domainId, sourceRef, seq, chunkText)

		// Embed FIRST so a provider outage (no API key, rate limit, etc.)
		// leaves no orphan chunk row behind. The idempotent chunk id
		// means a later retry of the same source+seq+text writes the
		// row and vector together.
		vec, err := provider.Embed(ctx, chunkText)
		if err != nil {
			return nil, fmt.Errorf("knowledge.ingest: embed chunk %d: %w", seq, err)
		}

		insertQuery := fmt.Sprintf(
			`mutationCreateDocumentChunk({chunkId: %s, domainId: %s, text: %s, source: %s, sourceRef: %s, seq: %d, tokenCount: %d})`,
			quoteString(chunkId),
			quoteString(domainId),
			quoteString(chunkText),
			quoteString(source),
			quoteString(sourceRef),
			seq,
			approxTokens(chunkText),
		)
		if _, err := i.engine.Execute(ctx, insertQuery); err != nil {
			return nil, fmt.Errorf("knowledge.ingest: insert chunk %d: %w", seq, err)
		}

		if err := i.storeVector(ctx, chunkId, "v1:common:documentChunk", vec); err != nil {
			return nil, fmt.Errorf("knowledge.ingest: persist vector chunk %d: %w", seq, err)
		}
		stored++
	}

	i.Logger.Info("knowledge.ingest: completed",
		"elapsed_ms", time.Since(handlerStart).Milliseconds(),
		"domainId", domainId,
		"sourceRef", sourceRef,
		"chunks", stored,
		"provider", providerName,
	)

	return nil, nil
}

// storeVector writes a single pgvector row, mirroring what
// embedding.storeHandler does. Inlined here so we can call it tightly
// from the ingest loop without a DSL round-trip per chunk.
func (i *Integration) storeVector(ctx context.Context, nodeId, conceptName string, vec []float32) error {
	sqlText := `
		INSERT INTO node_vectors (id, concept, vector_field, embedding, created_at, updated_at)
		VALUES ($1, $2, 'content', $3::vector, NOW(), NOW())
		ON CONFLICT (id, vector_field) DO UPDATE SET
		  embedding = EXCLUDED.embedding,
		  concept = EXCLUDED.concept,
		  updated_at = NOW()
	`
	_, err := i.db().ExecContext(ctx, sqlText, nodeId, conceptName, vectorLiteral(vec))
	return err
}

func (i *Integration) db() *sql.DB {
	if i.dbGetter == nil {
		return nil
	}
	return i.dbGetter()
}

func (i *Integration) resolvePartition(ctx context.Context) string {
	if i.partitionFunc != nil {
		if p := i.partitionFunc(ctx); p != "" {
			return p
		}
	}
	return "default"
}

// chunkIdFor derives a stable chunk id. Include the chunk text hash so
// two ingests of the same source with the same seq but different content
// (say, the source was edited) generate different ids and the engine
// keeps both as time-series versions under their own ids instead of
// silently overwriting.
func chunkIdFor(domainId, sourceRef string, seq int, text string) string {
	// Bare deterministic hash -- NO colon-composed prefix. validateShortId
	// rejects shortIds containing colons unless concept-prefixed; the hash
	// map already carries every uniqueness factor (kind/domainId/sourceRef/
	// seq/text), so determinism + idempotent re-ingest are preserved.
	// Mirrors seedChunkId in seed_domain_content.go.
	return string(id.New().MustFromMap(map[string]any{
		"kind":      "knowledge-chunk",
		"domainId":  domainId,
		"sourceRef": sourceRef,
		"seq":       seq,
		"text":      text,
	}))
}

// approxTokens is a rough tokens-per-4-chars heuristic. Good enough for
// budgeting the systemKnowledge block; we don't need a real tokenizer.
func approxTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// quoteString produces a MemQL-safe double-quoted string literal.
func quoteString(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, string(r)...)
		}
	}
	b = append(b, '"')
	return string(b)
}

func intArg(args map[string]any, key string, fallback int) int {
	v, ok := args[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case int32:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	}
	return fallback
}

func toStringSlice(v any) []string {
	switch vv := v.(type) {
	case []string:
		out := make([]string, 0, len(vv))
		for _, s := range vv {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

func pgStringArray(items []string) string {
	if len(items) == 0 {
		return "{}"
	}
	b := strings.Builder{}
	b.WriteString("{")
	for i, item := range items {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("\"")
		b.WriteString(strings.ReplaceAll(item, "\"", "\\\""))
		b.WriteString("\"")
	}
	b.WriteString("}")
	return b.String()
}

func vectorLiteral(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%g", v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

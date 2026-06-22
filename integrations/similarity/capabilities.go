// Package similarity exposes the `similarTo` DSL operator backed by
// pgvector. It is the generalised successor to knowledge.lookup:
// any concept whose nodes carry a `content`-field vector in
// `node_vectors` can be queried by cosine-similarity to a free-text
// query, scoped to a concept and (optionally) a payload-domain list.
//
// Why this is separate from integrations/knowledge:
//
//   - knowledge owns the document-chunk INGESTION pipeline: chunker
//   - embedder + idempotent chunk row writes. It's a write-side
//     protocol adapter -- it reaches into pgvector and MemoryNodes
//     because that's the integration's core job.
//   - similarity owns RETRIEVAL. The retrieval path does not care
//     whether the underlying concept is a documentChunk, a
//     transcript snippet, an agent utterance -- anything with an
//     embedding in node_vectors is fair game. Decoupling retrieval
//     from the chunker means any concept that grows a content
//     vector can be queried the same way.
//
// DSL surface (see queries/v1/common/builtin/builtinSimilarTo.memql):
//
//	similarTo({
//	  text:    "<free-form query>",
//	  concept: "v1:common:documentChunk",
//	  domains: ["copresent-ui"],   // optional
//	  limit:   5,                   // default 5
//	  provider: "embedding3Small"   // default
//	})
//
// The handler embeds the text via the named embedding provider,
// runs the generic pgvector cosine join (with a CTE that picks the
// latest payload per id -- same time-series "latest" handling that
// knowledge.lookup needed), and returns the top-K nodes in
// similarity order. PreserveOrder=true on the capability ensures
// the engine's downstream default sort preserves that order.
package similarity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

const (
	defaultLimit    = 5
	defaultProvider = "embedding3Small"
)

// Integration holds the state needed by the similarTo handler.
// Constructed by the plug-in factory and populated with DB, embedding
// provider, and partition resolver via setters at registration time.
type Integration struct {
	Logger *slog.Logger

	dbGetter          func() *sql.DB
	embeddingProvider func(name string) (memql.EmbeddingAIProvider, error)
	partitionFunc     func(ctx context.Context) string
}

// New constructs a similarity integration.
func New(logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{Logger: logger}
}

// SetDBGetter injects the lazy DB handle getter. See
// integrations/knowledge/plugin.go for the reason this is lazy
// (bun.DB isn't live until MemoryNodesDatabase.Start has fired).
func (i *Integration) SetDBGetter(f func() *sql.DB) { i.dbGetter = f }

// SetEmbeddingProvider injects the provider registry lookup.
func (i *Integration) SetEmbeddingProvider(f func(name string) (memql.EmbeddingAIProvider, error)) {
	i.embeddingProvider = f
}

// SetPartitionFunc injects the context-to-partition resolver. The
// partition comes from the request envelope; all pgvector rows are
// partition-scoped.
func (i *Integration) SetPartitionFunc(f func(ctx context.Context) string) { i.partitionFunc = f }

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "similarity" }

// Capabilities lists the DSL-callable functions. Only similarTo at
// present; more vector operators can slot in alongside it as they
// land (e.g. a kNN variant, a hybrid lexical+vector path).
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "similarTo",
			Description: "Retrieve top-K nodes of the given concept ranked by cosine similarity to the supplied free-text query. Optionally scoped to a payload-domain list.",
			Handler:     i.similarToHandler,
			ArgsSchema: map[string]string{
				"text":     "string (required) - the free-text query to embed and search by",
				"concept":  "string (required) - concept id of the nodes to search (e.g. v1:common:documentChunk)",
				"domains":  "[]string (optional) - payload.domainId values to scope the search to; empty = all domains",
				"limit":    "int (optional) - max nodes to return (default 5)",
				"provider": "string (optional) - embedding provider name (default embedding3Small)",
			},
			// The handler already sorts by cosine similarity and we
			// want that order preserved end-to-end (the engine's
			// default sort-by-createdAt-desc would otherwise shuffle
			// them). PreserveOrder tells the engine to stamp
			// monotonic CreatedAt on the returned slice so the
			// downstream default sort reproduces our order.
			PreserveOrder: true,
		},
	}
}

// similarToHandler is the single DSL-facing entry point for vector
// similarity retrieval. Mirrors the proven pattern from the retired
// knowledge.lookup handler but generalises the target concept and
// keeps the handler's concern narrow: embed + cosine + domain filter.
func (i *Integration) similarToHandler(ctx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error) {
	handlerStart := time.Now()

	text, _ := args["text"].(string)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("similarity.similarTo: text is required")
	}
	concept, _ := args["concept"].(string)
	if strings.TrimSpace(concept) == "" {
		return nil, fmt.Errorf("similarity.similarTo: concept is required")
	}
	providerName, _ := args["provider"].(string)
	if providerName == "" {
		providerName = defaultProvider
	}

	// limit tolerance: the DSL parser produces int64 for integer
	// literals, JSON-unmarshal produces float64 when the caller marshaled
	// a Go map. Accept both so the handler is source-agnostic.
	limit := 0
	limitPresent := false
	if v, ok := args["limit"]; ok {
		limitPresent = true
		switch n := v.(type) {
		case int:
			limit = n
		case int64:
			limit = int(n)
		case int32:
			limit = int(n)
		case float64:
			limit = int(n)
		case float32:
			limit = int(n)
		}
	}
	if limit <= 0 {
		if limitPresent {
			limit = defaultLimit
		} else if target > 0 {
			limit = target
		} else {
			limit = defaultLimit
		}
	}

	domainIds := toStringSlice(args["domains"])

	if i.dbGetter == nil || i.dbGetter() == nil {
		return nil, fmt.Errorf("similarity.similarTo: database not configured")
	}
	if i.embeddingProvider == nil {
		return nil, fmt.Errorf("similarity.similarTo: embedding provider not configured")
	}

	i.Logger.Info("similarity.similarTo: handler start",
		"concept", concept,
		"domains", domainIds,
		"limit_arg_present", limitPresent,
		"limit_resolved", limit,
		"target_param", target,
		"text_len", len(text),
		"text_preview", truncateStr(text, 120),
		"provider", providerName,
	)

	provider, err := i.embeddingProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("similarity.similarTo: resolve provider %q: %w", providerName, err)
	}
	embedStart := time.Now()
	vec, err := provider.Embed(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("similarity.similarTo: embed query: %w", err)
	}
	embedElapsed := time.Since(embedStart)

	vecLiteral := vectorLiteral(vec)

	// Same-shape SQL as the retired knowledge.lookup but parameterised
	// on concept so the operator is reusable for any vector-indexed
	// concept (not just documentChunk). CTE picks the latest payload
	// per id (memQL is time-series; re-ingests append new versions)
	// then cosine-ranks on node_vectors.
	db := i.dbGetter()
	var rows *sql.Rows
	if len(domainIds) == 0 {
		sqlText := `
			WITH latest AS (
				SELECT DISTINCT ON (id) id, payload
				FROM "MemoryNodes"
				WHERE concept = $2
				ORDER BY id, "createdAt" DESC
			)
			SELECT latest.id, latest.payload,
			       1 - (nv.embedding <=> $1::vector) AS similarity
			FROM latest
			JOIN node_vectors nv ON nv.id = latest.id
			WHERE nv.vector_field = 'content'
			ORDER BY nv.embedding <=> $1::vector
			LIMIT $3
		`
		rows, err = db.QueryContext(ctx, sqlText, vecLiteral, concept, limit)
	} else {
		sqlText := `
			WITH latest AS (
				SELECT DISTINCT ON (id) id, payload
				FROM "MemoryNodes"
				WHERE concept = $2
				  AND (payload->>'domainId') = ANY($3)
				ORDER BY id, "createdAt" DESC
			)
			SELECT latest.id, latest.payload,
			       1 - (nv.embedding <=> $1::vector) AS similarity
			FROM latest
			JOIN node_vectors nv ON nv.id = latest.id
			WHERE nv.vector_field = 'content'
			ORDER BY nv.embedding <=> $1::vector
			LIMIT $4
		`
		rows, err = db.QueryContext(ctx, sqlText, vecLiteral, concept, pgStringArray(domainIds), limit)
	}
	if err != nil {
		return nil, fmt.Errorf("similarity.similarTo: query: %w", err)
	}
	defer rows.Close()

	var results []memorynodes.MemoryNode
	for rows.Next() {
		var (
			id         string
			payload    json.RawMessage
			similarity float64
		)
		if err := rows.Scan(&id, &payload, &similarity); err != nil {
			i.Logger.Warn("similarity.similarTo: scan row", "error", err)
			continue
		}
		var payloadMap map[string]any
		_ = json.Unmarshal(payload, &payloadMap)
		if payloadMap == nil {
			payloadMap = make(map[string]any)
		}
		payloadMap["_similarity"] = similarity
		merged, _ := json.Marshal(payloadMap)
		results = append(results, memorynodes.MemoryNode{
			ID:        id,
			Concept:   concept,
			Type:      memorynodes.NodeTypeObject,
			CreatedAt: time.Now().UTC(),
			Payload:   merged,
		})
	}

	var topSim float64
	if len(results) > 0 {
		var p map[string]any
		_ = json.Unmarshal(results[0].Payload, &p)
		if s, ok := p["_similarity"].(float64); ok {
			topSim = s
		}
	}
	i.Logger.Info("similarity.similarTo: handler completed",
		"elapsed_ms", time.Since(handlerStart).Milliseconds(),
		"embed_ms", embedElapsed.Milliseconds(),
		"concept", concept,
		"domains", domainIds,
		"limit", limit,
		"rows", len(results),
		"top_similarity", topSim,
	)
	return results, nil
}

// --- helpers (copied from integrations/knowledge so this package is
// self-contained; they're small enough that sharing wasn't worth a
// new internal package) -------------------------------------------------

func toStringSlice(v any) []string {
	switch vv := v.(type) {
	case []string:
		return vv
	case []any:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			if s, ok := item.(string); ok {
				out = append(out, s)
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

func truncateStr(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Package actionsearch exposes the `searchActions` DSL builtin (#1758, epic
// #1734): a pgvector cosine search over the action library's `intent`
// embeddings, so the planner can find a reusable action for a sub-goal before
// reasoning it from scratch.
//
// It mirrors the harness `recall` operator (integrations/harnessrecall) but is
// pure relevance (no recency term): an action's usefulness is its semantic fit
// to the sub-goal, not its age. The `intent` of each v1:actions:action is
// embedded into node_vectors (vectorField='intent') by the engine mint hook
// (component/memql/executor_mutation.go: embedActionIntent), and this search
// joins those vectors to the latest, active, owner-scoped action rows in one
// SQL statement.
//
// DSL surface (dsl/actions/queries.memql -> builtin searchActions):
//
//	searchActions({
//	  text:     "<sub-goal text, embedded server-side>",
//	  k:        5,                  // top-k (default 5)
//	  provider: "embedding3Small",  // embedding provider
//	})
//
// Each returned row carries the full action payload plus `_similarity`
// (cosine in [-1,1]) so the caller (the planner's REUSE/ADAPT/SYNTHESIZE
// decision) has the fitScore to threshold on.
package actionsearch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

const (
	defaultProvider = "embedding3Small"
	defaultK        = 5
)

// Integration holds the search handler's dependencies.
type Integration struct {
	Logger            *slog.Logger
	dbGetter          func() *sql.DB
	embeddingProvider func(name string) (memql.EmbeddingAIProvider, error)
	stagedConcept     func(conceptId string) bool
}

// New constructs an actionsearch integration.
func New(logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{Logger: logger}
}

// SetDBGetter injects the lazy DB handle getter.
func (i *Integration) SetDBGetter(f func() *sql.DB) { i.dbGetter = f }

// SetStagedConceptPredicate injects the staged-DATA visibility question
// (epic memql#3974, task memql#3984).
func (i *Integration) SetStagedConceptPredicate(f func(conceptId string) bool) { i.stagedConcept = f }

// SetEmbeddingProvider injects the provider registry lookup.
func (i *Integration) SetEmbeddingProvider(f func(name string) (memql.EmbeddingAIProvider, error)) {
	i.embeddingProvider = f
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "actionSearch" }

// Capabilities lists the DSL-callable functions.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "searchActions",
			Description: "Cosine-search the action library by a sub-goal text, returning the top-k active actions ranked by semantic fit (_similarity). Owner-scoped; embeds the query server-side and joins node_vectors (vectorField='intent') to the latest active action rows in one SQL statement. Powers the planner's REUSE/ADAPT/SYNTHESIZE library decision (#1758).",
			Handler:     i.searchHandler,
			ArgsSchema: map[string]string{
				"text":     "string (required) - sub-goal text, embedded server-side and matched by cosine similarity to action intents",
				"k":        "int (optional) - top-k actions to return (default 5, or the target param)",
				"provider": "string (optional) - embedding provider name (default embedding3Small)",
			},
			PreserveOrder: true,
		},
	}
}

type searchParams struct {
	text        string
	provider    string
	ownerUserId string
	k           int
}

func (i *Integration) searchHandler(ctx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error) {
	p, err := i.resolveParams(ctx, args, target)
	if err != nil {
		return nil, err
	}
	// staged-data: GATE -- see the note on searchSQL for why the check is here
	// rather than in the statement. searchActions feeds the planner's
	// REUSE/ADAPT/SYNTHESIZE decision, so a staged action is one the planner
	// could select and dispatch.
	if i.stagedConcept == nil {
		return nil, fmt.Errorf("actionSearch.searchActions: staged-concept predicate not wired; " +
			"refusing to search rather than answer as if nothing were staged")
	}
	if i.stagedConcept(memorynodes.ConceptActionsAction) {
		i.Logger.Info("actionSearch.searchActions: action data is STAGED; returning no rows",
			"concept", memorynodes.ConceptActionsAction)
		return nil, nil
	}

	if i.dbGetter == nil || i.dbGetter() == nil {
		return nil, fmt.Errorf("actionSearch.searchActions: database not configured")
	}
	if i.embeddingProvider == nil {
		return nil, fmt.Errorf("actionSearch.searchActions: embedding provider not configured")
	}

	provider, err := i.embeddingProvider(p.provider)
	if err != nil {
		return nil, fmt.Errorf("actionSearch.searchActions: resolve provider %q: %w", p.provider, err)
	}
	vec, err := provider.Embed(ctx, p.text)
	if err != nil {
		return nil, fmt.Errorf("actionSearch.searchActions: embed query: %w", err)
	}

	rows, err := i.dbGetter().QueryContext(ctx, searchSQL, vectorLiteral(vec), p.ownerUserId, p.k)
	if err != nil {
		return nil, fmt.Errorf("actionSearch.searchActions: query: %w", err)
	}
	defer rows.Close()

	results := make([]memorynodes.MemoryNode, 0, p.k)
	for rows.Next() {
		var (
			id         string
			payload    json.RawMessage
			similarity float64
		)
		if err := rows.Scan(&id, &payload, &similarity); err != nil {
			i.Logger.Warn("actionSearch.searchActions: scan row", "error", err)
			continue
		}
		var pm map[string]any
		_ = json.Unmarshal(payload, &pm)
		if pm == nil {
			pm = make(map[string]any)
		}
		pm["_similarity"] = similarity
		merged, _ := json.Marshal(pm)
		results = append(results, memorynodes.MemoryNode{
			ID:        id,
			Concept:   memorynodes.ConceptActionsAction,
			Type:      memorynodes.NodeTypeObject,
			Payload:   merged,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("actionSearch.searchActions: rows: %w", err)
	}
	return results, nil
}

func (i *Integration) resolveParams(ctx context.Context, args map[string]any, target int) (searchParams, error) {
	p := searchParams{provider: defaultProvider, k: defaultK}
	p.text, _ = args["text"].(string)
	if strings.TrimSpace(p.text) == "" {
		return p, fmt.Errorf("actionSearch.searchActions: text is required")
	}
	if pr, _ := args["provider"].(string); strings.TrimSpace(pr) != "" {
		p.provider = pr
	}
	if k, ok := toInt(args["k"]); ok && k > 0 {
		p.k = k
	} else if target > 0 {
		p.k = target
	}
	// Owner-scope = partition isolation (same key the mint stamps).
	if ac, ok := componentAuth.AccessFromContext(ctx); ok && ac != nil {
		p.ownerUserId = strings.TrimSpace(ac.UserId)
	}
	return p, nil
}

// searchSQL picks the latest version per action id, gates active + owner, then
// joins the intent vector and ranks by cosine similarity. Params:
//
//	$1 query vector literal
//	$2 owner user id ('' = unscoped, dev only)
//	$3 limit k
//
// staged-data: GATE -- enforced in searchHandler, not here. The `latest` CTE
// pins one FIXED concept, so the question has a constant answer per call and is
// cheapest asked before the round-trip. It also avoids the trap the inventory
// flagged at this exact statement: the CTE projects four expressions and
// `concept` is not one of them, so an outer conjunct would not compile -- and
// putting one on the outer SELECT would be wrong even if it did, because it
// drops the id rather than falling through.
const searchSQL = `
WITH latest AS (
    SELECT DISTINCT ON (id)
           id,
           payload,
           (payload->>'status')      AS status,
           (payload->>'ownerUserId') AS owner
    FROM "MemoryNodes"
    WHERE concept = 'v1:actions:action'
    ORDER BY id, "createdAt" DESC
)
SELECT latest.id,
       latest.payload,
       (1 - (nv.embedding <=> $1::vector)) AS similarity
FROM latest
JOIN node_vectors nv ON nv.id = latest.id
WHERE latest.status = 'active'
  AND ($2 = '' OR latest.owner = $2)
  AND nv.vector_field = 'intent'
ORDER BY similarity DESC
LIMIT $3
`

// vectorLiteral renders a float32 slice as a pgvector literal "[a,b,c]".
func vectorLiteral(vec []float32) string {
	if len(vec) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// toInt coerces a JSON-decoded numeric arg to int.
func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

package memql

// authoring_catalog_retrieval.go -- the fuzzy (similarTo-backed) tier of
// the compose-first catalog (epic memql#954, issue #957).
//
// authoring_catalog.go / authoring_catalog_match.go give the DETERMINISTIC
// half: an exact CatalogKey hit is a safe direct reuse, and when there is
// no exact hit DecideReuse hands back a CatalogMatchText to embed. This
// file closes the loop: it embeds that MatchText and runs a similarTo
// (pgvector cosine) query over the embeddings of the owner's cataloged
// constructs to return RANKED near-match candidates for a stated need.
//
// The retrieval reuses the EXISTING integrations rather than reaching into
// pgvector here: similarTo() (integration.similarity.similarTo) already
// embeds the query text via the named embedding provider and cosine-ranks
// node_vectors for a concept, returning nodes in similarity order with the
// score stamped on payload._similarity. Cataloged constructs grow their
// content vector at promotion time (the catalog-write path, the next
// increment) keyed by the construct id under vector_field='content', the
// same write-path documentChunk / harness:observation use -- so there is
// one embedding write-path and one retrieval path, not a bespoke pair.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// CatalogNearMatch is one ranked fuzzy-tier candidate: the cataloged
// construct's identity plus its cosine similarity to the stated need (1.0 =
// identical embedding, lower = further away). Ordered most-similar-first by
// the retrieval.
type CatalogNearMatch struct {
	CatalogEntry
	// Namespace is the construct's targetNamespace, so a caller composing the
	// match can resolve it under the right owner-scoped namespace.
	Namespace string `json:"namespace"`
	// Similarity is the cosine similarity in [0, 1] of the candidate's stored
	// embedding to the embedded need (the similarTo handler's _similarity).
	Similarity float64 `json:"similarity"`
}

// CatalogNearMatches embeds the supplied MatchText (typically a
// ReuseDecision.MatchText from DecideReuse) and runs similarTo over the
// owner's cataloged v1:authoring:construct embeddings, returning candidates
// ranked by cosine similarity for the stated need. limit caps the result
// count (<=0 falls back to the similarTo default). It is the fuzzy tier the
// planner consults AFTER an exact CatalogKey miss, before authoring net-new.
//
// similarTo embeds + cosine-ranks against ALL constructs of the concept that
// carry a content vector; only promoted (catalogued) constructs are embedded,
// so the search space is exactly the reusable catalog. The DecideReuse caller
// already narrowed by kind via the exact-match pass; we surface every kind's
// candidates here and let the caller filter, mirroring how the deterministic
// FindCatalogMatch leaves kind filtering explicit.
func (e *MemQLEngine) CatalogNearMatches(ctx context.Context, matchText string, limit int) ([]CatalogNearMatch, error) {
	if e == nil || e.integrations == nil {
		return nil, fmt.Errorf("catalog: engine has no integration registry")
	}
	handler, ok := e.integrations.Get("integration.similarity.similarTo")
	if !ok {
		return nil, fmt.Errorf("catalog: integration.similarity.similarTo is not registered on this binary")
	}
	return catalogNearMatches(ctx, handler, matchText, limit)
}

// catalogNearMatches is the registry-free core: it drives the supplied
// similarTo handler with the catalog-construct concept scope and maps the
// returned (already similarity-ordered) nodes to ranked CatalogNearMatch
// entries. Split out from the engine method so it is unit-testable with a
// fake handler -- the same pattern the integration handlers follow.
func catalogNearMatches(ctx context.Context, handler builtinExecutorHandler, matchText string, limit int) ([]CatalogNearMatch, error) {
	if strings.TrimSpace(matchText) == "" {
		return nil, fmt.Errorf("catalog: matchText is required for near-match retrieval")
	}
	args := map[string]any{
		"text":    matchText,
		"concept": memorynodes.ConceptAuthoringConstruct,
	}
	target := 0
	if limit > 0 {
		args["limit"] = limit
		target = limit
	}

	nodes, err := handler(ctx, args, target)
	if err != nil {
		return nil, fmt.Errorf("catalog: near-match similarTo: %w", err)
	}

	matches := make([]CatalogNearMatch, 0, len(nodes))
	for _, n := range nodes {
		if len(n.Payload) == 0 {
			continue
		}
		var p struct {
			Name            string  `json:"name"`
			Kind            string  `json:"kind"`
			CatalogKey      string  `json:"catalogKey"`
			TargetNamespace string  `json:"targetNamespace"`
			Similarity      float64 `json:"_similarity"`
		}
		if err := json.Unmarshal(n.Payload, &p); err != nil {
			// A row whose payload doesn't decode isn't a usable candidate;
			// skip it rather than fail the whole retrieval (same tolerance
			// the similarTo handler applies to a bad scan row).
			continue
		}
		matches = append(matches, CatalogNearMatch{
			CatalogEntry: CatalogEntry{
				Name:       p.Name,
				Kind:       p.Kind,
				CatalogKey: p.CatalogKey,
			},
			Namespace:  p.TargetNamespace,
			Similarity: p.Similarity,
		})
	}
	// similarTo returns the nodes in descending similarity order and the
	// engine's PreserveOrder contract keeps that order intact, so the slice
	// is already ranked most-similar-first; preserve it as-is.
	return matches, nil
}

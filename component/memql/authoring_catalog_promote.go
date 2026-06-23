package memql

// authoring_catalog_promote.go -- the catalog-WRITE path of the compose-first
// catalog (epic memql#954, issue #957): promote-with-provenance.
//
// authoring_catalog_retrieval.go is the READ side (embed a need, similarTo over
// the cataloged constructs). This file is what fills that search space: after a
// bundle activates, its net-new constructs are promoted INTO the owner's
// reusable catalog so later authoring can reuse them. Promotion computes the
// dedup signature (CatalogKey) and the embed text (CatalogMatchText) from the
// construct's own source, stamps the provenance (who = the owning bundle's
// owner, source bundle = catalogedFromBundleId, when = catalogedAt) via
// catalogueConstruct, and grows the construct's content vector in
// node_vectors so the fuzzy retrieval tier has something to compare against.
//
// The embedding write goes through the same integration.embedding.store
// capability documentChunk / harness:observation use (one embedding
// write-path, not a bespoke one), keyed by the construct id under
// vector_field='content' -- exactly what CatalogNearMatches' similarTo query
// joins on.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// CatalogPromotion is the computed plan for promoting one construct into the
// catalog: the deterministic signature + embed text derived from its source,
// plus the provenance the catalog-write records. Pure data -- computing it
// touches no engine, no DB -- so it is unit-testable on its own.
type CatalogPromotion struct {
	ConstructID  string `json:"constructId"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	FromBundleID string `json:"fromBundleId"`
	// CatalogKey is the name-independent dedup signature (CatalogKey).
	CatalogKey string `json:"catalogKey"`
	// MatchText is the name-independent embed text (CatalogMatchText) that
	// gets persisted AND embedded into node_vectors for the fuzzy tier.
	MatchText string `json:"matchText"`
}

// PlanCatalogPromotion computes the CatalogPromotion for a construct from its
// (id, kind, name, source) plus the bundle it is being promoted from. It runs
// the same CatalogKey + CatalogMatchText derivation the deterministic layer
// uses, so a promoted construct is findable by BOTH the exact-key matcher and
// the similarTo near-match tier. Returns an error if the source does not parse
// for the kind (you only promote constructs that compiled in the sandbox).
func PlanCatalogPromotion(constructID, kind, name, source, fromBundleID string) (CatalogPromotion, error) {
	if strings.TrimSpace(constructID) == "" {
		return CatalogPromotion{}, fmt.Errorf("catalog: constructId is required to promote")
	}
	if strings.TrimSpace(fromBundleID) == "" {
		return CatalogPromotion{}, fmt.Errorf("catalog: fromBundleId is required for provenance")
	}
	key, err := CatalogKey(kind, source)
	if err != nil {
		return CatalogPromotion{}, err
	}
	text, err := CatalogMatchText(kind, source)
	if err != nil {
		return CatalogPromotion{}, err
	}
	return CatalogPromotion{
		ConstructID:  constructID,
		Kind:         kind,
		Name:         name,
		FromBundleID: fromBundleID,
		CatalogKey:   key,
		MatchText:    text,
	}, nil
}

// PromoteConstructToCatalog is the catalog-write path: it computes the
// promotion plan, records it on the v1:authoring:construct row via
// catalogueConstruct (catalogued=true + catalogKey + catalogMatchText
// + provenance), then embeds the MatchText into node_vectors keyed by the
// construct id so the near-match retrieval tier can rank it.
//
// The mutation is the source of truth: if it fails, the promotion fails. The
// embedding is best-effort (mirrors embedHarnessObservation) -- the catalog
// row is durable regardless and a re-embed from the stored catalogMatchText
// can backfill; near-match retrieval just won't surface this construct until a
// vector exists.
func (e *MemQLEngine) PromoteConstructToCatalog(ctx context.Context, constructID, kind, name, source, fromBundleID string) (CatalogPromotion, error) {
	plan, err := PlanCatalogPromotion(constructID, kind, name, source, fromBundleID)
	if err != nil {
		return CatalogPromotion{}, err
	}
	if e == nil {
		return CatalogPromotion{}, fmt.Errorf("catalog: nil engine")
	}

	args, err := json.Marshal(map[string]string{
		"constructId":      plan.ConstructID,
		"catalogKey":       plan.CatalogKey,
		"catalogMatchText": plan.MatchText,
		"fromBundleId":     plan.FromBundleID,
	})
	if err != nil {
		return CatalogPromotion{}, fmt.Errorf("catalog: marshal promotion args: %w", err)
	}
	if _, err := e.Execute(ctx, "catalogueConstruct("+string(args)+")"); err != nil {
		return CatalogPromotion{}, fmt.Errorf("catalog: catalogueConstruct: %w", err)
	}

	e.embedCatalogConstruct(ctx, plan.ConstructID, plan.MatchText)
	return plan, nil
}

// embedCatalogConstruct embeds a freshly-promoted construct's CatalogMatchText
// into node_vectors so it is retrievable by CatalogNearMatches' similarTo
// query (vector_field='content', the documentChunk pattern). Best-effort: any
// failure (no embedding integration on this binary, empty text, embed error)
// is logged and swallowed -- the catalog row is already durable and a re-embed
// from the stored catalogMatchText can backfill.
func (e *MemQLEngine) embedCatalogConstruct(ctx context.Context, constructID, matchText string) {
	if e.integrations == nil || strings.TrimSpace(constructID) == "" || strings.TrimSpace(matchText) == "" {
		return
	}
	handler, ok := e.integrations.Get("integration.embedding.store")
	if !ok {
		// No embedding integration on this binary; nothing to do.
		return
	}
	args := map[string]any{
		"nodeId":      constructID,
		"text":        matchText,
		"concept":     memorynodes.ConceptAuthoringConstruct,
		"vectorField": "content",
	}
	if _, err := handler(ctx, args, 1); err != nil {
		if e.Logger != nil {
			e.Logger.Warn("catalog construct embed failed (near-match retrieval will skip this construct until backfilled)",
				"constructId", constructID, "error", err)
		}
		return
	}
	if e.Logger != nil {
		e.Logger.Debug("catalog construct embedded for near-match reuse", "constructId", constructID, "match_text_len", len(matchText))
	}
}

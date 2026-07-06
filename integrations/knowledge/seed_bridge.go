package knowledge

// Cross-domain "bridge" content (v2 of the knowledge-seeder plan).
//
// Per-domain chunks (the v1 seeder) cover what's INSIDE each domain.
// Bridge chunks cover the INTERSECTION between an agent's domains
// for a given role. They're a small supplementary corpus generated
// once per (role, sortedDomainIds) pair across the whole tenant --
// hash-keyed so identical combinations across many agents pay for
// the LLM call once and reuse the chunks forever.
//
// Architecture:
//
//   - Bridge id = sha256(roleSlug + ":" + sortedDomainIds.join(","))
//     truncated to 16 hex chars, prefixed "bridge-".
//   - The bridge metadata lives on a v1:knowledge:knowledgeBridge row.
//   - The bridge chunks live as v1:knowledge:documentChunk rows where
//     domainId == bridgeId. Same retrieval path as regular per-domain
//     chunks; no special retrieval logic needed.
//   - Agents whose (role, domains) match a bridge get the bridge id
//     auto-attached to their capabilities.domains so retrieval picks
//     up bridge content alongside their per-domain chunks.
//
// Trigger: this runs at the end of trainAgent (post step C). The
// training handler calls EnsureBridgeForAgent which:
//   1. Computes the deterministic bridge id from the agent's
//      (role, sortedDomains).
//   2. Checks if the bridge row already exists.
//   3. If not, generates ~10 chunks via the seedDomainBridge prompt,
//      stores them, creates the bridge row.
//   4. Returns the bridge id so the caller can attach it to the
//      agent's capabilities.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
)

// memorynodesMemoryNodeAlias makes memorynodes.MemoryNode reachable
// inside this file without forcing every callsite to spell the long
// import path. The alias is unexported because it's an internal
// convenience.
type memorynodesMemoryNodeAlias = memorynodes.MemoryNode

const bridgeRecipeVersion = "v1"

// bridgeChunkSchemaJSON mirrors seedDomainContentSchemaJSON. We
// re-declare it (rather than reusing) because the bridge-prompt
// instructions encourage shorter chunks and a smaller mix; keeping
// the schema separate lets us tighten constraints later without
// affecting the per-domain seeder.
const bridgeChunkSchemaJSON = `{
  "type": "object",
  "additionalProperties": false,
  "description": "seedDomainBridge.v1",
  "properties": {
    "chunks": {
      "type": "array",
      "minItems": 0,
      "maxItems": 20,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "kind": {
            "type": "string",
            "enum": ["principle", "decisionRule", "factExample"]
          },
          "title": {
            "type": "string",
            "description": "Short noun-phrase title naming the cross-domain insight."
          },
          "body": {
            "type": "string",
            "description": "80-200 words. MUST explicitly name two or more of the input domains in the body. If it could have been written from inside ONE domain it's not a bridge."
          },
          "keyTerms": {
            "type": "array",
            "items": {"type": "string"}
          }
        },
        "required": ["kind", "title", "body", "keyTerms"]
      }
    }
  },
  "required": ["chunks"]
}`

// BridgeId derives the deterministic bridge id from an agent's
// roleSlug + the SORTED list of domain ids the agent has attached.
// Same (role, domainSet) produces the same id; same id produces the
// same bridge content (cached forever at the recipe version).
//
// Returns "" when there's nothing to bridge (less than 2 domains,
// or roleSlug missing). Caller should treat empty as "no bridge
// needed for this agent."
func BridgeId(roleSlug string, domainIds []string) string {
	role := strings.TrimSpace(roleSlug)
	if role == "" {
		return ""
	}
	cleaned := make([]string, 0, len(domainIds))
	seen := make(map[string]bool, len(domainIds))
	for _, d := range domainIds {
		d = strings.TrimSpace(d)
		if d == "" || strings.HasPrefix(d, "bridge-") || seen[d] {
			continue
		}
		seen[d] = true
		cleaned = append(cleaned, d)
	}
	if len(cleaned) < 2 {
		return ""
	}
	sort.Strings(cleaned)
	combinationKey := strings.Join(cleaned, ",")
	return "bridge-" + string(id.New().MustFromMap(map[string]any{
		"kind":           "bridge",
		"recipeVersion":  bridgeRecipeVersion,
		"role":           role,
		"combinationKey": combinationKey,
	}))
}

// CombinationKeyFor exposes the canonical, sorted-and-deduped
// domain-id list as a comma-joined string -- same format used inside
// BridgeId. Useful for logs / DB inspection.
func CombinationKeyFor(domainIds []string) string {
	cleaned := make([]string, 0, len(domainIds))
	seen := make(map[string]bool, len(domainIds))
	for _, d := range domainIds {
		d = strings.TrimSpace(d)
		if d == "" || strings.HasPrefix(d, "bridge-") || seen[d] {
			continue
		}
		seen[d] = true
		cleaned = append(cleaned, d)
	}
	sort.Strings(cleaned)
	return strings.Join(cleaned, ",")
}

// EnsureBridgeForAgent computes the bridge id for an agent's
// (roleSlug, domainIds) combination, generates the bridge corpus if
// it doesn't already exist, and returns the bridge id. Idempotent:
// repeated calls for the same combination across agents are
// existence-check-only after the first.
//
// Returns ("", nil) when no bridge is needed (< 2 domains or no role).
// Returns (bridgeId, nil) on success (whether the bridge was newly
// generated or already existed).
func (i *Integration) EnsureBridgeForAgent(
	ctx context.Context,
	roleSlug string,
	domainIds []string,
) (string, error) {
	bridgeId := BridgeId(roleSlug, domainIds)
	if bridgeId == "" {
		return "", nil
	}
	combinationKey := CombinationKeyFor(domainIds)

	// Existence check via the knowledgeBridge concept.
	exists, err := i.bridgeExists(ctx, bridgeId)
	if err != nil {
		i.Logger.Warn("knowledge.EnsureBridgeForAgent: existence check failed; assuming missing",
			"bridgeId", bridgeId, "err", err)
		exists = false
	}
	if exists {
		i.Logger.Debug("knowledge.bridge: cache hit",
			"bridgeId", bridgeId, "combinationKey", combinationKey)
		return bridgeId, nil
	}

	// Resolve the input domains' name + description for the prompt.
	domainEntries, roleLabel := i.resolveBridgeInputs(ctx, roleSlug, domainIds)
	if len(domainEntries) < 2 {
		// Couldn't resolve enough metadata to do a meaningful
		// bridge -- drop. (Likely a user-created domain that's
		// missing description; bridges only make sense for fully-
		// defined domain combinations.)
		return "", nil
	}

	written, err := i.generateBridgeContent(ctx, bridgeId, roleSlug, roleLabel, domainEntries, combinationKey)
	if err != nil {
		return "", err
	}

	// Stamp the bridge metadata row.
	createQuery := fmt.Sprintf(
		`mutation mutationCreateKnowledgeBridge(bridgeId: %s, roleSlug: %s, domainIds: %s, combinationKey: %s, chunkCount: %d, recipeVersion: %s, generatedAt: %s)`,
		quoteString(bridgeId),
		quoteString(roleSlug),
		jsonArrayFromCleanedIds(domainIds),
		quoteString(combinationKey),
		written,
		quoteString(bridgeRecipeVersion),
		quoteString(time.Now().UTC().Format(time.RFC3339)),
	)
	if _, err := i.engine.Execute(ctx, createQuery); err != nil {
		i.Logger.Warn("knowledge.bridge: createBridgeRow failed",
			"bridgeId", bridgeId, "err", err)
	}

	i.Logger.Info("knowledge.bridge: generated",
		"bridgeId", bridgeId,
		"roleSlug", roleSlug,
		"combinationKey", combinationKey,
		"chunksWritten", written,
	)
	return bridgeId, nil
}

// bridgeExists returns true if a knowledgeBridge row with the
// given id already exists in the active partition.
func (i *Integration) bridgeExists(ctx context.Context, bridgeId string) (bool, error) {
	q := fmt.Sprintf(`query queryKnowledgeBridgeById(bridgeId: %s)`, quoteString(bridgeId))
	res, err := i.engine.Execute(ctx, q)
	if err != nil {
		return false, err
	}
	return res != nil && res.Bundle != nil && len(res.Bundle.Nodes) > 0, nil
}

// resolveBridgeInputs reads each domain's name + description from
// the catalog (via standardDomains and the DB fallback). Also
// resolves the human-readable role label.
//
// Drops domain ids it can't resolve -- bridges need domain metadata
// to be useful.
func (i *Integration) resolveBridgeInputs(
	ctx context.Context,
	roleSlug string,
	domainIds []string,
) (entries []map[string]any, roleLabel string) {
	roleLabel = humanRoleLabel(roleSlug)
	cleaned := make([]string, 0, len(domainIds))
	seen := make(map[string]bool, len(domainIds))
	for _, d := range domainIds {
		d = strings.TrimSpace(d)
		if d == "" || strings.HasPrefix(d, "bridge-") || seen[d] {
			continue
		}
		seen[d] = true
		cleaned = append(cleaned, d)
	}
	sort.Strings(cleaned)
	catalog := allSeedDomains()
	for _, did := range cleaned {
		// Fast path: the shipped catalog + pack-registered domains (in-memory).
		var found *StandardDomain
		for j := range catalog {
			if catalog[j].ID == did {
				found = &catalog[j]
				break
			}
		}
		if found != nil {
			entries = append(entries, map[string]any{
				"id":          found.ID,
				"name":        found.Name,
				"description": found.Description,
			})
			continue
		}
		// Fallback: query DB (user-created domain).
		q := fmt.Sprintf(`query knowledgeDomainById(domainId: %s)`, quoteString(did))
		res, err := i.engine.Execute(ctx, q)
		if err != nil || res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
			continue
		}
		node := res.Bundle.Nodes[0]
		if node == nil || node.Payload == nil {
			continue
		}
		payloadJSON, err := node.Payload.MarshalJSON()
		if err != nil {
			continue
		}
		var dp struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(payloadJSON, &dp); err != nil {
			continue
		}
		entries = append(entries, map[string]any{
			"id":          did,
			"name":        dp.Name,
			"description": dp.Description,
		})
	}
	return entries, roleLabel
}

// humanRoleLabel converts a role slug like "customer-service" to
// "Customer Service" for the prompt body. Pure string transform;
// no external lookup needed for the role catalog.
func humanRoleLabel(slug string) string {
	parts := strings.Split(slug, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// generateBridgeContent runs the seedDomainBridge prompt + writes
// the chunks under the bridge id. Returns the number of chunks
// written (0 is a valid outcome -- the prompt may decline to
// generate when domains have no real intersection, e.g.
// {quantum-mechanics, restaurant-dining}).
func (i *Integration) generateBridgeContent(
	ctx context.Context,
	bridgeId string,
	roleSlug string,
	roleLabel string,
	domainEntries []map[string]any,
	combinationKey string,
) (int, error) {
	data := map[string]any{
		"roleSlug":    roleSlug,
		"roleLabel":   roleLabel,
		"domains":     domainEntries,
		"targetCount": 10,
	}
	raw, err := i.engine.InvokeAIStructured(
		ctx,
		"seedDomainBridge",
		data,
		"seedDomainBridge",
		json.RawMessage(bridgeChunkSchemaJSON),
		true,
	)
	if err != nil {
		return 0, fmt.Errorf("seedDomainBridge AI call: %w", err)
	}
	var payload seedChunkPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return 0, fmt.Errorf("seedDomainBridge JSON parse: %w", err)
	}

	provider, err := i.embeddingProvider(defaultProvider)
	if err != nil {
		return 0, fmt.Errorf("resolve embedding provider %q: %w", defaultProvider, err)
	}

	// Build a synthetic StandardDomain for the storeSeedChunk path so
	// the chunk rows carry a sensible name + tier. We tier bridges as
	// "A" because the source LLM call already ran with the same
	// tier-A safety posture as per-domain content.
	bridgeAsDomain := StandardDomain{
		ID:   bridgeId,
		Name: fmt.Sprintf("Bridge: %s", combinationKey),
		Tier: "A",
	}

	written := 0
	for idx, c := range payload.Chunks {
		c.Title = strings.TrimSpace(c.Title)
		c.Body = strings.TrimSpace(c.Body)
		if c.Title == "" || c.Body == "" {
			continue
		}
		if err := i.storeSeedChunk(ctx, bridgeAsDomain, bridgeRecipeVersion, idx, c, "llm-bridge", "crossDomainBridge", provider); err != nil {
			i.Logger.Warn("knowledge.bridge: chunk write failed",
				"bridgeId", bridgeId, "chunkIndex", idx, "err", err)
			continue
		}
		written++
	}
	return written, nil
}

// ensureKnowledgeBridgeHandler is the DSL-callable wrapper around
// EnsureBridgeForAgent. Returns a synthetic memory node carrying
// the bridge id so callers (notably the training pipeline) can
// auto-attach the id to the agent's capabilities.
func (i *Integration) ensureKnowledgeBridgeHandler(
	ctx context.Context,
	args map[string]any,
	_ int,
) ([]memorynodesNode, error) {
	roleSlug, _ := args["roleSlug"].(string)
	domainIds := stringSliceFromAny(args["domainIds"])

	bridgeId, err := i.EnsureBridgeForAgent(ctx, roleSlug, domainIds)
	if err != nil {
		return nil, fmt.Errorf("knowledge.ensureKnowledgeBridge: %w", err)
	}

	payload, _ := json.Marshal(map[string]any{
		"bridgeId":       bridgeId,
		"roleSlug":       roleSlug,
		"combinationKey": CombinationKeyFor(domainIds),
	})
	return []memorynodesNode{
		{
			ID:        fmt.Sprintf("knowledgeBridge-result:%d", time.Now().UnixNano()),
			Concept:   "v1:knowledge:knowledgeBridge",
			Payload:   payload,
			CreatedAt: time.Now(),
		},
	}, nil
}

// memorynodesNode is a tiny re-alias to the existing MemoryNode type
// from the memory-nodes database package. We use this alias to keep
// import statements local to this file's responsibilities.
type memorynodesNode = memorynodesMemoryNodeAlias

// stringSliceFromAny is a defensive helper for DSL args -- the
// engine sometimes hands us []any instead of []string.
func stringSliceFromAny(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// jsonArrayFromCleanedIds is a tiny helper to format a sorted-deduped
// list of domain ids as a JSON array string for inline mutation
// queries. Mirrors knowledge/seed.go's jsonArray helper but does
// the cleaning step first.
func jsonArrayFromCleanedIds(ids []string) string {
	cleaned := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, d := range ids {
		d = strings.TrimSpace(d)
		if d == "" || strings.HasPrefix(d, "bridge-") || seen[d] {
			continue
		}
		seen[d] = true
		cleaned = append(cleaned, d)
	}
	sort.Strings(cleaned)
	out, _ := json.Marshal(cleaned)
	return string(out)
}

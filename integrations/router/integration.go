// Package router exposes the router's read-only admin capabilities to the
// MemQL DSL: the live model catalog, the routing policies, and the nightly
// fold of decision records into evidence.
//
// IT SEALS NO KEY. It used to carry a capability that encrypted a vendor API
// key and stored it as a cluster-wide secret, for a settings page that was
// removed; nothing ever read the secret back (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decision K18). The cluster has no key of its own: it reaches a vendor by
// workload identity federation. A person's API key is theirs, and is added,
// sealed, lent and removed through integrations/vendorkeys.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	routerlib "github.com/znasllc-io/memql/component/router"
)

// Integration implements memql.IntegrationProvider for router admin
// capabilities. Registered via the plug-in path.
type Integration struct {
	engine    memql.IntegrationEngineAccess
	providers *memql.ProviderRegistry
	policies  *memql.PolicyRegistry
	logger    *slog.Logger
	// ruleRenderer is the tree's ONE renderer of the routing-rule grammar
	// (epic memql#5146, D5). Injected rather than called directly, because the
	// evidence fold hashes what it renders and a second renderer of one grammar
	// drifts -- silently, since the approval would then carry to text nobody
	// read.
	//
	// It is now WIRED, to epic memql#5127's own renderer. It was nil while that
	// epic was unlanded and the fold refused to propose rather than rendering
	// its own text; the refusal path stays, because a node that never folds
	// evidence still installs none and must not quietly invent one.
	ruleRenderer func(routerlib.Form) (string, error)
}

// SetRuleRenderer installs the tree's rule renderer.
//
// It is a setter rather than a constructor argument because the renderer lives
// in a package this one does not otherwise need, and because a node that never
// folds evidence has no use for it. A fold with none REFUSES, loudly, rather
// than proposing a rule it rendered itself.
func (i *Integration) SetRuleRenderer(render func(routerlib.Form) (string, error)) {
	if i == nil {
		return
	}
	i.ruleRenderer = render
}

// New builds a Router admin integration.
func New(
	engine memql.IntegrationEngineAccess,
	providers *memql.ProviderRegistry,
	policies *memql.PolicyRegistry,
	logger *slog.Logger,
) *Integration {
	return &Integration{
		engine:    engine,
		providers: providers,
		policies:  policies,
		logger:    logger,
	}
}

func (i *Integration) IntegrationName() string { return "router" }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return append(evidenceScopeCapabilities(), []memql.IntegrationCapability{
		{
			Name:        "listModels",
			Description: "Return the full live model catalog -- every provider registered at engine startup with its vendor, model id, pricing, and availability. Feeds the /router/catalog page.",
			Handler:     i.handleListModels,
		},
		{
			Name:        "routingEvidenceFold",
			Description: "Fold the week's router calls into per-model, per-level evidence rows, and open a routingReview approval where the evidence carries a demotion. It PROPOSES and never applies: routing is a thing a person is accountable for, and a rule that appeared overnight is an edit nobody made.",
			Handler:     i.handleEvidenceFold,
		},
		{
			Name:        "listPolicies",
			Description: "Return all routing policies loaded from policies/v1/*.memql. Feeds the /router/policies page.",
			Handler:     i.handleListPolicies,
		},
	}...)
}

func (i *Integration) handleListModels(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i.providers == nil {
		return nil, fmt.Errorf("integration.router.listModels: provider registry not available")
	}
	// Iterate every registered provider and flatten to the shape the
	// /router/catalog UI expects.
	nodes := make([]memorynodes.MemoryNode, 0)
	for _, modality := range []memql.ProviderModality{memql.ModalityText, memql.ModalityTTS, memql.ModalitySTT, memql.ModalityEmbedding} {
		for _, entry := range i.providers.ProvidersByModality(modality) {
			if entry == nil {
				continue
			}
			pricing := entry.Config.Pricing()
			payload := map[string]any{
				"providerName":              entry.Config.Name,
				"providerType":              entry.Config.Type,
				"vendor":                    vendorFromType(entry.Config.Type),
				"model":                     entry.Config.Model,
				"description":               entry.Config.Description,
				"modality":                  string(entry.Config.ResolvedModality()),
				"isDefault":                 entry.Config.Default,
				"available":                 entry.Available,
				"inputCostPerMillion":       pricing.InputPerMillion,
				"outputCostPerMillion":      pricing.OutputPerMillion,
				"cachedInputCostPerMillion": pricing.CachedInputPerMillion,
				"pricingConfigured":         pricing.Configured(),
				"contextWindow":             entry.Config.ContextWindow(),
			}
			raw, _ := json.Marshal(payload)
			nodes = append(nodes, memorynodes.MemoryNode{
				ID:      "model:" + entry.Config.Name,
				Concept: "v1:router:modelcatalog",
				Payload: raw,
			})
		}
	}
	return nodes, nil
}

func (i *Integration) handleListPolicies(_ context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i.policies == nil {
		return nil, fmt.Errorf("integration.router.listPolicies: policy registry not available")
	}
	all := i.policies.All()
	nodes := make([]memorynodes.MemoryNode, 0, len(all))
	for _, p := range all {
		if p == nil {
			continue
		}
		// maxLatencyMs / maxTimeToFirstTokenMs / preferredRoles are GONE from
		// this projection with the annotations behind them (epic memql#5127).
		// They were shown to an operator as though they governed selection,
		// and nothing read them.
		payload := map[string]any{
			"name":        p.Name,
			"description": p.Description,
			"primary":     p.Primary,
			"fallbacks":   p.Fallbacks,
			"chain":       p.ProviderChain(),
		}
		raw, _ := json.Marshal(payload)
		nodes = append(nodes, memorynodes.MemoryNode{
			ID:      "policy:" + p.Name,
			Concept: "v1:router:policycatalog",
			Payload: raw,
		})
	}
	return nodes, nil
}

// vendorFromType mirrors the mapping in component/router -- kept here
// so the integration package doesn't import router (which would pull in
// the full router package just for a string helper).
func vendorFromType(typeName string) string {
	lower := strings.ToLower(strings.TrimSpace(typeName))
	switch {
	case strings.Contains(lower, "openai"):
		return "openai"
	case strings.Contains(lower, "anthropic"):
		return "anthropic"
	default:
		return lower
	}
}

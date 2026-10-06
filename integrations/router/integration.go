// Package router exposes BYOK credential + budget admin capabilities
// to the MemQL DSL so the frontend's /router/settings page can add,
// rotate, and delete API keys and budgets without the plaintext ever
// being persisted. Plaintext keys arrive through this integration's
// capabilities; they leave encrypted via `component/secret.Encrypt`
// and are inserted into v1:router:apikey by the ordinary mutation
// pipeline.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	routerlib "github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/component/secret"
)

// Integration implements memql.IntegrationProvider for router admin
// capabilities. Registered via the plug-in path.
type Integration struct {
	engine    memql.IntegrationEngineAccess
	providers *memql.ProviderRegistry
	policies  *memql.PolicyRegistry
	// ruleActivator arms runtime-authored routing rules. Installed from app/;
	// nil on a node with no authored runtime, where the capability refuses.
	ruleActivator RuleActivator
	logger        *slog.Logger
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
			Name:        "setApiKey",
			Description: "Encrypt a plaintext vendor API key and persist it as a v1:router:apikey row. Returns the inserted row id.",
			Handler:     i.handleSetApiKey,
		},
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
		{
			Name:        "activateRoutingRule",
			Description: "Render a routing rule from a structured form, run it through the authoring gates, and arm it live. Owner or developer. No model is involved: the construct is a deterministic rendering of the form.",
			Handler:     i.handleActivateRoutingRule,
		},
		{
			Name:        "retireRoutingRule",
			Description: "Retire a runtime-authored routing rule. A shipped rule is refused: the shipped set is re-read from the embedded tree on every boot, so one you want out of the way is out-ranked with a higher precedence rather than removed.",
			Handler:     i.handleRetireRoutingRule,
		},
	}...)
}

// secretNameForVendor maps a router vendor identifier to the
// canonical secret name used by ResolveSecret. The name deliberately
// matches the env-var convention so the same lookup resolves against
// v1:platform:partitionSecret (partition BYOK), v1:platform:globalSecret (instance
// default), or -- for callers that still read env -- the process env.
func secretNameForVendor(vendor string) string {
	return strings.ToUpper(strings.TrimSpace(vendor)) + "_API_KEY"
}

func (i *Integration) handleSetApiKey(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	vendor := strings.TrimSpace(stringArg(args, "vendor"))
	plaintext := strings.TrimSpace(stringArg(args, "plaintextKey"))
	label := strings.TrimSpace(stringArg(args, "label"))
	addedBy := strings.TrimSpace(stringArg(args, "addedBy"))

	if vendor == "" {
		return nil, fmt.Errorf("integration.router.setApiKey: vendor is required")
	}
	if plaintext == "" {
		return nil, fmt.Errorf("integration.router.setApiKey: plaintextKey is required")
	}

	ciphertext, fingerprint, err := secret.Encrypt(plaintext)
	if err != nil {
		// Most common failure: MEMQL_MASTER_KEY unset.
		// Surface a clear message so the /router/settings UI can
		// explain the operator needs to set the env var before BYOK
		// will work.
		return nil, fmt.Errorf("integration.router.setApiKey: encrypt: %w", err)
	}

	// Folded into v1:platform:partitionSecret (Phase 3 of the env-var refactor):
	//  * Secret name = <VENDOR>_API_KEY so ResolveSecret reuses the
	//    same name space as instance-wide defaults in v1:platform:globalSecret.
	//  * kind="vendor_api_key" lets the router resolver (and the UI's
	//    listRouterApiKeys query) filter by type.
	//  * Deterministic id `secret-vendor-<vendor>` means re-submitting
	//    rotates the key for that vendor in the current partition; the
	//    time-series history in MemoryNodes retains prior values.
	name := secretNameForVendor(vendor)
	id := fmt.Sprintf("secret-vendor-%s", strings.ToLower(vendor))

	mutArgs := map[string]any{
		"id":             id,
		"name":           name,
		"encryptedValue": ciphertext,
		"fingerprint":    fingerprint,
		"kind":           "vendor_api_key",
		"description":    label,
		"addedBy":        addedBy,
		"active":         true,
	}
	query, err := langparser.RenderCall("setPartitionSecret", mutArgs)
	if err != nil {
		return nil, fmt.Errorf("integration.router.setApiKey: render mutation: %w", err)
	}
	if _, err := i.engine.Execute(ctx, query); err != nil {
		return nil, fmt.Errorf("integration.router.setApiKey: execute mutation: %w", err)
	}

	// Return a redacted projection so the DSL caller can chain on it
	// without seeing the ciphertext.
	p := map[string]any{
		"vendor":      vendor,
		"name":        name,
		"fingerprint": fingerprint,
		"kind":        "vendor_api_key",
		"label":       label,
	}
	payloadJSON, _ := json.Marshal(p)
	return []memorynodes.MemoryNode{
		{
			ID:      id,
			Concept: "v1:platform:partitionSecret",
			Payload: payloadJSON,
		},
	}, nil
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

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// Package router exposes BYOK credential + budget admin capabilities
// to the memQL DSL so the CoPresent /router/settings page can add,
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
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
)

// Integration implements memql.IntegrationProvider for router admin
// capabilities. Registered via the plug-in path.
type Integration struct {
	engine    memql.IntegrationEngineAccess
	providers *memql.ProviderRegistry
	policies  *memql.PolicyRegistry
	logger    *slog.Logger
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
	return []memql.IntegrationCapability{
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
			Name:        "listPolicies",
			Description: "Return all routing policies loaded from policies/v1/*.memql. Feeds the /router/policies page.",
			Handler:     i.handleListPolicies,
		},
	}
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
	argsJSON, err := json.Marshal(mutArgs)
	if err != nil {
		return nil, fmt.Errorf("integration.router.setApiKey: marshal mutation args: %w", err)
	}
	query := fmt.Sprintf("setPartitionSecret(%s)", string(argsJSON))
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
		payload := map[string]any{
			"name":                  p.Name,
			"description":           p.Description,
			"primary":               p.Primary,
			"fallbacks":             p.Fallbacks,
			"chain":                 p.ProviderChain(),
			"maxLatencyMs":          p.MaxLatencyMs,
			"maxTimeToFirstTokenMs": p.MaxTimeToFirstTokenMs,
			"preferredRoles":        p.PreferredRoles,
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

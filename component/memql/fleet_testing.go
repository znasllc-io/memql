package memql

import "context"

// Cross-package test construction for the provider and policy registries.
//
// WHY NOT A _test.go FILE. The seam these serve spans packages:
// component/router walks a policy chain over a ProviderRegistry, and the
// property worth testing there -- that an unavailable fleet primary with no
// authored fallback refuses instead of quietly calling a paid provider --
// cannot be tested from inside this package, because this package has no
// router. Both constructors are unexported, so the router's test could not
// build the inputs at all.
//
// They are deliberately narrow: build a registry, put a named client in it,
// build a policy registry from name -> chain. Nothing here reaches into the
// live registries a running engine holds.

// NewProviderRegistryForTest returns an empty provider registry.
func NewProviderRegistryForTest() *ProviderRegistry {
	return newProviderRegistry()
}

// ContextWithProviderAdmissionForTest wires an explicit admission fixture for
// cross-package tests of goal-backed provider calls. Production contexts receive
// this guard from the serving model seam. An unwired test remains fail-closed;
// this constructor neither changes that default nor supplies a permissive guard.
func ContextWithProviderAdmissionForTest(ctx context.Context, guard ProviderAdmissionGuard) context.Context {
	return context.WithValue(ctx, workProviderGuardKey{}, guard)
}

// RegisterForTest inserts an AVAILABLE provider entry under a name.
//
// Available is true unconditionally, which is the point: these fixtures stand
// in for a provider whose credential resolved, so a test asserting "the
// authored fallback ran" is asserting about the chain rather than about auth.
func (r *ProviderRegistry) RegisterForTest(name, providerType, model string, client AIProvider) {
	if r == nil {
		return
	}
	r.setEntry(&ProviderConfigEntry{
		Config:    ProviderConfig{Name: name, Type: providerType, Model: model},
		Client:    client,
		Available: true,
	})
	r.markDeclared(name)
}

// RegisterWithParamsForTest is RegisterForTest with the record's params, which
// is where a provider's PRICES and its context window live.
//
// It is separate rather than a longer RegisterForTest because most fixtures do
// not care: they stand in for "a provider that answers". The ones that do care
// are the router's selector tests, where the whole question is what an
// unpriced or window-less record does -- and a fixture that could not express
// "declared no price" could not test the answer.
func (r *ProviderRegistry) RegisterWithParamsForTest(name, providerType, model string, params map[string]any, client AIProvider) {
	if r == nil {
		return
	}
	r.setEntry(&ProviderConfigEntry{
		Config:    ProviderConfig{Name: name, Type: providerType, Model: model, Params: params},
		Client:    client,
		Available: true,
	})
	r.markDeclared(name)
}

// FleetModelAdvertisedOnEveryMachineForTest is the catalog fleetcatalog's
// Project builds when every machine behind a model reports the same label:
// each machine carries the model's advertised capabilities and window.
//
// Eligibility is decided per online machine (FleetModel.eligibleFor), so a
// fixture that sets only the model-level union describes machines that
// advertise nothing. A machine that already carries attributes is left as it
// is, so a fixture can still make one machine differ from the rest.
func FleetModelAdvertisedOnEveryMachineForTest(m FleetModel) FleetModel {
	machines := make([]FleetMachine, len(m.Machines))
	for i, mm := range m.Machines {
		if mm.Capabilities == (FleetModelCapabilities{}) && mm.ContextWindow == 0 {
			mm.Capabilities = FleetModelCapabilities{
				Vision: m.Vision, AudioIn: m.AudioIn, AudioOut: m.AudioOut, ImageGen: m.ImageGen,
				Embeddings: m.Embeddings, Tools: m.Tools, StructuredOutput: m.StructuredOutput,
			}
			mm.ContextWindow = m.ContextWindow
		}
		machines[i] = mm
	}
	m.Machines = machines
	return m
}

// NewPolicyRegistryForTest builds a policy registry from name -> provider
// chain, where the first entry is the @primary and the rest are @fallback in
// try order -- the same reading ProviderChain() gives a parsed policy.
func NewPolicyRegistryForTest(chains map[string][]string) *PolicyRegistry {
	r := newPolicyRegistry()
	for name, chain := range chains {
		cfg := &PolicyConfig{Name: name}
		if len(chain) > 0 {
			cfg.Primary = chain[0]
			cfg.Fallbacks = append(cfg.Fallbacks, chain[1:]...)
		}
		r.byName[name] = cfg
	}
	return r
}

// SetDefaultForTest IS GONE with the registry default it pinned (epic
// memql#5137, D3). A one-shot cloud consent no longer resolves to "the
// registry's default" -- there is no such thing -- so a test that needs a
// consented cloud provider names it on the request.

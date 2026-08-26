package memql

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
	return newProviderRegistry("")
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

// SetDefaultForTest pins the registry default, which is what a one-shot cloud
// consent resolves to.
func (r *ProviderRegistry) SetDefaultForTest(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultProvider = name
	r.defaultPinned = true
}

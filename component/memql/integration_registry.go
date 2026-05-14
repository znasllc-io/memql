package memql

import (
	"fmt"
	"sort"
	"sync"
)

// IntegrationRegistry holds registered IntegrationProviders and their capabilities.
// It is thread-safe and supports dynamic registration after engine startup.
type IntegrationRegistry struct {
	mu           sync.RWMutex
	providers    map[string]IntegrationProvider
	capabilities map[string]*IntegrationCapability // keyed by FQN: integration.<name>.<cap>
}

func newIntegrationRegistry() *IntegrationRegistry {
	return &IntegrationRegistry{
		providers:    make(map[string]IntegrationProvider),
		capabilities: make(map[string]*IntegrationCapability),
	}
}

// Register adds an integration provider and indexes its capabilities by
// fully-qualified name (integration.<integrationName>.<capabilityName>).
// Returns an error if the provider or any capability name is already registered.
func (r *IntegrationRegistry) Register(provider IntegrationProvider) error {
	if provider == nil {
		return fmt.Errorf("integration provider is nil")
	}

	name := provider.IntegrationName()
	if name == "" {
		return fmt.Errorf("integration provider returned empty name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.providers[name]; exists {
		return fmt.Errorf("integration %q already registered", name)
	}

	r.providers[name] = provider

	for _, cap := range provider.Capabilities() {
		if cap.Name == "" {
			return fmt.Errorf("integration %q has a capability with empty name", name)
		}
		fqn := qualifiedCapabilityName(name, cap.Name)
		if _, exists := r.capabilities[fqn]; exists {
			return fmt.Errorf("integration capability %q already registered", fqn)
		}
		capCopy := cap
		r.capabilities[fqn] = &capCopy
	}

	return nil
}

// Get returns a capability handler by fully-qualified name.
func (r *IntegrationRegistry) Get(fqn string) (builtinExecutorHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cap, ok := r.capabilities[fqn]
	if !ok || cap == nil || cap.Handler == nil {
		return nil, false
	}
	return cap.Handler, true
}

// List returns all registered capability fully-qualified names, sorted alphabetically.
func (r *IntegrationRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.capabilities))
	for fqn := range r.capabilities {
		names = append(names, fqn)
	}
	sort.Strings(names)
	return names
}

// ProviderNames returns the names of all registered integration providers.
func (r *IntegrationRegistry) ProviderNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Provider returns the registered IntegrationProvider for the given name,
// or nil if none is registered. Callers that need a concrete integration
// type use this to look up by name and then type-assert.
func (r *IntegrationRegistry) Provider(name string) IntegrationProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[name]
}

// qualifiedCapabilityName builds the FQN for a capability.
func qualifiedCapabilityName(integrationName, capabilityName string) string {
	return "integration." + integrationName + "." + capabilityName
}

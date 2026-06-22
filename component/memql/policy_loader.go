package memql

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// PolicyRegistry holds the parsed AI Router policies loaded at engine
// startup. Policies are immutable for the life of the process -- we
// reload on restart, not at runtime. Keyed by policy name.
type PolicyRegistry struct {
	mu     sync.RWMutex
	byName map[string]*PolicyConfig
	byRole map[string]string // role -> policy name (first @preferredRole wins)
}

func newPolicyRegistry() *PolicyRegistry {
	return &PolicyRegistry{
		byName: make(map[string]*PolicyConfig),
		byRole: make(map[string]string),
	}
}

// Lookup returns the parsed policy for the given name, or (nil, false)
// if no such policy is registered.
func (r *PolicyRegistry) Lookup(name string) (*PolicyConfig, bool) {
	if r == nil {
		return nil, false
	}
	key := strings.TrimSpace(name)
	if key == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byName[key]
	return p, ok
}

// All returns a snapshot copy of every registered policy. Used by the
// catalog query and the policies admin page.
func (r *PolicyRegistry) All() []*PolicyConfig {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*PolicyConfig, 0, len(r.byName))
	for _, p := range r.byName {
		out = append(out, p)
	}
	return out
}

// DefaultForRole returns the name of the policy whose @preferredRole
// list contains the given role, or "" when no policy claims that role.
// Used by the replier to resolve "what policy should this agent use
// when it hasn't explicitly picked one?".
func (r *PolicyRegistry) DefaultForRole(role string) string {
	if r == nil {
		return ""
	}
	key := strings.TrimSpace(role)
	if key == "" {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byRole[key]
}

// Count returns the number of registered policies.
func (r *PolicyRegistry) Count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byName)
}

// loadAIPolicies returns an empty registry. Pass 3 of the DSL
// restructure migration retired the legacy walk over
// dsl/v1/policies/. AI Router policies now live in
// dsl/policies/routing.memql and load via a unified loader
// (LoadUnifiedRoutingPolicies) called from engine.go. This stub
// keeps the function signature so the bootstrap call site
// compiles unchanged.
//
// TODO: wire LoadUnifiedRoutingPolicies as the actual loader and
// delete this stub.
func loadAIPolicies(logger *slog.Logger) (*PolicyRegistry, error) {
	_ = logger
	_ = strings.TrimSpace("") // keep import alive until decommissioned
	return newPolicyRegistry(), nil
}

// formatPolicySummary returns a one-line human-readable summary of a
// policy, used in logs and the /router/policies admin page.
func formatPolicySummary(p *PolicyConfig) string {
	if p == nil {
		return ""
	}
	chain := p.ProviderChain()
	if len(chain) == 0 {
		return fmt.Sprintf("%s (empty chain)", p.Name)
	}
	return fmt.Sprintf("%s -> %s", p.Name, strings.Join(chain, " -> "))
}

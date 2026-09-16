package memql

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// PolicyRegistry holds the parsed AI Router policies loaded at engine
// startup. Shipped defaults are immutable; durable configuration snapshots overlay them. We
// reload on restart, not at runtime. Keyed by policy name.
type PolicyRegistry struct {
	mu       sync.RWMutex
	byName   map[string]*PolicyConfig
	defaults map[string]*PolicyConfig
	store    PolicyStore
}

func newPolicyRegistry() *PolicyRegistry {
	return &PolicyRegistry{
		byName: make(map[string]*PolicyConfig),
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

// DefaultForRole and its byRole index are DELETED (epic memql#5127). They
// answered "what policy should this agent use when it has not picked one?"
// from @preferredRole, an annotation nothing selected on -- and by the time
// they were removed the method had no callers at all.
//
// A rule answers that question now, and answers it better: `role` is one of
// @when's seven keys, the match is recorded on the decision, and the mapping
// lives in the DSL rather than in a map rebuilt from an annotation. Do not
// reintroduce a role index here; that is what the rule registry is.

// Count returns the number of registered policies.
func (r *PolicyRegistry) Count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byName)
}

// loadAIPolicies returns an empty registry (Pass 1). Pass 3 of the DSL
// restructure migration retired the legacy walk over dsl/v1/policies/. AI
// Router policies now live in dsl/policies/policies.memql and load via a
// second pass -- the unified loader `LoadUnifiedPolicies`, called right
// after this stub in engine_bootstrap.go's Init -- that overlays the real
// registry on top of this stub's empty one. This stub stays in place
// (rather than being deleted once the real loader was wired) so the
// bootstrap call site compiles unchanged, mirroring the provider loader
// immediately above it (loadAIProviders + LoadUnifiedProviders).
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

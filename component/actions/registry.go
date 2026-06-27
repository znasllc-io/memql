package actions

import (
	"fmt"
	"sort"
	"sync"
)

// Registry holds authored actions keyed by (name, version). It is safe for
// concurrent use: loaded once at startup, read on every action step.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]map[int]*Action // name -> version -> action
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]map[int]*Action{}}
}

// Register adds an action. A duplicate (name, version) is an error so a
// conflicting authored definition fails loud rather than silently winning.
func (r *Registry) Register(a *Action) error {
	if a == nil {
		return fmt.Errorf("actions: nil action")
	}
	if a.Name == "" {
		return fmt.Errorf("actions: action has empty name")
	}
	if a.Version < 1 {
		a.Version = 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	versions := r.byName[a.Name]
	if versions == nil {
		versions = map[int]*Action{}
		r.byName[a.Name] = versions
	}
	if _, exists := versions[a.Version]; exists {
		return fmt.Errorf("actions: duplicate action %s@%d (origin %s)", a.Name, a.Version, a.Origin)
	}
	versions[a.Version] = a
	return nil
}

// Lookup returns the action pinned to (name, version).
func (r *Registry) Lookup(name string, version int) (*Action, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	versions := r.byName[name]
	if versions == nil {
		return nil, false
	}
	a, ok := versions[version]
	return a, ok
}

// LookupLatest returns the highest-version action registered for name. Used
// for a floating reference ("name" with no @version).
func (r *Registry) LookupLatest(name string) (*Action, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	versions := r.byName[name]
	if len(versions) == 0 {
		return nil, false
	}
	best := -1
	for v := range versions {
		if v > best {
			best = v
		}
	}
	return versions[best], best >= 0
}

// Len returns the number of distinct (name, version) actions registered.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, versions := range r.byName {
		n += len(versions)
	}
	return n
}

// Names returns the registered action names, sorted (diagnostics / tests).
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

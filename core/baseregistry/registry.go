// Package baseregistry holds the generic thread-safe name -> *T store
// every per-construct registry (Function / Tool / Spec / Shape /
// Prompt / Policy / ...) embeds. The store handles the common bits
// (mutex, map, count, names, snapshot, list, lookup, add, upsert)
// behind a single parametric type.
//
// Construct-specific wrappers stay distinct types so callers keep
// using FunctionRegistry / ToolRegistry / etc. -- the wrapper embeds
// *Registry[T] via Go struct composition:
//
//	type FunctionRegistry struct {
//	    *baseregistry.Registry[Function]
//	}
//
// Per-construct clone + name validation are supplied as function
// parameters to New so the generic stays free of construct-specific
// imports.
package baseregistry

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry is a generic, thread-safe name -> *T store.
type Registry[T any] struct {
	mu       sync.RWMutex
	byName   map[string]*T
	kind     string             // construct label used in error messages
	clone    func(*T) *T        // egress copy; identity if no copy is needed
	validate func(string) error // ingress name validator; nil = no check
}

// New constructs a registry.
//
//	kind     -- label used in error messages ("function", "tool", ...)
//	clone    -- egress copy strategy. Pass nil to skip cloning (the
//	            store will hand out raw pointers).
//	validate -- optional name validator run before Add/Upsert.
func New[T any](kind string, clone func(*T) *T, validate func(string) error) *Registry[T] {
	if clone == nil {
		clone = func(t *T) *T { return t }
	}
	return &Registry[T]{
		byName:   make(map[string]*T),
		kind:     kind,
		clone:    clone,
		validate: validate,
	}
}

// Count returns the number of registered entries.
func (r *Registry[T]) Count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byName)
}

// Names returns the sorted list of registered identifiers.
func (r *Registry[T]) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Has reports whether an entry exists.
func (r *Registry[T]) Has(name string) bool {
	if r == nil {
		return false
	}
	key := strings.TrimSpace(name)
	if key == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byName[key]
	return ok
}

// Lookup returns the entry by name. The returned value is cloned
// per the registry's clone strategy. Returns (nil, false) when not
// found; this is the canonical comma-ok form.
func (r *Registry[T]) Lookup(name string) (*T, bool) {
	if r == nil {
		return nil, false
	}
	key := strings.TrimSpace(name)
	if key == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	item, ok := r.byName[key]
	if !ok || item == nil {
		return nil, false
	}
	return r.clone(item), true
}

// Get returns the entry by name or an error citing the kind label.
// Convenience wrapper around Lookup for callers that want the
// error-form.
func (r *Registry[T]) Get(name string) (*T, error) {
	if r == nil {
		return nil, fmt.Errorf("%s registry is not initialized", r.kindLabel())
	}
	key := strings.TrimSpace(name)
	if key == "" {
		return nil, fmt.Errorf("%s name is required", r.kindLabel())
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	item, ok := r.byName[key]
	if !ok || item == nil {
		return nil, fmt.Errorf("%s %q not found", r.kindLabel(), key)
	}
	return r.clone(item), nil
}

// Snapshot returns a cloned map of all entries. Empty registry
// returns nil to preserve existing caller expectations.
func (r *Registry[T]) Snapshot() map[string]*T {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.byName) == 0 {
		return nil
	}
	result := make(map[string]*T, len(r.byName))
	for name, item := range r.byName {
		if item == nil {
			continue
		}
		result[name] = r.clone(item)
	}
	return result
}

// Range calls fn for each registered entry in sorted-name order, WITHOUT
// cloning, under the registry read lock. fn returning false stops the walk.
// For read-only scans where List()'s per-entry deep clone is pure waste
// (e.g. the meta-command alias lookup, #2707); fn MUST NOT retain or mutate
// the item, and MUST NOT call back into the registry (the RLock is held).
func (r *Registry[T]) Range(fn func(name string, item *T) bool) {
	if r == nil || fn == nil {
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if item := r.byName[name]; item != nil {
			if !fn(name, item) {
				return
			}
		}
	}
}

// List returns all registered entries as a sorted slice.
func (r *Registry[T]) List() []*T {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.byName) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]*T, 0, len(names))
	for _, name := range names {
		if item := r.byName[name]; item != nil {
			result = append(result, r.clone(item))
		}
	}
	return result
}

// Add inserts a new entry. Errors when the name is already taken
// or fails validation.
func (r *Registry[T]) Add(name string, item *T) error {
	if r == nil {
		return fmt.Errorf("%s registry is not initialized", r.kindLabel())
	}
	if item == nil {
		return fmt.Errorf("%s is nil", r.kindLabel())
	}
	if r.validate != nil {
		if err := r.validate(name); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[name]; exists {
		return fmt.Errorf("%s %q already registered", r.kindLabel(), name)
	}
	r.byName[name] = r.clone(item)
	return nil
}

// Upsert inserts or replaces an entry. Used by unified loaders.
func (r *Registry[T]) Upsert(name string, item *T) error {
	if r == nil {
		return fmt.Errorf("%s registry is not initialized", r.kindLabel())
	}
	if item == nil {
		return fmt.Errorf("%s is nil", r.kindLabel())
	}
	if name == "" {
		return fmt.Errorf("%s name is required", r.kindLabel())
	}
	if r.validate != nil {
		if err := r.validate(name); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[name] = r.clone(item)
	return nil
}

// Remove deletes an entry by name. It is a no-op when the name is absent
// (so an idempotent caller -- e.g. a durable DEMOTE re-applied via the
// cross-node broadcast -- never errors), and when the registry is nil.
func (r *Registry[T]) Remove(name string) {
	if r == nil {
		return
	}
	key := strings.TrimSpace(name)
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byName, key)
}

func (r *Registry[T]) kindLabel() string {
	if r == nil || r.kind == "" {
		return "registry"
	}
	return r.kind
}

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
	if _, ok := r.byName[key]; ok {
		return true
	}
	_, ok := r.resolveBareLocked(key)
	return ok
}

// --- namespaced keys (memql#3897) ---------------------------------------
//
// Entries are keyed `<namespace>.<name>` for anything loaded from a file, and
// by the bare name for the engine-internal constructs that have no origin. That
// is what lets two namespaces declare a `plan` -- the constraint memql#3897 was
// opened to remove, and the one that made a product DSL bundle unable to name a
// construct the engine had already named.
//
// # Why a bare lookup still resolves
//
// A reference inside a compiled body is looked up at EXECUTION time, from a
// context that has no idea which file it came from -- `newFunctionValidator(
// fns.Snapshot(), nil)` in engine.go builds its view per query, and the only
// "origin" it carries is auth.CallOrigin. Load-time resolution rewrites those
// references to their qualified form (see component/memql's construct scope),
// and this is the floor underneath it: a reference the rewriter did not reach
// still resolves, PROVIDED the name is unambiguous.
//
// So the failure mode of an incomplete rewrite is the behaviour this tree
// already had, rather than a construct that stops resolving. What is NOT
// tolerated is ambiguity: once two namespaces declare the name, a bare lookup
// has no correct answer and picking one would be exactly the silent capture
// memql#3802 fixed for concepts -- the wrong construct bound, with OK=true. It
// is an error naming both namespaces instead.

// resolveBareLocked finds the single namespaced entry whose name half matches a
// bare key. Caller holds at least the read lock.
//
// Returns ok=false when nothing matches AND when more than one does. The second
// case is the important one: a guess here is a silently wrong construct.
func (r *Registry[T]) resolveBareLocked(bare string) (string, bool) {
	if strings.Contains(bare, ".") {
		return "", false // already qualified; a miss is a miss
	}
	var found string
	suffix := "." + bare
	for key := range r.byName {
		if !strings.HasSuffix(key, suffix) {
			continue
		}
		if found != "" {
			return "", false // ambiguous
		}
		found = key
	}
	if found == "" {
		return "", false
	}
	return found, true
}

// bareCandidatesLocked lists every namespaced key matching a bare name, so an
// ambiguity error can name them. Caller holds at least the read lock.
func (r *Registry[T]) bareCandidatesLocked(bare string) []string {
	suffix := "." + bare
	var out []string
	for key := range r.byName {
		if strings.HasSuffix(key, suffix) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
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
		resolved, found := r.resolveBareLocked(key)
		if !found {
			return nil, false
		}
		item = r.byName[resolved]
		if item == nil {
			return nil, false
		}
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
		resolved, found := r.resolveBareLocked(key)
		if !found {
			// AMBIGUITY IS ITS OWN ERROR, not a "not found" (memql#3897).
			// Two namespaces declaring the name is the state this change
			// exists to allow, so the refusal has to name them and say how
			// to disambiguate -- otherwise the first product bundle to
			// collide with a core name reads it as the construct having
			// vanished.
			if candidates := r.bareCandidatesLocked(key); len(candidates) > 1 {
				return nil, fmt.Errorf("%s %q is declared in %d namespaces (%s) -- "+
					"name it as <namespace>.%s, or import it with an alias",
					r.kindLabel(), key, len(candidates), strings.Join(candidates, ", "), key)
			}
			return nil, fmt.Errorf("%s %q not found", r.kindLabel(), key)
		}
		item = r.byName[resolved]
		if item == nil {
			return nil, fmt.Errorf("%s %q not found", r.kindLabel(), key)
		}
	}
	return r.clone(item), nil
}

// LookupIndex is Snapshot plus a BARE ALIAS for every unambiguous name
// (memql#3897).
//
// WHY IT EXISTS AS A SECOND METHOD. Snapshot has two kinds of consumer and the
// namespaced key broke exactly one of them:
//
//	for _, fn := range r.Snapshot()      iteration -- key shape is irrelevant
//	snapshot[someBareName]               KEYED LOOKUP -- was a direct map hit
//
// The keyed consumers are the ones resolving a construct reference out of a
// compiled body, from a context with no namespace (`newFunctionValidator(
// fns.Snapshot(), nil)`), so a qualified-only map turns every such reference
// into "not found". This gives them a map where both spellings work.
//
// AN AMBIGUOUS NAME GETS NO BARE KEY, deliberately, which is the same rule Get
// enforces: two namespaces declaring it means a bare reference has no correct
// answer, and the consumer's own "not found" is the right outcome -- far better
// than binding whichever one the map iteration happened to reach, which is the
// silent capture memql#3802 fixed for concepts.
//
// Counting consumers must keep using Snapshot: this map is deliberately larger
// than the registry.
func (r *Registry[T]) LookupIndex() map[string]*T {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.byName) == 0 {
		return nil
	}
	// TWO PASSES, because an alias must never overwrite or evict a REAL entry.
	// An engine-internal construct with no origin occupies the bare key
	// legitimately; a single-pass version that treated the collision as
	// ambiguity would delete it and take a working construct out of scope.
	out := make(map[string]*T, len(r.byName)*2)
	for key, item := range r.byName {
		if item != nil {
			out[key] = r.clone(item)
		}
	}
	candidates := map[string]string{}
	ambiguous := map[string]bool{}
	for key, item := range r.byName {
		if item == nil {
			continue
		}
		bare := nameHalf(key)
		if bare == key {
			continue // already unnamespaced; it IS the bare key
		}
		if _, real := r.byName[bare]; real {
			continue // a real unnamespaced entry owns this key
		}
		if _, seen := candidates[bare]; seen {
			ambiguous[bare] = true
			continue
		}
		candidates[bare] = key
	}
	for bare, key := range candidates {
		if ambiguous[bare] {
			continue
		}
		out[bare] = out[key]
	}
	return out
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
		// The NAME half, not the key (memql#3897). A key is
		// `<namespace>.<name>` and every per-construct validator checks
		// camelCase -- so validating the key would refuse every namespaced
		// entry for containing a dot and a lowercase namespace, which is the
		// key's own required shape.
		if err := r.validate(nameHalf(name)); err != nil {
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

// nameHalf is the construct name inside a `<namespace>.<name>` key.
//
// Split at the LAST dot: a namespace is a directory PATH and may contain more
// than one segment, while a construct name never contains a dot -- the parsers
// refuse it -- so the last dot is unambiguously the join.
func nameHalf(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[i+1:]
	}
	return key
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
		if err := r.validate(nameHalf(name)); err != nil {
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

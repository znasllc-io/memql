package memoryNodes

import (
	"fmt"
	"sync"
)

// Registry exposes read-only access to loaded concept metadata.
type Registry interface {
	Get(name string) (*Concept, error)
	List() []*Concept
}

// MemoryRegistry maintains a thread-safe map of concept names to Concept definitions.
//
// # Known gap: duplicate canonical ids last-win SILENTLY (memql#3008)
//
// The map is keyed by canonical id, so two declarations that assemble to the
// same id collapse to one entry and whichever registers last wins. Nothing on
// the boot path reports it -- there is no duplicate detector for concepts
// anywhere in loading.
//
// Recorded rather than fixed, deliberately. Boot also loads product bundles
// delivered at runtime through MEMQL_DSL_PATH, so turning last-wins into a
// refusal here could stop a deployed product from starting, and that call
// needs visibility into bundles outside this repo. The LINT closes the half
// that can be closed safely: component/memql/dslimports reports a same-id
// collision as a hard error naming both files and the id (see
// duplicateConceptCollision), so a tree in THIS repo cannot reach boot
// carrying one. Closing the boot half deserves its own issue and its own
// blast-radius measurement.
type MemoryRegistry struct {
	mu       sync.RWMutex
	concepts map[string]*Concept
}

// newMemoryRegistry constructs an empty registry instance.
func newMemoryRegistry() *MemoryRegistry {
	return &MemoryRegistry{
		concepts: make(map[string]*Concept),
	}
}

var (
	defaultRegistry          = newMemoryRegistry()
	_               Registry = (*MemoryRegistry)(nil)
)

// DefaultRegistry returns the globally shared Registry instance.
func DefaultRegistry() Registry {
	return defaultRegistry
}

// CloneDefaultRegistry returns an ISOLATED *MemoryRegistry seeded from a
// snapshot of the global default registry. Mutating the clone (via
// MergeAll / ReplaceAll) never touches the global default, and later
// registrations into the default never leak into a previously-taken
// clone -- the two share no underlying map.
//
// This backs the authoring sandbox's candidate-defined-concept path
// (issue #956): a planner-authored bundle can declare NEW concepts and
// overlay them onto the clone so later constructs in the same bundle
// bind against them, all without ever mutating the live engine's
// concept registry.
//
// Note the *Concept pointers are shared with the default (a shallow
// copy of the name->pointer map). That is intentional: the clone exists
// to ADD candidate concepts, not to rewrite existing ones, and Concept
// values are treated as immutable once registered.
func CloneDefaultRegistry() *MemoryRegistry {
	return defaultRegistry.Clone()
}

// Clone returns an isolated copy of this registry. The returned registry
// has its own concept map seeded from the receiver's current contents;
// subsequent mutations on either side are independent.
func (r *MemoryRegistry) Clone() *MemoryRegistry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	next := make(map[string]*Concept, len(r.concepts))
	for name, concept := range r.concepts {
		next[name] = concept
	}
	return &MemoryRegistry{concepts: next}
}

// ReplaceAll swaps the existing registry contents with the provided concepts.
func ReplaceAll(concepts map[string]*Concept) {
	defaultRegistry.ReplaceAll(concepts)
}

// MergeAll adds the provided concepts to the existing registry,
// overwriting entries with matching names. Unlike ReplaceAll it
// preserves entries not present in the input map.
//
// Used by the unified loader (Pass 2 of the DSL restructure) so it
// can register new-tree concepts on top of the legacy loader's
// registrations without clobbering anything. Once Pass 2 retires
// the legacy loader, callers can switch back to ReplaceAll.
func MergeAll(concepts map[string]*Concept) {
	defaultRegistry.MergeAll(concepts)
}

// MergeAll adds the provided concepts to the registry, replacing
// any existing entries with the same name. Empty inputs are no-ops.
func (r *MemoryRegistry) MergeAll(concepts map[string]*Concept) {
	if len(concepts) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, concept := range concepts {
		if name == "" || concept == nil {
			continue
		}
		r.concepts[name] = concept
	}
}

// ReplaceAll swaps the existing registry contents with the provided concepts.
func (r *MemoryRegistry) ReplaceAll(concepts map[string]*Concept) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(concepts) == 0 {
		r.concepts = make(map[string]*Concept)
		return
	}

	next := make(map[string]*Concept, len(concepts))
	for name, concept := range concepts {
		if name == "" || concept == nil {
			continue
		}
		next[name] = concept
	}

	r.concepts = next
}

// Get retrieves a concept by name.
func Get(name string) (*Concept, error) {
	return defaultRegistry.Get(name)
}

// Get retrieves a concept by name or returns an error when not found.
func (r *MemoryRegistry) Get(name string) (*Concept, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	concept, ok := r.concepts[name]
	if !ok {
		return nil, fmt.Errorf("concept %q not found", name)
	}

	return concept, nil
}

// List returns a slice copy of all registered concepts.
func List() []*Concept {
	return defaultRegistry.List()
}

// List returns a slice copy of all registered concepts.
func (r *MemoryRegistry) List() []*Concept {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]*Concept, 0, len(r.concepts))
	for _, concept := range r.concepts {
		result = append(result, concept)
	}
	return result
}

// All returns a copy of all registered concepts keyed by concept name.
func All() map[string]*Concept {
	return defaultRegistry.All()
}

// All returns a copy of all registered concepts keyed by concept name.
func (r *MemoryRegistry) All() map[string]*Concept {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]*Concept, len(r.concepts))
	for name, concept := range r.concepts {
		result[name] = concept
	}
	return result
}

package memql

// authoring_runtime.go -- Gate 4 of the planner-authored-automations
// pipeline (epic memql#954, issue #959): the LIVE, mutable, owner-scoped
// authored-construct runtime.
//
// Once a v1:authoring:bundle passes Gate 1 (isolated compile+bind, #956),
// Gate 2 (dry-run, #958) and Gate 3 (user approval), it ACTIVATES: its
// member constructs become live and resolvable at execution time. That
// runtime is the mutable layer this file implements -- modeled on the one
// proven mutable-after-startup registry in the engine, IntegrationRegistry
// (integration_registry.go): thread-safe register / lookup, supporting
// dynamic mutation after engine startup.
//
// Two properties make the authored layer different from the core registries:
//
//  1. OWNER-SCOPED. Every authored construct belongs to exactly one
//     v1:identity:user (the bundle owner). Two owners can register a
//     construct of the same name without collision; resolution is always
//     keyed by (ownerUserId, kind, name). An owner can never see or resolve
//     another owner's authored constructs.
//
//  2. CORE-FIRST RESOLUTION. The sealed core DSL (loaded at engine Init from
//     the embedded tree) is authoritative and immutable. The authored layer
//     is consulted ONLY AFTER core misses -- an authored construct can never
//     shadow a core one. ResolveConstruct() encodes that precedence.
//
// This file is the registry + resolver foundation. The per-owner automation
// scheduler (subscription under the author's authz envelope), the
// per-automation circuit breaker, and cluster leader-gating layer on top in
// later #959 increments.

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// AuthoredConstructStatus is the live lifecycle state of a registered
// authored construct. It mirrors the construct-grain status on
// v1:authoring:construct (draft / active / retired) plus the runtime-only
// `paused` state a user toggle or a circuit breaker drives without retiring
// (so a paused construct can be resumed in place).
type AuthoredConstructStatus string

const (
	// AuthoredActive: registered and resolvable at execution time.
	AuthoredActive AuthoredConstructStatus = "active"
	// AuthoredPaused: temporarily not resolvable (user toggle or circuit
	// breaker) without losing its registration -- resumable in place.
	AuthoredPaused AuthoredConstructStatus = "paused"
	// AuthoredRetired: permanently deactivated. A retired entry is dropped
	// from the registry; the status exists for the transition return value
	// and audit, not as a stored resting state.
	AuthoredRetired AuthoredConstructStatus = "retired"
)

// AuthoredConstruct is one live authored construct in the runtime layer: the
// (owner, kind, name) identity, its `.memql` source, the version it was
// activated at, and its current lifecycle status. The compiled form is kept
// opaque (any) so this layer does not couple to a specific construct kind's
// compiled representation -- the kind-specific runtimes (the authored
// scheduler, an authored query/spec resolver) type-assert it.
type AuthoredConstruct struct {
	OwnerUserId string                  `json:"ownerUserId"`
	Kind        string                  `json:"kind"`
	Name        string                  `json:"name"`
	BundleId    string                  `json:"bundleId"`
	Version     int                     `json:"version"`
	Source      string                  `json:"source"`
	Status      AuthoredConstructStatus `json:"status"`
	// Compiled is the cached compiled form (e.g. an *automations.Automation),
	// supplied by the kind-specific activation path. Opaque to the registry.
	Compiled any `json:"-"`
	// RegisteredAt / UpdatedAt are runtime provenance for management surfaces.
	RegisteredAt time.Time `json:"registeredAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// clone returns a deep-enough copy so callers can't mutate a live entry held
// under the registry lock. Compiled is shared by reference (its consumers
// treat it read-only).
func (c *AuthoredConstruct) clone() *AuthoredConstruct {
	cp := *c
	return &cp
}

// authoredKey is the resolution key: an authored construct is unique per
// (owner, kind, name). Version is data on the entry, not part of the key --
// activating a new version replaces the prior entry in place.
type authoredKey struct {
	owner string
	kind  string
	name  string
}

func (k authoredKey) String() string {
	return k.owner + "\x00" + k.kind + "\x00" + k.name
}

// AuthoredRuntimeRegistry holds the live, owner-scoped authored constructs.
// It is thread-safe and mutable after engine startup -- the activation /
// pause / retire paths mutate it while the engine serves traffic, exactly
// like IntegrationRegistry supports dynamic registration. It NEVER holds core
// constructs; the resolver layers it strictly after the sealed core.
type AuthoredRuntimeRegistry struct {
	mu         sync.RWMutex
	constructs map[authoredKey]*AuthoredConstruct
}

// NewAuthoredRuntimeRegistry builds an empty authored runtime layer.
func NewAuthoredRuntimeRegistry() *AuthoredRuntimeRegistry {
	return &AuthoredRuntimeRegistry{
		constructs: make(map[authoredKey]*AuthoredConstruct),
	}
}

// Register activates an authored construct into the runtime, keyed by
// (owner, kind, name). If a construct with the same key already exists, this
// is treated as a VERSION replacement: the new entry must carry a version
// strictly greater than the existing one (monotonic, mirroring the bundle
// supersession model), and replaces it in place. The construct's owner, kind,
// and name must be non-empty. The entry is stored ACTIVE.
//
// Register is safe to call against a running engine; it only mutates the
// authored layer and can never affect a core construct.
func (r *AuthoredRuntimeRegistry) Register(c *AuthoredConstruct) error {
	if c == nil {
		return fmt.Errorf("authored construct is nil")
	}
	if c.OwnerUserId == "" {
		return fmt.Errorf("authored construct has empty ownerUserId")
	}
	if c.Kind == "" || c.Name == "" {
		return fmt.Errorf("authored construct %q has empty kind or name", c.Name)
	}

	key := authoredKey{owner: c.OwnerUserId, kind: c.Kind, name: c.Name}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	entry := c.clone()
	entry.Status = AuthoredActive
	entry.UpdatedAt = now

	if existing, ok := r.constructs[key]; ok {
		// Version replacement: the new version must be strictly newer, so a
		// stale re-activation can't silently downgrade a live construct.
		if entry.Version <= existing.Version {
			return fmt.Errorf("authored construct %s/%s for owner %s: new version %d does not supersede existing version %d",
				c.Kind, c.Name, c.OwnerUserId, entry.Version, existing.Version)
		}
		entry.RegisteredAt = existing.RegisteredAt
		r.constructs[key] = entry
		return nil
	}

	entry.RegisteredAt = now
	r.constructs[key] = entry
	return nil
}

// Resolve returns the live authored construct for (owner, kind, name) when
// one is registered AND active. A paused construct does not resolve (it is
// registered but not live); a missing or retired construct returns false.
// The returned value is a clone -- callers may read it freely.
func (r *AuthoredRuntimeRegistry) Resolve(owner, kind, name string) (*AuthoredConstruct, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.constructs[authoredKey{owner: owner, kind: kind, name: name}]
	if !ok || entry.Status != AuthoredActive {
		return nil, false
	}
	return entry.clone(), true
}

// Lookup returns the registered entry for (owner, kind, name) regardless of
// status (active OR paused) -- used by management surfaces that need to see a
// paused construct. Retired constructs are removed from the registry, so they
// never appear. Returns a clone.
func (r *AuthoredRuntimeRegistry) Lookup(owner, kind, name string) (*AuthoredConstruct, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.constructs[authoredKey{owner: owner, kind: kind, name: name}]
	if !ok {
		return nil, false
	}
	return entry.clone(), true
}

// Pause flips a registered construct to paused WITHOUT removing it, so it
// stops resolving but can be resumed in place. Returns an error if no such
// construct is registered. Pausing an already-paused construct is a no-op
// (idempotent), which the circuit breaker relies on.
func (r *AuthoredRuntimeRegistry) Pause(owner, kind, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := authoredKey{owner: owner, kind: kind, name: name}
	entry, ok := r.constructs[key]
	if !ok {
		return fmt.Errorf("authored construct %s/%s for owner %s is not registered", kind, name, owner)
	}
	if entry.Status == AuthoredPaused {
		return nil
	}
	entry.Status = AuthoredPaused
	entry.UpdatedAt = time.Now().UTC()
	return nil
}

// Resume flips a paused construct back to active. Returns an error if no such
// construct is registered. Resuming an already-active construct is a no-op.
func (r *AuthoredRuntimeRegistry) Resume(owner, kind, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := authoredKey{owner: owner, kind: kind, name: name}
	entry, ok := r.constructs[key]
	if !ok {
		return fmt.Errorf("authored construct %s/%s for owner %s is not registered", kind, name, owner)
	}
	if entry.Status == AuthoredActive {
		return nil
	}
	entry.Status = AuthoredActive
	entry.UpdatedAt = time.Now().UTC()
	return nil
}

// Retire permanently removes a construct from the runtime. Returns an error
// if no such construct is registered. After Retire the (owner, kind, name)
// key is free for a fresh first-version registration.
func (r *AuthoredRuntimeRegistry) Retire(owner, kind, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := authoredKey{owner: owner, kind: kind, name: name}
	if _, ok := r.constructs[key]; !ok {
		return fmt.Errorf("authored construct %s/%s for owner %s is not registered", kind, name, owner)
	}
	delete(r.constructs, key)
	return nil
}

// ListForOwner returns every registered construct (active OR paused) owned by
// the given user, sorted by (kind, name) for stable output. Owner isolation:
// a caller can only enumerate its own authored constructs. Returns clones.
func (r *AuthoredRuntimeRegistry) ListForOwner(owner string) []*AuthoredConstruct {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*AuthoredConstruct, 0)
	for key, entry := range r.constructs {
		if key.owner == owner {
			out = append(out, entry.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Count returns the total number of registered constructs across all owners
// (active + paused). Used by tests and management telemetry.
func (r *AuthoredRuntimeRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.constructs)
}

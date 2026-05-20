package memql

import (
	"fmt"
	"sync"
)

// SeedDefinition is the engine-internal representation of a parsed +
// validated `seed` declaration, ready for materialization. Mirrors
// AgentDefinition / ShapeDefinition / etc. so the loader, registry,
// and materializer all share one stable surface.
type SeedDefinition struct {
	// Name is the seed declaration identifier (the `seed XXX` symbol).
	// Unique across all seed files; the registry rejects duplicates.
	Name string

	// Description, Version, TemplateFile are pass-through values
	// from the corresponding annotations.
	Description  string
	Version      string
	TemplateFile string

	// UseNamespace + UseConcept are the file-top `use <ns>.<concept>`
	// binding. The materializer resolves these to the canonical
	// concept id (e.g. "agents" + "agent" -> "v1:agents:agent") via
	// the concept registry.
	UseNamespace string
	UseConcept   string

	// Scope: "global" or "perUser". The materializer fans out
	// accordingly: one row in _system for global; one row per
	// v1:identity:user for perUser.
	Scope string

	// Body is the parsed field/block tree, captured here so the
	// materializer can lower it into row payload values without
	// re-parsing. Loader-level validation runs against the parsed
	// shape (e.g. required `id` field for global seeds).
	Body seedBlock

	// Origin records the source location for diagnostics (mirrors
	// AgentDefinition.Origin).
	Origin string
}

// SeedRegistry stores compiled SeedDefinitions by name. Read-mostly
// after startup; locked for thread safety so the runtime user-create
// hook can read the same registry the materializer wrote to during
// boot.
type SeedRegistry struct {
	mu     sync.RWMutex
	byName map[string]*SeedDefinition
}

// NewSeedRegistry returns an empty registry.
func NewSeedRegistry() *SeedRegistry {
	return &SeedRegistry{byName: make(map[string]*SeedDefinition)}
}

// Get returns a definition by name, ok=false when missing.
func (r *SeedRegistry) Get(name string) (*SeedDefinition, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.byName[name]
	return def, ok
}

// Names returns all registered seed names. Order is not stable;
// callers that need deterministic output should sort.
func (r *SeedRegistry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	return out
}

// All returns a snapshot of every registered definition. Used by the
// materializer's startup sweep and by the user-create event hook.
// The returned slice is safe to retain; the registry's internal map
// is not exposed.
func (r *SeedRegistry) All() []*SeedDefinition {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*SeedDefinition, 0, len(r.byName))
	for _, def := range r.byName {
		out = append(out, def)
	}
	return out
}

// PerUser returns just the @scope("perUser") seeds. Convenience for
// the user-creation event hook (which only re-runs perUser seeds).
func (r *SeedRegistry) PerUser() []*SeedDefinition {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*SeedDefinition
	for _, def := range r.byName {
		if def.Scope == "perUser" {
			out = append(out, def)
		}
	}
	return out
}

// Upsert registers a SeedDefinition. Names must be unique across the
// whole DSL tree; a collision returns an error and the second
// definition is dropped.
func (r *SeedRegistry) Upsert(def *SeedDefinition) error {
	if def == nil {
		return fmt.Errorf("nil seed definition")
	}
	if def.Name == "" {
		return fmt.Errorf("seed definition has empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, dup := r.byName[def.Name]; dup {
		return fmt.Errorf("duplicate seed name %q (already registered from %q)", def.Name, existing.Origin)
	}
	r.byName[def.Name] = def
	return nil
}

// compileSeedDecl lowers a parsed seedDecl into a SeedDefinition,
// applying loader-level validation. Today's checks:
//   - @scope must be set (the parser allows empty for legacy
//     tolerance; the loader requires it).
//   - useNamespace + useConcept must be non-empty (the parser
//     enforces this; double-checked here so a future parser refactor
//     doesn't accidentally drop the invariant).
//   - For @scope("global"), the body must declare an `id` field
//     (the row's deterministic id; required because the materializer
//     uses it for idempotent insert-if-missing).
//
// @scope("perUser") seeds have their id derived as `<seedName>-<userId>`
// by the materializer, so they don't need a body-declared id; an
// explicit one is rejected (it can't be both `<seedName>-<userId>`
// and a fixed value).
func compileSeedDecl(decl *seedDecl) (*SeedDefinition, error) {
	if decl == nil {
		return nil, fmt.Errorf("nil seed declaration")
	}
	// Default scope to "global" -- the dominant case post-migration.
	// @scope("perUser") still required explicitly because the
	// materializer fans out per user rather than writing one row.
	if decl.scope == "" {
		decl.scope = "global"
	}
	if decl.useConcept == "" {
		return nil, fmt.Errorf("seed %q has no concept binding (declare via canonical `seed <Concept> %s { ... }` signature or legacy file-top `use <ns>.<concept>` clause)", decl.name, decl.name)
	}

	if decl.scope == "global" {
		// Auto-derive `id` from the seed declaration name if the body
		// didn't supply one. The seed name IS the row's local id for
		// global seeds -- writing it twice was the pre-migration norm.
		if _, ok := decl.body.fields["id"]; !ok {
			decl.body.fields["id"] = seedValue{kind: seedString, str: decl.name}
			decl.body.keys = append([]string{"id"}, decl.body.keys...)
		}
	}
	if decl.scope == "perUser" {
		if _, ok := decl.body.fields["id"]; ok {
			return nil, fmt.Errorf("@scope(\"perUser\") seed %q cannot declare its own `id` (derived as `<seedName>-<userId>` by the materializer)", decl.name)
		}
	}

	return &SeedDefinition{
		Name:         decl.name,
		Description:  decl.description,
		Version:      decl.version,
		TemplateFile: decl.templateFile,
		UseNamespace: decl.useNamespace,
		UseConcept:   decl.useConcept,
		Scope:        decl.scope,
		Body:         decl.body,
	}, nil
}

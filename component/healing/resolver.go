// Package healing implements the self-healing two-tier base/overlay store
// resolution (Epic 4 / memql#2140, E4.2).
//
// The two tiers:
//
//   - BASE: the immutable, authored/embedded definition of a construct (an
//     automation, a precondition, a guard, a literal). The deploy spine's
//     source of truth; never LLM-healed. Supplied by a BaseProvider the
//     caller wires to the embedded construct (the DSL tree, the harness
//     registry, ...).
//   - OVERLAY: a healed override -- a v1:healing:healedOverride data row
//     produced by the repair loop (E4.4) and human-validated (E4.5). It
//     shadows its base construct when, and ONLY when, it is a VALID, active
//     overlay (the resolveValidOverride query already filters to those).
//
// Resolve prefers a valid overlay override and falls back to base. This
// package is deliberately pure of engine/DB types: the caller supplies an
// OverlayLookup (backed by the resolveValidOverride query) and a
// BaseProvider (backed by the embedded construct), mirroring how
// component/harness/actionreplay stays engine-free for unit-testability.
//
// Multi-node: the overlay lookup reads the shared Postgres, so every node
// resolves the same newest-valid override; a graph write to the concept
// rides the cache.invalidate broadcast (already mesh-forwarded) so a cached
// resolution is dropped on every replica. The resolver itself holds no
// cross-node state.
package healing

import (
	"context"
	"fmt"
)

// Override is the resolved overlay tier of a construct -- the healed
// definition that shadows base when it wins resolution. It is the
// projection the resolveValidOverride query returns (healedOverrideFull),
// reduced to the fields resolution + callers need.
type Override struct {
	// ID is the v1:healing:healedOverride row id.
	ID string
	// BaseConstructId is the construct this override defines (the resolution
	// key).
	BaseConstructId string
	// OverrideData is the healed construct body the resolver returns when
	// this override wins.
	OverrideData map[string]any
	// Version is the monotonic version of this override.
	Version int
	// Valid is always true for an override returned by the lookup (the query
	// filters to valid==true); carried for caller assertions + audit.
	Valid bool
}

// OverlayLookup returns the winning VALID, active overlay override for a
// base construct, or (nil, nil) when none exists. Backed by the
// resolveValidOverride DSL query (owned + tier=overlay + valid + active +
// newest-version). A non-nil error is a lookup failure, NOT "no override" --
// the resolver treats a lookup error as fail-CLOSED to base (the deploy
// spine), never as a silent heal.
type OverlayLookup func(ctx context.Context, baseConstructId string) (*Override, error)

// BaseProvider returns the immutable base definition of a construct, or
// (nil, false) when the construct id is unknown. Backed by the embedded
// authored construct (the DSL tree / harness registry). The base tier is
// never LLM-healed; this is the fallback resolution always lands on when no
// valid overlay override exists.
type BaseProvider func(baseConstructId string) (map[string]any, bool)

// Tier names which tier a Resolved came from.
type Tier string

const (
	// TierBase = resolution fell back to the immutable embedded base.
	TierBase Tier = "base"
	// TierOverlay = a valid overlay override shadowed the base.
	TierOverlay Tier = "overlay"
)

// Resolved is the outcome of two-tier resolution: the winning construct
// definition plus which tier it came from.
type Resolved struct {
	// Definition is the resolved construct body -- the overlay's overrideData
	// when an override won, otherwise the base definition.
	Definition map[string]any
	// Tier names the winning tier (overlay if an override shadowed base).
	Tier Tier
	// Override is the winning overlay override, or nil when resolution fell
	// back to base. Carried so callers can audit the version / id that took
	// effect.
	Override *Override
}

// Resolver performs two-tier base/overlay resolution: it prefers a valid
// overlay override (via OverlayLookup) and falls back to the immutable base
// (via BaseProvider). Construct once with the wired dependencies and reuse.
type Resolver struct {
	lookup OverlayLookup
	base   BaseProvider
}

// NewResolver wires a two-tier resolver. overlay is the resolveValidOverride-
// backed lookup; base is the embedded-construct provider. Both are required.
func NewResolver(overlay OverlayLookup, base BaseProvider) *Resolver {
	return &Resolver{lookup: overlay, base: base}
}

// Resolve returns the construct definition for baseConstructId, preferring a
// valid overlay override and falling back to the immutable base.
//
// Resolution order (the two-tier contract):
//
//  1. A valid, active overlay override exists -> return its overrideData
//     (TierOverlay). The healed definition shadows base.
//  2. No valid override (or the override carries no overrideData) -> return
//     the base definition (TierBase).
//  3. Neither a valid override nor a base exists -> error (the construct id
//     is unknown to both tiers).
//
// Fail-closed: an OverlayLookup error falls back to base rather than
// erroring, so a transient overlay-store hiccup degrades to the
// authored/deterministic spine instead of stranding the construct. (A base
// that is also missing surfaces the original lookup error for diagnosis.)
func (r *Resolver) Resolve(ctx context.Context, baseConstructId string) (*Resolved, error) {
	if r == nil {
		return nil, fmt.Errorf("healing: nil resolver")
	}

	var lookupErr error
	if r.lookup != nil {
		ov, err := r.lookup(ctx, baseConstructId)
		switch {
		case err != nil:
			// Fail-closed to base: record the error for the no-base case but
			// do not let an overlay-store hiccup silently win or hard-fail.
			lookupErr = err
		case ov != nil && len(ov.OverrideData) > 0:
			// A valid overlay override with a body shadows base.
			return &Resolved{
				Definition: ov.OverrideData,
				Tier:       TierOverlay,
				Override:   ov,
			}, nil
		}
	}

	if r.base != nil {
		if def, ok := r.base(baseConstructId); ok {
			return &Resolved{Definition: def, Tier: TierBase}, nil
		}
	}

	if lookupErr != nil {
		return nil, fmt.Errorf("healing: resolve %q: overlay lookup failed and no base exists: %w", baseConstructId, lookupErr)
	}
	return nil, fmt.Errorf("healing: resolve %q: no valid overlay override and no base construct", baseConstructId)
}

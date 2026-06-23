package healing

import (
	"context"
	"errors"
	"testing"
)

// Epic 4 / memql#2140: two-tier base/overlay resolution.
//
// The contract: resolution PREFERS a valid overlay override and FALLS BACK
// to the immutable base.

func baseProvider(defs map[string]map[string]any) BaseProvider {
	return func(id string) (map[string]any, bool) {
		d, ok := defs[id]
		return d, ok
	}
}

// A valid overlay override shadows its base.
func TestResolve_OverlayShadowsBase(t *testing.T) {
	base := baseProvider(map[string]map[string]any{
		"deployStaging": {"replicas": 1, "tier": "base"},
	})
	lookup := func(_ context.Context, id string) (*Override, error) {
		if id == "deployStaging" {
			return &Override{
				ID:              "ov-1",
				BaseConstructId: id,
				OverrideData:    map[string]any{"replicas": 3, "healed": true},
				Version:         2,
				Valid:           true,
			}, nil
		}
		return nil, nil
	}
	r := NewResolver(lookup, base)

	got, err := r.Resolve(context.Background(), "deployStaging")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Tier != TierOverlay {
		t.Fatalf("tier = %q, want overlay (a valid override must shadow base)", got.Tier)
	}
	if got.Definition["replicas"] != 3 || got.Definition["healed"] != true {
		t.Errorf("definition = %v, want the healed overlay body", got.Definition)
	}
	if got.Override == nil || got.Override.Version != 2 {
		t.Errorf("override not carried for audit: %+v", got.Override)
	}
}

// With no valid override, resolution falls back to the immutable base.
func TestResolve_FallsBackToBase(t *testing.T) {
	base := baseProvider(map[string]map[string]any{
		"deployStaging": {"replicas": 1, "tier": "base"},
	})
	// Lookup returns no override (nil, nil) -- the common case.
	lookup := func(_ context.Context, _ string) (*Override, error) { return nil, nil }
	r := NewResolver(lookup, base)

	got, err := r.Resolve(context.Background(), "deployStaging")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Tier != TierBase {
		t.Fatalf("tier = %q, want base (no valid override -> base)", got.Tier)
	}
	if got.Definition["replicas"] != 1 {
		t.Errorf("definition = %v, want the base body", got.Definition)
	}
	if got.Override != nil {
		t.Errorf("override should be nil on a base fallback, got %+v", got.Override)
	}
}

// An override with an empty body does NOT shadow base -- a placeholder/pending
// override must not blank out a construct.
func TestResolve_EmptyOverrideBodyFallsBackToBase(t *testing.T) {
	base := baseProvider(map[string]map[string]any{
		"deployStaging": {"replicas": 1},
	})
	lookup := func(_ context.Context, id string) (*Override, error) {
		return &Override{ID: "ov-empty", BaseConstructId: id, OverrideData: nil, Valid: true}, nil
	}
	r := NewResolver(lookup, base)

	got, err := r.Resolve(context.Background(), "deployStaging")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Tier != TierBase {
		t.Errorf("empty-body override must fall back to base, got tier %q", got.Tier)
	}
}

// A lookup error fails CLOSED to base -- a transient overlay-store hiccup
// degrades to the authored/deterministic spine, never strands the construct
// and never hard-fails when a base exists.
func TestResolve_LookupErrorFailsClosedToBase(t *testing.T) {
	base := baseProvider(map[string]map[string]any{
		"deployStaging": {"replicas": 1},
	})
	lookup := func(_ context.Context, _ string) (*Override, error) {
		return nil, errors.New("db hiccup")
	}
	r := NewResolver(lookup, base)

	got, err := r.Resolve(context.Background(), "deployStaging")
	if err != nil {
		t.Fatalf("lookup error with a base present should not error: %v", err)
	}
	if got.Tier != TierBase {
		t.Errorf("tier = %q, want base (fail-closed)", got.Tier)
	}
}

// A lookup error with NO base surfaces the lookup error for diagnosis (we
// cannot silently invent a definition).
func TestResolve_LookupErrorNoBaseSurfacesError(t *testing.T) {
	lookup := func(_ context.Context, _ string) (*Override, error) {
		return nil, errors.New("db hiccup")
	}
	r := NewResolver(lookup, baseProvider(nil))

	_, err := r.Resolve(context.Background(), "unknown")
	if err == nil {
		t.Fatalf("expected an error when both overlay lookup fails and no base exists")
	}
}

// An unknown construct (no override, no base) errors.
func TestResolve_UnknownConstructErrors(t *testing.T) {
	lookup := func(_ context.Context, _ string) (*Override, error) { return nil, nil }
	r := NewResolver(lookup, baseProvider(nil))

	_, err := r.Resolve(context.Background(), "nope")
	if err == nil {
		t.Fatalf("expected an error for a construct unknown to both tiers")
	}
}

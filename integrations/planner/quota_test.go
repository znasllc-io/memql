package planner

import (
	"context"
	"strings"
	"testing"
)

// entitlementResult wraps a single row in the shape() envelope the engine
// returns for a by-key query (single match -> unwrapped object under "data").
func entitlementResult(row map[string]any) map[string]any {
	return map[string]any{"data": row}
}

func TestEntitlementResolve_NoRowIsUnlimited(t *testing.T) {
	// execResponder unset -> fakeEngine.Execute returns (nil, nil), which
	// stands in for "no matching entitlement row".
	r := NewEntitlementResolver(&fakeEngine{}, testLogger())
	ent := r.Resolve(context.Background(), "v1:identity:user:alice")
	if !ent.Unlimited {
		t.Fatalf("no row should resolve to unlimited; got %+v", ent)
	}
	if ent.MaxConcurrentTasks != 0 {
		t.Fatalf("unlimited should report 0 cap; got %d", ent.MaxConcurrentTasks)
	}
}

func TestEntitlementResolve_EmptyAccountIsUnlimited(t *testing.T) {
	r := NewEntitlementResolver(&fakeEngine{}, testLogger())
	if ent := r.Resolve(context.Background(), "   "); !ent.Unlimited {
		t.Fatalf("empty account id should resolve to unlimited; got %+v", ent)
	}
}

func TestEntitlementResolve_NilEngineIsUnlimited(t *testing.T) {
	r := NewEntitlementResolver(nil, testLogger())
	if ent := r.Resolve(context.Background(), "v1:identity:user:alice"); !ent.Unlimited {
		t.Fatalf("nil engine should resolve to unlimited; got %+v", ent)
	}
}

func TestEntitlementResolve_FiniteCap(t *testing.T) {
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		if !strings.Contains(query, "accountEntitlement") {
			return nil, nil
		}
		// numbers arrive as float64 from a JSON decode -- exercise that path.
		return entitlementResult(map[string]any{
			"id":                 "accountEntitlement-deadbeef",
			"tier":               "pro",
			"maxConcurrentTasks": float64(5),
			"createdAt":          "2026-06-03T00:00:00Z",
		}), nil
	}}
	r := NewEntitlementResolver(fe, testLogger())
	ent := r.Resolve(context.Background(), "v1:identity:user:alice")
	if ent.Unlimited {
		t.Fatalf("finite pro cap should NOT be unlimited; got %+v", ent)
	}
	if ent.MaxConcurrentTasks != 5 {
		t.Fatalf("want cap 5; got %d", ent.MaxConcurrentTasks)
	}
	if ent.Tier != "pro" {
		t.Fatalf("want tier pro; got %q", ent.Tier)
	}
}

func TestEntitlementResolve_EnterpriseAlwaysUnlimited(t *testing.T) {
	// Enterprise with a stray finite number still resolves unlimited.
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		return entitlementResult(map[string]any{
			"id":                 "accountEntitlement-deadbeef",
			"tier":               "enterprise",
			"maxConcurrentTasks": float64(3),
			"createdAt":          "2026-06-03T00:00:00Z",
		}), nil
	}}
	r := NewEntitlementResolver(fe, testLogger())
	ent := r.Resolve(context.Background(), "v1:identity:user:alice")
	if !ent.Unlimited {
		t.Fatalf("enterprise tier should resolve unlimited regardless of number; got %+v", ent)
	}
	if ent.MaxConcurrentTasks != 0 {
		t.Fatalf("unlimited should report 0 cap; got %d", ent.MaxConcurrentTasks)
	}
}

func TestEntitlementResolve_NonPositiveCapIsUnlimited(t *testing.T) {
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		return entitlementResult(map[string]any{
			"id":                 "accountEntitlement-deadbeef",
			"tier":               "pro",
			"maxConcurrentTasks": float64(0),
			"createdAt":          "2026-06-03T00:00:00Z",
		}), nil
	}}
	r := NewEntitlementResolver(fe, testLogger())
	if ent := r.Resolve(context.Background(), "v1:identity:user:alice"); !ent.Unlimited {
		t.Fatalf("cap <= 0 should resolve unlimited; got %+v", ent)
	}
}

func TestEntitlementResolve_QueryErrorFailsOpen(t *testing.T) {
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		return nil, context.DeadlineExceeded
	}}
	r := NewEntitlementResolver(fe, testLogger())
	if ent := r.Resolve(context.Background(), "v1:identity:user:alice"); !ent.Unlimited {
		t.Fatalf("a query error must fail OPEN (unlimited); got %+v", ent)
	}
}

func TestEntitlementResolve_PicksLatestVersion(t *testing.T) {
	// Two time-series versions returned as a []any array; the newest
	// createdAt wins.
	fe := &fakeEngine{execResponder: func(query string) (any, error) {
		return map[string]any{"data": []any{
			map[string]any{"tier": "pro", "maxConcurrentTasks": float64(5), "createdAt": "2026-06-01T00:00:00Z"},
			map[string]any{"tier": "team", "maxConcurrentTasks": float64(20), "createdAt": "2026-06-03T00:00:00Z"},
		}}, nil
	}}
	r := NewEntitlementResolver(fe, testLogger())
	ent := r.Resolve(context.Background(), "v1:identity:user:alice")
	if ent.MaxConcurrentTasks != 20 || ent.Tier != "team" {
		t.Fatalf("latest version (team/20) should win; got %+v", ent)
	}
}

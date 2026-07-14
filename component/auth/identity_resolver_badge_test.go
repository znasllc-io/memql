package auth

// Role-ceiling clamp tests for class="badge" grants (memql#2513): the
// operator's effective role is min(row role, terminal ceiling), and a
// malformed grant fails closed to reader. Non-badge classes pass
// through untouched.

import (
	"context"
	"testing"
)

// stubRunner returns a fixed user row for every query.
type stubRunner struct {
	role string
}

func (s *stubRunner) ExecuteShaped(_ context.Context, _ string) (any, error) {
	return []any{map[string]any{
		"id":           "v1:identity:user:op-1",
		"primaryEmail": "op@example.test",
		"role":         s.role,
	}}, nil
}

func badgeClaims(ceiling string) map[string]any {
	claims := map[string]any{
		"sub":   "v1:identity:user:op-1",
		"class": "badge",
	}
	if ceiling != "" {
		claims["role_ceiling"] = ceiling
	}
	return claims
}

func TestLoadFromClaims_BadgeCeilingClamps(t *testing.T) {
	cases := []struct {
		name    string
		rowRole string
		ceiling string
		want    Role
	}{
		{"admin operator on writer kiosk clamps to writer", "admin", "writer", RoleWriter},
		{"owner operator on reader kiosk clamps to reader", "owner", "reader", RoleReader},
		{"reader operator on writer kiosk stays reader", "reader", "writer", RoleReader},
		{"writer operator on writer kiosk stays writer", "writer", "writer", RoleWriter},
		{"missing ceiling fails closed to reader", "admin", "", RoleReader},
		{"garbage ceiling fails closed to reader", "admin", "grand-vizier", RoleReader},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := NewIdentityResolver(&stubRunner{role: tc.rowRole}, nil)
			ac, err := r.LoadFromClaims(context.Background(), badgeClaims(tc.ceiling))
			if err != nil {
				t.Fatalf("LoadFromClaims: %v", err)
			}
			if ac.Role != tc.want {
				t.Errorf("Role = %q, want %q", ac.Role, tc.want)
			}
			if ac.UserId != "v1:identity:user:op-1" {
				t.Errorf("UserId = %q -- attribution must be the operator", ac.UserId)
			}
		})
	}
}

func TestLoadFromClaims_NonBadgeClassUnclamped(t *testing.T) {
	r := NewIdentityResolver(&stubRunner{role: "admin"}, nil)
	ac, err := r.LoadFromClaims(context.Background(), map[string]any{
		"sub": "v1:identity:user:op-1",
		// user-class token that HAPPENS to carry a stray ceiling claim:
		// the clamp is keyed on class, so it must not apply.
		"role_ceiling": "reader",
	})
	if err != nil {
		t.Fatalf("LoadFromClaims: %v", err)
	}
	if ac.Role != RoleAdmin {
		t.Errorf("Role = %q, want admin (clamp must only apply to class=badge)", ac.Role)
	}
}

func TestFallbackFromClaims_BadgeCeilingClamps(t *testing.T) {
	// Not-provisioned fallback path: role comes from the token claim,
	// then the ceiling clamps it the same way.
	claims := badgeClaims("reader")
	claims["role"] = "admin"
	ac := FallbackFromClaims(claims)
	if ac.Role != RoleReader {
		t.Errorf("Role = %q, want reader", ac.Role)
	}
}

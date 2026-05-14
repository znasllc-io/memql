package auth

import (
	"context"
	"testing"
	"time"
)

func TestClaimsFromContext(t *testing.T) {
	tests := []struct {
		name      string
		ctx       context.Context
		wantOk    bool
		wantValue map[string]any
	}{
		{
			name:      "empty context",
			ctx:       context.Background(),
			wantOk:    false,
			wantValue: nil,
		},
		{
			name: "context with claims",
			ctx: context.WithValue(context.Background(), ClaimsContextKey, map[string]any{
				"sub":  "user-123",
				"role": "developer",
			}),
			wantOk: true,
			wantValue: map[string]any{
				"sub":  "user-123",
				"role": "developer",
			},
		},
		{
			name:      "context with invalid claims type",
			ctx:       context.WithValue(context.Background(), ClaimsContextKey, "invalid"),
			wantOk:    false,
			wantValue: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, ok := ClaimsFromContext(tt.ctx)
			if ok != tt.wantOk {
				t.Errorf("ClaimsFromContext() ok = %v, want %v", ok, tt.wantOk)
			}
			if tt.wantValue != nil && claims != nil {
				if claims["sub"] != tt.wantValue["sub"] {
					t.Errorf("expected sub=%v, got %v", tt.wantValue["sub"], claims["sub"])
				}
			}
		})
	}
}

func TestContextWithClaims(t *testing.T) {
	claims := map[string]any{
		"sub":   "user-123",
		"email": "test@example.com",
		"role":  "developer",
	}

	ctx := ContextWithClaims(context.Background(), claims)

	// Retrieve claims from context
	retrieved, ok := ClaimsFromContext(ctx)
	if !ok {
		t.Fatal("expected to retrieve claims from context")
	}

	if retrieved["sub"] != "user-123" {
		t.Errorf("expected sub=user-123, got %v", retrieved["sub"])
	}
	if retrieved["email"] != "test@example.com" {
		t.Errorf("expected email=test@example.com, got %v", retrieved["email"])
	}
}

func TestCloneClaims(t *testing.T) {
	original := map[string]any{
		"sub":    "user-123",
		"email":  "test@example.com",
		"groups": []string{"group1", "group2"},
		"nested": map[string]any{
			"field1": "value1",
		},
	}

	cloned := CloneClaims(original)

	// Verify values match
	if cloned["sub"] != original["sub"] {
		t.Error("cloned sub doesn't match")
	}
	if cloned["email"] != original["email"] {
		t.Error("cloned email doesn't match")
	}

	// Verify it's a deep copy (modifying clone doesn't affect original)
	cloned["sub"] = "modified"
	cloned["new_field"] = "new_value"

	if original["sub"] == "modified" {
		t.Error("modifying clone affected original")
	}
	if _, ok := original["new_field"]; ok {
		t.Error("adding to clone affected original")
	}
}

func TestCloneClaimsNil(t *testing.T) {
	cloned := CloneClaims(nil)
	// CloneClaims returns nil for nil input
	if cloned != nil {
		t.Error("expected nil from CloneClaims(nil)")
	}
}

func TestTokenInfoFromContext(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		wantNil bool
	}{
		{
			name:    "empty context",
			ctx:     context.Background(),
			wantNil: true,
		},
		{
			name: "context with token info",
			ctx: context.WithValue(context.Background(), TokenInfoContextKey, &TokenInfo{
				Subject: "user-123",
			}),
			wantNil: false,
		},
		{
			name:    "context with invalid token info type",
			ctx:     context.WithValue(context.Background(), TokenInfoContextKey, "invalid"),
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TokenInfoFromContext(tt.ctx)
			if (result == nil) != tt.wantNil {
				t.Errorf("TokenInfoFromContext() nil = %v, want %v", result == nil, tt.wantNil)
			}
		})
	}
}

func TestContextWithToken(t *testing.T) {
	token := &TokenInfo{
		Subject: "user-123",
		Scopes:  []string{"read", "write"},
		Claims: map[string]any{
			"email": "test@example.com",
		},
	}

	ctx := ContextWithToken(context.Background(), token)

	// Retrieve token from context
	retrieved := TokenInfoFromContext(ctx)
	if retrieved == nil {
		t.Fatal("expected to retrieve token from context")
	}

	if retrieved.Subject != "user-123" {
		t.Errorf("expected Subject=user-123, got %s", retrieved.Subject)
	}
	if len(retrieved.Scopes) != 2 {
		t.Errorf("expected 2 scopes, got %d", len(retrieved.Scopes))
	}
}

func TestBuildTokenInfo(t *testing.T) {
	tests := []struct {
		name     string
		claims   map[string]any
		validate func(*testing.T, *TokenInfo)
	}{
		{
			name: "basic claims",
			claims: map[string]any{
				"sub":   "user-123",
				"email": "test@example.com",
			},
			validate: func(t *testing.T, ti *TokenInfo) {
				if ti.Subject != "user-123" {
					t.Errorf("expected Subject=user-123, got %s", ti.Subject)
				}
				if email, ok := ti.Claims["email"].(string); !ok || email != "test@example.com" {
					t.Errorf("expected email in claims=test@example.com")
				}
			},
		},
		{
			name: "with scope string",
			claims: map[string]any{
				"sub":   "user-123",
				"scope": "read write admin",
			},
			validate: func(t *testing.T, ti *TokenInfo) {
				if len(ti.Scopes) != 3 {
					t.Errorf("expected 3 scopes, got %d", len(ti.Scopes))
				}
			},
		},
		{
			name:   "empty claims",
			claims: map[string]any{},
			validate: func(t *testing.T, ti *TokenInfo) {
				if ti != nil {
					if ti.Subject != "" {
						t.Error("expected empty Subject from nil claims")
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := BuildTokenInfo(tt.claims)
			// BuildTokenInfo returns nil for empty/nil claims
			if token != nil && tt.validate != nil {
				tt.validate(t, token)
			}
		})
	}
}

func TestContextChaining(t *testing.T) {
	// Test that both claims and token can coexist in context
	claims := map[string]any{
		"sub":   "user-123",
		"email": "test@example.com",
	}
	token := &TokenInfo{
		Subject: "user-123",
		Claims: map[string]any{
			"email": "test@example.com",
		},
	}

	ctx := context.Background()
	ctx = ContextWithClaims(ctx, claims)
	ctx = ContextWithToken(ctx, token)

	// Both should be retrievable
	retrievedClaims, ok := ClaimsFromContext(ctx)
	if !ok {
		t.Error("expected to retrieve claims")
	}
	if retrievedClaims["sub"] != "user-123" {
		t.Error("claims not preserved")
	}

	retrievedToken := TokenInfoFromContext(ctx)
	if retrievedToken == nil {
		t.Error("expected to retrieve token")
	}
	if retrievedToken.Subject != "user-123" {
		t.Error("token not preserved")
	}
}

// ---------------------------------------------------------------------------
// Delegation Context Tests
// ---------------------------------------------------------------------------

func TestDelegationFromContext(t *testing.T) {
	t.Run("empty context returns nil", func(t *testing.T) {
		dc, ok := DelegationFromContext(context.Background())
		if ok || dc != nil {
			t.Error("expected nil delegation from empty context")
		}
	})

	t.Run("nil context returns nil", func(t *testing.T) {
		dc, ok := DelegationFromContext(nil)
		if ok || dc != nil {
			t.Error("expected nil delegation from nil context")
		}
	})

	t.Run("round-trip delegation context", func(t *testing.T) {
		original := &DelegationContext{
			DelegationId: "del-123",
			IdentityId:   "identity-456",
			DelegatingIdentity: UserIdentity{
				Subject: "human-subject",
				Email:   "human@example.com",
				Role:    "admin",
			},
			IdentityType:    "human",
			GuardianSubject: "",
			AgentId:         "agent-789",
			RoleCeiling:     RoleWriter,
			Scopes:          []string{"query:*", "mutation:cognition.*"},
			ExpiresAt:       time.Now().Add(24 * time.Hour),
		}

		ctx := ContextWithDelegation(context.Background(), original)
		retrieved, ok := DelegationFromContext(ctx)

		if !ok {
			t.Fatal("expected delegation from context")
		}
		if retrieved.DelegationId != "del-123" {
			t.Errorf("expected DelegationId=del-123, got %s", retrieved.DelegationId)
		}
		if retrieved.IdentityId != "identity-456" {
			t.Errorf("expected IdentityId=identity-456, got %s", retrieved.IdentityId)
		}
		if retrieved.DelegatingIdentity.Email != "human@example.com" {
			t.Errorf("expected email=human@example.com, got %s", retrieved.DelegatingIdentity.Email)
		}
		if retrieved.RoleCeiling != RoleWriter {
			t.Errorf("expected RoleCeiling=writer, got %s", retrieved.RoleCeiling)
		}
		if len(retrieved.Scopes) != 2 {
			t.Errorf("expected 2 scopes, got %d", len(retrieved.Scopes))
		}
		if retrieved.AgentId != "agent-789" {
			t.Errorf("expected AgentId=agent-789, got %s", retrieved.AgentId)
		}
	})

	t.Run("nil delegation is no-op", func(t *testing.T) {
		ctx := ContextWithDelegation(context.Background(), nil)
		dc, ok := DelegationFromContext(ctx)
		if ok || dc != nil {
			t.Error("expected nil delegation when set to nil")
		}
	})
}

func TestDelegationCoexistsWithClaimsAndToken(t *testing.T) {
	claims := map[string]any{
		"sub":   "agent-subject",
		"email": "agent@example.com",
		"role":  "admin",
	}
	token := &TokenInfo{
		Subject: "agent-subject",
		Claims:  claims,
	}
	dc := &DelegationContext{
		DelegationId: "del-test",
		DelegatingIdentity: UserIdentity{
			Subject: "human-subject",
			Email:   "human@example.com",
		},
		AgentId:     "agent-001",
		RoleCeiling: RoleReader,
	}

	ctx := context.Background()
	ctx = ContextWithClaims(ctx, claims)
	ctx = ContextWithToken(ctx, token)
	ctx = ContextWithDelegation(ctx, dc)

	// All three should be retrievable
	if _, ok := ClaimsFromContext(ctx); !ok {
		t.Error("claims not preserved after adding delegation")
	}
	if ti := TokenInfoFromContext(ctx); ti == nil {
		t.Error("token not preserved after adding delegation")
	}
	if d, ok := DelegationFromContext(ctx); !ok || d.DelegationId != "del-test" {
		t.Error("delegation not preserved")
	}
}

package auth

import (
	"context"
	"testing"
)

func TestUserIdentityFromContext(t *testing.T) {
	tests := []struct {
		name      string
		ctx       context.Context
		wantError bool
		validate  func(*testing.T, UserIdentity)
	}{
		{
			name:      "empty context",
			ctx:       context.Background(),
			wantError: true,
		},
		{
			name: "context with valid claims",
			ctx: ContextWithClaims(context.Background(), map[string]any{
				"sub":   "user-123",
				"email": "test@example.com",
				"role":  "developer",
			}),
			wantError: false,
			validate: func(t *testing.T, id UserIdentity) {
				if id.Subject != "user-123" {
					t.Errorf("expected Subject=user-123, got %s", id.Subject)
				}
				if id.Email != "test@example.com" {
					t.Errorf("expected email=test@example.com, got %s", id.Email)
				}
				if id.Role != "developer" {
					t.Errorf("expected role=developer, got %s", id.Role)
				}
			},
		},
		{
			name: "context with token info only",
			ctx: ContextWithToken(context.Background(), &TokenInfo{
				Subject: "user-456",
				Claims: map[string]any{
					"email": "token@example.com",
				},
			}),
			wantError: false,
			validate: func(t *testing.T, id UserIdentity) {
				if id.Subject != "user-456" {
					t.Errorf("expected Subject=user-456, got %s", id.Subject)
				}
				if id.Email != "token@example.com" {
					t.Errorf("expected email=token@example.com, got %s", id.Email)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity, err := UserIdentityFromContext(tt.ctx)
			if tt.wantError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.validate != nil {
				tt.validate(t, identity)
			}
		})
	}
}

func TestBuildIdentityFromToken(t *testing.T) {
	tests := []struct {
		name     string
		token    *TokenInfo
		validate func(*testing.T, UserIdentity)
	}{
		{
			name: "basic token info",
			token: &TokenInfo{
				Subject: "user-123",
				Claims: map[string]any{
					"email": "test@example.com",
				},
			},
			validate: func(t *testing.T, id UserIdentity) {
				if id.Subject != "user-123" {
					t.Errorf("expected Subject=user-123, got %s", id.Subject)
				}
				if id.Email != "test@example.com" {
					t.Errorf("expected email=test@example.com, got %s", id.Email)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := buildIdentityFromToken(tt.token)
			if tt.validate != nil {
				tt.validate(t, identity)
			}
		})
	}
}

func TestEmailExtraction(t *testing.T) {
	tests := []struct {
		name          string
		claims        map[string]any
		expectedEmail string
	}{
		{
			name: "email field",
			claims: map[string]any{
				"sub":   "user-123",
				"email": "test@example.com",
			},
			expectedEmail: "test@example.com",
		},
		{
			name: "preferred_username field",
			claims: map[string]any{
				"sub":                "user-123",
				"preferred_username": "username@example.com",
			},
			expectedEmail: "username@example.com",
		},
		{
			name: "email takes precedence",
			claims: map[string]any{
				"sub":                "user-123",
				"email":              "email@example.com",
				"preferred_username": "username@example.com",
			},
			expectedEmail: "email@example.com",
		},
		{
			name: "no email fields",
			claims: map[string]any{
				"sub": "user-123",
			},
			expectedEmail: "user-123", // Falls back to subject
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := ContextWithClaims(context.Background(), tt.claims)
			identity, err := UserIdentityFromContext(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if identity.Email != tt.expectedEmail {
				t.Errorf("expected email=%s, got %s", tt.expectedEmail, identity.Email)
			}
		})
	}
}

func TestRoleExtraction(t *testing.T) {
	tests := []struct {
		name         string
		claims       map[string]any
		expectedRole string
	}{
		{
			name: "role field",
			claims: map[string]any{
				"sub":  "user-123",
				"role": "developer",
			},
			expectedRole: "developer",
		},
		{
			name: "roles array",
			claims: map[string]any{
				"sub":   "user-123",
				"roles": []string{"developer", "admin"},
			},
			expectedRole: "developer",
		},
		{
			name: "no role fields returns empty",
			claims: map[string]any{
				"sub": "user-123",
			},
			expectedRole: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := ContextWithClaims(context.Background(), tt.claims)
			identity, err := UserIdentityFromContext(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if identity.Role != tt.expectedRole {
				t.Errorf("expected role=%s, got %s", tt.expectedRole, identity.Role)
			}
		})
	}
}

package auth

import (
	"context"
	"testing"
	"time"
)

func TestUserFromContext(t *testing.T) {
	tests := []struct {
		name     string
		ctx      context.Context
		wantOk   bool
		validate func(*testing.T, UserContext)
	}{
		{
			name:   "empty context",
			ctx:    context.Background(),
			wantOk: false,
		},
		{
			name: "context with admin role",
			ctx: ContextWithClaims(context.Background(), map[string]any{
				"sub":   "user-123",
				"email": "admin@example.com",
				"role":  "admin",
			}),
			wantOk: true,
			validate: func(t *testing.T, uc UserContext) {
				if uc.ID != "user-123" {
					t.Errorf("expected ID=user-123, got %s", uc.ID)
				}
				if !IsAdmin(uc) {
					t.Error("expected admin role")
				}
			},
		},
		{
			name: "context with reader role",
			ctx: ContextWithClaims(context.Background(), map[string]any{
				"sub":   "user-456",
				"email": "reader@example.com",
				"role":  "reader",
			}),
			wantOk: true,
			validate: func(t *testing.T, uc UserContext) {
				if !IsReader(uc) {
					t.Error("expected reader role")
				}
			},
		},
		{
			name: "context with owner role",
			ctx: ContextWithClaims(context.Background(), map[string]any{
				"sub":   "user-789",
				"email": "owner@example.com",
				"role":  "owner",
			}),
			wantOk: true,
			validate: func(t *testing.T, uc UserContext) {
				if !IsOwner(uc) {
					t.Error("expected owner role")
				}
			},
		},
		{
			name: "context with writer role",
			ctx: ContextWithClaims(context.Background(), map[string]any{
				"sub":   "user-wrt",
				"email": "writer@example.com",
				"role":  "writer",
			}),
			wantOk: true,
			validate: func(t *testing.T, uc UserContext) {
				if !IsWriter(uc) {
					t.Error("expected writer role")
				}
			},
		},
		{
			name: "legacy developer role migrates to admin",
			ctx: ContextWithClaims(context.Background(), map[string]any{
				"sub":   "user-dev",
				"email": "dev@example.com",
				"role":  "developer",
			}),
			wantOk: true,
			validate: func(t *testing.T, uc UserContext) {
				if !IsAdmin(uc) {
					t.Errorf("expected developer to migrate to admin, got %s", uc.Role)
				}
			},
		},
		{
			name: "legacy manager role migrates to writer",
			ctx: ContextWithClaims(context.Background(), map[string]any{
				"sub":   "user-mgr",
				"email": "manager@example.com",
				"role":  "manager",
			}),
			wantOk: true,
			validate: func(t *testing.T, uc UserContext) {
				if !IsWriter(uc) {
					t.Errorf("expected manager to migrate to writer, got %s", uc.Role)
				}
			},
		},
		{
			name: "legacy user role migrates to reader",
			ctx: ContextWithClaims(context.Background(), map[string]any{
				"sub":   "user-legacy",
				"email": "legacy@example.com",
				"role":  "user",
			}),
			wantOk: true,
			validate: func(t *testing.T, uc UserContext) {
				if !IsReader(uc) {
					t.Errorf("expected legacy user to migrate to reader, got %s", uc.Role)
				}
			},
		},
		{
			name: "legacy advocate role migrates to reader",
			ctx: ContextWithClaims(context.Background(), map[string]any{
				"sub":   "user-adv",
				"email": "advocate@example.com",
				"role":  "advocate",
			}),
			wantOk: true,
			validate: func(t *testing.T, uc UserContext) {
				if !IsReader(uc) {
					t.Errorf("expected advocate to migrate to reader, got %s", uc.Role)
				}
			},
		},
		{
			name: "context with groups",
			ctx: ContextWithClaims(context.Background(), map[string]any{
				"sub":    "user-grp",
				"email":  "grp@example.com",
				"role":   "reader",
				"groups": []string{"sales-team", "memql-users"},
			}),
			wantOk: true,
			validate: func(t *testing.T, uc UserContext) {
				if len(uc.Groups) != 2 {
					t.Errorf("expected 2 groups, got %d", len(uc.Groups))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userCtx, ok := UserFromContext(tt.ctx)
			if ok != tt.wantOk {
				t.Errorf("UserFromContext() ok = %v, want %v", ok, tt.wantOk)
				return
			}
			if ok && tt.validate != nil {
				tt.validate(t, userCtx)
			}
		})
	}
}

func TestIsOwner(t *testing.T) {
	tests := []struct {
		role     string
		expected bool
	}{
		{"owner", true},
		{"admin", false},
		{"writer", false},
		{"reader", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			ctx := UserContext{ID: "test", Role: Role(tt.role)}
			if got := IsOwner(ctx); got != tt.expected {
				t.Errorf("IsOwner(%q) = %v, want %v", tt.role, got, tt.expected)
			}
		})
	}
}

func TestIsAdmin(t *testing.T) {
	tests := []struct {
		role     string
		expected bool
	}{
		{"admin", true},
		{"owner", false},
		{"writer", false},
		{"reader", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			ctx := UserContext{ID: "test", Role: Role(tt.role)}
			if got := IsAdmin(ctx); got != tt.expected {
				t.Errorf("IsAdmin(%q) = %v, want %v", tt.role, got, tt.expected)
			}
		})
	}
}

func TestIsWriter(t *testing.T) {
	tests := []struct {
		role     string
		expected bool
	}{
		{"writer", true},
		{"owner", false},
		{"admin", false},
		{"reader", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			ctx := UserContext{ID: "test", Role: Role(tt.role)}
			if got := IsWriter(ctx); got != tt.expected {
				t.Errorf("IsWriter(%q) = %v, want %v", tt.role, got, tt.expected)
			}
		})
	}
}

func TestIsReader(t *testing.T) {
	tests := []struct {
		role     string
		expected bool
	}{
		{"reader", true},
		{"owner", false},
		{"admin", false},
		{"writer", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			ctx := UserContext{ID: "test", Role: Role(tt.role)}
			if got := IsReader(ctx); got != tt.expected {
				t.Errorf("IsReader(%q) = %v, want %v", tt.role, got, tt.expected)
			}
		})
	}
}

func TestAtLeastAdmin(t *testing.T) {
	tests := []struct {
		role     string
		expected bool
	}{
		{"owner", true},
		{"admin", true},
		{"writer", false},
		{"reader", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			ctx := UserContext{ID: "test", Role: Role(tt.role)}
			if got := AtLeastAdmin(ctx); got != tt.expected {
				t.Errorf("AtLeastAdmin(%q) = %v, want %v", tt.role, got, tt.expected)
			}
		})
	}
}

func TestCanWrite(t *testing.T) {
	tests := []struct {
		role     string
		expected bool
	}{
		{"owner", true},
		{"admin", true},
		{"writer", true},
		{"reader", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			ctx := UserContext{ID: "test", Role: Role(tt.role)}
			if got := CanWrite(ctx); got != tt.expected {
				t.Errorf("CanWrite(%q) = %v, want %v", tt.role, got, tt.expected)
			}
		})
	}
}

func TestCanRead(t *testing.T) {
	tests := []struct {
		role     string
		expected bool
	}{
		{"owner", true},
		{"admin", true},
		{"writer", true},
		{"reader", true},
		{"unknown", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			ctx := UserContext{ID: "test", Role: Role(tt.role)}
			if got := CanRead(ctx); got != tt.expected {
				t.Errorf("CanRead(%q) = %v, want %v", tt.role, got, tt.expected)
			}
		})
	}
}

func TestCanCreateAgent(t *testing.T) {
	tests := []struct {
		role     string
		expected bool
	}{
		{"owner", true},
		{"admin", true},
		{"writer", false},
		{"reader", false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			ctx := UserContext{ID: "test", Role: Role(tt.role)}
			if got := CanCreateAgent(ctx); got != tt.expected {
				t.Errorf("CanCreateAgent(%q) = %v, want %v", tt.role, got, tt.expected)
			}
		})
	}
}

func TestCanManageUser(t *testing.T) {
	tests := []struct {
		name       string
		actorRole  string
		targetRole string
		expected   bool
	}{
		{"owner can manage reader", "owner", "reader", true},
		{"owner can manage writer", "owner", "writer", true},
		{"owner can manage admin", "owner", "admin", true},
		{"owner cannot manage another owner", "owner", "owner", false},
		{"admin can manage writer", "admin", "writer", true},
		{"admin can manage reader", "admin", "reader", true},
		{"admin cannot manage owner", "admin", "owner", false},
		{"admin cannot manage another admin", "admin", "admin", false},
		{"writer cannot manage", "writer", "reader", false},
		{"reader cannot manage", "reader", "reader", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actor := UserContext{ID: "actor", Role: Role(tt.actorRole)}
			target := UserContext{ID: "target", Role: Role(tt.targetRole)}
			if got := CanManageUser(actor, target); got != tt.expected {
				t.Errorf("CanManageUser(%q, %q) = %v, want %v", tt.actorRole, tt.targetRole, got, tt.expected)
			}
		})
	}
}

func TestPrivilegedUser(t *testing.T) {
	tests := []struct {
		role     string
		expected bool
	}{
		{"owner", true},
		{"admin", true},
		{"writer", false},
		{"reader", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			ctx := UserContext{ID: "test", Role: Role(tt.role)}
			got := IsPrivilegedUser(ctx)
			if got != tt.expected {
				t.Errorf("IsPrivilegedUser(%q) = %v, want %v", tt.role, got, tt.expected)
			}
		})
	}
}

func TestUserContextFromMultipleRoles(t *testing.T) {
	tests := []struct {
		name        string
		role        string
		expectOwner bool
		expectAdmin bool
		expectWrt   bool
		expectRead  bool
	}{
		{
			name:        "owner is owner",
			role:        "owner",
			expectOwner: true,
		},
		{
			name:        "admin is admin",
			role:        "admin",
			expectAdmin: true,
		},
		{
			name:      "writer is writer",
			role:      "writer",
			expectWrt: true,
		},
		{
			name:       "reader is reader",
			role:       "reader",
			expectRead: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := ContextWithClaims(context.Background(), map[string]any{
				"sub":   "user-123",
				"email": "test@example.com",
				"role":  tt.role,
			})

			userCtx, ok := UserFromContext(ctx)
			if !ok {
				t.Fatal("expected to get user from context")
			}

			if IsOwner(userCtx) != tt.expectOwner {
				t.Errorf("IsOwner(%s) = %v, want %v", userCtx.Role, IsOwner(userCtx), tt.expectOwner)
			}
			if IsAdmin(userCtx) != tt.expectAdmin {
				t.Errorf("IsAdmin(%s) = %v, want %v", userCtx.Role, IsAdmin(userCtx), tt.expectAdmin)
			}
			if IsWriter(userCtx) != tt.expectWrt {
				t.Errorf("IsWriter(%s) = %v, want %v", userCtx.Role, IsWriter(userCtx), tt.expectWrt)
			}
			if IsReader(userCtx) != tt.expectRead {
				t.Errorf("IsReader(%s) = %v, want %v", userCtx.Role, IsReader(userCtx), tt.expectRead)
			}
		})
	}
}

func TestMigrateRole(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"developer", "admin"},
		{"manager", "writer"},
		{"user", "reader"},
		{"advocate", "reader"},
		{"member", "reader"},
		{"guest", "reader"},
		{"owner", "owner"},
		{"admin", "admin"},
		{"writer", "writer"},
		{"reader", "reader"},
		{"unknown", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := migrateRole(tt.input); got != tt.expected {
				t.Errorf("migrateRole(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Delegation RBAC Tests
// ---------------------------------------------------------------------------

func TestRoleLevel(t *testing.T) {
	tests := []struct {
		role  Role
		level int
	}{
		{RoleOwner, 0},
		{RoleAdmin, 1},
		{RoleWriter, 2},
		{RoleReader, 3},
		{Role("unknown"), 3},
		{Role(""), 3},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			if got := RoleLevel(tt.role); got != tt.level {
				t.Errorf("RoleLevel(%q) = %d, want %d", tt.role, got, tt.level)
			}
		})
	}

	// Verify ordering: owner < admin < writer < reader
	if RoleLevel(RoleOwner) >= RoleLevel(RoleAdmin) {
		t.Error("expected owner level < admin level")
	}
	if RoleLevel(RoleAdmin) >= RoleLevel(RoleWriter) {
		t.Error("expected admin level < writer level")
	}
	if RoleLevel(RoleWriter) >= RoleLevel(RoleReader) {
		t.Error("expected writer level < reader level")
	}
}

func TestRoleAtMost(t *testing.T) {
	tests := []struct {
		name     string
		identity Role
		ceiling  Role
		want     Role
	}{
		{"admin capped at admin", RoleAdmin, RoleAdmin, RoleAdmin},
		{"admin capped at reader", RoleAdmin, RoleReader, RoleReader},
		{"reader capped at admin", RoleReader, RoleAdmin, RoleReader},
		{"owner capped at writer", RoleOwner, RoleWriter, RoleWriter},
		{"reader capped at owner", RoleReader, RoleOwner, RoleReader},
		{"writer capped at writer", RoleWriter, RoleWriter, RoleWriter},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RoleAtMost(tt.identity, tt.ceiling); got != tt.want {
				t.Errorf("RoleAtMost(%q, %q) = %q, want %q", tt.identity, tt.ceiling, got, tt.want)
			}
		})
	}
}

func TestScopeAllows(t *testing.T) {
	tests := []struct {
		name      string
		scopes    []string
		operation string
		want      bool
	}{
		{"empty scopes allow everything", nil, "anything", true},
		{"empty slice allows everything", []string{}, "anything", true},
		{"wildcard allows anything", []string{"*"}, "query:activeAgents", true},
		{"exact match", []string{"query:activeAgents"}, "query:activeAgents", true},
		{"exact mismatch", []string{"query:activeAgents"}, "mutation:createAgent", false},
		{"prefix wildcard match", []string{"query:*"}, "query:activeAgents", true},
		{"prefix wildcard mismatch", []string{"query:*"}, "mutation:createAgent", false},
		{"nested prefix wildcard", []string{"mutation:cognition.*"}, "mutation:cognition.scoreUtterance", true},
		{"nested prefix mismatch", []string{"mutation:cognition.*"}, "mutation:email.sendEmail", false},
		{"multiple scopes", []string{"query:*", "mutation:cognition.*"}, "mutation:cognition.scoreUtterance", true},
		{"multiple scopes mismatch", []string{"query:*", "mutation:cognition.*"}, "mutation:email.sendEmail", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScopeAllows(tt.scopes, tt.operation); got != tt.want {
				t.Errorf("ScopeAllows(%v, %q) = %v, want %v", tt.scopes, tt.operation, got, tt.want)
			}
		})
	}
}

func TestEffectiveRole(t *testing.T) {
	tests := []struct {
		name     string
		dc       *DelegationContext
		wantRole Role
	}{
		{
			name:     "nil delegation returns reader",
			dc:       nil,
			wantRole: RoleReader,
		},
		{
			name: "admin identity with admin ceiling",
			dc: &DelegationContext{
				DelegatingIdentity: UserIdentity{Role: "admin"},
				RoleCeiling:        RoleAdmin,
			},
			wantRole: RoleAdmin,
		},
		{
			name: "admin identity with reader ceiling",
			dc: &DelegationContext{
				DelegatingIdentity: UserIdentity{Role: "admin"},
				RoleCeiling:        RoleReader,
			},
			wantRole: RoleReader,
		},
		{
			name: "reader identity with admin ceiling",
			dc: &DelegationContext{
				DelegatingIdentity: UserIdentity{Role: "reader"},
				RoleCeiling:        RoleAdmin,
			},
			wantRole: RoleReader,
		},
		{
			name: "owner identity with writer ceiling",
			dc: &DelegationContext{
				DelegatingIdentity: UserIdentity{Role: "owner"},
				RoleCeiling:        RoleWriter,
			},
			wantRole: RoleWriter,
		},
		{
			name: "invalid role defaults to reader",
			dc: &DelegationContext{
				DelegatingIdentity: UserIdentity{Role: "bogus"},
				RoleCeiling:        RoleAdmin,
			},
			wantRole: RoleReader,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveRole(tt.dc); got != tt.wantRole {
				t.Errorf("EffectiveRole() = %q, want %q", got, tt.wantRole)
			}
		})
	}
}

func TestIsDelegationExpired(t *testing.T) {
	tests := []struct {
		name string
		dc   *DelegationContext
		want bool
	}{
		{"nil delegation is expired", nil, true},
		{"zero expiry is persistent", &DelegationContext{}, false},
		{
			"future expiry is not expired",
			&DelegationContext{ExpiresAt: time.Now().Add(1 * time.Hour)},
			false,
		},
		{
			"past expiry is expired",
			&DelegationContext{ExpiresAt: time.Now().Add(-1 * time.Hour)},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDelegationExpired(tt.dc); got != tt.want {
				t.Errorf("IsDelegationExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUserFromContextWithDelegation(t *testing.T) {
	// Create a context with admin claims
	ctx := ContextWithClaims(context.Background(), map[string]any{
		"sub":   "agent-subject",
		"email": "agent@example.com",
		"role":  "admin",
	})

	// Without delegation, role should be admin
	uc, ok := UserFromContext(ctx)
	if !ok {
		t.Fatal("expected user from context")
	}
	if uc.Role != RoleAdmin {
		t.Errorf("without delegation, expected admin, got %s", uc.Role)
	}

	// With delegation capping to reader role
	dc := &DelegationContext{
		DelegationId: "test-delegation",
		DelegatingIdentity: UserIdentity{
			Subject: "human-subject",
			Email:   "human@example.com",
			Role:    "admin",
		},
		RoleCeiling: RoleReader,
		AgentId:     "test-agent",
	}
	ctx = ContextWithDelegation(ctx, dc)

	uc, ok = UserFromContext(ctx)
	if !ok {
		t.Fatal("expected user from context with delegation")
	}
	if uc.Role != RoleReader {
		t.Errorf("with delegation ceiling=reader, expected reader, got %s", uc.Role)
	}
}

func TestIsValidRole(t *testing.T) {
	tests := []struct {
		role Role
		want bool
	}{
		{RoleOwner, true},
		{RoleAdmin, true},
		{RoleWriter, true},
		{RoleReader, true},
		{Role("manager"), false},
		{Role("user"), false},
		{Role("unknown"), false},
		{Role(""), false},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			if got := IsValidRole(tt.role); got != tt.want {
				t.Errorf("IsValidRole(%q) = %v, want %v", tt.role, got, tt.want)
			}
		})
	}
}

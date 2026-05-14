// Package auth provides an IntegrationProvider for user/group lookup operations.
// Auth middleware (identity-issued JWT verification) lives in
// component/identity/verifier/ and component/auth/. This integration
// exposes user identity operations to the MemQL DSL.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	componentAuth "github.com/visionarys-io/memql/component/auth"
	"github.com/visionarys-io/memql/component/memql"
)

// AuthIntegration exposes auth/identity operations as DSL-callable capabilities.
type AuthIntegration struct{}

// NewAuthIntegration creates an auth integration.
func NewAuthIntegration() *AuthIntegration {
	return &AuthIntegration{}
}

// IntegrationName returns the stable identifier.
func (a *AuthIntegration) IntegrationName() string {
	return "auth"
}

// Capabilities returns DSL-callable auth operations.
func (a *AuthIntegration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "resolveUser",
			Description: "Resolve the current authenticated user from request context. Returns user identity.",
			Handler:     a.handleResolveUser,
			ArgsSchema:  map[string]string{},
		},
		{
			Name:        "checkPermission",
			Description: "Check if the current user has a specific role.",
			Handler:     a.handleCheckPermission,
			ArgsSchema: map[string]string{
				"role": "string",
			},
		},
	}
}

func (a *AuthIntegration) handleResolveUser(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	identity, err := componentAuth.UserIdentityFromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("no authenticated user in context: %w", err)
	}

	payloadBytes, _ := json.Marshal(map[string]any{
		"subject":    identity.Subject,
		"email":      identity.Email,
		"firstName":  identity.FirstName,
		"lastName":   identity.LastName,
		"role":       identity.Role,
		"resolvedAt": time.Now().UTC().Format(time.RFC3339),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("auth-user:%s", identity.Subject),
		Concept:   "integration:auth:user",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

func (a *AuthIntegration) handleCheckPermission(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	role, _ := args["role"].(string)
	if role == "" {
		return nil, fmt.Errorf("auth.checkPermission requires role")
	}

	identity, err := componentAuth.UserIdentityFromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("no authenticated user in context: %w", err)
	}

	hasRole := identity.Role == role

	payloadBytes, _ := json.Marshal(map[string]any{
		"subject":   identity.Subject,
		"role":      role,
		"hasRole":   hasRole,
		"checkedAt": time.Now().UTC().Format(time.RFC3339),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("auth-check:%s:%s", identity.Subject, role),
		Concept:   "integration:auth:permission",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

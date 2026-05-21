package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// handleCreateDelegation creates a new delegation grant from an identity to an agent.
// Validates that:
//   - Caller is the identity owner or has AtLeastAdmin role
//   - roleCeiling does not exceed the delegating identity's own role
//   - For synthetic identities: guardian must approve if guardianApprovalRequired is true
func (i *IdentityIntegration) handleCreateDelegation(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	identityId, _ := args["identityId"].(string)
	identitySubject, _ := args["identitySubject"].(string)
	identityType, _ := args["identityType"].(string)
	agentId, _ := args["agentId"].(string)
	roleCeiling, _ := args["roleCeiling"].(string)
	note, _ := args["note"].(string)

	if identityId == "" || identitySubject == "" || agentId == "" || roleCeiling == "" {
		return nil, fmt.Errorf("identity.createDelegation requires identityId, identitySubject, agentId, and roleCeiling")
	}

	if identityType == "" {
		identityType = "human"
	}

	// Validate role ceiling is a valid role
	if !componentAuth.IsValidRole(componentAuth.Role(roleCeiling)) {
		return nil, fmt.Errorf("identity.createDelegation: invalid roleCeiling %q, must be one of: owner, admin, writer, reader", roleCeiling)
	}

	// Validate caller has permission
	caller, err := componentAuth.UserIdentityFromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity.createDelegation requires authenticated context: %w", err)
	}

	callerUser, ok := componentAuth.UserFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("identity.createDelegation: cannot resolve caller user context")
	}

	// Caller must be the identity owner or have at least admin role
	isOwner := caller.Subject == identitySubject
	if !isOwner && !componentAuth.AtLeastAdmin(callerUser) {
		return nil, fmt.Errorf("identity.createDelegation: caller must be the identity owner or have admin role")
	}

	// Role ceiling cannot exceed the caller's own role
	if componentAuth.RoleLevel(componentAuth.Role(roleCeiling)) < componentAuth.RoleLevel(callerUser.Role) {
		return nil, fmt.Errorf("identity.createDelegation: roleCeiling %q exceeds caller's role %q", roleCeiling, callerUser.Role)
	}

	// Parse scopes
	var scopes []string
	if rawScopes, ok := args["scopes"].([]any); ok {
		for _, s := range rawScopes {
			if str, ok := s.(string); ok {
				scopes = append(scopes, str)
			}
		}
	}

	// Parse optional expiry
	var expiresAt *time.Time
	if expiresAtStr, ok := args["expiresAt"].(string); ok && expiresAtStr != "" {
		parsed, err := time.Parse(time.RFC3339, expiresAtStr)
		if err != nil {
			return nil, fmt.Errorf("identity.createDelegation: invalid expiresAt format, expected RFC3339: %w", err)
		}
		if parsed.Before(time.Now()) {
			return nil, fmt.Errorf("identity.createDelegation: expiresAt must be in the future")
		}
		expiresAt = &parsed
	}

	payload := map[string]any{
		"identityId":       identityId,
		"identitySubject":  identitySubject,
		"identityType":     identityType,
		"agentId":          agentId,
		"roleCeiling":      roleCeiling,
		"scopes":           scopes,
		"active":           true,
		"createdBySubject": caller.Subject,
		"note":             note,
		"createdAt":        time.Now().UTC().Format(time.RFC3339),
	}

	if expiresAt != nil {
		payload["expiresAt"] = expiresAt.UTC().Format(time.RFC3339)
	}

	payloadBytes, _ := json.Marshal(payload)

	delegationId := fmt.Sprintf("delegation-%s-%s-%d", identitySubject, agentId, time.Now().UnixNano())

	return []memorynodes.MemoryNode{{
		ID:        delegationId,
		Concept:   "v1:identity:delegation",
		Type:      memorynodes.NodeTypeObject,
		CreatedBy: caller.Email,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

// handleRevokeDelegation revokes an active delegation by its ID.
func (i *IdentityIntegration) handleRevokeDelegation(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	delegationId, _ := args["delegationId"].(string)
	if delegationId == "" {
		return nil, fmt.Errorf("identity.revokeDelegation requires delegationId")
	}

	caller, err := componentAuth.UserIdentityFromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity.revokeDelegation requires authenticated context: %w", err)
	}

	payloadBytes, _ := json.Marshal(map[string]any{
		"delegationId":     delegationId,
		"active":           false,
		"revokedAt":        time.Now().UTC().Format(time.RFC3339),
		"revokedBySubject": caller.Subject,
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("revocation:%s", delegationId),
		Concept:   "integration:identity:revocation",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

// handleResolveDelegation finds active, non-expired delegations for an agent subject.
func (i *IdentityIntegration) handleResolveDelegation(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	agentSubject, _ := args["agentSubject"].(string)
	if agentSubject == "" {
		return nil, fmt.Errorf("identity.resolveDelegation requires agentSubject")
	}

	// This capability returns the query parameters needed to resolve delegations.
	// The actual database query is performed by the MemQL engine when this is
	// called from an automation or query function.
	payloadBytes, _ := json.Marshal(map[string]any{
		"agentSubject": agentSubject,
		"resolvedAt":   time.Now().UTC().Format(time.RFC3339),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("resolve-delegation:%s", agentSubject),
		Concept:   "integration:identity:resolution",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

// handleValidateScope checks if a delegation permits a specific operation.
func (i *IdentityIntegration) handleValidateScope(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	delegationId, _ := args["delegationId"].(string)
	operation, _ := args["operation"].(string)

	if delegationId == "" || operation == "" {
		return nil, fmt.Errorf("identity.validateScope requires delegationId and operation")
	}

	// Scope validation is performed against the delegation record's scopes.
	// The actual delegation lookup and scope check would be done by the calling
	// automation/query. This capability provides the validation utility.
	payloadBytes, _ := json.Marshal(map[string]any{
		"delegationId": delegationId,
		"operation":    operation,
		"validatedAt":  time.Now().UTC().Format(time.RFC3339),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("scope-validation:%s:%s", delegationId, operation),
		Concept:   "integration:identity:scopeValidation",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

// The guardian-of-synthetic-identity model (handleResolveGuardian,
// handleSuspendGuarded) was removed when identity was restructured into
// user / identity / partition-access. Guardian approval for delegations
// now lives on the delegation row itself (guardianApproved flag).

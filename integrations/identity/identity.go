// Package identity provides an IntegrationProvider for delegation
// management. It exposes capabilities for creating, resolving, revoking,
// and validating delegations between user-owned identities and AI agents.
//
// The guardian-of-synthetic-identity model that lived here previously
// was retired when the user / identity / partition-access model was
// restructured. Guardian approval now lives on v1:identity:delegation
// directly (guardianApproved flag on the delegation row).
package identity

import (
	"log/slog"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/memql"
)

// IdentityIntegration exposes delegation operations as DSL-callable
// capabilities. Auth middleware (identity-issued JWT verification)
// lives in component/identity/verifier/ and component/auth/.
// Per-row authorization is enforced inside DSL queries + mutations;
// see docs/public/operate/auth/per-row-authz-audit.md.
type IdentityIntegration struct {
	// engine and db are nil on a node whose plug-in factory had neither.
	// Only ownership transfer (memql#4838) and GitHub Connect (memql#4913)
	// need them; every delegation capability above is pure and keeps working
	// without them, which is why the factory does not refuse when they are
	// absent.
	engine memql.IntegrationEngineAccess
	db     func() *bun.DB
	// logger records what a TYPED REFUSAL would otherwise swallow. A
	// capability that answers a reason rather than an error is easier for a
	// client and harder for an operator: without this, "the state row could
	// not be written" reaches a person as connect_state_invalid and reaches
	// nobody else at all. Nil on a node whose factory had none.
	logger *slog.Logger
}

// NewIdentityIntegration creates an identity integration.
func NewIdentityIntegration() *IdentityIntegration {
	return &IdentityIntegration{}
}

// NewIdentityIntegrationWithEngine creates one that can also transfer row
// ownership -- the capability that needs to read every declared concept and
// write back through the engine.
func NewIdentityIntegrationWithEngine(engine memql.IntegrationEngineAccess, db func() *bun.DB, logger *slog.Logger) *IdentityIntegration {
	return &IdentityIntegration{engine: engine, db: db, logger: logger}
}

// IntegrationName returns the stable identifier.
func (i *IdentityIntegration) IntegrationName() string {
	return "identity"
}

// Capabilities returns DSL-callable delegation operations.
func (i *IdentityIntegration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "createDelegation",
			Description: "Create a delegation grant from a user-owned identity to an AI agent with a role ceiling, optional scope list, and optional expiry.",
			Handler:     i.handleCreateDelegation,
			ArgsSchema: map[string]string{
				"identityId":      "string",
				"identitySubject": "string",
				"identityType":    "string",
				"agentId":         "string",
				"roleCeiling":     "string",
				"scopes":          "array?",
				"expiresAt":       "string?",
				"note":            "string?",
			},
		},
		{
			Name: "transferRowOwnership",
			Description: "Reassign every row one principal owns to another, optionally narrowed to " +
				"one concept or one row. Cluster-owner only; audited on v1:identity:auditEvent. " +
				"This is what makes rank-strict writes survivable: with peer rows read-only at " +
				"every rung, an offboarded principal's rows are otherwise unwritable forever.",
			Handler: i.handleTransferRowOwnership,
			ArgsSchema: map[string]string{
				"fromUserId": "string",
				"toUserId":   "string",
				"concept":    "string?",
				"rowId":      "string?",
			},
		},
		{
			Name: "githubConnectBegin",
			Description: "Begin GitHub Connect: write a short-lived state row bound to the caller " +
				"and answer the authorize URL the browser navigates to. Answers a typed reason " +
				"rather than an error when this cluster has no GitHub App, so the Source stop can " +
				"offer the pasted-token path instead of rendering a failure.",
			Handler: i.handleGithubConnectBegin,
			ArgsSchema: map[string]string{
				"returnPath": "string?",
			},
		},
		{
			Name:        "revokeDelegation",
			Description: "Revoke an active delegation by setting it inactive and recording the revoker.",
			Handler:     i.handleRevokeDelegation,
			ArgsSchema: map[string]string{
				"delegationId": "string",
			},
		},
		{
			Name:        "resolveDelegation",
			Description: "Find active, non-expired delegations for an agent's authenticated subject.",
			Handler:     i.handleResolveDelegation,
			ArgsSchema: map[string]string{
				"agentSubject": "string",
			},
		},
		{
			Name:        "validateScope",
			Description: "Check if a delegation permits a specific operation.",
			Handler:     i.handleValidateScope,
			ArgsSchema: map[string]string{
				"delegationId": "string",
				"operation":    "string",
			},
		},
	}
}

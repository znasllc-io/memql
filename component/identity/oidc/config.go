package oidc

import (
	"errors"
	"fmt"
	"strings"
)

// PROVIDER CONFIGURATION, AND THE ONE DECISION THIS EPIC MUST NOT LEAVE
// IMPLICIT (memql#4611).
//
// -----------------------------------------------------------------------------
// IS THE IdP AUTHORITATIVE? NO -- AND THAT IS STATED, NOT ASSUMED
// -----------------------------------------------------------------------------
//
// memql#4611 asks for "a decision on whether the IdP becomes authoritative --
// what happens to passkeys and magic links on a federated cluster, and what the
// break-glass path is when the IdP is unreachable", and says the owner recovery
// key "should be stated explicitly rather than assumed". So:
//
//   1. Enabling an upstream provider does NOT disable magic links or passkeys.
//      `MEMQL_IDENTITY_OIDC_EXCLUSIVE=true` is the separate, deliberate act
//      that does -- and even then it exempts the owner, for (3).
//
//   2. THE EXEMPTION IS NOT A LOOPHOLE, IT IS THE POINT. A federated cluster
//      whose IdP is unreachable has nobody who can sign in: not the operator,
//      not the owner, not the person who could fix the federation. That is a
//      cluster that locks its own administrator out of the machinery for
//      restoring access, and no configuration should be able to produce it.
//
//   3. THE BREAK-GLASS PATH IS THE OWNER RECOVERY KEY (memql#3958/#3964), and
//      it already has exactly the right shape: one active row per cluster,
//      bound to one owner, redeemed exactly once to register a passkey when
//      that owner has lost every sign-in route. Federation does not touch it,
//      cannot disable it, and `recoverykey.EnsureForAllOwners` keeps minting it
//      on every boot. A cluster that turns exclusive federation on is a cluster
//      whose recovery key had better be somewhere the cluster is not -- which
//      is what the claim path already tells the operator (memql#4628).
//
//   4. DEPROVISIONING IS THE DIRECTORY'S, AND ONLY FOR FEDERATED SESSIONS.
//      Removing somebody from the directory stops them signing in through the
//      IdP. It does not retroactively revoke a passkey they registered
//      earlier, and pretending otherwise would be the more dangerous claim.
//      Under `exclusive` it IS the whole story for every non-owner, which is
//      the mode's actual value.

// Config is one upstream provider. Nil/zero means federation is off.
type Config struct {
	// Enabled gates the whole feature. Everything else is inert when false.
	// Env: MEMQL_IDENTITY_OIDC_ENABLED
	Enabled bool

	// DisplayName is what the sign-in button says ("Continue with Microsoft").
	// Env: MEMQL_IDENTITY_OIDC_DISPLAY_NAME
	DisplayName string

	// Issuer is the provider's issuer URL. For Entra ID:
	//   https://login.microsoftonline.com/<tenant-id>/v2.0
	// TenantId below composes this when only the tenant is known, which is the
	// value an operator actually has.
	// Env: MEMQL_IDENTITY_OIDC_ISSUER
	Issuer string

	// TenantId composes an Entra issuer when Issuer is unset. The one
	// vendor-specific concession in this package, and it is here because the
	// alternative is an operator hand-composing a URL whose shape they have no
	// way to check.
	// Env: MEMQL_IDENTITY_OIDC_TENANT_ID
	TenantId string

	// ClientID / ClientSecret identify THIS cluster to the provider. The
	// secret is optional -- a public client using PKCE has none, and sending
	// an empty one to a provider expecting none is a rejection.
	// Env: MEMQL_IDENTITY_OIDC_CLIENT_ID / MEMQL_IDENTITY_OIDC_CLIENT_SECRET
	ClientID     string
	ClientSecret string

	// Scopes overrides the default openid/email/profile.
	// Env: MEMQL_IDENTITY_OIDC_SCOPES (space or comma separated)
	Scopes []string

	// GroupsClaim names the claim carrying group membership. There is no
	// standard one: Entra uses `groups`, others use `roles`. Empty means
	// groups are not read, and therefore that no group mapping applies.
	// Env: MEMQL_IDENTITY_OIDC_GROUPS_CLAIM
	GroupsClaim string

	// GroupRoles maps directory groups to cluster roles.
	// Env: MEMQL_IDENTITY_OIDC_GROUP_ROLES ("group=role,group=role")
	GroupRoles GroupRoleMap

	// Exclusive disables magic-link and passkey sign-in for NON-OWNER users.
	// See the header: the owner exemption is what keeps a federated cluster
	// recoverable, and it is not configurable.
	// Env: MEMQL_IDENTITY_OIDC_EXCLUSIVE
	Exclusive bool

	// DomainHint is passed through to skip the account picker for a known
	// tenant. Cosmetic; providers that do not know it ignore it.
	// Env: MEMQL_IDENTITY_OIDC_DOMAIN_HINT
	DomainHint string

	// GroupRolesError carries a MEMQL_IDENTITY_OIDC_GROUP_ROLES value that did
	// not parse, so Validate can refuse boot over it.
	//
	// A FIELD RATHER THAN A DROPPED ERROR, because the failure is silent in the
	// worst direction: an operator who wrote a mapping believes roles are being
	// granted, and a cluster that started with it absent puts everybody on the
	// default while the configuration says otherwise.
	GroupRolesError string
}

// EntraIssuerFor composes the v2.0 issuer for a tenant id.
func EntraIssuerFor(tenantId string) string {
	tenantId = strings.TrimSpace(tenantId)
	if tenantId == "" {
		return ""
	}
	return "https://login.microsoftonline.com/" + tenantId + "/v2.0"
}

// ResolvedIssuer is the issuer to discover against: the explicit one, else the
// Entra composition of TenantId.
func (c Config) ResolvedIssuer() string {
	if v := strings.TrimRight(strings.TrimSpace(c.Issuer), "/"); v != "" {
		return v
	}
	return EntraIssuerFor(c.TenantId)
}

// Validate refuses a half-configured provider at BOOT rather than at first
// sign-in.
//
// The same fail-closed rule the Anthropic federation config already follows
// (CLAUDE.md: "a partial config REFUSES BOOT rather than falling back"). A
// federation that is on but unusable is worse than one that is off: the button
// appears, people click it, and the failure arrives per-user rather than to the
// operator who could fix it.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	issuer := c.ResolvedIssuer()
	if issuer == "" {
		return errors.New("MEMQL_IDENTITY_OIDC_ENABLED is set but neither MEMQL_IDENTITY_OIDC_ISSUER " +
			"nor MEMQL_IDENTITY_OIDC_TENANT_ID names a provider")
	}
	if !strings.HasPrefix(issuer, "https://") {
		return fmt.Errorf("the OIDC issuer %q is not https; the keys that verify an id token "+
			"would be fetched over a channel anyone on the path can choose", issuer)
	}
	if strings.TrimSpace(c.ClientID) == "" {
		return errors.New("MEMQL_IDENTITY_OIDC_ENABLED is set but MEMQL_IDENTITY_OIDC_CLIENT_ID is empty; " +
			"without it the provider cannot tell which application is asking, and an id token could " +
			"not be audience-checked")
	}
	if strings.TrimSpace(c.GroupRolesError) != "" {
		return fmt.Errorf("MEMQL_IDENTITY_OIDC_GROUP_ROLES does not parse: %s", c.GroupRolesError)
	}
	if len(c.GroupRoles) > 0 && strings.TrimSpace(c.GroupsClaim) == "" {
		// A group mapping with no claim to read is a mapping the operator
		// believes is granting roles and which grants nothing. Silence here
		// would mean everybody lands on the cluster default while the config
		// says otherwise.
		return errors.New("MEMQL_IDENTITY_OIDC_GROUP_ROLES is configured but " +
			"MEMQL_IDENTITY_OIDC_GROUPS_CLAIM is empty, so no group would ever be read and the " +
			"mapping would grant nothing")
	}
	return nil
}

// AllowsLocalSignIn reports whether magic-link / passkey sign-in remains open
// to a user of this role.
//
// THE OWNER IS ALWAYS ALLOWED, and no configuration changes that -- see the
// header. Passing the role rather than a bool keeps the exemption at the one
// place that can state why it exists.
func (c Config) AllowsLocalSignIn(role string) bool {
	if !c.Enabled || !c.Exclusive {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(role), "owner")
}

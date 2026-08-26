package http

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/oidc"
)

// THE POLICY EDGE OF FEDERATION (memql#4611).
//
// The protocol is tested in component/identity/oidc. What these cover is the
// part that is this cluster's decision rather than the provider's, and where
// getting it wrong is a way IN rather than a broken sign-in.

// A FEDERATED SIGN-IN MUST NOT BE A WAY AROUND invite_only. `directory` mode
// exists precisely to say "the provider is the gate"; every other mode has its
// own answer, and a cluster that turned federation on without choosing
// `directory` must admit only people it would have admitted anyway.
func TestOidcProvisionRespectsTheRegistrationMode(t *testing.T) {
	claims := oidc.Claims{
		Issuer: "https://idp", Subject: "sub-1",
		Email: "stranger@example.com", EmailVerified: true,
	}

	for _, mode := range []identity.RegistrationMode{
		identity.RegistrationModeInviteOnly,
		identity.RegistrationModeWaitlist,
	} {
		s := &Server{Cfg: identity.Config{RegistrationMode: mode}}
		if _, err := s.provisionOidcUser(t.Context(), claims); err == nil {
			t.Errorf("%s admitted a federated stranger with no invitation; federation would be a "+
				"way around the mode the operator chose", mode)
		}
	}

	// domain_restricted still applies its allowlist to a federated address.
	s := &Server{Cfg: identity.Config{
		RegistrationMode:    identity.RegistrationModeDomainRestricted,
		RegistrationDomains: []string{"allowed.example"},
	}}
	if _, err := s.provisionOidcUser(t.Context(), claims); err == nil {
		t.Error("domain_restricted admitted an address outside its allowlist through federation")
	}
}

// An UNVERIFIED email may not create an account either, not only link to one.
// Otherwise somebody registers at an address they do not control, and the
// person who does control it is then refused when they arrive verified.
func TestOidcProvisionRefusesAnUnverifiedEmail(t *testing.T) {
	s := &Server{Cfg: identity.Config{RegistrationMode: identity.RegistrationModeDirectory}}
	_, err := s.provisionOidcUser(t.Context(), oidc.Claims{
		Issuer: "https://idp", Subject: "sub-1",
		Email: "unverified@example.com", EmailVerified: false,
	})
	if err == nil {
		t.Fatal("an unverified email created an account")
	}
	if !strings.Contains(err.Error(), "did not verify") {
		t.Errorf("the refusal does not name the cause: %v", err)
	}
}

func TestOidcProvisionRefusesNoEmail(t *testing.T) {
	// No address means no way to reach them, no way to match them later, and
	// nothing in the audit trail a human can read.
	s := &Server{Cfg: identity.Config{RegistrationMode: identity.RegistrationModeDirectory}}
	if _, err := s.provisionOidcUser(t.Context(), oidc.Claims{
		Issuer: "https://idp", Subject: "sub-1", EmailVerified: true,
	}); err == nil {
		t.Fatal("an account was created for a claim set carrying no email")
	}
}

// THE RANK MODEL IS RESTATED HERE because component/identity must not import
// component/auth. A restatement that drifts would silently change which
// directory group wins, so it is asserted against the real one.
func TestOidcRoleRankMatchesTheClusterRoleSet(t *testing.T) {
	// Every role component/auth considers valid must rank above the unknown
	// floor here, and the ORDER must agree.
	ordered := []string{"reader", "writer", "developer", "admin", "owner"}
	for i := 1; i < len(ordered); i++ {
		lo, hi := ordered[i-1], ordered[i]
		if oidcRoleRank(lo) >= oidcRoleRank(hi) {
			t.Errorf("rank(%s)=%d is not below rank(%s)=%d; a group mapping would pick the wrong "+
				"role when somebody is in both groups", lo, oidcRoleRank(lo), hi, oidcRoleRank(hi))
		}
	}
	for _, role := range ordered {
		if !auth.IsValidRole(auth.Role(role)) {
			t.Errorf("%q ranks here but component/auth does not consider it a role", role)
		}
		if oidcRoleRank(role) == 0 {
			t.Errorf("%q ranks at the unknown floor", role)
		}
	}
	// And every role component/auth names must be rankable here, or a mapping
	// onto it would silently lose to anything else.
	for _, role := range auth.ValidRoles() {
		if oidcRoleRank(string(role)) == 0 {
			t.Errorf("component/auth names role %q, which ranks at the unknown floor here -- a "+
				"group mapped to it would lose every tie", role)
		}
	}
	if oidcRoleRank("wizard") != 0 {
		t.Error("an unknown role does not rank at the floor")
	}
}

// The break-glass property, asserted where a reader of the identity server will
// look for it rather than only in the oidc package.
func TestExclusiveFederationLeavesTheOwnerARoute(t *testing.T) {
	cfg := identity.Config{OIDC: oidc.Config{
		Enabled: true, Exclusive: true, Issuer: "https://idp", ClientID: "x",
	}}
	if !cfg.OIDC.AllowsLocalSignIn("owner") {
		t.Fatal("exclusive federation closed the owner's local sign-in. A federated cluster whose " +
			"IdP is unreachable would then have nobody able to sign in, including the person who " +
			"could fix the federation.")
	}
	if cfg.OIDC.AllowsLocalSignIn("admin") {
		t.Error("exclusive federation still admits a non-owner locally")
	}
}

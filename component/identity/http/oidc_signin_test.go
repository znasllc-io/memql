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

// THE RANK MODEL IS NO LONGER RESTATED HERE (epic memql#4832, D1), so what is
// asserted has changed shape: not "does the copy still agree", but "is there
// still no copy".
//
// The deleted `oidcRoleRank` ranked admin (4) above developer (3) -- the very
// ordering the epic removed from MemQL OS as "the defect" -- and the test that
// guarded it passed the whole time, because it only compared pairs the two
// orderings agree on. It never named the one pair that differed.
func TestClusterRoleRankIsTheOneModelAndNotACopy(t *testing.T) {
	// EXACT agreement with component/auth for every role it names. A
	// restatement reintroduced here fails on the first slug it gets wrong,
	// rather than on whichever pair a hand-written table happened to compare.
	for _, role := range auth.ValidRoles() {
		if got, want := clusterRoleRank(string(role)), auth.RoleRank(role); got != want {
			t.Errorf("clusterRoleRank(%q) = %d, but auth.RoleRank says %d -- this adapter has "+
				"grown an ordering of its own", role, got, want)
		}
		if clusterRoleRank(string(role)) == 0 {
			t.Errorf("component/auth names role %q, which ranks at the unknown floor here -- a "+
				"group mapped to it would lose every tie", role)
		}
	}

	// THE PAIR THE OLD TEST NEVER NAMED, spelled out because it is the one the
	// restatement got backwards and the one whose fix changes a live mapping.
	if clusterRoleRank("developer") <= clusterRoleRank("admin") {
		t.Error("developer must outrank admin: that is the cluster's one ladder (D1), and a " +
			"person in both directory groups must resolve the same way here as everywhere else")
	}

	// Operator-authored `group=role` strings are not row values, so the fold
	// is real here in a way it is not on the invitation path.
	if clusterRoleRank("  Owner  ") != auth.RoleRank(auth.RoleOwner) {
		t.Error("an operator-spelled role with padding or capitals ranks at the floor, so the " +
			"group mapping it configures would silently lose every tie")
	}

	if clusterRoleRank("wizard") != 0 {
		t.Error("an unknown role does not rank at the floor")
	}
}

// The reachable positive: the ranker is not merely correct in isolation, it is
// the one MapRole actually consults. Somebody in BOTH groups resolves to
// developer -- the answer that changed when the copy went away.
func TestGroupRoleMapPrefersDeveloperOverAdmin(t *testing.T) {
	m := oidc.GroupRoleMap{"eng": "developer", "ops": "admin"}
	if got := m.MapRole([]string{"ops", "eng"}, clusterRoleRank); got != "developer" {
		t.Errorf("MapRole picked %q for somebody in both groups, want developer -- the cluster "+
			"ranks developer (300) above admin (200)", got)
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

package oidc

import "testing"

// rank is component/auth's model, restated here as the injected function the
// package takes. Kept local because oidc must not import component/auth, which
// sits below identity in the dependency order.
func rank(role string) int {
	switch role {
	case "owner":
		return 5
	case "admin":
		return 4
	case "developer":
		return 3
	case "writer":
		return 2
	case "reader":
		return 1
	}
	return 0
}

func verified(email string) Claims {
	return Claims{Issuer: "https://idp", Subject: "sub-1", Email: email, EmailVerified: true}
}

// -----------------------------------------------------------------------------
// THE LINKING RULE
// -----------------------------------------------------------------------------

func TestAnEstablishedLinkWinsOverEmail(t *testing.T) {
	// Once (issuer, subject) names a user, a changed email claim is a CHANGED
	// EMAIL, not a different person. Re-matching on it would move somebody's
	// account when their surname changed.
	d := DecideLink(verified("new.name@example.com"), LinkLookup{
		UserIdByLink:             "user-original",
		UserIdByEmail:            "user-somebody-else",
		EmailBelongsToActiveUser: true,
	})
	if d.Action != LinkExisting || d.UserId != "user-original" {
		t.Fatalf("got %+v, want the established link to win", d)
	}
}

func TestAVerifiedEmailLinksToAnExistingUser(t *testing.T) {
	// The bootstrap, used exactly once: the first time a known person arrives
	// through the IdP, before any (issuer, subject) link exists. Without it,
	// federating a live cluster mints a duplicate row for every existing user.
	d := DecideLink(verified("person@example.com"), LinkLookup{
		UserIdByEmail:            "user-existing",
		EmailBelongsToActiveUser: true,
	})
	if d.Action != LinkByEmail || d.UserId != "user-existing" {
		t.Fatalf("got %+v, want a link to the existing row", d)
	}
}

// THE SECURITY TEST OF THIS FILE. An unverified `email` claim is a string the
// directory did not check, so linking on it means anyone who can set their own
// email at the upstream can take over the matching MemQL account. That is the
// classic OIDC account-takeover, and it is one boolean away at all times.
func TestAnUnverifiedEmailNeverInheritsAnAccount(t *testing.T) {
	c := verified("person@example.com")
	c.EmailVerified = false

	d := DecideLink(c, LinkLookup{
		UserIdByEmail:            "user-existing",
		EmailBelongsToActiveUser: true,
	})
	if d.Action == LinkByEmail || d.UserId != "" {
		t.Fatalf("got %+v -- an unverified email claim inherited an existing account", d)
	}
	if d.Action != LinkRegister {
		t.Fatalf("got %+v, want LinkRegister: this person may still be entitled to register, "+
			"they just may not inherit somebody else's row", d)
	}
	if d.Reason != "oidc_email_unverified" {
		t.Errorf("reason = %q; every refusal keeps a distinct one", d.Reason)
	}
}

func TestAMatchOnADeactivatedUserIsRefusedRatherThanDuplicated(t *testing.T) {
	// Registering here would mint a SECOND row for an address a deactivated
	// one already holds -- the duplicate this whole file exists to prevent --
	// and hand access back to somebody it was deliberately removed from.
	d := DecideLink(verified("gone@example.com"), LinkLookup{
		UserIdByEmail:            "user-deactivated",
		EmailBelongsToActiveUser: false,
	})
	if d.Action != LinkRefuse {
		t.Fatalf("got %+v, want a refusal", d)
	}
	if d.Reason != "oidc_email_matches_deactivated_user" {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestNobodyMatchedMeansRegister(t *testing.T) {
	d := DecideLink(verified("stranger@example.com"), LinkLookup{})
	if d.Action != LinkRegister {
		t.Fatalf("got %+v, want LinkRegister", d)
	}
	// And whether they MAY register is the registration mode's decision, not
	// this function's -- so it must not answer LinkRefuse here.
	if d.Reason != "oidc_new_user" {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestIncompleteClaimsAreRefused(t *testing.T) {
	for _, c := range []Claims{
		{Issuer: "https://idp"},
		{Subject: "sub-1"},
		{},
	} {
		if d := DecideLink(c, LinkLookup{}); d.Action != LinkRefuse {
			t.Errorf("claims %+v produced %s, want a refusal", c, d.Action)
		}
	}
}

func TestEveryDecisionCarriesADistinctReason(t *testing.T) {
	// memql#4601's constraint: every refusal keeps a distinct audit reason. It
	// is what makes a support question answerable from the trail instead of
	// from a guess.
	seen := map[string]string{}
	cases := []struct {
		name string
		c    Claims
		l    LinkLookup
	}{
		{"link", verified("a@x"), LinkLookup{UserIdByLink: "u1"}},
		{"by email", verified("a@x"), LinkLookup{UserIdByEmail: "u2", EmailBelongsToActiveUser: true}},
		{"deactivated", verified("a@x"), LinkLookup{UserIdByEmail: "u3"}},
		{"new", verified("a@x"), LinkLookup{}},
		{"unverified", Claims{Issuer: "i", Subject: "s", Email: "a@x"}, LinkLookup{}},
		{"no email", Claims{Issuer: "i", Subject: "s"}, LinkLookup{}},
		{"incomplete", Claims{}, LinkLookup{}},
	}
	for _, tc := range cases {
		r := DecideLink(tc.c, tc.l).Reason
		if prev, dup := seen[r]; dup {
			t.Errorf("%s reuses %s's reason %q", tc.name, prev, r)
		}
		seen[r] = tc.name
	}
}

// -----------------------------------------------------------------------------
// ROLE MAPPING
// -----------------------------------------------------------------------------

func TestTheHighestMatchingGroupWins(t *testing.T) {
	// Group membership is a SET and a person is legitimately in several.
	// Taking the first match would make the outcome depend on the order the
	// directory happened to return them, which is not a thing an operator can
	// reason about.
	m := GroupRoleMap{"g-admins": "admin", "g-everyone": "reader", "g-devs": "developer"}
	got := m.MapRole([]string{"g-everyone", "g-admins", "g-devs"}, rank)
	if got != "admin" {
		t.Fatalf("role = %q, want admin", got)
	}
	// And the reverse order must give the same answer.
	if again := m.MapRole([]string{"g-devs", "g-admins", "g-everyone"}, rank); again != got {
		t.Fatalf("order changed the answer: %q then %q", got, again)
	}
}

func TestAnUnmappedGroupIsNotABan(t *testing.T) {
	// "" means the cluster default, NOT "no access". Conflating them would
	// make a missing group mapping silently equivalent to a ban.
	m := GroupRoleMap{"g-admins": "admin"}
	if got := m.MapRole([]string{"g-nobody-mapped-this"}, rank); got != "" {
		t.Fatalf("role = %q, want empty (the cluster default)", got)
	}
}

func TestParseGroupRoleMapRefusesWhatItCannotHonour(t *testing.T) {
	valid := func(r string) bool { return rank(r) > 0 }

	good, err := ParseGroupRoleMap("g-admins=admin, g-devs=developer", valid)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if good["g-admins"] != "admin" || good["g-devs"] != "developer" {
		t.Fatalf("parsed %+v", good)
	}

	for _, bad := range []string{"g-admins", "g-admins=", "=admin", "g-admins=wizard"} {
		if _, err := ParseGroupRoleMap(bad, valid); err == nil {
			t.Errorf("%q was accepted; a mapping that does not parse is one the operator "+
				"believes is granting roles and which grants nothing", bad)
		}
	}
	// An empty mapping is not an error -- it is the ordinary case.
	if _, err := ParseGroupRoleMap("", valid); err != nil {
		t.Errorf("empty mapping rejected: %v", err)
	}
}

// -----------------------------------------------------------------------------
// the break-glass decision (memql#4611 asks for this to be EXPLICIT)
// -----------------------------------------------------------------------------

func TestExclusiveFederationNeverLocksOutTheOwner(t *testing.T) {
	c := Config{Enabled: true, Exclusive: true, ClientID: "x", Issuer: "https://idp"}

	if !c.AllowsLocalSignIn("owner") {
		t.Fatal("exclusive federation locked out the owner. A federated cluster whose IdP is " +
			"unreachable would then have nobody able to sign in -- not the operator, not the " +
			"person who could fix the federation. No configuration may produce that.")
	}
	for _, role := range []string{"admin", "developer", "writer", "reader", ""} {
		if c.AllowsLocalSignIn(role) {
			t.Errorf("exclusive federation still admits local sign-in for %q", role)
		}
	}
}

func TestFederationDoesNotDisableLocalSignInByItself(t *testing.T) {
	// Enabling a provider is not the same act as making it exclusive, and
	// conflating them would silently disable passkeys on every cluster that
	// tried federation once.
	c := Config{Enabled: true, ClientID: "x", Issuer: "https://idp"}
	for _, role := range []string{"owner", "admin", "reader"} {
		if !c.AllowsLocalSignIn(role) {
			t.Errorf("merely enabling federation disabled local sign-in for %q", role)
		}
	}
}

// -----------------------------------------------------------------------------
// config
// -----------------------------------------------------------------------------

func TestConfigRefusesHalfConfigurationAtBoot(t *testing.T) {
	// A federation that is on but unusable is worse than one that is off: the
	// button appears, people click it, and the failure arrives per-user rather
	// than to the operator who could fix it.
	cases := []struct {
		name string
		c    Config
	}{
		{"no issuer", Config{Enabled: true, ClientID: "x"}},
		{"no client id", Config{Enabled: true, Issuer: "https://idp"}},
		{"plaintext issuer", Config{Enabled: true, Issuer: "http://idp", ClientID: "x"}},
		{"group map with no claim", Config{
			Enabled: true, Issuer: "https://idp", ClientID: "x",
			GroupRoles: GroupRoleMap{"g": "admin"},
		}},
	}
	for _, tc := range cases {
		if err := tc.c.Validate(); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	// Off is always valid -- the whole feature is inert.
	if err := (Config{}).Validate(); err != nil {
		t.Errorf("a disabled provider was rejected: %v", err)
	}
}

func TestTenantIdComposesTheEntraIssuer(t *testing.T) {
	// The one vendor-specific concession, and it is here because the operator
	// has the tenant id rather than an issuer URL whose shape they cannot check.
	c := Config{Enabled: true, TenantId: "aaaa-bbbb", ClientID: "x"}
	if got := c.ResolvedIssuer(); got != "https://login.microsoftonline.com/aaaa-bbbb/v2.0" {
		t.Fatalf("issuer = %q", got)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a tenant-only config was refused: %v", err)
	}
	// An explicit issuer wins, so a non-Microsoft provider is not second class.
	c.Issuer = "https://accounts.google.com"
	if got := c.ResolvedIssuer(); got != "https://accounts.google.com" {
		t.Fatalf("explicit issuer lost to the tenant composition: %q", got)
	}
}

func TestAnUnparseableGroupMappingRefusesBoot(t *testing.T) {
	// The failure is silent in the worst direction: an operator who wrote a
	// mapping believes roles are being granted, and a cluster that started
	// with it absent puts everybody on the cluster default while the
	// configuration says otherwise.
	c := Config{
		Enabled: true, Issuer: "https://idp", ClientID: "x",
		GroupsClaim:     "groups",
		GroupRolesError: `group role mapping entry g-admins: expected group=role`,
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("a cluster booted with a group mapping that does not parse")
	}
	if !contains(err.Error(), "g-admins") {
		t.Errorf("the error does not name the offending entry: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

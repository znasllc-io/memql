package groups

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// The guards (epic memql#5165, D7). Every one has a refusing test and an
// admitting one -- a guard tested only in the direction it refuses passes
// identically when it refuses everything.

func callerCtx(userID string, role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userID,
		Role:   role,
	})
}

func TestResolveCallerRefusesEveryNonPerson(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"no access context at all", context.Background()},
		{"a blank user id", callerCtx("", auth.RoleOwner)},
		// The CANONICAL constructors, not a hand-built approximation. Both
		// predicates read a PAIR of fields -- IsAnonymousActor wants
		// IsAnonymous AND RoleAnonymous, IsConnector wants a resolvable
		// connector name -- so an actor assembled field by field in a test
		// can miss the guard while looking exactly like the thing it is
		// meant to represent.
		{"an anonymous actor", auth.ContextWithAccess(context.Background(), auth.AnonymousActor())},
		{"a connector actor", auth.ContextWithAccess(context.Background(), auth.ConnectorActor("shopify"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveCaller(tc.ctx)
			if err == nil {
				t.Fatal("want a refusal, got nil")
			}
			if RefusalCode(err) != CodeNoCaller {
				t.Fatalf("error %q carries code %q, want %q", err, RefusalCode(err), CodeNoCaller)
			}
		})
	}
}

func TestResolveCallerAdmitsAPersonAndFoldsTheirRole(t *testing.T) {
	// The positive control, plus the case-folding: AccessContext.Role is
	// stamped straight off the user row, and an unfolded value ranks 0 and
	// matches no capability set.
	c, err := resolveCaller(callerCtx("u-1", auth.Role("Admin")))
	if err != nil {
		t.Fatalf("a signed-in admin: %v", err)
	}
	if string(c.role) != "admin" {
		t.Fatalf("role = %q, want it folded to %q", c.role, "admin")
	}
	if c.rank != auth.RoleRank(auth.RoleAdmin) {
		t.Fatalf("rank = %d, want %d", c.rank, auth.RoleRank(auth.RoleAdmin))
	}
}

func TestCapabilityGuardFollowsTheSeededGrants(t *testing.T) {
	// `update` on group is what four of the five verbs check, and it was
	// seeded by this epic -- before it, every one of them refused every
	// caller. That is what this pins.
	for _, tc := range []struct {
		role  auth.Role
		verb  string
		admit bool
	}{
		{auth.RoleOwner, auth.VerbUpdate, true},
		{auth.RoleAdmin, auth.VerbUpdate, true},
		{auth.RoleOwner, auth.VerbCreate, true},
		{auth.RoleAdmin, auth.VerbCreate, true},
		// A developer manages code, not people: no group grant at all.
		{auth.RoleDeveloper, auth.VerbUpdate, false},
		{auth.RoleWriter, auth.VerbUpdate, false},
		{auth.RoleReader, auth.VerbUpdate, false},
		{auth.RoleWriter, auth.VerbCreate, false},
	} {
		c, err := resolveCaller(callerCtx("u-1", tc.role))
		if err != nil {
			t.Fatalf("resolveCaller(%s): %v", tc.role, err)
		}
		err = c.requireCapability(context.Background(), tc.verb)
		if tc.admit && err != nil {
			t.Fatalf("%s holding %s on group: want admit, got %v", tc.role, tc.verb, err)
		}
		if !tc.admit {
			if err == nil {
				t.Fatalf("%s holding %s on group: want a refusal", tc.role, tc.verb)
			}
			if RefusalCode(err) != CodeCapabilityMissing {
				t.Fatalf("%s/%s: code %q, want %q", tc.role, tc.verb, RefusalCode(err), CodeCapabilityMissing)
			}
		}
	}
}

func TestRankGuardRefusesAPeerAndAdmitsSomebodyBelow(t *testing.T) {
	admin, err := resolveCaller(callerCtx("u-admin", auth.RoleAdmin))
	if err != nil {
		t.Fatal(err)
	}
	// STRICTLY below. A peer cannot move a peer, because placing somebody in
	// a client's group is granting them reach into that client's work, and
	// admin-places-admin would make that self-service at one rung.
	if err := admin.requireTargetBelow("u-other", "admin"); err == nil {
		t.Fatal("an admin placing an admin: want a refusal")
	} else if RefusalCode(err) != CodeRankNotBelowCaller {
		t.Fatalf("code %q, want %q", RefusalCode(err), CodeRankNotBelowCaller)
	}
	// developer ranks 300, ABOVE admin's 200 -- the ordering memql#4833
	// established, and the one every requirement written under the old OS
	// ladder has to be re-read against.
	if err := admin.requireTargetBelow("u-dev", "developer"); err == nil {
		t.Fatal("an admin placing a developer: want a refusal, because developer outranks admin")
	}
	for _, below := range []string{"writer", "reader", "user", "viewer"} {
		if err := admin.requireTargetBelow("u-x", below); err != nil {
			t.Fatalf("an admin placing a %s: want admit, got %v", below, err)
		}
	}
}

func TestRankGuardCaseFoldsTheTarget(t *testing.T) {
	// The dangerous direction. An unfolded TARGET role ranks 0, which reads
	// as "the least privileged person in the cluster" -- so a guard that
	// folds the caller and not the target admits every target.
	admin, err := resolveCaller(callerCtx("u-admin", auth.RoleAdmin))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.requireTargetBelow("u-owner", "OWNER"); err == nil {
		t.Fatal("an admin placing an \"OWNER\": want a refusal -- an unfolded target role ranks 0 and fails open")
	}
}

func TestOnlyAnOwnerMayPlaceAnOwner(t *testing.T) {
	admin, _ := resolveCaller(callerCtx("u-admin", auth.RoleAdmin))
	if err := admin.requireTargetBelow("u-owner", "owner"); err == nil {
		t.Fatal("an admin placing an owner: want a refusal")
	}
	owner, _ := resolveCaller(callerCtx("u-owner-1", auth.RoleOwner))
	// An owner outranks every non-owner, so the ordinary rank rule carries
	// them; what the carve-out adds is that a custom role authored ABOVE
	// owner still cannot reach one.
	if err := owner.requireTargetBelow("u-admin", "admin"); err != nil {
		t.Fatalf("an owner placing an admin: want admit, got %v", err)
	}
}

func TestMembershipIdIsDerivedFromThePair(t *testing.T) {
	// D11: re-adding somebody writes a new VERSION at the same id, which is
	// what makes "is this person a member" a question with one answer.
	a := MembershipID("v1:identity:group:acct-x", "v1:identity:user:u-1")
	b := MembershipID("acct-x", "u-1")
	if a != b {
		t.Fatalf("the id depends on id spelling: %q vs %q", a, b)
	}
	if a != "acct-x-u-1" {
		t.Fatalf("MembershipID = %q, want %q", a, "acct-x-u-1")
	}
	if MembershipID("g1", "u1") == MembershipID("g1", "u2") {
		t.Fatal("two users share one membership id")
	}
}

func TestAccountGroupIdIsDerivedAndReadable(t *testing.T) {
	if got := AccountGroupID("v1:accounts:account:acme"); got != "acct-acme" {
		t.Fatalf("AccountGroupID = %q, want %q", got, "acct-acme")
	}
	if AccountGroupID("acme") != AccountGroupID("v1:accounts:account:acme") {
		t.Fatal("the derived id depends on which spelling it was handed")
	}
}

func TestRefusalCodeReadsThroughAWrap(t *testing.T) {
	// The store wraps errors as "groups: <verb>: %w", so a code that only
	// worked at the front of the string would stop being readable exactly
	// where it matters.
	err := refusal(CodeSelfAddRefused, "a person may not add themselves")
	if RefusalCode(err) != CodeSelfAddRefused {
		t.Fatalf("bare: code %q", RefusalCode(err))
	}
	wrapped := refusal(CodeSelfAddRefused, "x")
	wrapped = wrapError("groups: groupMemberAdd", wrapped)
	if RefusalCode(wrapped) != CodeSelfAddRefused {
		t.Fatalf("wrapped: code %q", RefusalCode(wrapped))
	}
	if RefusalCode(nil) != "" {
		t.Fatal("a nil error carries a code")
	}
	if code := RefusalCode(errString("some unrelated failure")); code != "" {
		t.Fatalf("an unrelated error carries code %q", code)
	}
}

// wrapError mirrors the store's wrap so the test measures the real shape.
func wrapError(prefix string, err error) error {
	return errString(prefix + ": " + err.Error())
}

type errString string

func (e errString) Error() string { return string(e) }

// The refusal codes are the contract the OS keys its copy on, so the set has
// to be measured rather than restated.
//
// READ OUT OF THE SOURCE, not listed here. A hand-kept list satisfies every
// "nothing uncovered" assertion while covering only what somebody remembered
// to add -- a peer session hit exactly that shape on a sibling package, where
// a scanner matching `Code\w+ = "..."` found ZERO codes in a package whose
// constants are private and passed. So this reads the declarations, asserts a
// POSITIVE (that it found some at all), and then checks the properties.
func TestEveryRefusalCodeIsDistinct(t *testing.T) {
	declared := declaredRefusalCodes(t)
	if len(declared) < 8 {
		t.Fatalf("found %d refusal codes in guards.go; the scan is not seeing the "+
			"declarations, so every assertion below is measuring an empty set", len(declared))
	}
	seen := map[string]string{}
	for name, code := range declared {
		if !strings.HasPrefix(code, "group_") {
			t.Errorf("%s = %q does not carry the group_ prefix every OS key matches on", name, code)
		}
		if prior, dup := seen[code]; dup {
			t.Errorf("%s and %s are both %q -- one refusal would be indistinguishable "+
				"from another at the only place a person reads them", prior, name, code)
		}
		seen[code] = name
	}
}

// TestEveryRefusalCodeIsReachable is the other half, and the one a restated
// list cannot give: every code declared must actually be RETURNED somewhere.
//
// A code nobody raises is a contract the OS copies for a refusal that cannot
// happen, and it reads exactly like one that can.
func TestEveryRefusalCodeIsReachable(t *testing.T) {
	declared := declaredRefusalCodes(t)
	body := packageSource(t)
	for name, code := range declared {
		// The constant is referenced by NAME at the refusal site, so look for
		// the name rather than the string -- searching for the literal would
		// find the declaration itself and pass vacuously.
		uses := strings.Count(body, name)
		if uses < 2 {
			t.Errorf("%s (%q) is declared and never returned. A code nobody raises is a "+
				"refusal the OS has copy for and can never show", name, code)
		}
	}
}

// declaredRefusalCodes reads the `Code... = "group_..."` constants out of
// guards.go.
func declaredRefusalCodes(t *testing.T) map[string]string {
	t.Helper()
	src, err := os.ReadFile("guards.go")
	if err != nil {
		t.Fatalf("read guards.go: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*(Code\w+)\s*=\s*"([^"]+)"`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out[m[1]] = m[2]
	}
	return out
}

// packageSource concatenates every non-test .go file in the package.
func packageSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		b.Write(src)
	}
	if b.Len() == 0 {
		t.Fatal("no package source read; both tests above would measure nothing")
	}
	return b.String()
}

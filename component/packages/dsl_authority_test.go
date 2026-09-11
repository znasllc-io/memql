package packages

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// TestActorFromContextResolvesTheD9Authority is the whole of "devs and owners
// only, admin refused".
//
// The pipeline is handed a resolved boolean and cannot see a role, so this
// seam is the ONLY place the role -> authority mapping exists. A test at the
// gate proves the boolean is honoured; only this one proves the boolean is
// right.
func TestActorFromContextResolvesTheD9Authority(t *testing.T) {
	for _, tc := range []struct {
		role auth.Role
		want bool
		why  string
	}{
		{auth.RoleOwner, true, "the owner deployed DSL before this change and must still"},
		{auth.RoleDeveloper, true, "the point of the change: developer is engineering authority"},
		{auth.RoleAdmin, false, "admin is user-management authority and does NOT author constructs (#1529 section 4)"},
		{auth.RoleWriter, false, "writer holds no construct grant"},
		{auth.RoleReader, false, "reader holds no construct grant"},
		{auth.Role(""), false, "an empty role must fail closed, not panic"},
		{auth.Role("no-such-role"), false, "an unknown slug holds nothing and ranks nothing"},
	} {
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: "v1:identity:user:someone",
			Role:   tc.role,
		})
		got := actorFromContext(ctx).MayDeployDsl
		if got != tc.want {
			t.Errorf("role %q: MayDeployDsl = %v, want %v -- %s", tc.role, got, tc.want, tc.why)
		}
	}
}

// TestActorFromContextFailsClosedWithNoAccessContext pins the branch that
// makes the zero Actor safe. It is easy to "tidy" this into a constructor that
// later grows a default, and the default that matters here is false.
func TestActorFromContextFailsClosedWithNoAccessContext(t *testing.T) {
	got := actorFromContext(context.Background())
	if got.MayDeployDsl {
		t.Fatal("no access context resolved to MayDeployDsl=true -- an unauthenticated caller may deploy DSL")
	}
	if got.UserId != "" {
		t.Fatalf("no access context resolved a user id %q", got.UserId)
	}
}

// TestBorrowedAuthorityDoesNotCarryTheD9Authority is why component/packages
// needs an injected role resolver rather than reading the actor it is given.
//
// auth.ContextWithUserActor is the borrow the auto-deploy feed and the
// work-spine use. It deliberately stamps Role: RoleWriter with Unranked: true
// -- the IDENTITY is borrowed, the AUTHORITY is not -- so resolving the D9
// answer from such a context can only ever say no, whatever role the person
// actually holds. Anything that "fixes" auto-deploy by routing it through
// actorFromContext is fixing nothing, and this test says so.
func TestBorrowedAuthorityDoesNotCarryTheD9Authority(t *testing.T) {
	ctx := auth.ContextWithUserActor(context.Background(), "v1:identity:user:a-real-developer")
	if actorFromContext(ctx).MayDeployDsl {
		t.Fatal("a borrowed-authority context resolved MayDeployDsl=true -- " +
			"ContextWithUserActor no longer stamps a rankless writer, and the " +
			"auto-deploy resolver in autodeploy.go may now be redundant")
	}
}

// TestAnAutomaticRunResolvesItsOwnersAuthority is the case a one-line change
// at actorFromContext silently does not reach.
//
// packages.Actor has TWO producers: actorFromContext, and the struct literal
// in startAutoRun. Before this, that literal carried no authority at all, so
// the D9 gate refused EVERY DSL-carrying automatic run -- the cluster owner's
// included -- while a comment in autodeploy.go asserted the gate "already
// ran". A developer who armed the switch would watch their manual deploy work
// and the identical push refuse, stickily: the run id derives from the
// version, so the append-only guard dismisses the retry as already handled.
func TestAnAutomaticRunResolvesItsOwnersAuthority(t *testing.T) {
	roleFn := func(role auth.Role) RoleResolver {
		return func(context.Context, string) (auth.Role, error) { return role, nil }
	}

	for _, tc := range []struct {
		name    string
		roles   RoleResolver
		refused bool
		why     string
	}{
		{"developer", roleFn(auth.RoleDeveloper), false,
			"the whole point: an armed developer's push must deploy its DSL"},
		{"owner", roleFn(auth.RoleOwner), false,
			"the owner could never auto-deploy DSL before this change either"},
		{"admin", roleFn(auth.RoleAdmin), true,
			"admin does not author constructs, on the manual path or this one"},
		{"writer", roleFn(auth.RoleWriter), true, "writer holds no construct grant"},
		{"no resolver", nil, true,
			"a node that cannot resolve roles must refuse, never deploy under a blank authority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, validPackage(), autoPackage())
			h.deps.Roles = tc.roles

			_, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-new")
			refused := RefusalCode(err) == CodeDslRequiresAuthoring

			if refused != tc.refused {
				t.Fatalf("refused=%v, want %v -- %s (err=%v)", refused, tc.refused, tc.why, err)
			}
			if tc.refused {
				// AT START still holds on this path: a refusal must not have
				// paid for a build first.
				if len(h.builder.built) != 0 || h.roller.rolls != 0 {
					t.Fatalf("a refused automatic run built or rolled: built=%v rolls=%d",
						h.builder.built, h.roller.rolls)
				}
			}
		})
	}
}

// TestAnAutomaticRunFailsClosedWhenTheRoleReadFails separates "the owner may
// not author" from "we could not find out". The first is an answer; the second
// is an error, and deploying on it would roll the cluster's DSL on the
// strength of a failed database read.
func TestAnAutomaticRunFailsClosedWhenTheRoleReadFails(t *testing.T) {
	h := newHarness(t, validPackage(), autoPackage())
	h.deps.Roles = func(context.Context, string) (auth.Role, error) {
		return auth.RoleOwner, context.DeadlineExceeded
	}

	started, err := h.deps.startAutoRun(context.Background(), autoPackage(), "sha-new")
	if started {
		t.Fatal("an automatic run started despite a failed role read")
	}
	if err == nil {
		t.Fatal("a failed role read must surface as an error, not a silent skip")
	}
	if len(h.builder.built) != 0 {
		t.Fatalf("a failed role read reached the builder: %v", h.builder.built)
	}
}

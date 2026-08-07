package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// agent_role_slug_unique_db_test.go -- memql#3207.
//
// The acceptance criterion is explicit that the refusal must be proven
// "through the MUTATION path rather than by asserting a validator in
// isolation", and that the idempotent re-seed must be TESTED rather than
// reasoned about. Both are here, driving the real engine.
//
// Postgres-gated like its neighbours (readMergeTestEngine skips when no DB is
// reachable). CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1, so
// a skip there is a failure rather than a green.

// userCtxFor is the shape a cockpit caller arrives as: an authenticated
// subject that is not a system actor.
func userCtxFor(subject string) context.Context {
	return auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: subject})
}

// THE TEST THAT FAILS AGAINST MAIN. `createAgentRole` opens
// `id: args.agentRoleId ?? args.slug`, so a caller supplying an explicit
// agentRoleId alongside a slug a seeded row already owns mints a SECOND active
// row carrying that slug. Nothing refused it: the predefined lock keys on the
// row id (there is no prior row at the new id) and the merged payload's
// `predefined` is false, so by that guard's own contract it is an ordinary
// user-role write.
func TestAgentRoleSlugUniquenessRefusesAShadowRowThroughTheMutationPath(t *testing.T) {
	eng, _, sysCtx := readMergeTestEngine(t)

	slug := "slug-unique-" + uniqueSuffix("role")

	// Seeded exactly as the SeedMaterializer does: explicit id == slug.
	runMutation(t, sysCtx, eng, "createAgentRole", map[string]any{
		"agentRoleId": slug,
		"slug":        slug,
		"name":        "Family Doctor",
		"predefined":  true,
	})

	_, err := eng.Execute(userCtxFor("user-slug-shadow"),
		`mutation createAgentRole(agentRoleId: "`+slug+`-shadow", slug: "`+slug+`", name: "Free Money Doctor")`)
	if err == nil {
		t.Fatal("a non-system actor minted a SECOND active agentRole carrying an already-owned " +
			"slug, through the real mutation path.\n\n" +
			"`v1:agents:agentRole.slug` documents itself as canonical and never renamed, and " +
			"`activeAgentRoles` is @public and unscoped -- so both rows appear in every catalog " +
			"read and a user-facing role picker shows a duplicate whose name the minter chose. " +
			"memql#3066 made findRoleBySlug prefer the predefined row; it left the collision. " +
			"memql#3207 is the collision.")
	}
	for _, want := range []string{"v1:agents:agentRole", slug} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the rejection did not come from the slug-uniqueness guard (missing %q): %v", want, err)
		}
	}
}

// The acceptance criterion the guard is most likely to violate: the seeder
// re-runs createAgentRole for all ~97 predefined roles on EVERY startup, with
// an explicit id. If self-exclusion is wrong, boot fails.
func TestAgentRoleSlugUniquenessLeavesTheIdempotentReseedAlone(t *testing.T) {
	eng, _, sysCtx := readMergeTestEngine(t)

	slug := "slug-reseed-" + uniqueSuffix("role")
	args := map[string]any{
		"agentRoleId": slug,
		"slug":        slug,
		"name":        "Family Doctor",
		"predefined":  true,
	}

	runMutation(t, sysCtx, eng, "createAgentRole", args)
	// Boot again. And again -- the seeder is not once-only.
	runMutation(t, sysCtx, eng, "createAgentRole", args)
	runMutation(t, sysCtx, eng, "createAgentRole", args)
}

// A DSL-expressed fix (deriving `id` from `slug` in the mutation body) could
// not cover this path: the raw `insert(` short-circuit skips the mutation
// template entirely. A validator in executeWrite still fires. The sibling
// predefined-lock guard records the same asymmetry for itself.
func TestAgentRoleSlugUniquenessCoversTheRawInsertPath(t *testing.T) {
	eng, _, sysCtx := readMergeTestEngine(t)

	slug := "slug-rawins-" + uniqueSuffix("role")
	runMutation(t, sysCtx, eng, "createAgentRole", map[string]any{
		"agentRoleId": slug,
		"slug":        slug,
		"name":        "Family Doctor",
		"predefined":  true,
	})

	_, err := eng.Execute(userCtxFor("user-slug-rawinsert"),
		`insert("v1:agents:agentRole", id="`+slug+`-raw", payload={"slug":"`+slug+`","name":"Raw","active":true})`)
	if err == nil {
		t.Error("the raw insert() path minted a shadow row on an owned slug. A guard that only " +
			"covers the named-mutation surface leaves the bypass the rowauthz work already " +
			"tracks as live.")
	}
}

// The rule is scoped to ACTIVE rows, so a deactivated role must not burn its
// slug forever. Pins the scoping against a stricter-than-intended
// implementation.
// Driven as a USER, not the seeder: the guard carves out the system actor, so
// a system-context version of this would pass without exercising the rule.
func TestAgentRoleSlugUniquenessAllowsReclaimingADeactivatedSlug(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	userCtx := userCtxFor("user-slug-reclaim")

	slug := "slug-reclaim-" + uniqueSuffix("role")
	runMutation(t, userCtx, eng, "createAgentRole", map[string]any{
		"agentRoleId": slug + "-old",
		"slug":        slug,
		"name":        "Retired Role",
		"active":      false,
	})

	// A different id may take the slug, because nothing active holds it.
	runMutation(t, userCtx, eng, "createAgentRole", map[string]any{
		"agentRoleId": slug + "-new",
		"slug":        slug,
		"name":        "Live Role",
		"active":      true,
	})
}

// The variant a create-only check misses: keep the id, RENAME the slug onto one
// an active row already holds.
func TestAgentRoleSlugUniquenessRefusesRenamingOntoATakenSlug(t *testing.T) {
	eng, _, sysCtx := readMergeTestEngine(t)

	suffix := uniqueSuffix("role")
	taken := "slug-taken-" + suffix
	mine := "slug-mine-" + suffix

	runMutation(t, sysCtx, eng, "createAgentRole", map[string]any{
		"agentRoleId": taken, "slug": taken, "name": "Family Doctor", "predefined": true,
	})
	runMutation(t, sysCtx, eng, "createAgentRole", map[string]any{
		"agentRoleId": mine, "slug": mine, "name": "My Own Role",
	})

	_, err := eng.Execute(userCtxFor("user-slug-rename"),
		`mutation createAgentRole(agentRoleId: "`+mine+`", slug: "`+taken+`", name: "My Own Role")`)
	if err == nil {
		t.Error("an existing row was renamed onto a slug an active row already holds. Uniqueness " +
			"has to be checked on the slug being WRITTEN, not only on newly created ids.")
	}
}

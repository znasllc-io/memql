package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// THE MODULES FLIP, AGAINST A REAL DATABASE (Connect Shopify design, section
// 9, D4).
//
// A developer turns a storefront pack on through SetPackEnabled, which writes
// under internal origin AFTER AuthorizeSetPackEnabled has admitted them. The
// same developer calling the mutation directly must be refused: once the pack
// has a row by the clusterOwner tier's write guard, and as a create by the
// create floor (create_rank_floor.go). Those two are what keep the audited path
// the only path.

// registerFlipTestPack registers a throwaway pack domain, optionally declared a
// storefront pack, for the length of one test.
func registerFlipTestPack(t *testing.T, domain string, storefront bool) {
	t.Helper()
	memqldsl.RegisterTree(domain, testPackTree(t))
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })
	if storefront {
		testStorefrontPack(t, domain)
	}
}

// packStateVersions counts every stored version of one pack's row, straight
// off the table under no actor, so a count is what was written and not what a
// tier let somebody see.
func packStateVersions(t *testing.T, eng *MemQLEngine, domain string) int {
	t.Helper()
	n, err := eng.database().NewSelect().Model((*memorynodes.MemoryNode)(nil)).
		Where("concept = ?", PackStateConceptID).
		Where("id = ?", PackStateConceptID+":"+domain).
		Count(context.Background())
	if err != nil {
		t.Fatalf("count packState %s: %v", domain, err)
	}
	return n
}

func TestADeveloperFlipsAStorefrontPackThatHasNoRow(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("packflip-first")
	domain := "sfp-" + suffix
	registerFlipTestPack(t, domain, true)
	user := "user-" + suffix
	seedPrincipal(t, eng, user, auth.RoleDeveloper)
	ctx := rankActorCtx(user, auth.RoleDeveloper)

	actor, _, refusal, err := eng.SetPackEnabled(ctx, domain, true, "taking the storefront live")
	if refusal != nil || err != nil {
		t.Fatalf("a developer's storefront flip was refused: refusal=%+v err=%v", refusal, err)
	}
	if actor.ID != user || actor.Role != auth.RoleDeveloper {
		t.Fatalf("the flip reported actor %+v, want the developer who made it", actor)
	}

	states, err := ReadPackStates(context.Background(), db)
	if err != nil {
		t.Fatalf("ReadPackStates: %v", err)
	}
	st, ok := states[domain]
	if !ok || !st.Enabled {
		t.Fatalf("no enabled packState row landed for %s: %+v", domain, st)
	}
	// Internal origin opens the gates; it does not replace the person. The row
	// must say who flipped it.
	if st.UpdatedBy != user {
		t.Fatalf("the row's createdBy is %q, want the developer %q: provenance was lost", st.UpdatedBy, user)
	}

	// A second flip onto the row that now exists lands too -- the write guard
	// judges a stored row, and this is the path it lets through.
	if _, _, refusal, err := eng.SetPackEnabled(ctx, domain, false, "off again"); refusal != nil || err != nil {
		t.Fatalf("a developer's second flip was refused: refusal=%+v err=%v", refusal, err)
	}
	if n := packStateVersions(t, eng, domain); n != 2 {
		t.Fatalf("%s has %d stored versions, want 2", domain, n)
	}
}

func TestADevelopersDirectMutationOnAPackWithARowIsRefused(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("packflip-direct")
	domain := "sfp-" + suffix
	registerFlipTestPack(t, domain, true)

	owner := "owner-" + suffix
	seedPrincipal(t, eng, owner, auth.RoleOwner)
	if _, _, refusal, err := eng.SetPackEnabled(rankActorCtx(owner, auth.RoleOwner), domain, true, ""); refusal != nil || err != nil {
		t.Fatalf("the owner's flip was refused: refusal=%+v err=%v", refusal, err)
	}

	dev := "user-" + suffix
	seedPrincipal(t, eng, dev, auth.RoleDeveloper)
	_, err := runSiteMutation(t, rankActorCtx(dev, auth.RoleDeveloper), eng, "setPackEnabled", map[string]any{
		"id":         domain,
		"packDomain": domain,
		"enabled":    false,
	})
	if err == nil {
		t.Fatal("a developer's direct setPackEnabled on a pack that has a row was ALLOWED. " +
			"Only SetPackEnabled's audited path may flip it")
	}
	if !strings.Contains(err.Error(), "row-authz") {
		t.Fatalf("the refusal was %q, not the write guard's", err)
	}
	if n := packStateVersions(t, eng, domain); n != 1 {
		t.Fatalf("%s has %d stored versions after the refused write, want 1", domain, n)
	}
}

// THE FRESH-ID OVERRIDE, found in review. setPackEnabled takes `id` and
// `packDomain` separately and the pack reader keys on `packDomain`, so a row
// under a NEW id naming an existing pack would override that pack's switch --
// and a new id is a create, which the write guard never judges. The create
// floor refuses it below owner, and a pack with no row at all the same way.
func TestADevelopersDirectCreateOfAPackRowIsRefused(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("packflip-freshid")
	domain := "sfp-" + suffix
	registerFlipTestPack(t, domain, false)

	owner := "owner-" + suffix
	seedPrincipal(t, eng, owner, auth.RoleOwner)
	if _, _, refusal, err := eng.SetPackEnabled(rankActorCtx(owner, auth.RoleOwner), domain, true, ""); refusal != nil || err != nil {
		t.Fatalf("the owner's flip was refused: refusal=%+v err=%v", refusal, err)
	}

	dev := "user-" + suffix
	seedPrincipal(t, eng, dev, auth.RoleDeveloper)
	devCtx := rankActorCtx(dev, auth.RoleDeveloper)
	for name, args := range map[string]map[string]any{
		"a fresh id naming a pack that has a row": {"id": domain + "-zz", "packDomain": domain, "enabled": false},
		"the first row of a pack with none":       {"id": domain + "-new", "packDomain": domain + "-new", "enabled": true},
	} {
		if _, err := runSiteMutation(t, devCtx, eng, "setPackEnabled", args); err == nil {
			t.Fatalf("%s: a developer's direct setPackEnabled create was ALLOWED", name)
		}
		if n := packStateVersions(t, eng, args["id"].(string)); n != 0 {
			t.Fatalf("%s: refused, yet %d version(s) landed", name, n)
		}
	}
	if n := packStateVersions(t, eng, domain); n != 1 {
		t.Fatalf("the owner's row has %d stored versions, want 1", n)
	}
}

// The engine is not reached when the check refuses: nothing is written for a
// developer on a pack that is not a storefront pack, nor for an admin on one
// that is.
func TestARefusedFlipWritesNothing(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("packflip-refused")
	storefront := "sfp-" + suffix
	plain := "plain-" + suffix
	registerFlipTestPack(t, storefront, true)
	registerFlipTestPack(t, plain, false)

	cases := []struct {
		role auth.Role
		pack string
	}{
		{auth.RoleDeveloper, plain},
		{auth.RoleAdmin, storefront},
		{auth.RoleWriter, storefront},
	}
	for _, tc := range cases {
		t.Run(string(tc.role)+"/"+tc.pack, func(t *testing.T) {
			user := "user-" + string(tc.role) + "-" + suffix
			seedPrincipal(t, eng, user, tc.role)
			_, _, refusal, err := eng.SetPackEnabled(rankActorCtx(user, tc.role), tc.pack, true, "")
			if err != nil {
				t.Fatalf("a refused flip reached the write and failed there: %v", err)
			}
			if refusal == nil || refusal.Code != moduleCodePermissionDenied {
				t.Fatalf("a %s flipping %s was not refused: %+v", tc.role, tc.pack, refusal)
			}
			if n := packStateVersions(t, eng, tc.pack); n != 0 {
				t.Fatalf("a refused flip wrote %d versions of %s", n, tc.pack)
			}
		})
	}
}

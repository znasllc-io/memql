package rbac

import (
	"context"
	"testing"

	componentAuth "github.com/znasllc-io/memql/component/auth"
)

// TestGovernanceBuiltinAgreesWithCore is the E1.6 (memql#2074) end-to-end
// agreement check for the governance surface: the DSL-facing builtin path
// (integration.rbac.governPrincipal, the @executor the governanceCanManagePrincipal
// logic calls) must yield EXACTLY the decision the authoritative Go core
// (component/auth.GovernPrincipal) yields, for every (actorRank, targetRank,
// verb, owner) combination the request path can present. If the integration's
// arg coercion or owner-derivation ever drifts from the core, this fails -- so
// enforcement stays consistent between "called as a DSL builtin" and "called
// as the Go primitive E1.5 routed the Can* helpers through".
func TestGovernanceBuiltinAgreesWithCore(t *testing.T) {
	i := New()
	ctx := context.Background()

	ranks := []struct {
		rank    int
		slug    string
		isOwner bool
	}{
		{50, "viewer", false},
		{100, "user", false},
		{200, "admin", false},
		{300, "developer", false},
		{400, "owner", true},
	}
	verbs := []string{"read", "create", "update", "delete"}

	for _, a := range ranks {
		for _, tgt := range ranks {
			for _, verb := range verbs {
				// Same actor/target ids => exercise BOTH the self path (equal ids)
				// and the cross-user path (distinct ids).
				for _, ids := range [][2]string{{"x", "x"}, {"x", "y"}} {
					args := map[string]any{
						"actorUserId":    ids[0],
						"actorRank":      int64(a.rank),
						"actorIsOwner":   a.isOwner,
						"targetUserId":   ids[1],
						"targetRank":     int64(tgt.rank),
						"targetRoleSlug": tgt.slug,
						"verb":           verb,
					}
					nodes, err := i.handleGovernPrincipal(ctx, args, 0)
					if err != nil || len(nodes) != 1 {
						t.Fatalf("handleGovernPrincipal: err=%v nodes=%d", err, len(nodes))
					}
					gotBuiltin := allowed(t, nodes[0].Payload)

					wantCore := componentAuth.GovernPrincipal(
						componentAuth.Principal{UserId: ids[0], Rank: a.rank, IsOwner: a.isOwner},
						componentAuth.Principal{UserId: ids[1], Rank: tgt.rank, IsOwner: tgt.isOwner},
						componentAuth.GovernVerb(verb),
					)
					if gotBuiltin != wantCore {
						t.Errorf("governPrincipal builtin disagrees with core: actor(%s,%d,owner=%v) target(%s,%d,owner=%v) verb=%s ids=%v -- builtin=%v core=%v",
							a.slug, a.rank, a.isOwner, tgt.slug, tgt.rank, tgt.isOwner, verb, ids, gotBuiltin, wantCore)
					}
				}
			}
		}
	}
}

// TestCreatePrincipalBuiltinAgreesWithCore is the same agreement check for the
// create != edit split builtin (integration.rbac.canCreatePrincipal vs
// component/auth.CanCreatePrincipal).
func TestCreatePrincipalBuiltinAgreesWithCore(t *testing.T) {
	i := New()
	ctx := context.Background()
	ranks := []int{50, 100, 200, 300, 400}
	for _, actorRank := range ranks {
		for _, newRank := range ranks {
			args := map[string]any{
				"actorRank":    int64(actorRank),
				"actorIsOwner": actorRank == 400,
				"newRank":      int64(newRank),
			}
			nodes, err := i.handleCanCreatePrincipal(ctx, args, 0)
			if err != nil || len(nodes) != 1 {
				t.Fatalf("handleCanCreatePrincipal: err=%v nodes=%d", err, len(nodes))
			}
			gotBuiltin := allowed(t, nodes[0].Payload)
			wantCore := componentAuth.CanCreatePrincipal(
				componentAuth.Principal{Rank: actorRank, IsOwner: actorRank == 400}, newRank,
			)
			if gotBuiltin != wantCore {
				t.Errorf("canCreatePrincipal builtin disagrees with core: actorRank=%d newRank=%d -- builtin=%v core=%v",
					actorRank, newRank, gotBuiltin, wantCore)
			}
		}
	}
}

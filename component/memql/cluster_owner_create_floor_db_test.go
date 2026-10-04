package memql

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE CREATE HALF OF THE CLUSTER-OWNER TIER, AGAINST A REAL ENGINE (memql#5624).
//
// The write guard judges the row stored at a write's target id, and a create
// has none, so a create on a @rowAuthz(clusterOwner) concept was judged by
// nothing but its mutation's own declarations -- and the raw insert() literal
// declares nothing. Connect Shopify closed two of these concepts with a
// two-entry table; this is the tier.
//
// Three properties, each with its own test:
//
//   - EVERY concept on the tier refuses a non-owner's create. The population is
//     read off the loaded registry, so a concept that declares the tier
//     tomorrow is covered by this test the day it does.
//   - The tier's own audience is admitted: a cluster owner, internal origin,
//     and the connector a concept names.
//   - Every kind of legitimate creator the sweep found still creates, under the
//     context it really uses.
//
// A refusal is evidence only beside proof that nothing landed, so the refused
// cases read the table too: an error raised after the insert would otherwise
// pass.

// clusterOwnerTierConcepts is every loaded concept declaring the cluster-owner
// tier, with or without its read floor.
func clusterOwnerTierConcepts(t *testing.T) []string {
	t.Helper()
	var out []string
	for name, c := range memoryNodes.All() {
		if c != nil && c.RowAuthz != nil && c.RowAuthz.Tier == langparser.RowAuthzClusterOwner {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func TestEveryClusterOwnerTierConceptRefusesANonOwnersCreate(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	concepts := clusterOwnerTierConcepts(t)
	// POSITIVE CONTROL. The tree declares the tier on ~100 concepts (33 of
	// MemQL's own plus 65 Shopify mirrors); a handful would mean the registry
	// did not load, and the loop below would pass over nothing.
	if len(concepts) < 90 {
		t.Fatalf("only %d concepts declare the cluster-owner tier in the loaded registry; this test would "+
			"measure almost nothing", len(concepts))
	}

	suffix := uniqueSuffix("co-create-floor")
	writer := rankActorCtx("user-"+suffix, auth.RoleWriter)
	for _, name := range concepts {
		t.Run(name, func(t *testing.T) {
			id := "floor-" + suffix
			err := rawInsertFor(t, writer, eng, name, id, map[string]any{})
			if err == nil {
				t.Fatalf("a writer created a %s row with a raw insert(). The concept declares the "+
					"cluster-owner tier, and nothing judged the create", name)
			}
			// A mirror refuses everyone but its connector before the floor is
			// reached, and two email concepts refuse every write without
			// internal origin beside it. Any other refusal is not this one:
			// an empty payload that failed validation would read as a pass on
			// the unfixed engine.
			if !IsMirrorWriteRefused(err) && !strings.Contains(err.Error(), "memql#5624") &&
				!strings.Contains(err.Error(), "requires internal origin") {
				t.Fatalf("%s's create was refused, but not by the create floor: %v", name, err)
			}
			assertRowCount(t, db, name, id, 0)
		})
	}
}

// The tier's audience on two concepts whose creates are the harm: a territory
// is a commerce record nobody below owner may author, and a grant is a
// capability. The named, client-reachable upsertTerritory is driven too, to
// show the floor sits where the mutation and the raw literal converge.
func TestAClusterOwnerTierCreateAdmitsOnlyTheTiersAudience(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)

	for _, tc := range []struct {
		role    auth.Role
		allowed bool
	}{
		{auth.RoleWriter, false},
		{auth.RoleAdmin, false},
		{auth.RoleDeveloper, false},
		{auth.RoleOwner, true},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			suffix := uniqueSuffix("co-audience-" + string(tc.role))
			user := "user-" + suffix
			seedPrincipal(t, eng, user, tc.role)
			ctx := rankActorCtx(user, tc.role)

			type write struct {
				concept, id string
				err         error
			}
			_, namedErr := runSiteMutation(t, ctx, eng, "upsertTerritory", map[string]any{
				"territoryId": "named-territory-" + suffix, "storeId": "store-" + suffix, "name": "South",
			})
			writes := map[string]write{
				"raw territory insert": {"v1:commerce:territory", "raw-territory-" + suffix,
					rawInsertFor(t, ctx, eng, "v1:commerce:territory", "raw-territory-"+suffix,
						map[string]any{"storeId": "store-" + suffix, "name": "North"})},
				"raw grant insert": {"v1:rbac:grant", "raw-grant-" + suffix,
					rawInsertFor(t, ctx, eng, "v1:rbac:grant", "raw-grant-"+suffix, map[string]any{
						"subjectKind": "user", "subjectId": user, "verb": "execute",
						"resourceType": "app:co-create-floor", "effect": "allow",
					})},
				"upsertTerritory": {"v1:commerce:territory", "named-territory-" + suffix, namedErr},
			}

			for what, w := range writes {
				if tc.allowed {
					if w.err != nil {
						t.Fatalf("a cluster owner's %s was refused: %v", what, w.err)
					}
					assertRowCount(t, db, w.concept, w.id, 1)
					continue
				}
				if w.err == nil {
					t.Fatalf("a %s's %s on a fresh id was ADMITTED: the tier does not judge a create, and "+
						"without the create floor every signed-in caller can write one", tc.role, what)
				}
				if !strings.Contains(w.err.Error(), "memql#5624") || !strings.Contains(w.err.Error(), `"owner"`) {
					t.Fatalf("%s's refusal was %q, which is not the create floor naming the role required", what, w.err)
				}
				assertRowCount(t, db, w.concept, w.id, 0)
			}
		})
	}
}

// A grant row IS a capability: grantsForSubjects reads every v1:rbac:grant
// naming a subject straight off the table, under the engine's own identity.
// So before the floor, a writer's raw insert of a grant naming themselves was
// a capability they gave themselves -- writeGrant is @serverOnly and grantSet
// checks the writer's authority in Go, and the raw literal consults neither.
func TestANonOwnerCannotGrantThemselvesACapabilityByWritingTheRow(t *testing.T) {
	eng := grantEngine(t)
	suffix := uniqueSuffix("co-self-grant")
	member := "member-" + suffix
	ctx := rankActorCtx(member, auth.RoleWriter)
	subject, _ := eng.subjectFor(ctx)

	if auth.CapableFor(ctx, subject, auth.VerbExecute, grantTestResource) {
		t.Fatal("the member already holds the probe capability; the test would prove nothing")
	}
	err := rawInsertFor(t, ctx, eng, "v1:rbac:grant", "self-grant-"+suffix, map[string]any{
		"subjectKind": auth.SubjectKindUser, "subjectId": member, "verb": auth.VerbExecute,
		"resourceType": grantTestResource, "effect": auth.GrantAllow, "active": true,
	})
	// The capability is asked FIRST, so the unfixed engine fails on the harm
	// itself rather than on the write that caused it.
	if auth.CapableFor(ctx, subject, auth.VerbExecute, grantTestResource) {
		t.Fatalf("the member now holds a capability they wrote for themselves (insert error: %v)", err)
	}
	if err == nil {
		t.Fatal("a writer inserted a grant naming themselves")
	}
}

// Every kind of creator the sweep found, under the context it really runs
// with. Each would be refused by a floor that admitted only a cluster owner's
// AccessContext, which is why each is here.
func TestEveryLegitimateClusterOwnerTierCreatorStillCreates(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("co-creators")

	t.Run("the campaigns worker's system actor, a synthetic cluster owner", func(t *testing.T) {
		// component/campaigns' systemActorContext: RoleOwner, unranked, synthetic.
		ctx := auth.ContextWithSystemActor(context.Background(), "campaigns-test")
		digest := strings.Repeat("c", 56) + fmt.Sprintf("%08x", len(suffix))
		if _, err := runSiteMutation(t, ctx, eng, "recordSuppression", map[string]any{
			"emailDigest": digest, "reason": "manual", "domain": "example.test",
		}); err != nil {
			t.Fatalf("recordSuppression under the campaigns worker's actor was refused: %v", err)
		}
		assertRowCount(t, db, "v1:campaigns:suppression", digest, 1)
	})

	t.Run("a @serverOnly creator under internal origin, for an admin it authorized", func(t *testing.T) {
		// grantSet's shape: the admin's own actor, internal origin stamped for
		// the one write after the Go authority check.
		admin := "admin-" + suffix
		ctx := auth.ContextWithInternalOrigin(rankActorCtx(admin, auth.RoleAdmin))
		id := auth.GrantRowID(auth.SubjectKindUser, "grantee-"+suffix, auth.VerbRead, "app:co-create-floor")
		if _, err := runSiteMutation(t, ctx, eng, "writeGrant", map[string]any{
			"grantId": id, "subjectKind": auth.SubjectKindUser, "subjectId": "grantee-" + suffix,
			"verb": auth.VerbRead, "resourceType": "app:co-create-floor", "effect": auth.GrantAllow,
		}); err != nil {
			t.Fatalf("writeGrant under an authorized admin's internal-origin context was refused: %v", err)
		}
		assertRowCount(t, db, "v1:rbac:grant", BareShortId(id), 1)
	})

	t.Run("a registered-tree automation, a reader-role system actor under internal origin", func(t *testing.T) {
		// component/automations' contextWithSystemActor for a non-maintenance
		// automation: system:automation:<name>, RoleReader -- NOT a cluster
		// owner -- with the internal origin a trusted body runs under.
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: "system:automation:creatorTest", Role: auth.RoleReader, Unranked: true, Synthetic: true,
		})
		ctx = auth.ContextWithInternalOrigin(auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "system:automation:creatorTest"}))
		id := "window-" + suffix
		if err := rawInsertFor(t, ctx, eng, "v1:campaigns:reputationWindow", id, map[string]any{
			"sendingIdentity": "news@example.test", "domain": "example.test",
			"windowStart": "2026-09-27", "nodeId": "node-" + suffix,
		}); err != nil {
			t.Fatalf("a trusted automation's create was refused: %v", err)
		}
		assertRowCount(t, db, "v1:campaigns:reputationWindow", id, 1)
	})

	t.Run("the seed materializer's context", func(t *testing.T) {
		ctx := auth.ContextWithInternalOrigin(systemActorContext(context.Background()))
		id := "policy-" + suffix
		if err := rawInsertFor(t, ctx, eng, "v1:work:feedbackPolicy", id, map[string]any{
			"validateAnswers": true, "reusableAfterSignatures": 2,
		}); err != nil {
			t.Fatalf("a seed-shaped create was refused: %v", err)
		}
		assertRowCount(t, db, "v1:work:feedbackPolicy", id, 1)
	})

	t.Run("the connector a mirror names, under its own actor and nothing else", func(t *testing.T) {
		// The sync runtime's applier: the connector actor, no internal origin.
		ctx := auth.ContextWithConnectorActor(context.Background(), "shopify")
		id := "mirror-" + suffix
		if err := rawInsertFor(t, ctx, eng, "v1:shopify:product", id, map[string]any{
			"storeId": "store-" + suffix, "gid": "gid://shopify/Product/" + suffix,
			"updatedAt": "2026-09-27T12:00:00Z", "syncedAt": "2026-09-27T12:00:00Z", "deleted": false,
		}); err != nil {
			t.Fatalf("the shopify connector's mirror create was refused: %v", err)
		}
		assertRowCount(t, db, "v1:shopify:product", id, 1)

		// And only where a concept names it: territory is MemQL's alone.
		other := "connector-territory-" + suffix
		if err := rawInsertFor(t, ctx, eng, "v1:commerce:territory", other, map[string]any{
			"storeId": "store-" + suffix, "name": "East",
		}); err == nil {
			t.Fatal("the shopify connector created a concept that does not name it")
		}
		assertRowCount(t, db, "v1:commerce:territory", other, 0)
	})

	t.Run("an audit writer: internal origin over the caller it records", func(t *testing.T) {
		// component/identity's EngineAuditSink and the integration audit
		// writers record a decision under the caller's own context, stamped
		// for the one createAuditEvent they compose.
		person := "person-" + suffix
		call := func(eventId string) string {
			return fmt.Sprintf(`mutation createAuditEvent(eventId: %s, occurredAt: "2026-09-27T12:00:00Z", `+
				`category: "authorization", action: "role_created", actorUserId: %s, outcome: "success")`,
				langparser.QuoteString(eventId), langparser.QuoteString(person))
		}
		stamped := auth.ContextWithInternalOrigin(rowAuthzCallerCtx(person))
		if _, err := eng.Execute(stamped, call("audit-stamped-"+suffix)); err != nil {
			t.Fatalf("an audit writer's stamped createAuditEvent was refused: %v", err)
		}
		assertRowCount(t, db, "v1:identity:auditEvent", "audit-stamped-"+suffix, 1)

		// The same call straight from the person is a trail entry they wrote
		// about themselves -- the forgery the floor closes.
		if _, err := eng.Execute(rowAuthzCallerCtx(person), call("audit-forged-"+suffix)); err == nil {
			t.Fatal("a writer's own createAuditEvent landed: any signed-in caller can still forge the trail")
		}
		assertRowCount(t, db, "v1:identity:auditEvent", "audit-forged-"+suffix, 0)
	})
}

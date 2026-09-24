package packages

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/edge"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// store_attach_db_test.go -- which store a storefront's deploy ATTACHES, run
// against a real engine (Connect Shopify, D5).
//
// store_binding_test.go drives the publish stage with a stub resolver, so it
// proves what the stage does with an answer. It cannot prove the answer: that
// the production resolver asks the SAME question the engine's binding guard
// asks at createSite. Since developers read every store (D3), reading a store
// and attaching one are different questions -- attaching also takes the store
// part (execute app:deployables/store). A resolver that answered only the read
// would hand createSite a store the caller may not attach, and the engine
// would refuse the whole deploy with capability_not_held rather than placing
// the draft. Only real rows and the real guard can tell those apart.
//
// The developer here is DENIED the store part by a grant rather than simply
// not holding it, so the case stays "may read, may not attach" whether or not
// the developer role holds the part by default.
//
// Postgres-gated like its neighbours; MEMQL_REQUIRE_DB=1 turns the skip into a
// failure.

// publishSkipped is the real enginePublisher with the one call that needs
// object storage answered in memory: EnsureSite (createSite under the caller)
// and BindSiteToStore run for real, which are the writes under test.
type publishSkipped struct {
	*enginePublisher
	published []string
}

func (p *publishSkipped) PublishBundle(_ context.Context, siteId string, _ edge.Bundle) (PublishResult, error) {
	p.published = append(p.published, siteId)
	return PublishResult{SiteId: siteId, BundleRef: "blob://sites/x/v1/", Version: "v1"}, nil
}

func boundStoreOf(t *testing.T, db *bun.DB, siteId string) string {
	t.Helper()
	var storeId string
	err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(payload->'binding'->>'storeId', '') FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`,
		siteId).Scan(&storeId)
	if err != nil {
		t.Fatalf("reading the binding of %s: %v", siteId, err)
	}
	return storeId
}

func TestAStorefrontDeployAttachesOnlyAStoreTheCallerMayAttach(t *testing.T) {
	eng, db := dbEngine(t)
	eng.InstallGrantResolution()
	t.Cleanup(func() {
		auth.SetGrantSource(nil)
		auth.SetMembershipSource(nil)
	})

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	storeId := "store-attach-" + suffix
	domain := "store-attach-" + suffix + ".myshopify.com"
	developer := "store-attach-dev-" + suffix
	owner := "store-attach-owner-" + suffix
	accountId := "store-attach-org-" + suffix
	grantId := auth.GrantRowID("user", developer, auth.VerbExecute, "app:deployables/store")
	var siteIds []string
	t.Cleanup(func() {
		for _, id := range append(siteIds, "v1:shopify:store:"+storeId, "v1:rbac:grant:"+grantId, "v1:accounts:account:"+accountId) {
			_, _ = db.NewDelete().TableExpr(`"MemoryNodes"`).Where("id = ?", id).Exec(context.Background())
		}
	})

	mustExecute(t, eng, tieSeederCtx(), fmt.Sprintf(
		`mutation createStore(storeId: %s, domain: %s, storefrontTokenRef: "SHOPIFY_STOREFRONT_TOKEN")`,
		langparser.QuoteString(storeId), langparser.QuoteString(domain)))
	// A new deployable names the organization it belongs to; both callers
	// below may deploy into it, so the store is the only thing that differs.
	mustExecute(t, eng, tieSeederCtx(), fmt.Sprintf(
		`mutation createClientAccount(accountId: %s, name: "Store attach organization")`,
		langparser.QuoteString(accountId)))
	mustExecute(t, eng, tieSeederCtx(), fmt.Sprintf(
		`mutation writeGrant(grantId: %s, subjectKind: "user", subjectId: %s, verb: "execute", resourceType: "app:deployables/store", effect: "deny", grantedBy: "store-attach-seeder")`,
		langparser.QuoteString(grantId), langparser.QuoteString(developer)))

	devCtx := tieActorCtx(developer, auth.RoleDeveloper)
	ownerCtx := tieActorCtx(owner, auth.RoleOwner)

	// THE PRECONDITION that makes this "may read, may not attach": the
	// developer reads the store (the D3 floor)...
	if rows := mustExecute(t, eng, devCtx, fmt.Sprintf(`query storeByDomain(domain: %s)`, langparser.QuoteString(domain))); len(rows) != 1 {
		t.Fatalf("the developer reads %d store rows, want 1 -- the case below is not the one it claims", len(rows))
	}

	deploy := func(ctx context.Context, who string) ([]DeployableOutcome, *publishSkipped, error) {
		s := &store{engine: eng, logger: discardLogger()}
		pub := &publishSkipped{enginePublisher: &enginePublisher{engine: eng, store: s, logger: discardLogger()}}
		d := &Deps{Store: s, Publisher: pub, Logger: discardLogger(), Stores: s.resolveStore}
		req := DeployRequest{
			PackageId: "v1:platform:package:store-attach-" + who + "-" + suffix,
			Placements: map[string]Placement{"storefront": {
				Hostname:  "store-attach-" + who + "-" + suffix + "." + restampSiteDomain(),
				AccountId: accountId,
			}},
		}
		outcomes, err := d.publish(ctx, req, map[string]any{}, storefrontOnlyReport(domain),
			map[string]edge.Bundle{"storefront": {}})
		for _, o := range outcomes {
			if o.SiteId != "" {
				siteIds = append(siteIds, o.SiteId)
			}
		}
		return outcomes, pub, err
	}

	// ...AND IS NOT LET ATTACH IT, so the deploy places an unattached draft
	// with the note instead of being refused by the binding guard.
	outcomes, pub, err := deploy(devCtx, "dev")
	if err != nil {
		t.Fatalf("a developer who may read but not attach the store was refused the deploy: %v", err)
	}
	if len(pub.published) != 1 {
		t.Fatalf("the draft's files must be put in place: %v", pub.published)
	}
	assertUnattachedNote(t, outcomes, domain)
	devSiteId := outcomes[0].SiteId
	if got := boundStoreOf(t, db, devSiteId); got != "" {
		t.Fatalf("the developer's draft is bound to %q, want no binding", got)
	}

	// THE REACHABLE POSITIVE: an owner's deploy of the same manifest binds
	// the store at create, through the same resolver and the same guard.
	outcomes, _, err = deploy(ownerCtx, "owner")
	if err != nil {
		t.Fatalf("an owner's deploy of a storefront naming a readable store failed: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].Refusal != nil {
		t.Fatalf("an owner's bound storefront carries a note: %+v", outcomes)
	}
	if got := memqlengine.BareShortId(boundStoreOf(t, db, outcomes[0].SiteId)); got != storeId {
		t.Fatalf("the owner's storefront is bound to %q, want %q", got, storeId)
	}
	if !strings.HasPrefix(outcomes[0].SiteId, "v1:platform:site:") {
		t.Errorf("unexpected site id %q", outcomes[0].SiteId)
	}

	// A REDEPLOY THAT CHANGES NOTHING SAYS NOTHING. Once somebody who may
	// attach the store has bound the developer's draft to the store its
	// manifest names, the developer's redeploy leaves that binding as it is --
	// so it must neither be refused nor carry a note that the store "could not
	// be resolved": the developer reads it, and it is the one already bound.
	if err := (&enginePublisher{engine: eng, logger: discardLogger()}).BindSiteToStore(tieSeederCtx(), devSiteId, storeId); err != nil {
		t.Fatalf("binding the developer's draft as the seeder: %v", err)
	}
	outcomes, _, err = deploy(devCtx, "dev")
	if err != nil {
		t.Fatalf("the developer's redeploy of a correctly bound storefront was refused: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].SiteId != devSiteId {
		t.Fatalf("the redeploy did not find its site %s: %+v", devSiteId, outcomes)
	}
	if outcomes[0].Refusal != nil {
		t.Fatalf("the redeploy of a storefront already bound to its manifest's store carries a note: %+v", outcomes[0].Refusal)
	}
	if got := memqlengine.BareShortId(boundStoreOf(t, db, devSiteId)); got != storeId {
		t.Fatalf("the redeploy moved the binding to %q, want %q", got, storeId)
	}
}

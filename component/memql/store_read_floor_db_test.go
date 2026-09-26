package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// DEVELOPERS READ v1:shopify:store, AND NOTHING ELSE CHANGES (Connect Shopify
// design, section 7, decision D3).
//
// This is the test `tierDecidesTheRead` asks for when it takes the four store
// reads: the one that fails if the reasoning is wrong. The concept is
// `@rowAuthz(clusterOwner, rankFloor="developer")` and the four reads carry no
// caller-scope conjunct, so the tier is the whole answer. Three halves, because
// each one alone passes against a wrong rule:
//
//	developer and owner get the row    the widening D3 decides. With the old
//	                                   `actor.isClusterOwner` conjunct still in
//	                                   the filter, a developer read nothing.
//	admin and writer get ZERO ROWS     the floor is a floor, and the answer
//	and no error                       below it is an empty result rather than
//	                                   a refusal: canReadStore, resolveStore and
//	                                   the rankless auto-deploy writer all read
//	                                   "not readable" off zero rows, which is
//	                                   why this is a tier and not @requiresRank.
//	a developer's writes to a store    rankFloor widens the READ. The write
//	that exists are refused            guard still judges the stored row under
//	                                   plain clusterOwner.
//
// Postgres-gated: skips when no database is reachable, and MEMQL_REQUIRE_DB=1
// turns that skip into a failure.

func TestStoreReadsAnswerForDeveloperAndAnswerNothingBelowTheFloor(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("storefloor")
	seedStoreWithDevelopmentStore(t, eng, suffix)

	// A refusal and an empty result are DIFFERENT ANSWERS, and the distinction
	// is the subject here -- so -1 marks the refusal rather than folding it
	// into zero.
	rowsOf := func(ctx context.Context, q string) (int, error) {
		res, err := eng.Execute(ctx, q)
		if err != nil {
			return -1, err
		}
		return len(res.Bundle.GetNodes()), nil
	}

	storeId := langparser.QuoteString(storeIdFor(suffix))
	reads := []struct{ name, query string }{
		{"storeById", fmt.Sprintf(`query storeById(storeId: %s)`, storeId)},
		{"storeByDomain", fmt.Sprintf(`query storeByDomain(domain: %s)`, langparser.QuoteString(suffix+".myshopify.com"))},
		{"stores", `query stores()`},
		{"developmentStoresFor", fmt.Sprintf(`query developmentStoresFor(storeId: %s)`, storeId)},
	}
	callers := []struct {
		role  auth.Role
		reads bool
	}{
		{auth.RoleOwner, true},
		{auth.RoleDeveloper, true},
		{auth.RoleAdmin, false},
		{auth.RoleWriter, false},
	}

	for _, c := range callers {
		user := fmt.Sprintf("%s-%s", c.role, suffix)
		seedPrincipal(t, eng, user, c.role)
		ctx := rankActorCtx(user, c.role)
		for _, r := range reads {
			t.Run(string(c.role)+"/"+r.name, func(t *testing.T) {
				got, err := rowsOf(ctx, r.query)
				if err != nil {
					t.Fatalf("%s REFUSED a %s: %v. Below the floor the answer must be zero rows, "+
						"not an error -- canReadStore, resolveStore and the auto-deploy feed read "+
						"\"not readable\" off an empty result.", r.name, c.role, err)
				}
				if c.reads && got == 0 {
					t.Fatalf("%s answered zero rows to a %s. D3: developers and owners read "+
						"v1:shopify:store, and the tier's rankFloor=\"developer\" does nothing while "+
						"a filter still ANDs actor.isClusterOwner == true.", r.name, c.role)
				}
				if !c.reads && got > 0 {
					t.Fatalf("%s answered %d rows to a %s. The floor is developer (300); admin "+
						"(200) and below read nothing.", r.name, got, c.role)
				}
			})
		}
	}
}

// The WRITE half: a developer can now read the row, and still cannot change it.
//
// The owner control is what makes the refusal evidence. Without it, a mutation
// that failed for every caller -- a broken fixture, a renamed argument -- would
// pass as "refused".
func TestADeveloperCannotChangeAStoreThatExists(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("storewrite")
	seedStoreWithDevelopmentStore(t, eng, suffix)

	dev := "developer-" + suffix
	owner := "owner-" + suffix
	seedPrincipal(t, eng, dev, auth.RoleDeveloper)
	seedPrincipal(t, eng, owner, auth.RoleOwner)

	writes := []struct {
		name string
		args map[string]any
	}{
		{"updateStore", map[string]any{"storeId": storeIdFor(suffix), "name": "renamed by a developer"}},
		{"setStoreStatus", map[string]any{"storeId": storeIdFor(suffix), "status": "paused"}},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			before := storeVersions(t, db, storeIdFor(suffix))
			_, err := runSiteMutation(t, rankActorCtx(dev, auth.RoleDeveloper), eng, w.name, w.args)
			if err == nil {
				t.Fatalf("a developer's %s on an existing store was ALLOWED. rankFloor widens the "+
					"read only; changing a store that exists stays owner-only (D3).", w.name)
			}
			t.Logf("refused as expected: %v", err)
			if after := storeVersions(t, db, storeIdFor(suffix)); after != before {
				t.Fatalf("the refused %s still wrote: %d stored versions before, %d after", w.name, before, after)
			}

			if _, err := runSiteMutation(t, rankActorCtx(owner, auth.RoleOwner), eng, w.name, w.args); err != nil {
				t.Fatalf("the owner control failed: %s refused a cluster owner (%v), so the "+
					"developer refusal above measures nothing", w.name, err)
			}
		})
	}
}

// seedStoreWithDevelopmentStore writes a live store and one development store
// attached to it, through createStore under internal origin -- the way the
// first-boot seed and Connect write them -- so developmentStoresFor has a row
// to answer with.
func seedStoreWithDevelopmentStore(t *testing.T, eng *MemQLEngine, suffix string) {
	t.Helper()
	ctx := auth.ContextWithInternalOrigin(rankActorCtx("store-seeder-"+suffix, auth.RoleOwner))
	if err := createStoreFor(t, ctx, eng, suffix); err != nil {
		t.Fatalf("seed the live store: %v", err)
	}
	if _, err := runSiteMutation(t, ctx, eng, "createStore", map[string]any{
		"storeId":              storeIdFor(suffix) + "-dev",
		"domain":               suffix + "-dev.myshopify.com",
		"isDevelopment":        true,
		"developmentOfStoreId": storeIdFor(suffix),
	}); err != nil {
		t.Fatalf("seed the development store: %v", err)
	}
}

// storeVersions counts the stored versions of one store straight off the
// table, under no actor, so the number is what was written rather than what a
// tier shows.
func storeVersions(t *testing.T, db *bun.DB, bareId string) int {
	t.Helper()
	n, err := db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).
		Where("concept = ?", conceptShopifyStoreForTest).
		Where("id = ?", conceptShopifyStoreForTest+":"+bareId).
		Count(context.Background())
	if err != nil {
		t.Fatalf("count store versions %s: %v", bareId, err)
	}
	return n
}

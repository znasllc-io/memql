package memql

import (
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// MayWriteRow is the write guard's question asked without writing (Connect
// Shopify 12.1). The guard itself is covered beside it; what this measures is
// that the ANSWER is the guard's for every branch a caller can reach it by --
// the owner, a cluster owner, the account grant (which only widens when the
// per-request scope is installed), and nobody else.
//
// Postgres-gated like its neighbours; MEMQL_REQUIRE_DB=1 turns the skip into a
// failure.
func TestMayWriteRowAnswersTheWriteGuardsQuestion(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("maywrite")

	account := "acct-" + suffix
	builder := "builder-" + suffix
	member := "member-" + suffix
	stranger := "stranger-" + suffix
	for _, u := range []string{builder, member, stranger} {
		seedPrincipal(t, eng, u, auth.RoleWriter)
	}
	seedGroup(t, eng, "g-"+account, "Acme", "account", account)
	seedMembership(t, eng, "g-"+account, member, "active")
	tied := "site-tied-" + suffix
	untied := "site-untied-" + suffix
	seedSiteFor(t, eng, tied, builder, account)
	seedSiteFor(t, eng, untied, builder, "")

	for _, tc := range []struct {
		name   string
		userId string
		role   auth.Role
		siteId string
		want   bool
	}{
		{"the owner writes their own site", builder, auth.RoleWriter, untied, true},
		{"a cluster owner writes anybody's site", "owner-" + suffix, auth.RoleOwner, untied, true},
		{"an account member writes a tied site", member, auth.RoleWriter, tied, true},
		{"an account member does not write an untied site", member, auth.RoleWriter, untied, false},
		{"a stranger writes nothing", stranger, auth.RoleWriter, tied, false},
		{"a row that does not exist is not writable", builder, auth.RoleWriter, "site-missing-" + suffix, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := eng.MayWriteRow(rankActorCtx(tc.userId, tc.role), conceptPlatformSite, tc.siteId)
			if err != nil {
				t.Fatalf("MayWriteRow: %v", err)
			}
			if got != tc.want {
				t.Fatalf("MayWriteRow = %v, want %v", got, tc.want)
			}
		})
	}
}

// MayReadConcept is the read tier's answer with no row in hand (Connect Shopify
// 12.6 step 8, on a first Connect, when there is no store row to read back). It
// is only worth anything if it is the answer the read gives once a row exists,
// so each caller is asked both, over a real store row: the cluster-owner tier's
// read floor admits the developer and the owner and nobody below.
func TestMayReadConceptIsTheReadTiersAnswerWithoutARow(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("mayreadconcept")
	seedStoreWithDevelopmentStore(t, eng, suffix)
	read := fmt.Sprintf(`query storeById(storeId: %s)`, langparser.QuoteString(storeIdFor(suffix)))

	for _, tc := range []struct {
		role auth.Role
		want bool
	}{
		{auth.RoleOwner, true},
		{auth.RoleDeveloper, true},
		{auth.RoleAdmin, false},
		{auth.RoleWriter, false},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			user := fmt.Sprintf("mrc-%s-%s", tc.role, suffix)
			seedPrincipal(t, eng, user, tc.role)
			ctx := rankActorCtx(user, tc.role)
			got, err := eng.MayReadConcept(ctx, "v1:shopify:store")
			if err != nil {
				t.Fatalf("MayReadConcept: %v", err)
			}
			res, err := eng.Execute(ctx, read)
			if err != nil {
				t.Fatalf("the real read: %v", err)
			}
			if reads := len(MaterializeRows(res)) > 0; got != tc.want || reads != tc.want {
				t.Fatalf("MayReadConcept = %v and the read returned a row = %v, want both %v", got, reads, tc.want)
			}
		})
	}

	// A tier that compares a row has no answer without one.
	if _, err := eng.MayReadConcept(rankActorCtx("mrc-owner-"+suffix, auth.RoleOwner), conceptPlatformSite); err == nil {
		t.Error("the owned tier answered without a row")
	}
}

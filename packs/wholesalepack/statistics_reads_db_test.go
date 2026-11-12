package wholesalepack_test

// statistics_reads_db_test.go -- the two reads the store statistics walk asks
// of this pack: which buyers a store has granted, and which stores have
// wholesale set up. Real engine, real Postgres.
//
// Both read every organization's rows, so each carries two locks and each is
// shown to hold alone: @serverOnly refuses a client whoever it is, and
// `actor.isClusterOwner == true` answers zero rows to an internal-origin
// caller who is not a cluster owner. That caller is the MERCHANT who owns the
// rows -- the one row authorization admits (owner="ownerUserId") -- so the
// zero rows are the predicate's and not the tier's.

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// statsClusterOwner is a cluster owner with internal origin: the walk's own
// standing.
func statsClusterOwner() context.Context {
	return auth.ContextWithInternalOrigin(statsOwnerCtx())
}

// statsOwnerCtx is a cluster owner with no origin stamped.
func statsOwnerCtx() context.Context {
	claims := map[string]any{"sub": "dbtest-wholesale-stats-owner", "role": "owner"}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "dbtest-wholesale-stats-owner", Role: auth.RoleOwner})
}

// storeIDList renders a []string argument.
func storeIDList(ids ...string) string {
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		quoted = append(quoted, wsQuote(id))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func grantedCall(ids ...string) string {
	return `query wholesaleStatisticsGranted(storeIds: ` + storeIDList(ids...) + `)`
}

func settingsCall(ids ...string) string {
	return `query wholesaleStatisticsSettings(storeIds: ` + storeIDList(ids...) + `)`
}

// writeEntitlement writes one entitlement through the pack's own mutation,
// as the merchant, against an application written the same way.
func writeEntitlement(t *testing.T, eng *memql.MemQLEngine, merchant, storeID, name, email, state string) {
	t.Helper()
	ctx := asSiteOwner(merchant)
	appID := "app-stats-" + name + "-" + merchant
	wsExec(t, eng, ctx, `createApplicationRow(applicationId: `+wsQuote(appID)+`, storeId: `+wsQuote(storeID)+
		`, companyName: "Probe Co", applicantName: "Probe", applicantEmail: `+wsQuote(email)+`)`)
	wsExec(t, eng, ctx, `writeEntitlementRow(entitlementId: `+wsQuote("ent-stats-"+name+"-"+merchant)+
		`, applicationId: `+wsQuote(appID)+`, storeId: `+wsQuote(storeID)+`, buyerEmail: `+wsQuote(email)+
		`, adapter: "customerTag", state: `+wsQuote(state)+`)`)
}

func writeSettings(t *testing.T, eng *memql.MemQLEngine, merchant, storeID string) {
	t.Helper()
	wsExec(t, eng, asSiteOwner(merchant), `setWholesaleSettingsRow(settingsId: `+wsQuote("settings-stats-"+storeID)+
		`, storeId: `+wsQuote(storeID)+`, applicationsOpen: true, entitlementAdapter: "customerTag")`)
}

func buyerEmails(rows []map[string]any) []string {
	out := []string{}
	for _, row := range rows {
		if email, _ := row["buyerEmail"].(string); email != "" {
			out = append(out, email)
		}
	}
	slices.Sort(out)
	return out
}

// The pack's rows hold a store id in either form, and the engine does not join
// the two, so the walk passes both: asked both, both are found; asked one,
// only the row stored in that form.
func TestTheGrantedReadFindsABuyerUnderEitherFormOfTheStoreId(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	canonical := "v1:shopify:store:" + store
	writeEntitlement(t, eng, merchant, store, "bare", "bare@example.com", "granted")
	writeEntitlement(t, eng, merchant, canonical, "canonical", "canonical@example.com", "granted")

	both := buyerEmails(wsRows(t, eng, statsClusterOwner(), grantedCall(store, canonical)))
	if want := []string{"bare@example.com", "canonical@example.com"}; !slices.Equal(both, want) {
		t.Errorf("asked both forms of the store id, the granted read answers %v; want %v", both, want)
	}
	bare := buyerEmails(wsRows(t, eng, statsClusterOwner(), grantedCall(store)))
	if want := []string{"bare@example.com"}; !slices.Equal(bare, want) {
		t.Errorf("asked the bare store id, the granted read answers %v; want %v", bare, want)
	}
	prefixed := buyerEmails(wsRows(t, eng, statsClusterOwner(), grantedCall(canonical)))
	if want := []string{"canonical@example.com"}; !slices.Equal(prefixed, want) {
		t.Errorf("asked the canonical store id, the granted read answers %v; want %v", prefixed, want)
	}
}

func TestOnlyGrantedEntitlementsAreAnswered(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	for _, state := range []string{"pending", "granted", "failed", "revoked"} {
		writeEntitlement(t, eng, merchant, store, state, state+"@example.com", state)
	}
	rows := wsRows(t, eng, statsClusterOwner(), grantedCall(store))
	if got, want := buyerEmails(rows), []string{"granted@example.com"}; !slices.Equal(got, want) {
		t.Errorf("the granted read answers %v; want only the granted buyer %v", got, want)
	}
	for _, row := range rows {
		if row["state"] != "granted" {
			t.Errorf("the granted read answered a row in state %v", row["state"])
		}
	}
}

func TestTheSettingsReadSaysWhichStoresAreSetUp(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, live, dev := wholesaleScope()
	writeSettings(t, eng, merchant, live)

	rows := wsRows(t, eng, statsClusterOwner(), settingsCall(live, dev))
	var stores []string
	for _, row := range rows {
		stores = append(stores, row["storeId"].(string))
	}
	if !slices.Equal(stores, []string{live}) {
		t.Errorf("asked about a store with settings and one without, the settings read answers %v; want only %s", stores, live)
	}
}

func TestThePacksStatisticsReadsAreForAClusterOwnerWithInternalOrigin(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	writeEntitlement(t, eng, merchant, store, "locks", "locks@example.com", "granted")
	writeSettings(t, eng, merchant, store)

	for name, call := range map[string]string{
		"wholesaleStatisticsGranted":  grantedCall(store),
		"wholesaleStatisticsSettings": settingsCall(store),
	} {
		t.Run(name, func(t *testing.T) {
			// The reachable positive.
			if rows := wsRows(t, eng, statsClusterOwner(), call); len(rows) != 1 {
				t.Fatalf("a cluster owner with internal origin read %d rows of %s; want the one seeded, "+
					"so nothing below measures a lock", len(rows), name)
			}
			// The merchant who owns the rows, with internal origin: row
			// authorization admits them, the predicate does not.
			if rows := wsRows(t, eng, auth.ContextWithInternalOrigin(asSiteOwner(merchant)), call); len(rows) != 0 {
				t.Errorf("%s answered %d rows to an internal-origin caller who is not a cluster owner; want 0", name, len(rows))
			}
			// A cluster owner from a client is refused as server-only.
			_, err := eng.Execute(auth.ContextWithClientOrigin(statsOwnerCtx()), call)
			if err == nil || !strings.Contains(err.Error(), "server-only") {
				t.Errorf("%s from a client, as a cluster owner: err = %v; want it refused as server-only", name, err)
			}
		})
	}
}

// O4 (dsl/pack_enablement.go): a disabled pack keeps its concepts and loads
// none of its queries, so the statistics reads are unknown calls on a node
// where the pack is off. The walk asks the pack's state before it asks them.
func TestWithThePackDisabledItsStatisticsQueriesAreNotLoaded(t *testing.T) {
	_, store, _ := wholesaleScope()
	enabled := liveWholesaleEngine(t)
	for _, call := range []string{grantedCall(store), settingsCall(store)} {
		if _, err := enabled.Execute(statsClusterOwner(), call); err != nil {
			t.Fatalf("with the pack enabled, %s: %v -- the disabled reading below would prove nothing", call, err)
		}
	}
	disabled := liveWholesaleEngine(t, wholesalepack.Domain)
	for _, call := range []string{grantedCall(store), settingsCall(store)} {
		if _, err := disabled.Execute(statsClusterOwner(), call); err == nil {
			t.Errorf("with the pack disabled, %s still resolved; a disabled pack loads none of its queries", call)
		}
	}
}

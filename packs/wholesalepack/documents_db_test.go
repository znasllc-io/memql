package wholesalepack_test

// documents_db_test.go -- A STORE'S DOCUMENTS against a real database and a
// real engine: the store's person publishes and reads them through the
// organization gate, a person without the part and a holder in another
// organization are refused, and a version is found whichever spelling of the
// store id it was written under.
//
// Postgres-gated like every *_db_test.go.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// documentCalls is the two document builtins on a site and a store.
func documentCalls(site, store string) map[string]string {
	return map[string]string{
		"wholesalePublishDocument": `builtin wholesalePublishDocument(siteId: ` + quote(site) + `, storeId: ` + quote(store) +
			`, kind: "terms", label: "Wholesale terms", body: "Net 30.")`,
		"wholesaleDocuments": `builtin wholesaleDocuments(siteId: ` + quote(site) + `, storeId: ` + quote(store) + `)`,
	}
}

// kindOf is one kind's entry of a wholesaleDocuments reply.
func kindOf(t *testing.T, reply map[string]any, kind string) map[string]any {
	t.Helper()
	for _, k := range listRows(reply["kinds"]) {
		if k["kind"] == kind {
			return k
		}
	}
	t.Fatalf("the reply names no %s: %+v", kind, reply)
	return nil
}

// THE STORE'S PERSON PUBLISHES A VERSION AND READS IT BACK: the row is the
// merchant's, of the store's organization, published by the person, and its
// people read it.
func TestTheStoresPersonPublishesAndReadsItsDocuments(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "docs-holder")
	got := answerOf(t, eng, signedIn(s.staff), `builtin wholesalePublishDocument(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
		`, kind: "map", label: "  MAP policy 2026  ", body: "hello")`)
	versionID := asString(got["documentVersionId"])
	if versionID == "" || got["sha256"] != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("the publish answered %+v", got)
	}
	rows := wsRows(t, eng, signedIn(s.merchant), `query documentVersionsForStore(storeId: `+quote(s.store)+`, kind: "map")`)
	if len(rows) != 1 || rows[0]["id"] != versionID || rows[0]["ownerUserId"] != s.owner ||
		bareID(asString(rows[0]["accountId"])) != s.account || rows[0]["publishedBy"] != userRef(s.staff) ||
		rows[0]["label"] != "MAP policy 2026" || rows[0]["body"] != "hello" {
		t.Fatalf("the merchant reads %+v, want the one version, theirs, of %s, published by %s", rows, s.account, s.staff)
	}
	reply := answerOf(t, eng, signedIn(s.staff), documentCalls(s.site, s.store)["wholesaleDocuments"])
	current, _ := kindOf(t, reply, wholesalepack.DocumentMap)["current"].(map[string]any)
	if current["id"] != versionID || current["body"] != "hello" || current["publishedByName"] != s.staff {
		t.Fatalf("the current MAP policy reads %+v", current)
	}
	if kindOf(t, reply, wholesalepack.DocumentTerms)["current"] != nil {
		t.Fatalf("terms nobody published read as %+v", kindOf(t, reply, wholesalepack.DocumentTerms))
	}
	one := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleDocuments(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
		`, versionId: `+quote(versionID)+`)`)
	if v, _ := one["version"].(map[string]any); v["id"] != versionID || v["body"] != "hello" {
		t.Fatalf("the version by id reads %+v", one)
	}
	// THE LARGEST BODY reaches the row whole.
	largest := strings.Repeat("Frame 410.00\n", 204800/13) + strings.Repeat("x", 204800%13)
	answerOf(t, eng, signedIn(s.staff), `builtin wholesalePublishDocument(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+
		`, kind: "priceSheet", label: "Spring list", body: `+quote(largest)+`)`)
	sheet := wsRows(t, eng, signedIn(s.staff), `query documentVersionsForStore(storeId: `+quote(s.store)+`, kind: "priceSheet")`)
	if len(sheet) != 1 || sheet[0]["body"] != largest {
		t.Fatalf("a body of %d bytes was not stored whole (%d rows)", len(largest), len(sheet))
	}
}

// A PERSON OF THE SAME ORGANIZATION WITHOUT THE PART is refused by both
// document builtins: the merchant here is one, in a group with no part.
func TestAPersonOfTheOrganizationWithoutThePartIsRefusedTheDocuments(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "docs-nopart")
	for name, call := range documentCalls(s.site, s.store) {
		if _, err := eng.Execute(signedIn(s.merchant), call); err == nil || !strings.Contains(err.Error(), memql.CodeCapabilityNotHeld) {
			t.Errorf("%s by a person of the organization without the part: %v, want %s", name, err, memql.CodeCapabilityNotHeld)
		}
	}
	if rows := wsRows(t, eng, signedIn(s.merchant), `query documentVersionsForStore(storeId: `+quote(s.store)+`, kind: "terms")`); len(rows) != 0 {
		t.Fatalf("a refused publish wrote %+v", rows)
	}
}

// A HOLDER OF THE PART IN ANOTHER ORGANIZATION is refused by both, on the
// store's own site and through a site of their organization bound to the store.
func TestAHolderInAnotherOrganizationIsRefusedTheDocuments(t *testing.T) {
	eng := liveWholesaleEngine(t)
	a, b := newAccessStore(t, eng, "docs-a"), newAccessStore(t, eng, "docs-b")
	for name, call := range documentCalls(a.site, a.store) {
		if _, err := eng.Execute(signedIn(b.staff), call); err == nil || !strings.Contains(err.Error(), memql.CodeCapabilityNotHeld) {
			t.Errorf("%s by B's person on A's site: %v, want %s", name, err, memql.CodeCapabilityNotHeld)
		}
	}
	bound := "site-docs-b-on-a-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedSiteIn(t, eng, bound, b.merchant, a.store, b.account)
	for _, store := range []string{a.store, "v1:shopify:store:" + a.store} {
		for name, call := range documentCalls(bound, store) {
			if _, err := eng.Execute(signedIn(b.staff), call); err == nil || !strings.Contains(err.Error(), "another organization") {
				t.Errorf("store named %q: %s by B's person through B's site on A's store: %v", store, name, err)
			}
		}
	}
	if rows := wsRows(t, eng, signedIn(a.staff), `query documentVersionsForStore(storeId: `+quote(a.store)+`, kind: "terms")`); len(rows) != 0 {
		t.Fatalf("a refused publish wrote %+v", rows)
	}
}

// A VERSION WRITTEN UNDER THE OTHER SPELLING OF THE STORE ID is found: one
// under the canonical form by a reader naming the bare id, and the reverse.
func TestAVersionUnderTheOtherSpellingOfTheStoreIdIsFound(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "docs-spelling")
	wsExec(t, eng, seedCtx(), `appendWholesaleDocumentVersion(ownerUserId: `+quote(s.owner)+`, storeId: `+
		quote("v1:shopify:store:"+s.store)+`, accountId: `+quote(s.account)+`, kind: "priceSheet", label: "Canonical list", `+
		`body: "prices", sha256: "x", publishedBy: `+quote(userRef(s.staff))+`, publishedAt: `+
		quote(time.Now().UTC().Format(time.RFC3339Nano))+`)`)
	wsExec(t, eng, seedCtx(), `appendWholesaleDocumentVersion(ownerUserId: `+quote(s.owner)+`, storeId: `+quote(s.store)+
		`, accountId: `+quote(s.account)+`, kind: "terms", label: "Bare terms", body: "terms", sha256: "y", publishedBy: `+
		quote(userRef(s.staff))+`, publishedAt: `+quote(time.Now().UTC().Format(time.RFC3339Nano))+`)`)
	for _, asked := range []string{s.store, "v1:shopify:store:" + s.store} {
		current, err := wholesalepack.CurrentDocumentsForTest(seedCtx(), eng, asked)
		if err != nil {
			t.Fatalf("currentDocuments(%q): %v", asked, err)
		}
		if current[wholesalepack.DocumentPriceSheet]["label"] != "Canonical list" || current[wholesalepack.DocumentTerms]["label"] != "Bare terms" {
			t.Fatalf("currentDocuments(%q) = %+v, want both versions whichever spelling they hold", asked, current)
		}
		reply := answerOf(t, eng, signedIn(s.staff), documentCalls(s.site, asked)["wholesaleDocuments"])
		if cur, _ := kindOf(t, reply, wholesalepack.DocumentPriceSheet)["current"].(map[string]any); cur["label"] != "Canonical list" {
			t.Fatalf("asked as %q, the price sheet reads %+v", asked, cur)
		}
	}
}

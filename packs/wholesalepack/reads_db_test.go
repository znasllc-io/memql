package wholesalepack_test

// reads_db_test.go -- THE READS THE WHOLESALE SECTION OPENS, against a real
// database and a real engine: the organization gate and the site check admit
// the store's own people and its operators, and nobody of another organization
// learns anything of the store through any of them.
//
// Postgres-gated like every *_db_test.go.

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// storeWide is the four reads keyed by a site and a store.
func storeWide(site, store string) map[string]string {
	args := `(siteId: ` + quote(site) + `, storeId: ` + quote(store) + `)`
	return map[string]string{
		"wholesaleList":        `builtin wholesaleList` + strings.Replace(args, ")", `, stage: "all")`, 1),
		"wholesaleDecisionLog": `builtin wholesaleDecisionLog` + args,
		"wholesaleBuyers":      `builtin wholesaleBuyers` + args,
		"wholesaleExportStore": `builtin wholesaleExportStore` + args,
	}
}

func listRows(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// A SITE-TARGETED READ IS REFUSED FOR A SITE NOT BOUND TO THE STORE, and for a
// site of another organization bound to it: A's site with B's store.
func TestAStoreWideReadRefusesASiteNotBoundToTheStore(t *testing.T) {
	eng := liveWholesaleEngine(t)
	a, b := newAccessStore(t, eng, "wide-a"), newAccessStore(t, eng, "wide-b")
	if err := b.approve(eng, signedIn(b.staff)); err != nil {
		t.Fatalf("approve B's application: %v", err)
	}
	// A site of A's organization bound to B's store: it serves the store, and
	// belongs to another organization than the store's settings.
	bound := "site-wide-a-on-b-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedSiteIn(t, eng, bound, a.merchant, b.store, a.account)
	for _, site := range []string{a.site, bound} {
		for name, call := range storeWide(site, b.store) {
			if _, err := eng.Execute(signedIn(a.staff), call); err == nil {
				t.Errorf("%s through %s on B's store answered A's person", name, site)
			}
		}
	}
	// B'S OWN SITE: the organization gate refuses A's person outright, and
	// opening B's application too.
	for name, call := range storeWide(b.site, b.store) {
		if _, err := eng.Execute(signedIn(a.staff), call); err == nil || !strings.Contains(err.Error(), memql.CodeCapabilityNotHeld) {
			t.Errorf("%s on B's site by A's person: %v, want %s", name, err, memql.CodeCapabilityNotHeld)
		}
	}
	if _, err := eng.Execute(signedIn(a.staff), `builtin wholesaleGet(applicationId: `+quote(b.appID)+`)`); err == nil ||
		!strings.Contains(err.Error(), memql.CodeCapabilityNotHeld) {
		t.Errorf("wholesaleGet of B's application by A's person: %v, want %s", err, memql.CodeCapabilityNotHeld)
	}
	// A'S OWN STORE carries nothing of B's: one row, one count, no decision line,
	// no buyer, and nothing in the export.
	own := storeWide(a.site, a.store)
	list := answerOf(t, eng, signedIn(a.staff), own["wholesaleList"])
	counts, _ := list["counts"].(map[string]any)
	total := 0.0
	for _, n := range counts {
		f, _ := n.(float64)
		total += f
	}
	if rows := listRows(list["rows"]); len(rows) != 1 || bareID(asString(rows[0]["applicationId"])) != a.appID || total != 1 {
		t.Fatalf("A's list is %+v with counts %v, want A's one application", rows, counts)
	}
	if rows := listRows(answerOf(t, eng, signedIn(a.staff), own["wholesaleDecisionLog"])["rows"]); len(rows) != 0 {
		t.Fatalf("A's decision log carries %+v", rows)
	}
	if rows := listRows(answerOf(t, eng, signedIn(a.staff), own["wholesaleBuyers"])["rows"]); len(rows) != 0 {
		t.Fatalf("A's buyers carry %+v", rows)
	}
	doc := asString(answerOf(t, eng, signedIn(a.staff), own["wholesaleExportStore"])["json"])
	if strings.Contains(doc, b.appID) || strings.Contains(doc, b.store) || !strings.Contains(doc, a.appID) {
		t.Fatalf("A's export carries B's store or lacks A's application: %s", doc)
	}
}

// A PERSON HOLDING THE PART LISTS AND OPENS THEIR ORGANIZATION'S APPLICATIONS:
// the list, the application with its client's own fields and its decisions,
// the decision log and the buyers, all naming the application in one form.
func TestAPersonHoldingThePartListsAndOpensTheirOrganizationsApplications(t *testing.T) {
	memqldsl.RegisterTree("northwind", os.DirFS(filepath.Join("testdata", "clients", "northwind")))
	t.Cleanup(func() { memqldsl.UnregisterTree("northwind") })
	eng := liveWholesaleEngine(t)
	s := newAccessStoreWith(t, eng, "opens", url.Values{"ein": {"12-3456789"}})
	decider := "user-robin-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedPersonIn(t, eng, decider, "Robin Staff", "writer", true, "acct-"+s.account, s.account, "added")
	ctx := signedIn(decider)
	wide := storeWide(s.site, s.store)

	list := answerOf(t, eng, ctx, `builtin wholesaleList(siteId: `+quote(s.site)+`, storeId: `+quote(s.store)+`)`)
	rows := listRows(list["rows"])
	if len(rows) != 1 || bareID(asString(rows[0]["applicationId"])) != s.appID || rows[0]["stage"] != wholesalepack.StageReadyToReview ||
		rows[0]["primary"] != "12-3456789" || rows[0]["companyName"] != "Trading opens" {
		t.Fatalf("the list is %+v", rows)
	}
	appID := asString(rows[0]["applicationId"])
	t.Logf("the list names the application %q", appID)

	if _, err := eng.Execute(ctx, `builtin wholesaleRecordDecision(applicationId: `+quote(s.appID)+`, transition: "approve", note: "known")`); err != nil {
		t.Fatalf("approve: %v", err)
	}
	for _, named := range []string{s.appID, "v1:wholesale:application:" + s.appID} {
		got := answerOf(t, eng, ctx, `builtin wholesaleGet(applicationId: `+quote(named)+`)`)
		app, _ := got["application"].(map[string]any)
		details, _ := got["details"].(map[string]any)
		decisions := listRows(got["decisions"])
		if asString(app["id"]) != appID || got["state"] != wholesalepack.StateApproved || got["operator"] != false {
			t.Fatalf("named %q: the application is %+v, state %v", named, app, got["state"])
		}
		if len(decisions) != 1 || decisions[0]["decidedByName"] != "Robin Staff" || decisions[0]["note"] != "known" {
			t.Fatalf("named %q: the decisions are %+v", named, decisions)
		}
		fields := listRows(details["fields"])
		if details["state"] != "shown" || len(fields) == 0 {
			t.Fatalf("named %q: the details are %+v", named, details)
		}
		shown := false
		for _, f := range fields {
			shown = shown || (f["label"] == "Ein" && f["value"] == "12-3456789")
		}
		if !shown {
			t.Fatalf("named %q: the details do not show the EIN: %+v", named, fields)
		}
		if acts, _ := got["acts"].([]any); len(acts) == 0 || acts[0] != "remove" {
			t.Fatalf("named %q: the acts are %v", named, got["acts"])
		}
	}
	log := listRows(answerOf(t, eng, ctx, wide["wholesaleDecisionLog"])["rows"])
	if len(log) != 1 || asString(log[0]["applicationId"]) != appID || log[0]["decidedByName"] != "Robin Staff" || log[0]["transition"] != "approve" {
		t.Fatalf("the decision log is %+v", log)
	}
	buyers := listRows(answerOf(t, eng, ctx, wide["wholesaleBuyers"])["rows"])
	if len(buyers) != 1 || asString(buyers[0]["applicationId"]) != appID {
		t.Fatalf("the buyers are %+v", buyers)
	}
	export := answerOf(t, eng, ctx, wide["wholesaleExportStore"])
	if !strings.HasPrefix(asString(export["fileName"]), "wholesale-") || !strings.Contains(asString(export["json"]), "12-3456789") {
		t.Fatalf("the export is %+v", export)
	}
}

// A CLUSTER OPERATOR READS THE LIST, THE APPLICATION AND THE DECISION LOG, and
// is offered no decision.
func TestAClusterOperatorReadsTheListTheApplicationAndTheDecisionLog(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "op-reads")
	operator := asOperator("operator-" + s.staff)
	wide := storeWide(s.site, s.store)
	if rows := listRows(answerOf(t, eng, operator, wide["wholesaleList"])["rows"]); len(rows) != 1 || bareID(asString(rows[0]["applicationId"])) != s.appID {
		t.Fatalf("the operator lists %+v", rows)
	}
	got := answerOf(t, eng, operator, `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	if acts, _ := got["acts"].([]any); got["operator"] != true || len(acts) != 0 {
		t.Fatalf("the operator opens an application ready to review with operator %v and acts %v, want none", got["operator"], got["acts"])
	}
	if err := s.approve(eng, signedIn(s.staff)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	log := listRows(answerOf(t, eng, operator, wide["wholesaleDecisionLog"])["rows"])
	if len(log) != 1 || bareID(asString(log[0]["applicationId"])) != s.appID {
		t.Fatalf("the operator's decision log is %+v", log)
	}
	got = answerOf(t, eng, operator, `builtin wholesaleGet(applicationId: `+quote(s.appID)+`)`)
	if acts, _ := got["acts"].([]any); len(acts) != 1 || acts[0] != "retryAccess" {
		// Approved with no access granted yet: the one act an operator has.
		t.Fatalf("the operator is offered %v on an approved application with no access, want retryAccess", got["acts"])
	}
}

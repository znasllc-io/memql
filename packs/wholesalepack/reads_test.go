package wholesalepack

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// reads_test.go -- the reads the Wholesale section opens, as functions over
// rows: the list, one application, the decision log, the buyers and the
// export, behind the reader seam.

const (
	readsStore = "store-reads-1"
	readsSite  = "site-reads-1"
)

// storeFake answers the pack's reads of one store, the way the engine does
// for the two things these reads depend on: it does not join the two forms of
// an application id, and a row naming no merchant (written before the account
// tie) is read by the system and by nobody on the store's side.
type storeFake struct {
	apps       []map[string]any // the store's applications the CALLER reads
	hidden     []map[string]any // applications only the system reads
	decisions  []map[string]any
	ents       []map[string]any
	users      map[string]map[string]any // bare id -> user row
	extensions map[string]memql.ExtensionRow
	sysOnlyExt map[string]bool // bare application id -> an extension row the caller cannot read
	settings   map[string]any
	pages      map[string]int   // RowsPage calls per query
	docs       []map[string]any // document versions, read by the system only
}

func newStoreFake() *storeFake {
	return &storeFake{
		users:      map[string]map[string]any{},
		extensions: map[string]memql.ExtensionRow{},
		sysOnlyExt: map[string]bool{},
		settings:   map[string]any{"id": SettingsRowID(readsStore), "storeId": readsStore, "applicationsOpen": true, "tokenHash": "settings-digest"},
		pages:      map[string]int{},
	}
}

func isSystem(ctx context.Context) bool { return auth.IsSystemActorId(actorOf(ctx)) }

// visible is whether ctx reads row: a row naming no merchant is the system's.
func visible(ctx context.Context, row map[string]any) bool {
	return isSystem(ctx) || trimmed(row["ownerUserId"]) != ""
}

func (f *storeFake) Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	switch query {
	case "siteById":
		if bareID(args["siteId"]) == readsSite {
			return []map[string]any{{"id": "v1:platform:site:" + readsSite, "accountId": "acct-reads",
				"binding": map[string]any{"storeId": readsStore}}}, nil
		}
		// A site of ANOTHER organization that binds the same store.
		if bareID(args["siteId"]) == "site-other-org" {
			return []map[string]any{{"id": "v1:platform:site:site-other-org", "accountId": "acct-other",
				"binding": map[string]any{"storeId": readsStore}}}, nil
		}
	case "wholesaleSettingsById":
		if args["settingsId"] == SettingsRowID(readsStore) && isSystem(ctx) {
			return []map[string]any{f.settings}, nil
		}
	case "applicationById":
		rows := f.apps
		if isSystem(ctx) {
			rows = append(append([]map[string]any{}, f.apps...), f.hidden...)
		}
		for _, app := range rows {
			if bareID(trimmed(app["id"])) == bareID(args["applicationId"]) {
				return []map[string]any{app}, nil
			}
		}
	case "decisionsForApplication":
		return f.byApplication(ctx, f.decisions, args["applicationId"]), nil
	case "entitlementForApplication":
		return f.byApplication(ctx, f.ents, args["applicationId"]), nil
	case "userByIdSystem":
		if !auth.OriginFromContext(ctx).IsInternal() {
			return nil, fmt.Errorf("userByIdSystem without internal origin")
		}
		if row, ok := f.users[args["userId"]]; ok {
			return []map[string]any{row}, nil
		}
	}
	return nil, nil
}

// byApplication is a read keyed by applicationId AS WRITTEN, newest first.
func (f *storeFake) byApplication(ctx context.Context, rows []map[string]any, applicationID string) []map[string]any {
	var out []map[string]any
	for _, row := range rows {
		if trimmed(row["applicationId"]) == applicationID && visible(ctx, row) {
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rowTime(out[i]["createdAt"]).After(rowTime(out[j]["createdAt"])) })
	return out
}

// engineCap is the most rows the engine answers a sorted query that declares
// no page size, saying nothing about the rest.
const engineCap = 500

func newestFirst(rows []map[string]any) []map[string]any {
	sort.SliceStable(rows, func(i, j int) bool { return rowTime(rows[i]["createdAt"]).After(rowTime(rows[j]["createdAt"])) })
	return rows
}

func (f *storeFake) RowsWithList(ctx context.Context, query string, args map[string]string,
	_ string, list []string) ([]map[string]any, error) {
	if query != "decisionsForApplications" {
		return nil, nil
	}
	var out []map[string]any
	for _, id := range list {
		for _, d := range f.byApplication(ctx, f.decisions, id) {
			if trimmed(d["storeId"]) == args["storeId"] {
				out = append(out, d)
			}
		}
	}
	out = newestFirst(out)
	if len(out) > engineCap {
		out = out[:engineCap]
	}
	return out, nil
}

// inStore is whether a row's storeId is the one asked or one of the forms.
func inStore(row map[string]any, storeID string, forms []string) bool {
	held := trimmed(row["storeId"])
	if held == storeID {
		return true
	}
	for _, f := range forms {
		if held == f {
			return true
		}
	}
	return false
}

// RowsPage pages fifty at a time; the cursor is the offset.
func (f *storeFake) RowsPage(ctx context.Context, query string, args map[string]string, cursor string) ([]map[string]any, string, error) {
	return f.RowsPageWithList(ctx, query, args, "", nil, cursor)
}

func (f *storeFake) RowsPageWithList(ctx context.Context, query string, args map[string]string, _ string, list []string,
	cursor string) ([]map[string]any, string, error) {
	f.pages[query]++
	size := 50
	var all []map[string]any
	switch query {
	case "applicationsForStore":
		rows := f.apps
		if isSystem(ctx) {
			rows = append(append([]map[string]any{}, f.apps...), f.hidden...)
		}
		for _, app := range rows {
			if inStore(app, args["storeId"], list) {
				all = append(all, app)
			}
		}
		all = newestFirst(all)
	case "decisionsForStore":
		for _, d := range f.decisions {
			if visible(ctx, d) && inStore(d, args["storeId"], list) {
				all = append(all, d)
			}
		}
		all = newestFirst(all)
	case "decisionsForApplicationIds", "entitlementsForApplicationIds":
		size = 200
		rows := f.decisions
		if query == "entitlementsForApplicationIds" {
			rows = f.ents
		}
		for _, id := range list {
			all = append(all, f.byApplication(ctx, rows, id)...)
		}
		all = newestFirst(all)
	case "documentVersionsForStore":
		size = 200
		if isSystem(ctx) {
			all = docsMatching(f.docs, args["storeId"], args["kind"])
		}
	}
	from, _ := strconv.Atoi(cursor)
	if from > len(all) {
		from = len(all)
	}
	to := from + size
	if to > len(all) {
		to = len(all)
	}
	next := ""
	if to < len(all) {
		next = strconv.Itoa(to)
	}
	return all[from:to], next, nil
}

type fakeExtensions struct{ f *storeFake }

func (e fakeExtensions) ShopperExtensionRow(ctx context.Context, pack, form, rowID string) (memql.ExtensionRow, bool, error) {
	if pack != Domain || form != "application" {
		return memql.ExtensionRow{}, false, nil
	}
	id := bareID(rowID)
	if e.f.sysOnlyExt[id] && !isSystem(ctx) {
		return memql.ExtensionRow{}, false, nil
	}
	row, ok := e.f.extensions[id]
	return row, ok, nil
}

func (f *storeFake) provider() *Provider {
	return &Provider{reader: f, extensions: fakeExtensions{f}, hasExtension: func() bool { return true }}
}

var readsClock = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

// app is an application of the store, the merchant's, created minutes after
// the clock; i is its bare id's number.
func (f *storeFake) app(i int, company, applicant, email string, minutes int) string {
	id := "app-" + strconv.Itoa(i)
	f.apps = append(f.apps, map[string]any{"id": applicationConcept + ":" + id, "storeId": readsStore, "siteId": readsSite,
		"ownerUserId": "v1:identity:user:merchant-1", "accountId": "acct-reads", "companyName": company,
		"applicantName": applicant, "applicantEmail": email, "createdAt": readsClock.Add(time.Duration(minutes) * time.Minute)})
	// newest first, as applicationsForStore sorts
	sort.SliceStable(f.apps, func(i, j int) bool { return rowTime(f.apps[i]["createdAt"]).After(rowTime(f.apps[j]["createdAt"])) })
	return id
}

// decide records a decision naming applicationID in the form given.
func (f *storeFake) decide(applicationID, transition string, minutes int, tied bool) {
	row := map[string]any{"id": "dec-" + strconv.Itoa(len(f.decisions)), "applicationId": applicationID, "storeId": readsStore,
		"transition": transition, "decidedBy": "v1:identity:user:staff-1", "note": "n",
		"createdAt": readsClock.Add(time.Duration(minutes) * time.Minute)}
	if tied {
		row["ownerUserId"] = "v1:identity:user:merchant-1"
		row["accountId"] = "acct-reads"
	}
	f.decisions = append(f.decisions, row)
}

func staffReads() context.Context { return staffCtx("v1:identity:user:staff-1") }

func listOf(t *testing.T, p *Provider, args map[string]any) map[string]any {
	t.Helper()
	full := map[string]any{"siteId": readsSite, "storeId": readsStore}
	for k, v := range args {
		full[k] = v
	}
	return decodedBy(t)(p.list(staffReads(), full, 0))
}

func rowsOf(t *testing.T, reply map[string]any) []map[string]any {
	t.Helper()
	raw, _ := json.Marshal(reply["rows"])
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return rows
}

func companies(rows []map[string]any) []string {
	var out []string
	for _, r := range rows {
		out = append(out, asString(r["companyName"]))
	}
	return out
}

// THE QUEUE IS WHAT THE LIST OPENS ON, OLDEST FIRST: how long an application
// has waited is what the store's people answer to.
func TestTheListDefaultsToReadyToReviewOldestFirst(t *testing.T) {
	f := newStoreFake()
	f.app(1, "Second Trading", "B", "b@example.com", 20)
	f.app(2, "First Trading", "A", "a@example.com", 10)
	decided := f.app(3, "Decided Trading", "C", "c@example.com", 5)
	f.decide(applicationRef(decided), TransitionApprove, 30, true)
	got := listOf(t, f.provider(), nil)
	if rows := rowsOf(t, got); strings.Join(companies(rows), ",") != "First Trading,Second Trading" {
		t.Fatalf("the default list is %v, want the two ready to review, oldest first", companies(rows))
	}
	if rows := rowsOf(t, listOf(t, f.provider(), map[string]any{"stage": "all"})); strings.Join(companies(rows), ",") !=
		"Second Trading,First Trading,Decided Trading" {
		t.Fatalf("all is %v, want newest first", companies(rows))
	}
	row := rowsOf(t, got)[0]
	if row["applicationId"] != applicationConcept+":app-2" || row["stage"] != StageReadyToReview || row["applicantEmail"] != "a@example.com" ||
		row["createdAt"] != readsClock.Add(10*time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("a row is %+v", row)
	}
	if _, err := f.provider().list(staffReads(), map[string]any{"siteId": readsSite, "storeId": readsStore, "stage": "pending"}, 0); err == nil {
		t.Fatal("an unknown stage was listed")
	}
}

// EVERY STAGE IS COUNTED WHICHEVER IS OPEN, folded from the WHOLE log: an old
// decision nobody on the store's side can read, in either form of the id.
func TestTheListCountsEveryStage(t *testing.T) {
	f := newStoreFake()
	f.app(1, "Ready", "A", "a@example.com", 1)
	approved := f.app(2, "Approved", "B", "b@example.com", 2)
	f.decide(bareID(approved), TransitionApprove, 10, false) // old, untied, bare
	rejected := f.app(3, "Rejected", "C", "c@example.com", 3)
	f.decide(applicationRef(rejected), TransitionReject, 11, true)
	revoked := f.app(4, "Revoked", "D", "d@example.com", 4)
	f.decide(applicationRef(revoked), TransitionApprove, 12, true)
	f.decide(bareID(revoked), TransitionRevoke, 13, false)
	got := listOf(t, f.provider(), map[string]any{"stage": "approved"})
	counts, _ := got["counts"].(map[string]any)
	want := map[string]float64{StageWaitingForTerms: 0, StageReadyToReview: 1, StateApproved: 1, StateRejected: 1, StateRevoked: 1}
	for stage, n := range want {
		if counts[stage] != n {
			t.Errorf("counts[%s] = %v, want %v (counts %v)", stage, counts[stage], n, counts)
		}
	}
	if rows := rowsOf(t, got); len(rows) != 1 || rows[0]["companyName"] != "Approved" || rows[0]["stage"] != StateApproved {
		t.Fatalf("the approved list is %+v", rows)
	}
}

// AN APPLICATION WHOSE ONLY DECISION IS AN OLD UNTIED APPROVAL IS APPROVED, in
// the list and in wholesaleGet alike (X27).
func TestAnOldUntiedApprovalIsApprovedInTheListAndTheApplication(t *testing.T) {
	f := newStoreFake()
	id := f.app(1, "Old Approval", "A", "a@example.com", 1)
	f.decide(bareID(id), TransitionApprove, 5, false)
	if rows := rowsOf(t, listOf(t, f.provider(), map[string]any{"stage": "all"})); len(rows) != 1 || rows[0]["stage"] != StateApproved {
		t.Fatalf("the list shows %+v, want approved", rows)
	}
	for _, named := range []string{id, applicationRef(id)} {
		got := decodedBy(t)(f.provider().get(staffReads(), map[string]any{"applicationId": named}, 0))
		if got["state"] != StateApproved || got["stage"] != StateApproved {
			t.Fatalf("named %q, wholesaleGet answers %v / %v, want approved", named, got["state"], got["stage"])
		}
		if ds, _ := got["decisions"].([]any); len(ds) != 1 {
			t.Fatalf("named %q, wholesaleGet answers %d decisions, want the old one", named, len(ds))
		}
	}
}

func TestTheListSearchesCompanyApplicantAndEmail(t *testing.T) {
	f := newStoreFake()
	f.app(1, "Harbor Goods", "Ann Lee", "ann@one.example.com", 1)
	f.app(2, "Inland Supply", "Bo HARBORSON", "bo@two.example.com", 2)
	f.app(3, "Third Co", "Cy Diaz", "cy@harbor.example.com", 3)
	f.app(4, "Nothing Here", "Di Moss", "di@four.example.com", 4)
	got := rowsOf(t, listOf(t, f.provider(), map[string]any{"search": "  HarBor "}))
	if strings.Join(companies(got), ",") != "Harbor Goods,Inland Supply,Third Co" {
		t.Fatalf("searching harbor lists %v", companies(got))
	}
}

func TestTheListCarriesTheExtensionsTwoDisplayFacts(t *testing.T) {
	f := newStoreFake()
	id := f.app(1, "Facts Trading", "A", "a@example.com", 1)
	f.app(2, "No Facts", "B", "b@example.com", 2)
	f.extensions[id] = memql.ExtensionRow{Primary: "12-3456789", Secondary: "Retail"}
	rows := rowsOf(t, listOf(t, f.provider(), nil))
	if rows[0]["primary"] != "12-3456789" || rows[0]["secondary"] != "Retail" {
		t.Fatalf("the row with an extension is %+v", rows[0])
	}
	if rows[1]["primary"] != "" || rows[1]["secondary"] != "" {
		t.Fatalf("the row without one is %+v", rows[1])
	}
}

// PAST THE FIRST PAGE, AND NO FURTHER THAN A THOUSAND: the list reads twenty
// pages of fifty and says it stopped.
func TestTheListReadsPastTheFirstPageAndStopsAtAThousand(t *testing.T) {
	f := newStoreFake()
	for i := 0; i < 1020; i++ {
		f.app(i, "Company "+strconv.Itoa(i), "A", "a@example.com", i)
	}
	got := listOf(t, f.provider(), map[string]any{"limit": 10})
	counts, _ := got["counts"].(map[string]any)
	if counts[StageReadyToReview] != float64(1000) || got["truncated"] != true {
		t.Fatalf("counts %v, truncated %v; want 1000 read and truncated", counts, got["truncated"])
	}
	if f.pages["applicationsForStore"] != 20 {
		t.Fatalf("read %d pages, want 20", f.pages["applicationsForStore"])
	}
	first := rowsOf(t, got)
	// THE NEWEST THOUSAND ARE KEPT: the twenty oldest (0 to 19) are left out,
	// so the queue, oldest first, opens on the twenty-first.
	if len(first) == 0 || first[0]["companyName"] != "Company 20" {
		t.Fatalf("the queue opens on %v, want Company 20 (the oldest twenty are past the cap)", companies(first))
	}
	if len(first) != 10 || asString(got["next"]) == "" {
		t.Fatalf("the first page has %d rows and next %q", len(first), got["next"])
	}
	second := rowsOf(t, listOf(t, f.provider(), map[string]any{"limit": 10, "after": got["next"]}))
	if len(second) != 10 || second[0]["applicationId"] == first[9]["applicationId"] {
		t.Fatalf("the second page repeats the first: %v then %v", companies(first), companies(second))
	}
	small := newStoreFake()
	small.app(1, "Only", "A", "a@example.com", 1)
	if got := listOf(t, small.provider(), nil); got["truncated"] != false || asString(got["next"]) != "" {
		t.Fatalf("one application: truncated %v, next %v", got["truncated"], got["next"])
	}
}

func TestActsFollowTheStageAndTheAccess(t *testing.T) {
	for _, c := range []struct {
		in   ActsInput
		want string
	}{
		{ActsInput{Stage: StageWaitingForTerms}, "reject"},
		{ActsInput{Stage: StageReadyToReview}, "reject,approve"},
		{ActsInput{Stage: StateApproved, AccessState: EntitlementGranted, EntitlementExists: true}, "remove"},
		{ActsInput{Stage: StateApproved, AccessState: "pending", EntitlementExists: true}, "remove,retryAccess"},
		{ActsInput{Stage: StateApproved, AccessState: "failed", EntitlementExists: true}, "remove,retryAccess"},
		{ActsInput{Stage: StateApproved, AccessState: "none"}, "remove,retryAccess"},
		{ActsInput{Stage: StateRevoked, AccessState: EntitlementGranted, EntitlementExists: true}, "retryAccess"},
		{ActsInput{Stage: StateRevoked, AccessState: EntitlementRevoked, EntitlementExists: true}, ""},
		{ActsInput{Stage: StateRevoked, AccessState: "none"}, ""},
		{ActsInput{Stage: StateRejected}, ""},
	} {
		if got := strings.Join(ActsFor(c.in), ","); got != c.want {
			t.Errorf("ActsFor(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// AN OPERATOR IS OFFERED A RETRY AND NOTHING ELSE (ruling 30, X26): under the
// two conditions a person of the store is, and no decision ever.
func TestAnOperatorIsOfferedOnlyRetryAccess(t *testing.T) {
	for _, c := range []struct {
		in   ActsInput
		want string
	}{
		{ActsInput{Stage: StageWaitingForTerms, Operator: true}, ""},
		{ActsInput{Stage: StageReadyToReview, Operator: true}, ""},
		{ActsInput{Stage: StateApproved, AccessState: EntitlementGranted, EntitlementExists: true, Operator: true}, ""},
		{ActsInput{Stage: StateApproved, AccessState: "failed", EntitlementExists: true, Operator: true}, "retryAccess"},
		{ActsInput{Stage: StateApproved, AccessState: "none", Operator: true}, "retryAccess"},
		{ActsInput{Stage: StateRevoked, AccessState: "pending", EntitlementExists: true, Operator: true}, "retryAccess"},
		{ActsInput{Stage: StateRevoked, AccessState: EntitlementRevoked, EntitlementExists: true, Operator: true}, ""},
		{ActsInput{Stage: StateRejected, Operator: true}, ""},
	} {
		if got := strings.Join(ActsFor(c.in), ","); got != c.want {
			t.Errorf("ActsFor(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func detailsOf(t *testing.T, got map[string]any) map[string]any {
	t.Helper()
	d, _ := got["details"].(map[string]any)
	return d
}

func TestAStaffAddedApplicationHasNoFurtherDetails(t *testing.T) {
	f := newStoreFake()
	id := f.app(1, "Added By Staff", "A", "a@example.com", 1)
	f.apps[0]["source"] = SourceStaff
	f.apps[0]["addedBy"] = "v1:identity:user:staff-1"
	f.users["staff-1"] = map[string]any{"id": "v1:identity:user:staff-1", "displayName": "Robin Staff"}
	f.extensions[id] = memql.ExtensionRow{Fields: []memql.ExtensionField{{Name: "taxNumber", Label: "Tax number", Value: "x"}}}
	got := decodedBy(t)(f.provider().get(staffReads(), map[string]any{"applicationId": id}, 0))
	if d := detailsOf(t, got); d["state"] != "none" {
		t.Fatalf("a staff-added application's details are %+v, want none", d)
	}
	app, _ := got["application"].(map[string]any)
	if app["source"] != SourceStaff || app["addedByName"] != "Robin Staff" {
		t.Fatalf("the application is %+v, want staff, added by Robin Staff", app)
	}
	noExt := newStoreFake()
	other := noExt.app(1, "Storefront", "A", "a@example.com", 1)
	p := noExt.provider()
	p.hasExtension = func() bool { return false }
	if d := detailsOf(t, decodedBy(t)(p.get(staffReads(), map[string]any{"applicationId": other}, 0))); d["state"] != "none" {
		t.Fatalf("with no extension declared the details are %+v, want none", d)
	}
}

func TestAnExtensionRowTheCallerCannotReadIsUnreadable(t *testing.T) {
	f := newStoreFake()
	id := f.app(1, "Hidden Details", "A", "a@example.com", 1)
	f.extensions[id] = memql.ExtensionRow{Fields: []memql.ExtensionField{{Name: "taxNumber", Label: "Tax number", Value: "12-3456789"}}}
	f.sysOnlyExt[id] = true
	d := detailsOf(t, decodedBy(t)(f.provider().get(staffReads(), map[string]any{"applicationId": id}, 0)))
	if d["state"] != "unreadable" {
		t.Fatalf("details are %+v, want unreadable", d)
	}
	if fields, _ := d["fields"].([]any); len(fields) != 0 {
		t.Fatalf("an unreadable row showed its fields: %+v", d)
	}
	f.sysOnlyExt[id] = false
	d = detailsOf(t, decodedBy(t)(f.provider().get(staffReads(), map[string]any{"applicationId": id}, 0)))
	fields, _ := d["fields"].([]any)
	if d["state"] != "shown" || len(fields) != 1 || fields[0].(map[string]any)["label"] != "Tax number" {
		t.Fatalf("a readable row's details are %+v", d)
	}
}

// AN APPLICATION THE CALLER CANNOT READ IS NOT ANSWERED, and nothing is read as
// the system for it.
func TestAnApplicationTheCallerCannotReadIsNotOpened(t *testing.T) {
	f := newStoreFake()
	f.hidden = append(f.hidden, map[string]any{"id": applicationConcept + ":app-h", "storeId": readsStore, "companyName": "Hidden"})
	if _, err := f.provider().get(staffReads(), map[string]any{"applicationId": "app-h"}, 0); err == nil {
		t.Fatal("an application the caller cannot read was opened")
	}
}

func TestTheExportCarriesNoTokenHash(t *testing.T) {
	f := newStoreFake()
	id := f.app(1, "Export Trading", "A", "a@example.com", 1)
	f.apps[0]["tokenHash"] = "app-digest"
	f.decide(applicationRef(id), TransitionApprove, 5, true)
	f.ents = append(f.ents, map[string]any{"id": EntitlementRowID(id), "applicationId": applicationRef(id), "state": EntitlementGranted,
		"ownerUserId": "v1:identity:user:merchant-1", "tokenHash": "ent-digest", "createdAt": readsClock})
	f.extensions[id] = memql.ExtensionRow{Fields: []memql.ExtensionField{{Name: "taxNumber", Label: "Tax number", Value: "12-3456789"}}}
	p := f.provider()
	p.now = func() time.Time { return readsClock }
	got := decodedBy(t)(p.exportStore(staffReads(), map[string]any{"siteId": readsSite, "storeId": readsStore}, 0))
	if got["fileName"] != "wholesale-"+readsStore+"-20261101.json" {
		t.Fatalf("the file is named %v", got["fileName"])
	}
	doc := asString(got["json"])
	if strings.Contains(doc, "tokenHash") || strings.Contains(doc, "digest") {
		t.Fatalf("the export carries a token hash: %s", doc)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("the export is not JSON: %v", err)
	}
	apps, _ := parsed["applications"].([]any)
	decisions, _ := parsed["decisions"].([]any)
	ents, _ := parsed["entitlements"].([]any)
	if len(apps) != 1 || len(decisions) != 1 || len(ents) != 1 || parsed["settings"] == nil {
		t.Fatalf("the export holds %d applications, %d decisions, %d entitlements, settings %v", len(apps), len(decisions), len(ents), parsed["settings"])
	}
	if d, _ := apps[0].(map[string]any)["details"].(map[string]any); d["state"] != "shown" {
		t.Fatalf("the exported application's details are %+v", d)
	}
}

// THE DECISION LOG IS THE STORE'S DECISIONS ON THE APPLICATIONS THE CALLER
// READS, newest first, each with its company and the decider's name.
func TestTheDecisionLogJoinsEachDecisionToItsApplication(t *testing.T) {
	f := newStoreFake()
	a := f.app(1, "Alpha", "A", "a@example.com", 1)
	b := f.app(2, "Beta", "B", "b@example.com", 2)
	f.decide(applicationRef(a), TransitionApprove, 10, true)
	f.decide(b, TransitionReject, 11, true) // bare
	f.decide(applicationRef("app-foreign"), TransitionReject, 12, true)
	f.users["staff-1"] = map[string]any{"displayName": "Robin Staff"}
	got := decodedBy(t)(f.provider().decisionLog(staffReads(), map[string]any{"siteId": readsSite, "storeId": readsStore}, 0))
	rows := rowsOf(t, got)
	if len(rows) != 2 || rows[0]["companyName"] != "Beta" || rows[1]["companyName"] != "Alpha" ||
		rows[0]["applicationId"] != applicationConcept+":"+b || rows[0]["decidedByName"] != "Robin Staff" {
		t.Fatalf("the log is %+v", rows)
	}
	one := decodedBy(t)(f.provider().decisionLog(staffReads(), map[string]any{"siteId": readsSite, "storeId": readsStore, "limit": 1}, 0))
	if r := rowsOf(t, one); len(r) != 1 || r[0]["companyName"] != "Beta" || asString(one["next"]) == "" {
		t.Fatalf("a page of one is %+v, next %v", r, one["next"])
	}
	two := decodedBy(t)(f.provider().decisionLog(staffReads(), map[string]any{"siteId": readsSite, "storeId": readsStore, "limit": 1, "after": one["next"]}, 0))
	if r := rowsOf(t, two); len(r) != 1 || r[0]["companyName"] != "Alpha" {
		t.Fatalf("the second page is %+v", r)
	}
}

// THE BUYERS ARE EVERY APPROVED APPLICATION, with its access.
func TestTheBuyersAreTheApprovedApplicationsWithTheirAccess(t *testing.T) {
	f := newStoreFake()
	granted := f.app(1, "Granted", "A", "a@example.com", 1)
	f.decide(applicationRef(granted), TransitionApprove, 10, true)
	f.ents = append(f.ents, map[string]any{"applicationId": applicationRef(granted), "state": EntitlementGranted,
		"ownerUserId": "v1:identity:user:merchant-1", "shopifyCustomerId": "gid://shopify/Customer/1", "createdAt": readsClock.Add(11 * time.Minute)})
	none := f.app(2, "No Access", "B", "b@example.com", 2)
	f.decide(bareID(none), TransitionApprove, 12, false)
	f.app(3, "Waiting", "C", "c@example.com", 3)
	rows := rowsOf(t, decodedBy(t)(f.provider().buyers(staffReads(), map[string]any{"siteId": readsSite, "storeId": readsStore}, 0)))
	if len(rows) != 2 || rows[0]["companyName"] != "No Access" || rows[0]["accessState"] != "none" ||
		rows[1]["accessState"] != EntitlementGranted || rows[1]["shopifyCustomerId"] != "gid://shopify/Customer/1" ||
		rows[1]["grantedAt"] != readsClock.Add(11*time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("the buyers are %+v", rows)
	}
}

// A STORE-WIDE READ CHECKS THE SITE BEFORE IT READS THE STORE.
func TestAStoreWideReadRefusesASiteThatDoesNotServeTheStore(t *testing.T) {
	f := newStoreFake()
	f.app(1, "Alpha", "A", "a@example.com", 1)
	p := f.provider()
	for name, call := range map[string]func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error){
		"list": p.list, "decisionLog": p.decisionLog, "buyers": p.buyers, "exportStore": p.exportStore,
	} {
		if _, err := call(staffReads(), map[string]any{"siteId": "site-elsewhere", "storeId": readsStore}, 0); err == nil {
			t.Errorf("%s answered for a site that does not serve the store", name)
		}
	}
	if f.pages["applicationsForStore"] != 0 || f.pages["decisionsForStore"] != 0 {
		t.Fatalf("a refused read still read the store: %v", f.pages)
	}
}

// hide adds an application of the SAME store that only the system reads, with
// a decision and an entitlement of its own.
func (f *storeFake) hide(id, company string) {
	f.hidden = append(f.hidden, map[string]any{"id": applicationConcept + ":" + id, "storeId": readsStore, "siteId": readsSite,
		"ownerUserId": "v1:identity:user:someone-else", "companyName": company, "applicantName": "H", "applicantEmail": "h@hidden.example.com",
		"createdAt": readsClock.Add(time.Hour)})
	f.decide(applicationRef(id), TransitionApprove, 70, true)
	f.ents = append(f.ents, map[string]any{"applicationId": applicationRef(id), "state": EntitlementGranted,
		"ownerUserId": "v1:identity:user:someone-else", "createdAt": readsClock.Add(71 * time.Minute)})
}

// AN APPLICATION OF THE STORE THE CALLER CANNOT READ IS IN NONE OF THE READS:
// no row, no count, no buyer, no line in the log, nothing in the export.
func TestAHiddenApplicationOfTheStoreIsLeftOutOfEveryRead(t *testing.T) {
	f := newStoreFake()
	seen := f.app(1, "Seen Trading", "A", "a@example.com", 1)
	f.decide(applicationRef(seen), TransitionApprove, 5, true)
	f.hide("app-hidden", "Hidden Trading")
	store := map[string]any{"siteId": readsSite, "storeId": readsStore}
	list := listOf(t, f.provider(), map[string]any{"stage": "all"})
	if rows := rowsOf(t, list); len(rows) != 1 || rows[0]["companyName"] != "Seen Trading" {
		t.Errorf("the list shows %v", companies(rows))
	}
	counts, _ := list["counts"].(map[string]any)
	for stage, n := range counts {
		if want := map[bool]float64{true: 1, false: 0}[stage == StateApproved]; n != want {
			t.Errorf("counts[%s] = %v, want %v (counts %v)", stage, n, want, counts)
		}
	}
	if rows := rowsOf(t, decodedBy(t)(f.provider().buyers(staffReads(), store, 0))); len(rows) != 1 || rows[0]["companyName"] != "Seen Trading" {
		t.Errorf("the buyers are %v", companies(rows))
	}
	if rows := rowsOf(t, decodedBy(t)(f.provider().decisionLog(staffReads(), store, 0))); len(rows) != 1 || rows[0]["companyName"] != "Seen Trading" {
		t.Errorf("the decision log is %+v", rows)
	}
	doc := asString(decodedBy(t)(f.provider().exportStore(staffReads(), store, 0))["json"])
	if strings.Contains(doc, "app-hidden") || strings.Contains(doc, "Hidden Trading") || strings.Contains(doc, "someone-else") {
		t.Errorf("the export carries the hidden application: %s", doc)
	}
}

// THE EXPORT NAMES EACH APPLICATION IN ONE FORM: an old decision and an old
// entitlement stored under the bare id are exported under the row's own id.
func TestTheExportNamesEachApplicationInOneForm(t *testing.T) {
	f := newStoreFake()
	id := f.app(1, "Old Rows Trading", "A", "a@example.com", 1)
	f.decide(bareID(id), TransitionApprove, 5, false)
	f.ents = append(f.ents, map[string]any{"applicationId": bareID(id), "state": EntitlementGranted, "createdAt": readsClock.Add(6 * time.Minute)})
	doc := asString(decodedBy(t)(f.provider().exportStore(staffReads(), map[string]any{"siteId": readsSite, "storeId": readsStore}, 0))["json"])
	var parsed struct {
		Applications []map[string]any `json:"applications"`
		Decisions    []map[string]any `json:"decisions"`
		Entitlements []map[string]any `json:"entitlements"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatal(err)
	}
	want := applicationConcept + ":" + id
	if len(parsed.Applications) != 1 || parsed.Applications[0]["id"] != want || len(parsed.Decisions) != 1 || len(parsed.Entitlements) != 1 {
		t.Fatalf("the export is %s", doc)
	}
	if parsed.Decisions[0]["applicationId"] != want || parsed.Entitlements[0]["applicationId"] != want {
		t.Fatalf("the decision names %v and the entitlement %v, want both %s", parsed.Decisions[0]["applicationId"],
			parsed.Entitlements[0]["applicationId"], want)
	}
}

// A STORE IS READ UNDER EITHER FORM OF ITS ID (ruling 31): an application held
// under each form is listed, counted and exported whichever form is asked.
func TestAStoreIsReadUnderEitherFormOfItsId(t *testing.T) {
	f := newStoreFake()
	f.app(1, "Bare Store Trading", "A", "a@example.com", 1)
	f.app(2, "Canonical Store Trading", "B", "b@example.com", 2)
	for _, app := range f.apps {
		if app["companyName"] == "Canonical Store Trading" {
			app["storeId"] = "v1:shopify:store:" + readsStore
		}
	}
	for _, asked := range []string{readsStore, "v1:shopify:store:" + readsStore} {
		store := map[string]any{"siteId": readsSite, "storeId": asked}
		list := decodedBy(t)(f.provider().list(staffReads(), store, 0))
		counts, _ := list["counts"].(map[string]any)
		if rows := rowsOf(t, list); len(rows) != 2 || counts[StageReadyToReview] != float64(2) {
			t.Errorf("asked as %q the list shows %v, counts %v", asked, companies(rows), counts)
		}
		doc := asString(decodedBy(t)(f.provider().exportStore(staffReads(), store, 0))["json"])
		if !strings.Contains(doc, "Bare Store Trading") || !strings.Contains(doc, "Canonical Store Trading") {
			t.Errorf("asked as %q the export lacks one: %s", asked, doc)
		}
	}
}

// THE FOLD READS A BATCH TO ITS END (ruling 32): fifty applications with more
// decisions than the engine answers unpaged, one of them decided only by the
// OLDEST, still folds that one as decided.
func TestTheFoldReadsABatchToItsEnd(t *testing.T) {
	f := newStoreFake()
	oldest := f.app(0, "Oldest Decision", "A", "a@example.com", 0)
	f.decide(applicationRef(oldest), TransitionApprove, 1, true)
	for i := 1; i < 50; i++ {
		id := f.app(i, "Busy "+strconv.Itoa(i), "B", "b@example.com", i)
		for k := 0; k < 12; k++ {
			f.decide(applicationRef(id), TransitionApprove, 100+i*20+k, true)
		}
	}
	got := listOf(t, f.provider(), map[string]any{"stage": "all", "search": "oldest decision"})
	if rows := rowsOf(t, got); len(rows) != 1 || rows[0]["stage"] != StateApproved {
		t.Fatalf("the application decided only by the oldest of %d decisions shows %+v, want approved", len(f.decisions), rows)
	}
}

func TestALimitIsClampedWhateverItsSize(t *testing.T) {
	for v, want := range map[any]int{nil: 50, 0: 50, -3: 50, 7: 7, float64(7): 7, float64(1e20): 200, int64(math.MaxInt64): 200,
		json.Number("99999999999999999999"): 200, 500: 200} {
		if got := limitOf(map[string]any{"limit": v}); got != want {
			t.Errorf("limitOf(%v) = %d, want %d", v, got, want)
		}
	}
}

// A SITE OF ANOTHER ORGANIZATION THAT BINDS THE STORE is refused by every
// store-wide read before a page is read, however the store is spelled (ruling
// 34): the store's settings are tied to its own organization.
func TestAStoreWideReadRefusesASiteOfAnotherOrganizationUnderEitherForm(t *testing.T) {
	for _, store := range []string{readsStore, "v1:shopify:store:" + readsStore} {
		f := newStoreFake()
		f.settings["accountId"] = "acct-reads"
		f.app(1, "Alpha", "A", "a@example.com", 1)
		p := f.provider()
		for name, call := range map[string]func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error){
			"list": p.list, "decisionLog": p.decisionLog, "buyers": p.buyers, "exportStore": p.exportStore,
		} {
			_, err := call(staffReads(), map[string]any{"siteId": "site-other-org", "storeId": store}, 0)
			if err == nil || !strings.Contains(err.Error(), "another organization") {
				t.Errorf("store named %q: %s through a site of another organization: %v", store, name, err)
			}
		}
		if len(f.pages) != 0 {
			t.Fatalf("store named %q: a refused read still read pages: %v", store, f.pages)
		}
	}
}

// AN APPLICATION WITH NO EXTENSION ROW HAS NO FURTHER DETAILS: none, not an
// empty shown.
func TestAnApplicationWithNoExtensionRowHasNoDetails(t *testing.T) {
	f := newStoreFake()
	id := f.app(1, "No Row", "A", "a@example.com", 1)
	if d := detailsOf(t, decodedBy(t)(f.provider().get(staffReads(), map[string]any{"applicationId": id}, 0))); d["state"] != "none" {
		t.Fatalf("details are %+v, want none", d)
	}
	f.extensions[id] = memql.ExtensionRow{}
	if d := detailsOf(t, decodedBy(t)(f.provider().get(staffReads(), map[string]any{"applicationId": id}, 0))); d["state"] != "shown" {
		t.Fatalf("a readable row with no field to show is %+v, want shown", d)
	}
}

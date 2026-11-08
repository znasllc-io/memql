package reviewspack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// reads_test.go -- WHAT THE ADMIN PANEL READS: ONE REVIEW, THE DECISION LOG,
// THE SETTINGS, AND WHO CAN MODERATE.
//
// The moderation list (list_test.go) answers the queue. A moderator who opens
// one review needs its whole decision log and the acts they may take on it
// NOW; the store's log answers who decided what, newest first, across every
// review; the settings answer what is in force with the defaults applied; and
// the store's people answer who can moderate it, the people its removal
// approver is chosen from. Each read says what the viewer may do, computed
// here with the rules the writes enforce, so the panel never offers an act the
// engine would refuse.

type readsReader struct {
	reviews   []map[string]any // reviewsInStore; reviewById picks one
	decisions []map[string]any // reviewDecisionsForReview(s)
	settings  map[string]any   // reviewSettingsForStore
}

func (r *readsReader) Rows(_ context.Context, query string, args map[string]string) ([]map[string]any, error) {
	switch query {
	case "reviewsInStore":
		return r.reviews, nil
	case "reviewById":
		for _, row := range r.reviews {
			if canonicalReviewID(asString(row["id"])) == canonicalReviewID(args["reviewId"]) {
				return []map[string]any{row}, nil
			}
		}
		return nil, nil
	case "reviewDecisionsForReview":
		var out []map[string]any
		for _, d := range r.decisions {
			if canonicalReviewID(asString(d["reviewId"])) == canonicalReviewID(args["reviewId"]) {
				out = append(out, d)
			}
		}
		return out, nil
	case "reviewSettingsForStore":
		if r.settings == nil {
			return nil, nil
		}
		return []map[string]any{r.settings}, nil
	case "siteById":
		return []map[string]any{liveSite}, nil
	}
	return nil, nil
}

func (r *readsReader) RowsWithList(_ context.Context, query string, _ map[string]string, _ string, list []string) ([]map[string]any, error) {
	if query != "reviewDecisionsForReviews" {
		return nil, nil
	}
	within := map[string]bool{}
	for _, id := range list {
		within[canonicalReviewID(id)] = true
	}
	var out []map[string]any
	for _, d := range r.decisions {
		if within[canonicalReviewID(asString(d["reviewId"]))] {
			out = append(out, d)
		}
	}
	return out, nil
}

func readReview(id, product, title string, revision int) map[string]any {
	return map[string]any{"id": id, "storeId": "store-live", "accountId": "acct-shop", "ownerUserId": "merchant-1",
		"productHandle": product, "title": title, "body": "Kept my feet warm", "rating": 4, "authorName": "Dana",
		"authorEmail": "dana@example.com", "verifiedBuyer": true, "revision": revision,
		"createdAt": t0.Format(time.RFC3339Nano)}
}

func withReason(d map[string]any, reason, note string) map[string]any {
	d["reason"] = reason
	if note != "" {
		d["note"] = note
	}
	return d
}

func getReview(t *testing.T, ctx context.Context, r *readsReader, id string) (map[string]any, error) {
	t.Helper()
	nodes, err := (&Provider{reader: r}).get(ctx, map[string]any{"reviewId": id}, 0)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func actsOf(v any) string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, asString(a))
	}
	return strings.Join(out, ",")
}

// ONE REVIEW COMES WITH ITS WHOLE LOG, NEWEST FIRST, and the state the log
// gives it now: published on revision 2 after an ask to fix on revision 1.
func TestOneReviewComesWithItsWholeLogNewestFirst(t *testing.T) {
	r := &readsReader{
		reviews: []map[string]any{readReview("r1", "boot", "Warm", 2)},
		decisions: []map[string]any{
			withReason(decisionRow("r1", ActionAskToFix, 1, 0), "personal_information", "Your phone number is in it"),
			decisionRow("r1", ActionPublish, 2, 10),
			decisionRow("r1", ActionEscalate, 2, 20),
		},
	}
	got, err := getReview(t, moderatorCtx("mod-1"), r, "r1")
	if err != nil {
		t.Fatal(err)
	}
	review, _ := got["review"].(map[string]any)
	if review["state"] != "published" || review["contentHash"] == "" || review["authorEmail"] != "dana@example.com" {
		t.Fatalf("review = %v, want it published, fingerprinted, with the writer's email for a moderator", review)
	}
	if at, _ := review["createdAt"].(string); !strings.HasPrefix(at, "2026-11-03T12:00:00") {
		t.Fatalf("createdAt = %v, want ISO time", review["createdAt"])
	}
	log, _ := got["decisions"].([]any)
	if len(log) != 3 {
		t.Fatalf("decisions = %v, want all three", log)
	}
	first, _ := log[0].(map[string]any)
	last, _ := log[2].(map[string]any)
	if first["action"] != ActionEscalate || last["action"] != ActionAskToFix {
		t.Fatalf("log = %v, want newest first", log)
	}
	if last["reason"] != "personal_information" || last["note"] != "Your phone number is in it" ||
		last["decidedBy"] != "moderator-1" || asInt(last["revision"]) != 1 || last["at"] == "" {
		t.Fatalf("an entry = %v, want its reason, note, who, revision and time", last)
	}
}

// THE ACTS ARE THE ONES THE ENGINE WOULD ACCEPT, for this viewer, on this
// review, now. A moderator moderates; only the owner the settings name
// removes and erases; the cluster's operators host the reviews and decide
// nothing.
func TestTheActsAreTheOnesTheEngineWouldAccept(t *testing.T) {
	waiting := &readsReader{reviews: []map[string]any{readReview("r1", "boot", "Warm", 1)},
		settings: map[string]any{"storeId": "store-live", "removalApproverUserId": "owner-1"}}
	published := &readsReader{reviews: []map[string]any{readReview("r1", "boot", "Warm", 1)},
		decisions: []map[string]any{decisionRow("r1", ActionPublish, 1, 0)},
		settings:  map[string]any{"storeId": "store-live", "removalApproverUserId": "owner-1"}}
	cases := []struct {
		name string
		ctx  context.Context
		r    *readsReader
		acts string
	}{
		{"a moderator on a waiting review", moderatorCtx("mod-1"), waiting, "publish,decline,askToFix,escalate"},
		{"the owner on a waiting review", moderatorCtx("owner-1"), waiting, "publish,decline,askToFix,escalate,erase"},
		{"a moderator on a published review", moderatorCtx("mod-1"), published, "escalate"},
		{"the owner on a published review", moderatorCtx("owner-1"), published, "escalate,remove,erase"},
		{"an operator", operatorCtxFor("staff-1"), waiting, ""},
	}
	for _, c := range cases {
		got, err := getReview(t, c.ctx, c.r, "r1")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if acts := actsOf(got["acts"]); acts != c.acts {
			t.Errorf("%s: acts = %q, want %q", c.name, acts, c.acts)
		}
	}
	got, _ := getReview(t, operatorCtxFor("staff-1"), waiting, "r1")
	if got["moderates"] != false {
		t.Fatalf("an operator's answer = %v, want moderates false so the panel can say who decides", got)
	}
}

func TestAReviewTheCallerCannotReadIsRefused(t *testing.T) {
	if _, err := getReview(t, moderatorCtx("mod-1"), &readsReader{}, "r9"); err == nil {
		t.Fatal("an unreadable review was answered")
	}
	if _, err := (&Provider{reader: &readsReader{}}).get(moderatorCtx("mod-1"), map[string]any{}, 0); err == nil {
		t.Fatal("a read with no reviewId was answered")
	}
}

func decisionLog(t *testing.T, r *readsReader, args map[string]any) map[string]any {
	t.Helper()
	nodes, err := (&Provider{reader: r}).decisionLog(moderatorCtx("mod-1"), args, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// THE STORE'S DECISION LOG is every decision on every one of its reviews,
// newest first, each with enough of its review to say what it was about --
// the product and the headline, never the writer -- and the state the review
// is in now.
func TestTheDecisionLogIsEveryDecisionNewestFirst(t *testing.T) {
	r := &readsReader{
		reviews: []map[string]any{readReview("r1", "boot", "Warm", 1), readReview("r2", "sock", "Itchy", 1)},
		decisions: []map[string]any{
			decisionRow("r1", ActionPublish, 1, 0),
			withReason(decisionRow("r2", ActionDecline, 1, 5), "profanity", ""),
			withReason(decisionRow("r1", ActionRemove, 1, 10), "defamatory", "Names a person"),
		},
	}
	got := decisionLog(t, r, map[string]any{"storeId": "store-live", "siteId": "site-1"})
	entries, _ := got["decisions"].([]any)
	if len(entries) != 3 || asInt(got["total"]) != 3 {
		t.Fatalf("log = %v, want three entries", got)
	}
	want := []struct{ action, review, product, state string }{
		{ActionRemove, "r1", "boot", "removed"}, {ActionDecline, "r2", "sock", "declined"}, {ActionPublish, "r1", "boot", "removed"},
	}
	for i, w := range want {
		e, _ := entries[i].(map[string]any)
		if e["action"] != w.action || bareAccount(asString(e["reviewId"])) != w.review || e["productHandle"] != w.product || e["reviewState"] != w.state {
			t.Fatalf("entry %d = %v, want %+v", i, e, w)
		}
		if _, leaked := e["authorEmail"]; leaked {
			t.Fatalf("entry %d carries the writer's email: %v", i, e)
		}
		if _, leaked := e["authorName"]; leaked {
			t.Fatalf("entry %d carries the writer's name: %v", i, e)
		}
	}
	if e, _ := entries[0].(map[string]any); e["title"] != "Warm" || e["reason"] != "defamatory" || e["note"] != "Names a person" {
		t.Fatalf("newest = %v, want its headline, reason and note", e)
	}
}

func TestTheDecisionLogIsPaged(t *testing.T) {
	r := &readsReader{reviews: []map[string]any{readReview("r1", "boot", "Warm", 1)}}
	for i := 0; i < 5; i++ {
		d := decisionRow("r1", ActionEscalate, 1, i)
		d["id"] = "d" + string(rune('a'+i))
		r.decisions = append(r.decisions, d)
	}
	first := decisionLog(t, r, map[string]any{"storeId": "store-live", "siteId": "site-1", "limit": 2})
	if entries, _ := first["decisions"].([]any); len(entries) != 2 || first["next"] == "" {
		t.Fatalf("first page = %v", first)
	}
	second := decisionLog(t, r, map[string]any{"storeId": "store-live", "siteId": "site-1", "limit": 2, "after": first["next"]})
	entries, _ := second["decisions"].([]any)
	if e, _ := entries[0].(map[string]any); len(entries) != 2 || e["id"] != "dc" {
		t.Fatalf("second page = %v, want it to start at the third newest", second)
	}
}

func TestTheDecisionLogNeedsTheStoreAndItsSite(t *testing.T) {
	p := &Provider{reader: &readsReader{}}
	if _, err := p.decisionLog(moderatorCtx("mod-1"), map[string]any{"siteId": "site-1"}, 0); err == nil {
		t.Fatal("a log with no store was answered")
	}
	if _, err := p.decisionLog(moderatorCtx("mod-1"), map[string]any{"storeId": "store-other", "siteId": "site-1"}, 0); err == nil {
		t.Fatal("a log for a store the site does not serve was answered")
	}
}

func readSettings(t *testing.T, ctx context.Context, r *readsReader) map[string]any {
	t.Helper()
	nodes, err := (&Provider{reader: r}).readSettings(ctx, map[string]any{"storeId": "store-live", "siteId": "site-1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// THE SETTINGS READ answers what is in force, defaults applied, and what this
// viewer may change: nobody but the cluster's operators sets a store up, and
// only the current owner or an operator names who approves removals.
func TestTheSettingsReadAnswersWhatIsInForce(t *testing.T) {
	none := readSettings(t, moderatorCtx("mod-1"), &readsReader{})
	s, _ := none["settings"].(map[string]any)
	if none["exists"] != false || asInt(s["linkLifetimeDays"]) != 14 || asInt(s["requestDelayDays"]) != 14 ||
		s["requestsEnabled"] != false || s["publicDisplay"] != false {
		t.Fatalf("no row = %v, want the defaults", none)
	}
	may, _ := none["may"].(map[string]any)
	if may["edit"] != false {
		t.Fatalf("a moderator may set up a store that has no settings: %v", may)
	}
	if may, _ := readSettings(t, operatorCtxFor("staff-1"), &readsReader{})["may"].(map[string]any); may["edit"] != true || may["nameApprover"] != true {
		t.Fatalf("an operator's may = %v, want both", may)
	}

	row := map[string]any{"storeId": "store-live", "linkLifetimeDays": 30, "requestsEnabled": true,
		"notifyEmail": "support@example.com", "removalApproverUserId": "owner-1", "requestTemplateId": "tmpl-ask"}
	set := readSettings(t, moderatorCtx("mod-1"), &readsReader{settings: row})
	s, _ = set["settings"].(map[string]any)
	if set["exists"] != true || asInt(s["linkLifetimeDays"]) != 30 || asInt(s["requestDelayDays"]) != 14 ||
		s["notifyEmail"] != "support@example.com" || s["removalApproverUserId"] != "owner-1" || s["requestTemplateId"] != "tmpl-ask" {
		t.Fatalf("a row = %v, want its values over the defaults", set)
	}
	if may, _ := set["may"].(map[string]any); may["edit"] != true || may["nameApprover"] != false {
		t.Fatalf("a moderator's may = %v, want edit only", may)
	}
	if may, _ := readSettings(t, moderatorCtx("owner-1"), &readsReader{settings: row})["may"].(map[string]any); may["nameApprover"] != true {
		t.Fatalf("the owner's may = %v, want them able to name who approves", may)
	}
	if viewer, _ := set["viewer"].(map[string]any); viewer["moderates"] != true || viewer["approvesRemovals"] != false {
		t.Fatalf("viewer = %v", set["viewer"])
	}
}

// ---------------------------------------------------------------------------
// Who can moderate (reviewsPeople)
// ---------------------------------------------------------------------------

// orgRoster is an organization's people as the pack reads them -- each
// organization's groups, their members, each person's own memberships and
// every person's user row -- and what the engine answers for each person in
// each of their organizations. It answers the people reads and stands in for
// the engine's resolver (organizationDecisions), so the rule over them is
// tested as a function, and it keeps a log of every read and every person the
// engine was asked about, so a test can say what an answer cost.
type orgRoster struct {
	groups  map[string][]map[string]any             // groupsForAccount, by account
	members map[string][]map[string]any             // membersOfGroup, by group; groupsForUser, by member
	users   map[string]map[string]any               // userByIdSystem, by user
	holds   map[string][]memql.OrganizationDecision // the engine's decisions, by user
	// failRead: the engine notes a failed grant read while it resolves.
	failRead bool
	// noteFailureIn: this read answers in full and notes a read it could not
	// make on its context, as the engine's tolerant readers do.
	noteFailureIn string
	// errorIn: this read fails outright.
	errorIn string
	asked   []string
	reads   []rosterRead
}

// rosterRead is one read the roster answered: the query, the id it was asked
// about, and who asked -- the actor on the context, and whether it carried
// internal origin.
type rosterRead struct {
	query, about, actor string
	internal            bool
}

func (r *orgRoster) rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, bool, error) {
	var out []map[string]any
	about := args["userId"]
	switch query {
	case "groupsForAccount":
		about = args["accountId"]
		out = r.groups[about]
	case "membersOfGroup":
		about = args["groupId"]
		out = r.members[about]
	case "groupsForUser":
		// Every group's rows for the person, whatever their status, as the
		// membersOfGroup double answers them: the status is the pack's to read.
		groups := make([]string, 0, len(r.members))
		for group := range r.members {
			groups = append(groups, group)
		}
		sort.Strings(groups)
		for _, group := range groups {
			for _, m := range r.members[group] {
				if asString(m["userId"]) == about {
					out = append(out, m)
				}
			}
		}
	case "userByIdSystem":
		if row, ok := r.users[about]; ok {
			out = []map[string]any{row}
		}
	default:
		return nil, false, nil
	}
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok {
		actor = ac.UserId
	}
	r.reads = append(r.reads, rosterRead{query: query, about: about, actor: actor, internal: auth.OriginFromContext(ctx).IsInternal()})
	if query == r.errorIn {
		return nil, true, errors.New("the rows could not be read")
	}
	if query == r.noteFailureIn {
		auth.NoteReadFailure(ctx, errors.New("part of the rows could not be read"))
	}
	return out, true, nil
}

// read is who each read of query was about, in the order they were made.
func (r *orgRoster) read(query string) []string {
	out := []string{}
	for _, read := range r.reads {
		if read.query == query {
			out = append(out, read.about)
		}
	}
	return out
}

func (r *orgRoster) ResolveOrganizationDecisionsFor(ctx context.Context, userId string, _ auth.Role, _ []auth.Decision) []memql.OrganizationDecision {
	r.asked = append(r.asked, userId)
	if r.failRead {
		auth.NoteReadFailure(ctx, errors.New("the grant rows could not be read"))
	}
	return r.holds[userId]
}

// withRoster answers the roster's reads and hands every other read on.
type withRoster struct {
	rowReader
	roster *orgRoster
}

func (w withRoster) Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	if rows, ok, err := w.roster.rows(ctx, query, args); ok {
		return rows, err
	}
	return w.rowReader.Rows(ctx, query, args)
}

func rosterGroup(id, account string) map[string]any {
	return map[string]any{"id": id, "name": id, "kind": "custom", "accountId": account, "status": "active"}
}

func rosterMember(group, user, origin, status string) map[string]any {
	return map[string]any{"id": group + "-" + user, "groupId": group, "userId": user, "origin": origin, "status": status}
}

func rosterUser(id, name, role string, active bool) map[string]any {
	return map[string]any{"id": id, "displayName": name, "primaryEmail": id + "@example.com", "role": role, "active": active}
}

// heldIn is the engine's answer for one person in one organization: each
// pair named is held there.
func heldIn(account string, pairs ...auth.VerbResource) []memql.OrganizationDecision {
	out := make([]memql.OrganizationDecision, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, memql.OrganizationDecision{AccountID: account, Decision: auth.Decision{Verb: p.Verb, Resource: p.Resource, Held: true}})
	}
	return out
}

var (
	readData        = auth.VerbResource{Verb: auth.VerbRead, Resource: auth.ResourceData}
	openDeployables = auth.VerbResource{Verb: auth.VerbRead, Resource: "app:deployables"}
	moderateReviews = auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/reviews"}
)

// shopRoster is the store's organization (acct-shop, the fixtures' own) and
// one other. Two of its people can moderate: owner-1, the removal approver the
// settings name, and mod-1, who is in two of its groups. Everybody else is in
// it for a reason that must keep them off the list -- a cluster operator, a
// role test's stand-in, someone deactivated, someone who left, someone without
// the reviews part, someone with the part and no way into Deployables -- or is
// in the other organization, where they hold everything.
func shopRoster() *orgRoster {
	persona := auth.RoleTestUserPrefix + "stand-in"
	moderates := func(account string) []memql.OrganizationDecision {
		return heldIn(account, readData, openDeployables, moderateReviews)
	}
	return &orgRoster{
		groups: map[string][]map[string]any{
			"acct-shop":  {rosterGroup("group-shop", "acct-shop"), rosterGroup("group-shop-support", "acct-shop")},
			"acct-other": {rosterGroup("group-other", "acct-other")},
		},
		members: map[string][]map[string]any{
			"group-shop": {
				rosterMember("group-shop", "mod-1", "added", "active"),
				rosterMember("group-shop", "owner-1", "added", "active"),
				rosterMember("group-shop", "staff-1", "added", "active"),
				rosterMember("group-shop", persona, "role-test", "active"),
				rosterMember("group-shop", "away-1", "added", "active"),
				rosterMember("group-shop", "gone-1", "added", "removed"),
				rosterMember("group-shop", "doorless-1", "added", "active"),
			},
			"group-shop-support": {
				rosterMember("group-shop-support", "viewer-1", "added", "active"),
				rosterMember("group-shop-support", "mod-1", "domain", "active"),
			},
			"group-other": {rosterMember("group-other", "outsider-1", "added", "active")},
		},
		users: map[string]map[string]any{
			"mod-1":      rosterUser("mod-1", "Robin Moderator", "shop-moderator", true),
			"owner-1":    rosterUser("owner-1", "Avery Owner", "user", true),
			"staff-1":    rosterUser("staff-1", "Sam Staff", "developer", true),
			persona:      rosterUser(persona, "Test Shop moderator", "shop-moderator", true),
			"away-1":     rosterUser("away-1", "Alex Away", "user", false),
			"gone-1":     rosterUser("gone-1", "Gale Gone", "user", true),
			"doorless-1": rosterUser("doorless-1", "Dee Doorless", "user", true),
			"viewer-1":   rosterUser("viewer-1", "Val Viewer", "reader", true),
			"outsider-1": rosterUser("outsider-1", "Oli Outsider", "user", true),
		},
		holds: map[string][]memql.OrganizationDecision{
			"mod-1":      moderates("acct-shop"),
			"owner-1":    moderates("acct-shop"),
			"staff-1":    moderates("acct-shop"),
			persona:      moderates("acct-shop"),
			"away-1":     moderates("acct-shop"),
			"gone-1":     moderates("acct-shop"),
			"doorless-1": heldIn("acct-shop", readData, moderateReviews),
			"viewer-1":   heldIn("acct-shop", readData, openDeployables),
			"outsider-1": moderates("acct-other"),
		},
	}
}

// peopleAnswer is the v1:reviews:people receipt as a client reads it.
type peopleAnswer struct {
	StoreID   string           `json:"storeId"`
	People    []map[string]any `json:"people"`
	Truncated *bool            `json:"truncated"`
}

// truncated is the answer's truncated, false when it carries none (askPeople
// has failed the test for that).
func (a peopleAnswer) truncated() bool {
	return a.Truncated != nil && *a.Truncated
}

// askPeople reads reviewsPeople's whole answer. Every answer says whether its
// list is cut (truncated), the empty one included: a client cannot tell a whole
// list from a cut one by looking at it.
func askPeople(t *testing.T, ctx context.Context, r rowReader, roster *orgRoster, args map[string]any) (peopleAnswer, error) {
	t.Helper()
	nodes, err := (&Provider{reader: withRoster{rowReader: r, roster: roster}, organizations: roster}).people(ctx, args, 0)
	if err != nil {
		return peopleAnswer{}, err
	}
	var out peopleAnswer
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.StoreID != "store-live" {
		t.Fatalf("storeId = %q, want the store asked about", out.StoreID)
	}
	if out.Truncated == nil {
		t.Errorf("the answer carries no truncated: %s", nodes[0].Payload)
	}
	return out, nil
}

func readPeople(t *testing.T, ctx context.Context, r rowReader, roster *orgRoster, args map[string]any) ([]map[string]any, error) {
	t.Helper()
	answer, err := askPeople(t, ctx, r, roster, args)
	return answer.People, err
}

func peopleIDs(people []map[string]any) []string {
	out := make([]string, 0, len(people))
	for _, p := range people {
		out = append(out, asString(p["userId"]))
	}
	return out
}

// THE PEOPLE WHO CAN MODERATE A STORE ARE ITS ORGANIZATION'S OWN: the active
// members of any of its groups whom the engine answers able to moderate its
// reviews there -- the organization's data, its Deployables door and the
// reviews part, the three things the organization gate asks of every reviews
// act -- and never one of the cluster's operators, who host the reviews and
// decide none. Each says whether they are the one who approves removals.
func TestReviewsPeopleListsTheStoresModerators(t *testing.T) {
	roster := shopRoster()
	people, err := readPeople(t, moderatorCtx("mod-1"), settingsReader(existingSettings()), roster,
		map[string]any{"storeId": "store-live", "siteId": "site-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(peopleIDs(people), ","); got != "owner-1,mod-1" {
		t.Fatalf("people = %s, want owner-1 and mod-1, by name -- nobody else in the organization can moderate, and nobody outside it is in it", got)
	}
	want := []map[string]any{
		{"userId": "owner-1", "displayName": "Avery Owner", "email": "owner-1@example.com", "mayRemove": true},
		{"userId": "mod-1", "displayName": "Robin Moderator", "email": "mod-1@example.com", "mayRemove": false},
	}
	for i, w := range want {
		if len(people[i]) != len(w) {
			t.Fatalf("person %d = %v, want exactly %v: nothing else of a user row leaves", i, people[i], w)
		}
		for k, v := range w {
			if people[i][k] != v {
				t.Fatalf("person %d %s = %v, want %v (%v)", i, k, people[i][k], v, people[i])
			}
		}
	}
	// The operator, the stand-in and the deactivated were dropped by who they
	// are, before the engine was asked about them; the rest because of what it
	// answered for them in this organization.
	asked := append([]string(nil), roster.asked...)
	sort.Strings(asked)
	if got := strings.Join(asked, ","); got != "doorless-1,mod-1,owner-1,viewer-1" {
		t.Fatalf("the engine was asked about %s", got)
	}

	// An operator reads the same people: who can moderate is the store's
	// answer, not the viewer's.
	again, err := readPeople(t, operatorCtxFor("staff-9"), settingsReader(existingSettings()), shopRoster(),
		map[string]any{"storeId": "store-live", "siteId": "site-1"})
	if err != nil || strings.Join(peopleIDs(again), ",") != "owner-1,mod-1" {
		t.Fatalf("an operator read %v (%v), want the same two people", peopleIDs(again), err)
	}
}

// crowd is the store's organization (acct-shop) with one group and nobody in
// it yet; join puts people there.
func crowd() *orgRoster {
	return &orgRoster{
		groups:  map[string][]map[string]any{"acct-shop": {rosterGroup("group-crowd", "acct-shop")}},
		members: map[string][]map[string]any{},
		users:   map[string]map[string]any{},
		holds:   map[string][]memql.OrganizationDecision{},
	}
}

// join makes one more person an active member of crowd's group, called name,
// able to moderate in acct-shop when moderates says so and holding nothing
// there otherwise.
func (r *orgRoster) join(id, name string, moderates bool) {
	r.members["group-crowd"] = append(r.members["group-crowd"], rosterMember("group-crowd", id, "added", "active"))
	r.users[id] = rosterUser(id, name, "user", true)
	if moderates {
		r.holds[id] = heldIn("acct-shop", readData, openDeployables, moderateReviews)
	}
}

var theStore = map[string]any{"storeId": "store-live", "siteId": "site-1"}

// EVERY MEMBER IS JUDGED BEFORE THE LIST IS CUT. A bound on the organization's
// members, taken before anybody was asked whether they can moderate, kept the
// first maxPeople ids whoever they were: an organization whose first hundred
// could not moderate listed nobody, and said nothing. The bound is the list's.
func TestTheListIsCutOnlyOnceEveryMemberIsJudged(t *testing.T) {
	roster := crowd()
	for i := 0; i < maxPeople; i++ {
		roster.join(fmt.Sprintf("a-%03d", i), fmt.Sprintf("Ash %03d", i), false)
	}
	for _, id := range []string{"b-1", "b-2", "b-3"} {
		roster.join(id, "Bea "+id, true)
	}
	answer, err := askPeople(t, moderatorCtx("b-1"), settingsReader(existingSettings()), roster, theStore)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(peopleIDs(answer.People), ","); got != "b-1,b-2,b-3" || answer.truncated() {
		t.Fatalf("people = %q (truncated %v), want b-1,b-2,b-3, the whole list", got, answer.truncated())
	}
}

// A LIST LONGER THAN THE BOUND IS CUT BY NAME, AND SAYS SO. The people kept are
// the first maxPeople as a person reads the list, by what they are called --
// not the first by id -- and truncated tells a client the list is not everyone
// who can moderate. A list exactly the bound is whole.
func TestALongListIsCutByNameAndSaysSo(t *testing.T) {
	roster := crowd()
	for i := 0; i <= maxPeople; i++ {
		// Ids and names run opposite ways: p-000 is called Pat 100, p-100 Pat 000.
		roster.join(fmt.Sprintf("p-%03d", i), fmt.Sprintf("Pat %03d", maxPeople-i), true)
	}
	answer, err := askPeople(t, moderatorCtx("p-000"), settingsReader(existingSettings()), roster, theStore)
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.People) != maxPeople || !answer.truncated() {
		t.Fatalf("listed %d people (truncated %v), want %d and truncated", len(answer.People), answer.truncated(), maxPeople)
	}
	for i, p := range answer.People {
		if want := fmt.Sprintf("Pat %03d", i); p["displayName"] != want {
			t.Fatalf("person %d = %v, want %s: the first %d by name", i, p["displayName"], want, maxPeople)
		}
	}

	exact := crowd()
	for i := 0; i < maxPeople; i++ {
		exact.join(fmt.Sprintf("p-%03d", i), fmt.Sprintf("Pat %03d", i), true)
	}
	whole, err := askPeople(t, moderatorCtx("p-000"), settingsReader(existingSettings()), exact, theStore)
	if err != nil {
		t.Fatal(err)
	}
	if len(whole.People) != maxPeople || whole.truncated() {
		t.Fatalf("listed %d of %d (truncated %v), want all and not truncated", len(whole.People), maxPeople, whole.truncated())
	}
}

// A STORE WHOSE SITE BELONGS TO NO ORGANIZATION HAS NOBODY TO MODERATE IT, and
// its people are never looked for.
func TestReviewsPeopleOfAStoreInNoOrganizationIsNobody(t *testing.T) {
	untied := map[string]any{"id": "site-1", "ownerUserId": "merchant-1", "binding": map[string]any{"storeId": "store-live"}}
	roster := shopRoster()
	people, err := readPeople(t, operatorCtxFor("staff-9"), &decideReader{site: untied, settings: map[string]any{"storeId": "store-live"}}, roster,
		map[string]any{"storeId": "store-live", "siteId": "site-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 0 || len(roster.asked) != 0 {
		t.Fatalf("people = %v (asked %v), want nobody", people, roster.asked)
	}
}

// WHO CAN MODERATE IS ANSWERED IN FULL OR NOT AT ALL: a grant read that failed
// under the answer could hide a deny, and a list drawn without it names a
// moderator the store's organization has barred. So could one of the
// deployment's own reads -- the groups, their members, a person's row -- that
// answered from part of what it reads, and the answer refuses that too.
func TestReviewsPeopleRefusesWhenAReadBehindItFails(t *testing.T) {
	roster := shopRoster()
	roster.failRead = true
	if _, err := readPeople(t, moderatorCtx("mod-1"), settingsReader(existingSettings()), roster,
		map[string]any{"storeId": "store-live", "siteId": "site-1"}); err == nil {
		t.Fatal("an answer was drawn from part of the grants")
	}
	for _, query := range []string{"groupsForAccount", "membersOfGroup", "userByIdSystem"} {
		roster := shopRoster()
		roster.noteFailureIn = query
		if _, err := readPeople(t, moderatorCtx("mod-1"), settingsReader(existingSettings()), roster,
			map[string]any{"storeId": "store-live", "siteId": "site-1"}); err == nil {
			t.Errorf("an answer was drawn from part of %s", query)
		}
	}
}

func TestReviewsPeopleNeedsTheStoreAndItsSite(t *testing.T) {
	roster := shopRoster()
	for _, args := range []map[string]any{
		{"siteId": "site-1"},
		{"storeId": "store-live"},
		{"storeId": "store-other", "siteId": "site-1"},
	} {
		if _, err := readPeople(t, moderatorCtx("mod-1"), settingsReader(existingSettings()), roster, args); err == nil {
			t.Errorf("%v was answered", args)
		}
	}
	if _, err := (&Provider{reader: withRoster{rowReader: settingsReader(existingSettings()), roster: roster}}).people(moderatorCtx("mod-1"),
		map[string]any{"storeId": "store-live", "siteId": "site-1"}, 0); err == nil {
		t.Fatal("a node with no resolver answered who can moderate")
	}
}

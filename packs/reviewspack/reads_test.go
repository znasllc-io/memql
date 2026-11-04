package reviewspack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// reads_test.go -- WHAT THE ADMIN PANEL READS: ONE REVIEW, THE DECISION LOG,
// THE SETTINGS.
//
// The moderation list (list_test.go) answers the queue. A moderator who opens
// one review needs its whole decision log and the acts they may take on it
// NOW; the store's log answers who decided what, newest first, across every
// review; and the settings answer what is in force with the defaults applied.
// Each read says what the viewer may do, computed here with the rules the
// writes enforce, so the panel never offers an act the engine would refuse.

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

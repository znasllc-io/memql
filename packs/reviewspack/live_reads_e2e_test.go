package reviewspack_test

// live_reads_e2e_test.go -- THE ADMIN PANEL'S READS AGAINST A REAL DATABASE.
//
// reads_test.go proves reviewsGet, reviewsDecisionLog and reviewsSettings as
// functions over rows. This proves them through the real engine: the DSL
// declarations reach their handlers, the organization gate admits a person
// holding the reviews part and refuses one who does not, and the answers
// carry what the panel draws -- the state, the log, the acts and the settings
// in force.
//
// Postgres-gated: skips when no database is reachable; MEMQL_REQUIRE_DB=1
// turns that skip into a failure.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

func TestLiveE2E_TheAdminPanelReadsOneReviewTheLogAndTheSettings(t *testing.T) {
	eng := livePackEngine(t)
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	merchant := "user-reviews-reads-" + n
	stranger := "user-reviews-reads-nopart-" + n
	store := "store-reviews-reads-" + n
	site := "site-reviews-reads-" + n
	handle := "boot-" + n
	seedSite(t, eng, site, merchant, store)
	grantReviewsPart(t, eng, merchant)

	call := `submitReview(body: "Warm all winter", productHandle: ` + quote(handle) +
		`, rating: 4, siteId: "site-reviews-e2e", storeId: ` + quote(store) +
		`, submissionId: ` + quote("sub-reads-"+n) + `)`
	if _, err := eng.Execute(asMerchant(merchant), call); err != nil {
		t.Fatalf("submitReview: %v", err)
	}
	// Set up by an operator, naming the merchant as the owner who approves removals.
	if _, err := eng.Execute(asPerson(merchant, auth.RoleOwner),
		`builtin reviewsUpdateSettings(storeId: `+quote(store)+`, siteId: `+quote(site)+`, linkLifetimeDays: 21, removalApproverUserId: `+quote(merchant)+`)`); err != nil {
		t.Fatalf("reviewsUpdateSettings: %v", err)
	}
	moderator := asPerson(merchant, auth.Role("user"))
	queue := payloadOf(t, eng, moderator, `builtin reviewsList(storeId: `+quote(store)+`, siteId: `+quote(site)+`)`)
	queued := queue["reviews"].([]any)[0].(map[string]any)
	reviewID := queued["id"].(string)
	if _, err := eng.Execute(moderator, `builtin reviewsDecide(reviewId: `+quote(reviewID)+`, action: "publish", revision: 1, contentHash: `+quote(asText(queued["contentHash"]))+`)`); err != nil {
		t.Fatalf("reviewsDecide: %v", err)
	}

	// ONE REVIEW: its state, its log and the acts this person may take now.
	one := payloadOf(t, eng, moderator, `builtin reviewsGet(reviewId: `+quote(reviewID)+`)`)
	review, _ := one["review"].(map[string]any)
	if review["state"] != "published" || asText(review["productHandle"]) != handle {
		t.Fatalf("reviewsGet review = %v, want it published", review)
	}
	log, _ := one["decisions"].([]any)
	if len(log) != 1 || log[0].(map[string]any)["action"] != "publish" || log[0].(map[string]any)["decidedBy"] != merchant {
		t.Fatalf("reviewsGet decisions = %v, want the one publish by the merchant", log)
	}
	if acts := joined(one["acts"]); acts != "escalate,remove,erase" {
		t.Fatalf("the owner's acts on a published review = %q, want escalate,remove,erase", acts)
	}
	// The cluster's operators read it and may take no act on it.
	staff := payloadOf(t, eng, asPerson(merchant, auth.RoleOwner), `builtin reviewsGet(reviewId: `+quote(reviewID)+`)`)
	if joined(staff["acts"]) != "" || staff["moderates"] != false {
		t.Fatalf("an operator's reviewsGet = %v, want no acts", staff)
	}

	// THE DECISION LOG, newest first, with the review it was about.
	logged := payloadOf(t, eng, moderator, `builtin reviewsDecisionLog(storeId: `+quote(store)+`, siteId: `+quote(site)+`)`)
	entries, _ := logged["decisions"].([]any)
	if len(entries) != 1 {
		t.Fatalf("reviewsDecisionLog = %v, want one entry", logged)
	}
	entry := entries[0].(map[string]any)
	if entry["action"] != "publish" || entry["reviewState"] != "published" || asText(entry["productHandle"]) != handle {
		t.Fatalf("log entry = %v", entry)
	}

	// THE SETTINGS in force, and what this person may change.
	settings := payloadOf(t, eng, moderator, `builtin reviewsSettings(storeId: `+quote(store)+`, siteId: `+quote(site)+`)`)
	values, _ := settings["settings"].(map[string]any)
	viewer, _ := settings["viewer"].(map[string]any)
	if settings["exists"] != true || values["linkLifetimeDays"] != float64(21) || values["requestDelayDays"] != float64(14) ||
		values["publicDisplay"] != false || viewer["approvesRemovals"] != true {
		t.Fatalf("reviewsSettings = %v", settings)
	}

	// A person without the reviews part reads none of it.
	for _, call := range []string{
		`builtin reviewsGet(reviewId: ` + quote(reviewID) + `)`,
		`builtin reviewsDecisionLog(storeId: ` + quote(store) + `, siteId: ` + quote(site) + `)`,
		`builtin reviewsSettings(storeId: ` + quote(store) + `, siteId: ` + quote(site) + `)`,
	} {
		if _, err := eng.Execute(asPerson(stranger, auth.Role("user")), call); err == nil {
			t.Errorf("a person without the reviews part read %s", call)
		}
	}
}

func joined(v any) string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, asText(a))
	}
	return strings.Join(out, ",")
}

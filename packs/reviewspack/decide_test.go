package reviewspack

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// decide_test.go -- A PERSON ON THE STORE'S SIDE DECIDES, AND ONLY ON WHAT
// THEY READ.
//
// reviewsDecide appends one reviewDecision row. These tests pin who may
// record one (never the cluster's own operators: they host the reviews and do
// not moderate them), that it applies to the revision the moderator read and
// no other, and that removal belongs to the one person the store's settings
// name.

// decideReader answers the reads reviewsDecide makes.
type decideReader struct {
	review    map[string]any   // reviewById
	decisions []map[string]any // reviewDecisionsForReview
	settings  map[string]any   // reviewSettingsForStore
	site      map[string]any   // siteById; nil answers the store's own site
}

// liveSite is a site bound to the store the fixtures act on.
// Like a real storefront it carries two bindings: the live store it serves and
// the development store a candidate is previewed against.
var liveSite = map[string]any{"id": "site-1", "ownerUserId": "merchant-1", "accountId": "acct-shop",
	"hostname": "shop.example.com", "binding": map[string]any{"storeId": "store-live"},
	"previewBinding": map[string]any{"storeId": "store-dev"}}

func operatorCtxFor(userID string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: auth.RoleOwner})
}

func (d *decideReader) Rows(_ context.Context, query string, args map[string]string) ([]map[string]any, error) {
	switch query {
	case "reviewById":
		if d.review == nil || canonicalReviewID(asString(d.review["id"])) != canonicalReviewID(args["reviewId"]) {
			return nil, nil
		}
		return []map[string]any{d.review}, nil
	case "reviewDecisionsForReview":
		return d.decisions, nil
	case "reviewSettingsForStore":
		if d.settings == nil {
			return nil, nil
		}
		return []map[string]any{d.settings}, nil
	case "siteById":
		if d.site != nil {
			return []map[string]any{d.site}, nil
		}
		return []map[string]any{liveSite}, nil
	}
	return nil, nil
}

func (d *decideReader) RowsWithList(context.Context, string, map[string]string, string, []string) ([]map[string]any, error) {
	return nil, nil
}

func moderatorCtx(userID string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: auth.Role("user")})
}

func operatorCtx(role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "staff-1", Role: role})
}

func aReview() map[string]any {
	return map[string]any{"id": "r1", "storeId": "store-live", "accountId": "acct-shop", "revision": 2,
		"ownerUserId": "merchant-1", "productHandle": "boot", "body": "Great boots"}
}

func decide(t *testing.T, ctx context.Context, r *decideReader, args map[string]any) (map[string]any, error) {
	t.Helper()
	w := &fakeWriter{}
	_, err := (&Provider{reader: r, writer: w}).decide(ctx, args, 0)
	if err != nil {
		w.none(t)
		return nil, err
	}
	row := w.only(t, "appendReviewDecision")
	return row, nil
}

func TestAModeratorPublishesTheRevisionTheyRead(t *testing.T) {
	row, err := decide(t, moderatorCtx("mod-1"), &decideReader{review: aReview()},
		map[string]any{"reviewId": "r1", "action": "publish", "revision": 2})
	if err != nil {
		t.Fatalf("publish refused: %v", err)
	}
	// OWNED BY THE MERCHANT, decided by the moderator: the public read runs
	// as the site owner and must be able to read every decision on its reviews.
	for k, want := range map[string]any{"reviewId": "v1:reviews:review:r1", "action": "publish", "storeId": "store-live",
		"accountId": "acct-shop", "decidedBy": "mod-1", "ownerUserId": "merchant-1"} {
		if row[k] != want {
			t.Fatalf("%s = %v, want %v (row %v)", k, row[k], want, row)
		}
	}
	if row["revision"] != 2 {
		t.Fatalf("revision = %v, want 2", row["revision"])
	}
	// A NEW ROW per decision, at a bare-slug id (the engine refuses a colon).
	id, _ := row["decisionId"].(string)
	if !strings.HasPrefix(id, "decision-r1-") || strings.Contains(id, ":") {
		t.Fatalf("decisionId = %q, want a new bare-slug id per decision", id)
	}
}

// THE STORE'S STORE IS READ OFF THE REVIEW AND THE DECIDER OFF THE CALLER.
// Neither is an argument a caller could set.
func TestDecidedByAndStoreAreNeverArguments(t *testing.T) {
	row, err := decide(t, moderatorCtx("mod-1"), &decideReader{review: aReview()},
		map[string]any{"reviewId": "r1", "action": "publish", "revision": 2,
			"decidedBy": "someone-else", "storeId": "store-other"})
	if err != nil {
		t.Fatal(err)
	}
	if row["decidedBy"] != "mod-1" || row["storeId"] != "store-live" {
		t.Fatalf("decidedBy=%v storeId=%v, want the caller and the review's own store", row["decidedBy"], row["storeId"])
	}
}

func TestTheClustersOwnOperatorsCannotModerate(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleDeveloper, auth.RoleAdmin} {
		_, err := decide(t, operatorCtx(role), &decideReader{review: aReview()},
			map[string]any{"reviewId": "r1", "action": "publish", "revision": 2})
		if err == nil {
			t.Fatalf("role %s recorded a decision; the people who host the reviews must not decide them", role)
		}
	}
}

func TestACallerWithNoIdentityCannotModerate(t *testing.T) {
	if _, err := decide(t, context.Background(), &decideReader{review: aReview()},
		map[string]any{"reviewId": "r1", "action": "publish", "revision": 2}); err == nil {
		t.Fatal("a decision with no caller was recorded")
	}
}

// A DECISION ON WORDS THE MODERATOR DID NOT READ IS REFUSED. The writer edited
// it while the moderator had it open; they must read it again.
func TestAStaleRevisionIsRefused(t *testing.T) {
	_, err := decide(t, moderatorCtx("mod-1"), &decideReader{review: aReview()},
		map[string]any{"reviewId": "r1", "action": "publish", "revision": 1})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("err = %v, want a refusal saying the review changed", err)
	}
}

func TestAnUnreadableReviewCannotBeDecided(t *testing.T) {
	if _, err := decide(t, moderatorCtx("mod-1"), &decideReader{},
		map[string]any{"reviewId": "r1", "action": "publish", "revision": 2}); err == nil {
		t.Fatal("a decision was recorded against a review the caller cannot read")
	}
}

func TestDeclineNeedsAListedReasonAndKeepsTheNote(t *testing.T) {
	if _, err := decide(t, moderatorCtx("mod-1"), &decideReader{review: aReview()},
		map[string]any{"reviewId": "r1", "action": "decline", "revision": 2}); err == nil {
		t.Fatal("a decline with no reason was recorded")
	}
	row, err := decide(t, moderatorCtx("mod-1"), &decideReader{review: aReview()},
		map[string]any{"reviewId": "r1", "action": "decline", "reason": "personal_information",
			"note": "names a child's school", "revision": 2})
	if err != nil {
		t.Fatal(err)
	}
	if row["reason"] != "personal_information" || row["note"] != "names a child's school" {
		t.Fatalf("row = %v", row)
	}
}

func TestOnlyTheNamedOwnerRemovesAPublishedReview(t *testing.T) {
	r := &decideReader{
		review:    aReview(),
		decisions: []map[string]any{decisionRow("r1", ActionPublish, 2, 0)},
		settings:  map[string]any{"storeId": "store-live", "removalApproverUserId": "owner-1"},
	}
	args := map[string]any{"reviewId": "r1", "action": "remove", "reason": "reviewer_request", "revision": 2}
	if _, err := decide(t, moderatorCtx("mod-1"), r, args); err == nil {
		t.Fatal("a moderator removed a published review; only the owner the settings name may")
	}
	if _, err := decide(t, moderatorCtx("owner-1"), r, args); err != nil {
		t.Fatalf("the named owner was refused: %v", err)
	}
	// No owner named: nobody can remove.
	r.settings = nil
	if _, err := decide(t, moderatorCtx("owner-1"), r, args); err == nil {
		t.Fatal("with no owner named, a removal must be refused rather than allowed to anyone")
	}
}

func TestEraseIsNotADecisionThisBuiltinRecords(t *testing.T) {
	if _, err := decide(t, moderatorCtx("owner-1"), &decideReader{review: aReview(),
		settings: map[string]any{"removalApproverUserId": "owner-1"}},
		map[string]any{"reviewId": "r1", "action": "erase", "revision": 2}); err == nil {
		t.Fatal("erase must go through reviewsEraseReviewer, which also blanks the saved versions")
	}
}

// AN OPTIONAL FIELD WITH NOTHING IN IT IS NOT SENT. The write mutation checks
// reason against the closed set, and "" is not a member: a publish names no
// reason, so the call carries none.
func TestAPublishSendsNoReasonOrNote(t *testing.T) {
	row, err := decide(t, moderatorCtx("mod-1"), &decideReader{review: aReview()},
		map[string]any{"reviewId": "r1", "action": "publish", "revision": 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"reason", "note"} {
		if v, sent := row[k]; sent {
			t.Fatalf("%s = %q was sent with a publish; an empty optional field must be omitted", k, v)
		}
	}
}

func TestADecisionRecordsTheFullReviewID(t *testing.T) {
	r := aReview()
	r["id"] = "v1:reviews:review:r1"
	row, err := decide(t, moderatorCtx("mod-1"), &decideReader{review: r},
		map[string]any{"reviewId": "r1", "action": "publish", "revision": 2})
	if err != nil {
		t.Fatal(err)
	}
	if row["reviewId"] != "v1:reviews:review:r1" {
		t.Fatalf("reviewId = %v, want the full id every query answers and every reader compares against", row["reviewId"])
	}
}

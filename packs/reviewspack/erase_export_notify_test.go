package reviewspack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// erase_export_notify_test.go -- ERASING A WRITER'S DATA, EXPORTING A STORE'S
// RECORD, AND TELLING THE MODERATOR A REVIEW IS WAITING.

type fakeRedactor struct {
	calls []redaction
}

type redaction struct {
	concept, id string
	fields      map[string]string
}

func (f *fakeRedactor) Redact(_ context.Context, concept, id string, fields map[string]string) (int64, error) {
	f.calls = append(f.calls, redaction{concept, id, fields})
	return 2, nil
}

func eraseFixture(decisions []map[string]any) (*Provider, *fakeWriter, *fakeRedactor) {
	r := aReview()
	r["requestId"] = "request-abc"
	w, red := &fakeWriter{}, &fakeRedactor{}
	reader := &decideReader{review: r, decisions: decisions, settings: map[string]any{"removalApproverUserId": "owner-1"}}
	return &Provider{coordinateRequest: fakeRequestCoordinator, reader: reader, writer: w, redactor: red}, w, red
}

func TestOnlyTheNamedOwnerErasesAndNothingIsTouchedBeforeThat(t *testing.T) {
	p, w, red := eraseFixture(nil)
	for _, ctx := range []context.Context{moderatorCtx("mod-1"), operatorCtxFor("staff-1")} {
		if _, err := p.eraseReviewer(ctx, map[string]any{"reviewId": "r1"}, 0); err == nil {
			t.Fatal("someone other than the named owner erased a writer's data")
		}
	}
	w.none(t)
	if len(red.calls) != 0 {
		t.Fatalf("a refused erasure rewrote rows: %+v", red.calls)
	}
}

// THE DECISION FIRST, so the review is hidden the moment anything is changed:
// a blanked review must never be shown as an empty published one.
func TestErasureHidesTheReviewThenBlanksEveryVersion(t *testing.T) {
	p, w, red := eraseFixture([]map[string]any{decisionRow("r1", ActionPublish, 2, 0)})
	nodes, err := p.eraseReviewer(moderatorCtx("owner-1"), map[string]any{"reviewId": "r1", "note": "asked by email"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d := w.only(t, "appendReviewDecision"); d["action"] != "erase" || d["note"] != "asked by email" {
		t.Fatalf("decision = %v, want an erase with its note", d)
	}
	if len(red.calls) != 2 {
		t.Fatalf("redactions = %+v, want the review and its link", red.calls)
	}
	review, link := red.calls[0], red.calls[1]
	if review.concept != "v1:reviews:review" || review.id != "v1:reviews:review:r1" {
		t.Fatalf("first redaction = %+v, want the review row at its full id", review)
	}
	for _, f := range []string{"authorName", "authorEmail", "title", "body"} {
		if v, ok := review.fields[f]; !ok || v != "" {
			t.Fatalf("review field %s is not blanked: %+v", f, review.fields)
		}
	}
	if review.fields["erasedAt"] == "" {
		t.Fatal("the review does not record when it was erased")
	}
	if link.concept != "v1:reviews:reviewRequest" || link.id != "v1:reviews:reviewRequest:request-abc" || link.fields["email"] != "" {
		t.Fatalf("second redaction = %+v, want the link's email blanked", link)
	}
	if !strings.Contains(string(nodes[0].Payload), `"state":"erased"`) {
		t.Fatalf("answer = %s", nodes[0].Payload)
	}
}

// A HALF-DONE ERASURE CAN BE FINISHED: the decision stands, so a second run
// rewrites the rows again without recording a second decision.
func TestErasingAnErasedReviewRedactsAgainWithoutASecondDecision(t *testing.T) {
	p, w, red := eraseFixture([]map[string]any{decisionRow("r1", ActionErase, 2, 0)})
	if _, err := p.eraseReviewer(moderatorCtx("owner-1"), map[string]any{"reviewId": "r1"}, 0); err != nil {
		t.Fatal(err)
	}
	w.none(t)
	if len(red.calls) != 2 {
		t.Fatalf("redactions = %+v, want the rows rewritten again", red.calls)
	}
}

// ---------------------------------------------------------------- export

type exportReader struct{ listReader }

func (e *exportReader) Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	switch query {
	case "reviewRequestsForStore":
		return []map[string]any{{"id": "v1:reviews:reviewRequest:request-abc", "email": "buyer@example.com", "tokenHash": "secret-hash", "tokenCiphertext": "sealed-retry-material", "productHandle": "boot"}}, nil
	case "reviewSettingsForStore":
		return []map[string]any{{"storeId": "store-live", "notifyEmail": "support@example.com", "accountId": "acct-shop"}}, nil
	}
	return e.listReader.Rows(ctx, query, args)
}

func TestTheExportIsTheStoresWholeRecord(t *testing.T) {
	r := &exportReader{listReader{
		reviews:   []map[string]any{listRow("a", 0), listRow("b", 5)},
		decisions: []map[string]any{decisionRow("a", ActionPublish, 1, 0)},
	}}
	nodes, err := (&Provider{reader: r}).exportStore(operatorCtxFor("staff-1"), map[string]any{"storeId": "store-live", "siteId": "site-1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		FileName string `json:"fileName"`
		JSON     string `json:"json"`
	}
	if err := json.Unmarshal(nodes[0].Payload, &answer); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(answer.FileName, ".json") || !strings.HasPrefix(answer.FileName, "reviews-") {
		t.Fatalf("fileName = %q", answer.FileName)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(answer.JSON), &doc); err != nil {
		t.Fatalf("the export is not JSON: %v", err)
	}
	if len(doc["reviews"].([]any)) != 2 || len(doc["decisions"].([]any)) != 1 || len(doc["requests"].([]any)) != 1 || doc["settings"] == nil {
		t.Fatalf("export = %v, want every review, decision, link and the settings", doc)
	}
	first := doc["reviews"].([]any)[0].(map[string]any)
	if first["state"] == nil {
		t.Fatal("an exported review does not carry its state")
	}
	if strings.Contains(answer.JSON, "secret-hash") || strings.Contains(answer.JSON, "tokenHash") || strings.Contains(answer.JSON, "tokenCiphertext") || strings.Contains(answer.JSON, "sealed-retry-material") {
		t.Fatal("the export carries private link material")
	}
}

func TestTheExportRefusesASiteNotBoundToTheStore(t *testing.T) {
	r := &exportReader{listReader{site: map[string]any{"binding": map[string]any{"storeId": "store-other"}}}}
	if _, err := (&Provider{reader: r}).exportStore(operatorCtxFor("staff-1"), map[string]any{"storeId": "store-live", "siteId": "site-x"}, 0); err == nil {
		t.Fatal("a store was exported through a site that does not serve it")
	}
}

// ---------------------------------------------------------------- notify

// fromAutomation is the context a store's trusted automation calls with.
func fromAutomation() context.Context {
	return auth.ContextWithInternalOrigin(context.Background())
}

func notifyProvider(settings map[string]any, decisions []map[string]any) (*Provider, *fakeWriter) {
	w := &fakeWriter{}
	r := aReview()
	r["productHandle"] = "floss-picks"
	r["authorName"] = "Dana"
	return &Provider{reader: &decideReader{review: r, decisions: decisions, settings: settings}, writer: w,
		osBase: func() string { return "https://os.example.test" }}, w
}

func TestAWaitingReviewMailsTheModeratorOncePerRevision(t *testing.T) {
	p, w := notifyProvider(map[string]any{"notifyEmail": "support@example.com"}, nil)
	if _, err := p.notifyModerator(fromAutomation(), map[string]any{"reviewId": "r1"}, 0); err != nil {
		t.Fatal(err)
	}
	mail := w.only(t, "stageOutboundRequest")
	if mail["target"] != "support@example.com" || mail["medium"] != "email" {
		t.Fatalf("mail = %v", mail)
	}
	if mail["dedupeKey"] != mail["requestId"] || !strings.Contains(asString(mail["requestId"]), "-2-") {
		t.Fatalf("mail = %v, want one stable id per review revision, deduplicated", mail)
	}
	body := asString(mail["body"]) + asString(mail["subject"])
	if !strings.Contains(body, "floss-picks") || !strings.Contains(body, "https://os.example.test") {
		t.Fatalf("mail = %v, want the product and the admin panel's address", mail)
	}
	if strings.Contains(body, "Great boots") || strings.Contains(body, "Dana") {
		t.Fatalf("mail = %v: it must carry no review text and nothing about the writer", mail)
	}
}

func TestNoAddressOrNoLongerWaitingMailsNobody(t *testing.T) {
	p, w := notifyProvider(map[string]any{}, nil)
	if _, err := p.notifyModerator(fromAutomation(), map[string]any{"reviewId": "r1"}, 0); err != nil {
		t.Fatal(err)
	}
	w.none(t)
	p, w = notifyProvider(map[string]any{"notifyEmail": "support@example.com"}, []map[string]any{decisionRow("r1", ActionPublish, 2, 0)})
	if _, err := p.notifyModerator(fromAutomation(), map[string]any{"reviewId": "r1"}, 0); err != nil {
		t.Fatal(err)
	}
	w.none(t)
}

// THE NOTICE IS FOR THE STORE'S OWN AUTOMATION: a client calling it could mail
// a store's moderator about any review it names, so a call without internal
// origin is refused before anything is read.
func TestTheNoticeRefusesAClient(t *testing.T) {
	p, w := notifyProvider(map[string]any{"notifyEmail": "support@example.com"}, nil)
	if _, err := p.notifyModerator(moderatorCtx("mod-1"), map[string]any{"reviewId": "r1"}, 0); err == nil {
		t.Fatal("a client call staged a moderator email")
	}
	w.none(t)
}

// ---------------------------------------------------------------- organizations

// A STORE-WIDE ACT IS JUDGED AGAINST THE SITE IT NAMES, so the site must be
// the store's own: once the store's settings carry an organization, a site of
// another organization -- even one bound to the same store -- is refused.
func TestAStoreWideActRefusesASiteOfAnotherOrganization(t *testing.T) {
	other := map[string]any{"id": "site-x", "ownerUserId": "merchant-1", "accountId": "acct-other",
		"binding": map[string]any{"storeId": "store-live"}}
	settings := existingSettings() // accountId acct-shop
	op := operatorCtxFor("staff-1")
	if _, err := (&Provider{reader: &decideReader{settings: settings, site: other}, writer: &fakeWriter{}}).updateSettings(op,
		map[string]any{"storeId": "store-live", "siteId": "site-x", "notifyEmail": "a@example.com"}, 0); err == nil {
		t.Error("settings were changed through another organization's site")
	}
	r := &exportReader{listReader{site: other}}
	if _, err := (&Provider{reader: r}).exportStore(op, map[string]any{"storeId": "store-live", "siteId": "site-x"}, 0); err == nil {
		t.Error("a store was exported through another organization's site")
	}
}

func TestSettingsTakeTheSitesOrganizationWhenSetUp(t *testing.T) {
	w := &fakeWriter{}
	p := &Provider{reader: &decideReader{site: liveSite}, writer: w}
	if _, err := p.updateSettings(operatorCtxFor("staff-1"), map[string]any{"storeId": "store-live", "siteId": "site-1", "notifyEmail": "a@example.com"}, 0); err != nil {
		t.Fatal(err)
	}
	if got := w.only(t, "putReviewSettings")["accountId"]; got != "acct-shop" {
		t.Fatalf("accountId = %v, want the site's organization recorded on the store's settings", got)
	}
}

// THE ORGANIZATION IS THE OPERATORS' TO SET. A store's own member updating
// settings that carry no organization yet does not tie them to their site's:
// the first site to touch a store must not decide whose store it is.
func TestOnlyAnOperatorRecordsTheStoresOrganization(t *testing.T) {
	untied := existingSettings()
	delete(untied, "accountId")
	w := &fakeWriter{}
	p := &Provider{reader: &decideReader{settings: untied, site: liveSite}, writer: w}
	if _, err := p.updateSettings(moderatorCtx("mod-1"), map[string]any{"storeId": "store-live", "siteId": "site-1", "notifyEmail": "a@example.com"}, 0); err != nil {
		t.Fatal(err)
	}
	if got, set := w.only(t, "putReviewSettings")["accountId"]; set {
		t.Fatalf("a member's update tied the store to organization %v", got)
	}
}

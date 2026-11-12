package wholesalepack

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// documents_test.go -- a store's terms, MAP policy and price sheet as
// append-only versions, the three settings beside them, and the rule that
// applications open only once the three documents exist, as decisions over
// rows behind the reader and writer seams.

// docsClock is when the first version in these tests is published.
var docsClock = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// docsHarness is a provider over one store, store-live, served by
// settingsSite (organization acct-north, merchant-1). Every version the
// provider appends is held by the reader at once, at a generated id, as the
// engine holds it.
type docsHarness struct {
	p      *Provider
	reader *fixtureReader
	writer *fakeWriter
	at     time.Time
}

func newDocsHarness(settings map[string]any) *docsHarness {
	h := &docsHarness{reader: &fixtureReader{sites: []map[string]any{settingsSite()}}, at: docsClock}
	if settings != nil {
		h.reader.settings = map[string][]map[string]any{"store-live": {settings}}
	}
	h.writer = &fakeWriter{inserted: func(mutation string, args map[string]any) string {
		id := documentVersionConcept + ":dv-" + strconv.Itoa(len(h.reader.docs)+1)
		row := map[string]any{"id": id, "createdAt": rowTime(args["publishedAt"])}
		for k, v := range args {
			row[k] = v
		}
		h.reader.docs = append(h.reader.docs, row)
		return id
	}}
	h.p = &Provider{reader: h.reader, writer: h.writer, now: func() time.Time {
		h.at = h.at.Add(time.Minute)
		return h.at
	}}
	return h
}

// publish is wholesalePublishDocument through site-1 on store-live.
func (h *docsHarness) publish(ctx context.Context, kind, label, body string) ([]byte, error) {
	nodes, err := h.p.publishDocument(ctx, map[string]any{"siteId": "site-1", "storeId": "store-live",
		"kind": kind, "label": label, "body": body}, 0)
	if err != nil {
		return nil, err
	}
	return nodes[0].Payload, nil
}

func (h *docsHarness) mustPublish(t *testing.T, kind, label, body string) map[string]any {
	t.Helper()
	raw, err := h.publish(staffCtx("v1:identity:user:staff-1"), kind, label, body)
	if err != nil {
		t.Fatalf("publish %s: %v", kind, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	return out
}

// openSettings is a settings row of store-live, open or closed, with
// termsRequired as given (absent when nil).
func openSettings(open bool, termsRequired any) map[string]any {
	row := map[string]any{"storeId": "store-live", "ownerUserId": "merchant-1", "applicationsOpen": open,
		"accountId": "acct-north", "siteId": "site-1", "createdAt": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if termsRequired != nil {
		row["termsRequired"] = termsRequired
	}
	return row
}

// THE FINGERPRINT IS OF THE BODY'S BYTES, and the body is kept exactly as given.
func TestADocumentVersionHoldsTheFingerprintOfItsBody(t *testing.T) {
	h := newDocsHarness(nil)
	got := h.mustPublish(t, DocumentTerms, "Terms 2026", "hello")
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	wrote := h.writer.only(t, "appendWholesaleDocumentVersion")
	if wrote["sha256"] != want || got["sha256"] != want {
		t.Fatalf("sha256 written %v, answered %v, want %s", wrote["sha256"], got["sha256"], want)
	}
	if got["documentVersionId"] != documentVersionConcept+":dv-1" {
		t.Fatalf("the receipt names version %v, want the id the engine generated", got["documentVersionId"])
	}
	if len(got) != 2 {
		t.Fatalf("the receipt is %+v, want {documentVersionId, sha256}", got)
	}
	// AS GIVEN: no trimming, no normalisation, because the digest is over exactly
	// these bytes.
	h.writer.writes = nil
	body := "  # Price sheet\r\n\n| Frame | 410.00 |\n  "
	h.mustPublish(t, DocumentPriceSheet, "  Spring list  ", body)
	wrote = h.writer.only(t, "appendWholesaleDocumentVersion")
	if wrote["body"] != body || wrote["label"] != "Spring list" {
		t.Fatalf("wrote body %q label %q, want the body as given and the label trimmed", wrote["body"], wrote["label"])
	}
	if wrote["sha256"] != sha256Hex(body) || len(asString(wrote["sha256"])) != 64 || strings.ToLower(asString(wrote["sha256"])) != wrote["sha256"] {
		t.Fatalf("sha256 %v is not the lowercase hex digest of the body as given", wrote["sha256"])
	}
}

// THE VERSION IS THE MERCHANT'S AND THE ORGANIZATION'S, PUBLISHED BY THE CALLER:
// owned by the site's owner, tied to the site's organization, written as the
// merchant, at the engine's clock, with every argument named.
func TestADocumentIsOwnedByTheMerchantAndPublishedByTheCaller(t *testing.T) {
	h := newDocsHarness(nil)
	h.mustPublish(t, DocumentMap, "MAP policy", "Advertise at list.")
	call := h.writer.writes[0]
	want := map[string]any{
		"ownerUserId": "v1:identity:user:merchant-1", "storeId": "store-live", "accountId": "acct-north",
		"kind": DocumentMap, "label": "MAP policy", "body": "Advertise at list.", "sha256": sha256Hex("Advertise at list."),
		"publishedBy": "v1:identity:user:staff-1", "publishedAt": docsClock.Add(time.Minute).Format(time.RFC3339Nano),
	}
	for k, v := range want {
		if call.args[k] != v {
			t.Errorf("%s = %v, want %v", k, call.args[k], v)
		}
	}
	if len(call.args) != len(want) {
		t.Errorf("the write names %d arguments, want every one (%d): %+v", len(call.args), len(want), call.args)
	}
	if call.actor != "v1:identity:user:merchant-1" {
		t.Fatalf("the version was written as %q, want the store's merchant", call.actor)
	}
	// A BARE CALLER ID IS RECORDED IN THE FORM A PERSON'S ID IS HELD IN.
	h.writer.writes = nil
	if _, err := h.publish(staffCtx("staff-2"), DocumentMap, "MAP policy", "v2"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := h.writer.writes[0].args["publishedBy"]; got != "v1:identity:user:staff-2" {
		t.Fatalf("publishedBy = %v, want the caller in canonical form", got)
	}
	// A SITE THAT DOES NOT SERVE THE STORE publishes nothing.
	h.writer.writes = nil
	h.reader.sites = append(h.reader.sites, map[string]any{"id": "site-x", "ownerUserId": "merchant-1", "accountId": "acct-north",
		"binding": map[string]any{"storeId": "store-other"}})
	if _, err := h.p.publishDocument(staffCtx("staff-1"), map[string]any{"siteId": "site-x", "storeId": "store-live",
		"kind": DocumentTerms, "label": "Terms", "body": "x"}, 0); err == nil || len(h.writer.writes) != 0 {
		t.Fatalf("a publish through a site that does not serve the store: err %v, writes %d", err, len(h.writer.writes))
	}
}

// VERSIONS ARE APPENDED, NEVER UPDATED, and the newest of a kind is current.
func TestVersionsAreAppendOnlyAndTheNewestIsCurrent(t *testing.T) {
	h := newDocsHarness(nil)
	first := h.mustPublish(t, DocumentTerms, "Terms v1", "first")
	second := h.mustPublish(t, DocumentTerms, "Terms v2", "second")
	if len(h.writer.writes) != 2 || h.writer.count("appendWholesaleDocumentVersion") != 2 {
		t.Fatalf("two publishes wrote %+v, want two appended versions and nothing else", h.writer.writes)
	}
	if first["documentVersionId"] == second["documentVersionId"] {
		t.Fatalf("the second version replaced the first at %v", first["documentVersionId"])
	}
	current, err := h.p.currentDocuments(context.Background(), "store-live")
	if err != nil {
		t.Fatalf("currentDocuments: %v", err)
	}
	if len(current) != 1 || current[DocumentTerms]["label"] != "Terms v2" {
		t.Fatalf("currentDocuments = %+v, want only terms, at its newest version", current)
	}

	got := decodedBy(t)(h.p.documents(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live"}, 0))
	kinds, _ := got["kinds"].([]any)
	if len(kinds) != 3 {
		t.Fatalf("kinds = %+v, want terms, map and priceSheet", got["kinds"])
	}
	for i, want := range []string{DocumentTerms, DocumentMap, DocumentPriceSheet} {
		if k, _ := kinds[i].(map[string]any); k["kind"] != want {
			t.Fatalf("kinds[%d] is %v, want %s", i, k["kind"], want)
		}
	}
	terms, _ := kinds[0].(map[string]any)
	cur, _ := terms["current"].(map[string]any)
	if cur["id"] != second["documentVersionId"] || cur["body"] != "second" || cur["label"] != "Terms v2" ||
		cur["publishedBy"] != "v1:identity:user:staff-1" || cur["sha256"] != sha256Hex("second") ||
		cur["publishedAt"] != docsClock.Add(2*time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("current terms = %+v", cur)
	}
	if _, named := cur["publishedByName"]; !named {
		t.Fatalf("current terms names no publishedByName: %+v", cur)
	}
	history, _ := terms["history"].([]any)
	if len(history) != 2 {
		t.Fatalf("history = %+v, want both versions", history)
	}
	for i, wantID := range []any{second["documentVersionId"], first["documentVersionId"]} {
		v, _ := history[i].(map[string]any)
		if v["id"] != wantID {
			t.Fatalf("history[%d] is %v, want %v (newest first)", i, v["id"], wantID)
		}
		if _, carries := v["body"]; carries {
			t.Fatalf("history[%d] carries its body", i)
		}
	}
	for _, k := range kinds[1:] {
		m, _ := k.(map[string]any)
		if h, _ := m["history"].([]any); m["current"] != nil || len(h) != 0 {
			t.Fatalf("%v has no version and answered %+v", m["kind"], m)
		}
	}

	// ONE VERSION, BODY INCLUDED, by its id in either form.
	for _, asked := range []string{asString(first["documentVersionId"]), bareID(asString(first["documentVersionId"]))} {
		one := decodedBy(t)(h.p.documents(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
			"versionId": asked}, 0))
		v, _ := one["version"].(map[string]any)
		if v["id"] != first["documentVersionId"] || v["body"] != "first" || v["kind"] != DocumentTerms || v["label"] != "Terms v1" {
			t.Fatalf("version %q = %+v", asked, one)
		}
	}
}

// A VERSION OF ANOTHER STORE IS ANSWERED EXACTLY AS AN UNKNOWN ONE.
func TestAVersionOfAnotherStoreIsAnsweredAsAnUnknownOne(t *testing.T) {
	h := newDocsHarness(nil)
	h.reader.docs = append(h.reader.docs, map[string]any{"id": documentVersionConcept + ":dv-other", "storeId": "store-other",
		"kind": DocumentTerms, "label": "Theirs", "body": "theirs", "createdAt": docsClock})
	ask := func(versionID string) error {
		_, err := h.p.documents(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
			"versionId": versionID}, 0)
		return err
	}
	other, unknown := ask(documentVersionConcept+":dv-other"), ask(documentVersionConcept+":dv-nowhere")
	if other == nil || unknown == nil {
		t.Fatalf("another store's version answered %v, an unknown one %v; both must be refused", other, unknown)
	}
	if strings.ReplaceAll(other.Error(), "dv-other", "X") != strings.ReplaceAll(unknown.Error(), "dv-nowhere", "X") {
		t.Fatalf("another store's version is told apart from an unknown one:\n  %v\n  %v", other, unknown)
	}
}

// A BODY OVER 200 KB IS REFUSED before anything is read; 200 KB exactly, and an
// empty body is refused.
func TestABodyOver200KBIsRefused(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{reader: refusingReader{t: t}, writer: writer}
	for name, body := range map[string]string{"204801 bytes": strings.Repeat("a", 204801), "empty": ""} {
		if _, err := p.publishDocument(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
			"kind": DocumentTerms, "label": "Terms", "body": body}, 0); err == nil {
			t.Errorf("a body of %s was accepted", name)
		}
	}
	// Bytes, not characters: 102401 two-byte characters are 204802 bytes.
	if _, err := p.publishDocument(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"kind": DocumentTerms, "label": "Terms", "body": strings.Repeat("é", 102401)}, 0); err == nil ||
		!strings.Contains(err.Error(), "204800 bytes") {
		t.Errorf("a body of 204802 bytes in fewer characters: %v", err)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("a refused body was written: %d writes", len(writer.writes))
	}
	h := newDocsHarness(nil)
	h.mustPublish(t, DocumentTerms, "Terms", strings.Repeat("a", 204800))
}

// A LABEL IS 1 TO 120 CHARACTERS ONCE TRIMMED.
func TestALabelOver120CharactersIsRefused(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{reader: refusingReader{t: t}, writer: writer}
	for name, label := range map[string]string{"121 characters": strings.Repeat("a", 121), "blank": "   "} {
		if _, err := p.publishDocument(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
			"kind": DocumentTerms, "label": label, "body": "x"}, 0); err == nil {
			t.Errorf("a label of %s was accepted", name)
		}
	}
	if len(writer.writes) != 0 {
		t.Fatalf("a refused label was written: %d writes", len(writer.writes))
	}
	// Characters, not bytes: 120 two-byte characters are a legal label.
	h := newDocsHarness(nil)
	h.mustPublish(t, DocumentTerms, "  "+strings.Repeat("é", 120)+"  ", "x")
	if got := h.writer.only(t, "appendWholesaleDocumentVersion")["label"]; got != strings.Repeat("é", 120) {
		t.Fatalf("label written %q", got)
	}
}

// A KIND OUTSIDE THE THREE IS REFUSED BEFORE ANYTHING IS READ OR WRITTEN, by a
// sentence naming the three.
func TestAnUnknownKindIsRefusedBeforeAnythingIsRead(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{reader: refusingReader{t: t}, writer: writer}
	_, err := p.publishDocument(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"kind": "returnsPolicy", "label": "Returns", "body": "x"}, 0)
	if err == nil || !strings.Contains(err.Error(), "terms, map or priceSheet") {
		t.Fatalf("an unknown kind: %v", err)
	}
	if len(writer.writes) != 0 {
		t.Fatalf("an unknown kind was written")
	}
}

// A PUBLISH NEEDS A SIGNED-IN PERSON: the version names who published it.
func TestAPublishWithNoSignedInPersonIsRefused(t *testing.T) {
	h := newDocsHarness(nil)
	if _, err := h.publish(context.Background(), DocumentTerms, "Terms", "x"); err == nil || len(h.writer.writes) != 0 {
		t.Fatalf("a publish with no person: err %v, writes %d", err, len(h.writer.writes))
	}
}

// docsFor is one current version of each kind named, on store-live.
func docsFor(kinds ...string) []map[string]any {
	var out []map[string]any
	for i, kind := range kinds {
		out = append(out, map[string]any{"id": documentVersionConcept + ":seed-" + kind, "storeId": "store-live", "kind": kind,
			"label": kind + " label", "body": kind + " body", "sha256": sha256Hex(kind + " body"),
			"createdAt": docsClock.Add(time.Duration(i) * time.Second)})
	}
	return out
}

// OPENING APPLICATIONS WHILE THE TERMS ARE REQUIRED is refused until the terms,
// the MAP policy and the price sheet each have a version -- by an update and by
// a set-up alike.
func TestOpeningWithTermsRequiredIsRefusedWithoutAllThreeDocuments(t *testing.T) {
	h := newDocsHarness(openSettings(false, true))
	h.reader.docs = docsFor(DocumentTerms, DocumentMap)
	_, err := h.p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"applicationsOpen": true}, 0)
	if err == nil || err.Error() != "Publish the terms, the MAP policy and the price sheet first." {
		t.Fatalf("opening with the price sheet missing: %v", err)
	}
	_, err = h.p.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"applicationsOpen": true}, 0)
	if err == nil || err.Error() != "Publish the terms, the MAP policy and the price sheet first." {
		t.Fatalf("a set-up opening with the price sheet missing: %v", err)
	}
	if len(h.writer.writes) != 0 {
		t.Fatalf("a refused opening was written: %+v", h.writer.writes)
	}
	h.reader.docs = docsFor(DocumentTerms, DocumentMap, DocumentPriceSheet)
	if _, err := h.p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"applicationsOpen": true}, 0); err != nil {
		t.Fatalf("opening with all three documents: %v", err)
	}
	wrote := h.writer.only(t, "putWholesaleSettingsRow")
	if wrote["applicationsOpen"] != true || wrote["termsRequired"] != true {
		t.Fatalf("the opening wrote %+v", wrote)
	}
	if _, err := h.p.setWholesaleSettings(operatorCtx("operator-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"applicationsOpen": true}, 0); err != nil {
		t.Fatalf("a set-up opening with all three documents: %v", err)
	}
	if set := h.writer.only(t, "setWholesaleSettingsRow"); set["termsRequired"] != true {
		t.Fatalf("the set-up did not keep termsRequired: %+v", set)
	}
}

// TURNING THE TERMS ON WHILE OPEN is refused until all three documents exist;
// turning them on while closing in the same change is not, because the rule is
// judged on the row as it would be after the change.
func TestTurningOnTermsWhileOpenIsRefusedWithoutAllThreeDocuments(t *testing.T) {
	h := newDocsHarness(openSettings(true, nil))
	_, err := h.p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"termsRequired": true}, 0)
	if err == nil || err.Error() != "Publish the terms, the MAP policy and the price sheet first." {
		t.Fatalf("turning the terms on with no documents: %v", err)
	}
	h.reader.docs = docsFor(DocumentMap, DocumentPriceSheet)
	if _, err := h.p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"termsRequired": true}, 0); err == nil || err.Error() != "Publish the terms, the MAP policy and the price sheet first." {
		t.Fatalf("turning the terms on with the terms missing: %v", err)
	}
	if len(h.writer.writes) != 0 {
		t.Fatalf("a refused change was written: %+v", h.writer.writes)
	}
	if _, err := h.p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"termsRequired": true, "applicationsOpen": false}, 0); err != nil {
		t.Fatalf("turning the terms on while closing: %v", err)
	}
	h.writer.writes = nil
	h.reader.docs = docsFor(DocumentTerms, DocumentMap, DocumentPriceSheet)
	if _, err := h.p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"termsRequired": true}, 0); err != nil {
		t.Fatalf("turning the terms on with all three: %v", err)
	}
	if wrote := h.writer.only(t, "putWholesaleSettingsRow"); wrote["termsRequired"] != true || wrote["applicationsOpen"] != true {
		t.Fatalf("turning the terms on wrote %+v", wrote)
	}
	// termsRequired must be a bool.
	if _, err := h.p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"termsRequired": "yes"}, 0); err == nil {
		t.Fatal("termsRequired \"yes\" was accepted")
	}
}

// TERMS REQUIRED, ABSENT, IS FALSE.
func TestTermsRequiredAbsentMeansFalse(t *testing.T) {
	if s := EffectiveSettings(openSettings(true, nil)); s.TermsRequired {
		t.Fatal("EffectiveSettings reads an absent termsRequired as true")
	}
	if s := EffectiveSettings(openSettings(true, true)); !s.TermsRequired {
		t.Fatal("EffectiveSettings does not read termsRequired true")
	}
	h := newDocsHarness(openSettings(true, nil))
	got := decodedBy(t)(h.p.storeSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live"}, 0))
	if got["termsRequired"] != false {
		t.Fatalf("termsRequired answered %v, want false", got["termsRequired"])
	}
	// An open store with no documents and no termsRequired takes applications.
	if err := h.p.termsRuleRefusal(context.Background(), "store-live", true, EffectiveSettings(openSettings(true, nil)).TermsRequired); err != nil {
		t.Fatalf("termsRuleRefusal with termsRequired absent: %v", err)
	}
}

// A LINK LIFETIME IS 1 TO 365 DAYS.
func TestLinkLifetimeOutsideOneTo365IsRefused(t *testing.T) {
	h := newDocsHarness(openSettings(false, nil))
	for _, bad := range []any{0, 366, -1, 1.5, "30"} {
		if _, err := h.p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
			"linkLifetimeDays": bad}, 0); err == nil {
			t.Errorf("linkLifetimeDays %v (%T) was accepted", bad, bad)
		}
	}
	if len(h.writer.writes) != 0 {
		t.Fatalf("a refused lifetime was written: %+v", h.writer.writes)
	}
	for _, good := range []any{1, 365, float64(90), json.Number("7")} {
		h.writer.writes = nil
		if _, err := h.p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
			"linkLifetimeDays": good}, 0); err != nil {
			t.Fatalf("linkLifetimeDays %v: %v", good, err)
		}
		want, _ := intArg(good)
		if got := h.writer.only(t, "putWholesaleSettingsRow")["linkLifetimeDays"]; got != want {
			t.Fatalf("linkLifetimeDays %v was written as %v (%T)", good, got, got)
		}
	}
}

// THE ACCEPTANCE SENTENCE DEFAULTS, and so does the link lifetime; an empty
// sentence goes back to the default.
func TestTheAcceptanceSentenceDefaults(t *testing.T) {
	if DefaultAcceptanceText != "I have read and accept the Wholesale Terms and Conditions and the MAP Policy shown above." {
		t.Fatalf("DefaultAcceptanceText = %q", DefaultAcceptanceText)
	}
	s := EffectiveSettings(openSettings(true, nil))
	if s.AcceptanceText != DefaultAcceptanceText || s.LinkLifetimeDays != DefaultLinkLifetimeDays || DefaultLinkLifetimeDays != 30 {
		t.Fatalf("absent settings read %q and %d days", s.AcceptanceText, s.LinkLifetimeDays)
	}
	h := newDocsHarness(openSettings(true, nil))
	ask := func() map[string]any {
		return decodedBy(t)(h.p.storeSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live"}, 0))
	}
	if got := ask(); got["acceptanceText"] != DefaultAcceptanceText || got["linkLifetimeDays"] != float64(30) {
		t.Fatalf("storeSettings answered %q and %v days", got["acceptanceText"], got["linkLifetimeDays"])
	}
	if _, err := h.p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"acceptanceText": "We accept the terms above."}, 0); err != nil {
		t.Fatalf("updateSettings: %v", err)
	}
	wrote := h.writer.only(t, "putWholesaleSettingsRow")
	if wrote["acceptanceText"] != "We accept the terms above." {
		t.Fatalf("acceptanceText written %v", wrote["acceptanceText"])
	}
	h.reader.settings["store-live"][0]["acceptanceText"] = wrote["acceptanceText"]
	h.reader.settings["store-live"][0]["linkLifetimeDays"] = float64(14)
	if got := ask(); got["acceptanceText"] != "We accept the terms above." || got["linkLifetimeDays"] != float64(14) {
		t.Fatalf("storeSettings answered %q and %v days", got["acceptanceText"], got["linkLifetimeDays"])
	}
	h.writer.writes = nil
	if _, err := h.p.updateSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live",
		"acceptanceText": ""}, 0); err != nil {
		t.Fatalf("updateSettings: %v", err)
	}
	if got := h.writer.only(t, "putWholesaleSettingsRow"); got["acceptanceText"] != "" || got["linkLifetimeDays"] != 14 {
		t.Fatalf("clearing the sentence wrote %+v", got)
	}
	if s := EffectiveSettings(map[string]any{"acceptanceText": "  "}); s.AcceptanceText != DefaultAcceptanceText {
		t.Fatalf("a blank sentence reads %q", s.AcceptanceText)
	}
}

// THE STORE'S SETTINGS NAME THE CURRENT VERSION OF EACH DOCUMENT, or null.
func TestTheStoreSettingsNameTheCurrentDocuments(t *testing.T) {
	h := newDocsHarness(openSettings(true, nil))
	h.mustPublish(t, DocumentTerms, "Terms v1", "one")
	h.mustPublish(t, DocumentTerms, "Terms v2", "two")
	got := decodedBy(t)(h.p.storeSettings(staffCtx("staff-1"), map[string]any{"siteId": "site-1", "storeId": "store-live"}, 0))
	docs, _ := got["documents"].(map[string]any)
	terms, _ := docs[DocumentTerms].(map[string]any)
	if terms["label"] != "Terms v2" || terms["sha256"] != sha256Hex("two") || len(terms) != 2 {
		t.Fatalf("documents.terms = %+v", docs[DocumentTerms])
	}
	for _, kind := range []string{DocumentMap, DocumentPriceSheet} {
		if v, named := docs[kind]; !named || v != nil {
			t.Fatalf("documents.%s = %v (named %v), want null", kind, v, named)
		}
	}
}

// THE EXPORT CARRIES EVERY VERSION OF EVERY DOCUMENT, bodies included, and no
// token hash.
func TestTheExportCarriesEveryDocumentVersionWithItsBody(t *testing.T) {
	f := newStoreFake()
	f.docs = []map[string]any{
		{"id": documentVersionConcept + ":t1", "storeId": readsStore, "kind": DocumentTerms, "label": "T1", "body": "terms one", "createdAt": readsClock},
		{"id": documentVersionConcept + ":t2", "storeId": "v1:shopify:store:" + readsStore, "kind": DocumentTerms, "label": "T2", "body": "terms two",
			"createdAt": readsClock.Add(time.Hour), "tokenHash": "doc-digest"},
		{"id": documentVersionConcept + ":p1", "storeId": readsStore, "kind": DocumentPriceSheet, "label": "P1", "body": "prices", "createdAt": readsClock},
		{"id": documentVersionConcept + ":x1", "storeId": "store-elsewhere", "kind": DocumentTerms, "label": "X", "body": "not ours", "createdAt": readsClock},
	}
	p := f.provider()
	p.now = func() time.Time { return readsClock }
	got := decodedBy(t)(p.exportStore(staffReads(), map[string]any{"siteId": readsSite, "storeId": readsStore}, 0))
	doc := asString(got["json"])
	if strings.Contains(doc, "digest") {
		t.Fatalf("the export carries a token hash: %s", doc)
	}
	var parsed struct {
		DocumentVersions []map[string]any `json:"documentVersions"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("the export is not JSON: %v", err)
	}
	var labels []string
	for _, v := range parsed.DocumentVersions {
		labels = append(labels, asString(v["label"])+":"+asString(v["body"]))
	}
	if strings.Join(labels, ",") != "T2:terms two,T1:terms one,P1:prices" {
		t.Fatalf("the export's documentVersions are %v, want every version of the store, by kind, newest first, bodies included", labels)
	}
}

// fixture support: the document versions a fixtureReader and a storeFake hold.

// docsMatching is the versions of rows on storeID of kind, newest first.
func docsMatching(rows []map[string]any, storeID, kind string) []map[string]any {
	var out []map[string]any
	for _, row := range rows {
		if trimmed(row["storeId"]) == storeID && trimmed(row["kind"]) == kind {
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rowTime(out[i]["createdAt"]).After(rowTime(out[j]["createdAt"])) })
	return out
}

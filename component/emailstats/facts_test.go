package emailstats

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// facts_test.go -- the fact rules of the 2026-11-08 statistics design
// (section 5, D18), over rows shaped the way the engine answers an in-process
// caller: canonical ids, row.createdAt as a time.Time, payload datetimes as
// RFC3339 text.

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// factKey keys facts the way the table does, by kind and source.
func factKey(f Fact) string { return f.Kind + "|" + f.SourceID }

func byKey(facts []Fact) map[string]Fact {
	out := make(map[string]Fact, len(facts))
	for _, f := range facts {
		out[factKey(f)] = f
	}
	return out
}

func kinds(facts []Fact) []string {
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.Kind)
	}
	sort.Strings(out)
	return out
}

func wantKinds(t *testing.T, what string, facts []Fact, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := kinds(facts); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: facts %v, want %v", what, got, want)
	}
}

func wantDay(t *testing.T, what string, f Fact, want string) {
	t.Helper()
	if !f.Day.Equal(day(want)) {
		t.Errorf("%s: day %s, want %s", what, f.Day.Format(time.DateOnly), want)
	}
}

// deliveryRow is a delivery as deliveryFull shapes it for an in-process caller.
func deliveryRow(id string, fields map[string]any) map[string]any {
	row := map[string]any{
		"id":         "v1:campaigns:delivery:" + id,
		"accountId":  "v1:accounts:account:org-1",
		"campaignId": "v1:campaigns:campaign:camp-1",
		"createdAt":  at("2026-11-08T00:00:05Z"),
	}
	for k, v := range fields {
		if v == nil {
			delete(row, k)
			continue
		}
		row[k] = v
	}
	return row
}

// A sent delivery counts on the day it was sent, which is not the day its
// row was written when the ledger caught up after midnight.
func TestASentDeliveryIsASentFactOnItsSendDay(t *testing.T) {
	facts := deliveryFacts(deliveryRow("d1", map[string]any{"status": "sent", "sentAt": "2026-11-07T23:59:59Z"}))
	wantKinds(t, "a sent delivery", facts, kindSent)
	if len(facts) != 1 {
		return
	}
	f := facts[0]
	wantDay(t, "the send", f, "2026-11-07")
	if f.SourceID != "d1" || f.AccountID != "org-1" || f.CampaignID != "camp-1" {
		t.Errorf("the fact is %+v, want source d1, organization org-1, campaign camp-1", f)
	}

	// A delivery the provider proved sent after the worker recorded it failed
	// can reach sent with no sentAt; it counts on the day of the version that
	// says so, as campaignStats counts it, rather than not at all.
	late := deliveryFacts(deliveryRow("d2", map[string]any{"status": "sent", "createdAt": at("2026-11-08T09:00:00Z")}))
	wantKinds(t, "a sent delivery with no sentAt", late, kindSent)
	if len(late) == 1 {
		wantDay(t, "the version that says sent", late[0], "2026-11-08")
	}

	for _, status := range []string{"pending", "", "queued"} {
		wantKinds(t, "a delivery in status "+status, deliveryFacts(deliveryRow("d3", map[string]any{"status": status, "sentAt": "2026-11-07T10:00:00Z"})))
	}
}

// Failed and skipped count on the day the row reached them; a skip is also a
// suppression exactly when campaignStats calls it one -- the suppression
// list's three reasons and the recipient's own bounced and complained -- and
// an unsubscribe is not.
func TestASuppressedSkipIsASuppressedFact(t *testing.T) {
	written := at("2026-11-06T22:00:00Z")
	failed := deliveryFacts(deliveryRow("f1", map[string]any{"status": "failed", "createdAt": written}))
	wantKinds(t, "a failed delivery", failed, kindFailed)
	if len(failed) == 1 {
		wantDay(t, "the failure", failed[0], "2026-11-06")
	}

	for _, reason := range []string{"hard_bounce", "complaint", "manual", "bounced", "complained"} {
		facts := deliveryFacts(deliveryRow("s-"+reason, map[string]any{"status": "skipped", "skipReason": reason, "createdAt": written}))
		wantKinds(t, "a skip for "+reason, facts, kindSkipped, kindSuppressed)
		for _, f := range facts {
			wantDay(t, f.Kind+" for "+reason, f, "2026-11-06")
		}
	}
	for _, reason := range []string{"unsubscribed", "", "pending", "Hard_Bounce"} {
		facts := deliveryFacts(deliveryRow("s-other", map[string]any{"status": "skipped", "skipReason": reason, "createdAt": written}))
		wantKinds(t, fmt.Sprintf("a skip for %q", reason), facts, kindSkipped)
	}
}

// The provider's verdict is a second fact beside the send, split in two:
// delivered and expanded reached a mailbox, the rest did not. It counts on the
// provider's own time, and on the version's when the report carried none.
func TestProviderStatusesSplitIntoDeliveredAndUndelivered(t *testing.T) {
	cases := map[string]string{
		"delivered":     kindDelivered,
		"expanded":      kindDelivered,
		"bounced":       kindUndelivered,
		"suppressed":    kindUndelivered,
		"quarantined":   kindUndelivered,
		"filtered-spam": kindUndelivered,
		"failed":        kindUndelivered,
	}
	for status, want := range cases {
		facts := deliveryFacts(deliveryRow("p-"+status, map[string]any{
			"status": "sent", "sentAt": "2026-11-05T10:00:00Z",
			"providerStatus": status, "providerAt": "2026-11-06T01:00:00Z",
		}))
		wantKinds(t, "provider status "+status, facts, kindSent, want)
		if f, ok := byKey(facts)[want+"|p-"+status]; ok {
			wantDay(t, "provider status "+status, f, "2026-11-06")
		}
	}

	undated := deliveryFacts(deliveryRow("p-undated", map[string]any{
		"status": "sent", "sentAt": "2026-11-05T10:00:00Z",
		"providerStatus": "delivered", "createdAt": at("2026-11-07T03:00:00Z"),
	}))
	if f, ok := byKey(undated)[kindDelivered+"|p-undated"]; !ok {
		t.Errorf("a delivered report with no providerAt gave %v, want a delivered fact", kinds(undated))
	} else {
		wantDay(t, "a report with no providerAt", f, "2026-11-07")
	}

	for _, status := range []string{"", "unknown", "Delivered"} {
		wantKinds(t, fmt.Sprintf("provider status %q", status), deliveryFacts(deliveryRow("p-none", map[string]any{
			"status": "sent", "sentAt": "2026-11-05T10:00:00Z", "providerStatus": status,
		})), kindSent)
	}
}

// Every delivery fact carries the rule that sent it, and the campaign.
func TestADeliveryFactCarriesItsRuleAndCampaign(t *testing.T) {
	facts := deliveryFacts(deliveryRow("r1", map[string]any{
		"status": "sent", "sentAt": "2026-11-05T10:00:00Z", "campaignId": nil,
		"emailRuleId": "v1:campaigns:emailRule:rule-1", "providerStatus": "bounced",
	}))
	wantKinds(t, "a rule send", facts, kindSent, kindUndelivered)
	for _, f := range facts {
		if f.RuleID != "rule-1" || f.CampaignID != "" {
			t.Errorf("%s: rule %q campaign %q, want rule-1 and no campaign", f.Kind, f.RuleID, f.CampaignID)
		}
	}
}

// An open or a click is a total keyed by the event and a unique keyed by the
// delivery, so a second open of one message adds to the total and not to the
// unique count.
func TestEngagementGivesATotalAndAUniqueFact(t *testing.T) {
	for kind, want := range map[string][2]string{"open": {kindOpen, kindOpenUnique}, "click": {kindClick, kindClickUnique}} {
		facts := engagementFacts(map[string]any{
			"id":         "v1:campaigns:engagementEvent:ev-" + kind,
			"accountId":  "v1:accounts:account:org-1",
			"campaignId": "v1:campaigns:campaign:camp-1",
			"deliveryId": "v1:campaigns:delivery:d1",
			"kind":       kind,
			"occurredAt": "2026-11-04T23:30:00Z",
			"createdAt":  at("2026-11-05T00:00:01Z"),
		})
		got := byKey(facts)
		total, ok := got[want[0]+"|ev-"+kind]
		if !ok {
			t.Errorf("a %s gave %v, want a %s keyed by the event", kind, kinds(facts), want[0])
			continue
		}
		unique, ok := got[want[1]+"|d1"]
		if !ok {
			t.Errorf("a %s gave %v, want a %s keyed by the delivery", kind, kinds(facts), want[1])
			continue
		}
		for _, f := range []Fact{total, unique} {
			wantDay(t, f.Kind, f, "2026-11-04")
			if f.AccountID != "org-1" || f.CampaignID != "camp-1" {
				t.Errorf("%s: %+v, want organization org-1 and campaign camp-1", f.Kind, f)
			}
		}
		if len(facts) != 2 {
			t.Errorf("a %s gave %d facts, want 2", kind, len(facts))
		}
	}

	if facts := engagementFacts(map[string]any{"id": "ev-x", "deliveryId": "d1", "kind": "delivered", "occurredAt": "2026-11-04T23:30:00Z"}); len(facts) != 0 {
		t.Errorf("an engagement of an unknown kind gave %v, want nothing", kinds(facts))
	}
}

// The consent stream's kinds map onto the statistics' words; a suppression is
// an operator's act, not a person's choice, and counts as nothing.
func TestConsentKindsMapToTheirFacts(t *testing.T) {
	cases := map[string]string{"withdraw": kindUnsubscribe, "bounce": kindBounce, "complaint": kindComplaint, "grant": kindSubscribe, "suppress": ""}
	for kind, want := range cases {
		facts := consentFacts(map[string]any{
			"id":          "v1:campaigns:consentEvent:ce-" + kind,
			"accountId":   "v1:accounts:account:org-1",
			"kind":        kind,
			"recipientId": "v1:campaigns:recipient:rcp-1",
			"occurredAt":  "2026-11-03T12:00:00Z",
			"createdAt":   at("2026-11-08T12:00:00Z"),
		})
		if want == "" {
			wantKinds(t, "a "+kind, facts)
			continue
		}
		wantKinds(t, "a "+kind, facts, want)
		if len(facts) == 1 {
			wantDay(t, "a "+kind, facts[0], "2026-11-03")
			if facts[0].SourceID != "ce-"+kind || facts[0].AccountID != "org-1" {
				t.Errorf("a %s: %+v, want source ce-%s in org-1", kind, facts[0], kind)
			}
			if facts[0].AudienceID != "" {
				t.Errorf("a %s carries audience %q before attribution", kind, facts[0].AudienceID)
			}
		}
	}
}

// Every id a fact carries is bare, whatever spelling the row arrived in: the
// statistics read compares them with the bare ids a caller's organizations
// resolve to, and a canonical id would match nothing.
func TestIdsAreBare(t *testing.T) {
	canonical := regexp.MustCompile(`^v[0-9]+:`)
	var facts []Fact
	facts = append(facts, deliveryFacts(map[string]any{
		"id": "v1:campaigns:delivery:d1", "accountId": "v1:accounts:account:org-1",
		"campaignId": "v1:campaigns:campaign:camp-1", "emailRuleId": "v1:campaigns:emailRule:rule-1",
		"status": "sent", "sentAt": "2026-11-05T10:00:00Z", "providerStatus": "delivered",
		"createdAt": at("2026-11-05T10:00:01Z"),
	})...)
	facts = append(facts, engagementFacts(map[string]any{
		"id": "v1:campaigns:engagementEvent:ev-1", "accountId": "v1:accounts:account:org-1",
		"campaignId": "v1:campaigns:campaign:camp-1", "deliveryId": "v1:campaigns:delivery:d1",
		"kind": "open", "occurredAt": "2026-11-05T11:00:00Z",
	})...)
	facts = append(facts, consentFacts(map[string]any{
		"id": "v1:campaigns:consentEvent:ce-1", "accountId": "v1:accounts:account:org-1",
		"campaignId": "v1:campaigns:campaign:camp-1", "kind": "withdraw", "occurredAt": "2026-11-05T12:00:00Z",
	})...)
	if len(facts) != 5 {
		t.Fatalf("got %v, want sent, delivered, open, open_unique and unsubscribe", kinds(facts))
	}
	for _, f := range facts {
		for name, id := range map[string]string{"source": f.SourceID, "organization": f.AccountID, "campaign": f.CampaignID, "rule": f.RuleID} {
			if canonical.MatchString(id) {
				t.Errorf("%s: the %s id %q is canonical, want it bare", f.Kind, name, id)
			}
		}
	}

	refs := newPageRefs()
	deliveryRefs(map[string]any{"id": "v1:campaigns:delivery:d9", "requestId": journeyRequest(strings.Repeat("ab", 32), "welcome")}, refs)
	consentRefs(map[string]any{"id": "v1:campaigns:consentEvent:ce-9", "kind": "grant", "recipientId": "v1:campaigns:recipient:rcp-9"}, refs)
	if refs.enrollmentOf["d9"] != strings.Repeat("ab", 32) || refs.recipientOf["ce-9"] != "rcp-9" {
		t.Errorf("the attribution references are %v and %v, want them keyed and valued by bare ids", refs.enrollmentOf, refs.recipientOf)
	}
	// Every kind that makes a fact notes its recipient, whose audience may be
	// the testing audience; a suppression makes none and asks for nothing.
	for kind, noted := range map[string]bool{"withdraw": true, "bounce": true, "complaint": true, "suppress": false} {
		consentRefs(map[string]any{"id": "v1:campaigns:consentEvent:ce-" + kind, "kind": kind, "recipientId": "v1:campaigns:recipient:rcp-" + kind}, refs)
		if got := refs.recipientOf["ce-"+kind]; (got == "rcp-"+kind) != noted {
			t.Errorf("a %s notes the recipient %q; want it noted: %v", kind, got, noted)
		}
	}
}

func journeyRequest(enrollment, step string) string { return "journey-" + enrollment + "-" + step }

// A journey send names its enrollment in its request id; nothing else does.
func TestJourneyEnrollmentOfParsesOnlyAJourneyRequestId(t *testing.T) {
	enrollment := strings.Repeat("0123456789abcdef", 4)
	for _, step := range []string{"welcome", "step-2", "a"} {
		got, ok := journeyEnrollmentOf(journeyRequest(enrollment, step))
		if !ok || got != enrollment {
			t.Errorf("journey request for step %q: got %q, %v; want %q", step, got, ok, enrollment)
		}
	}
	for _, requestID := range []string{
		"",
		"journey-short-x",
		"rule-" + enrollment + "-welcome",
		"newsletter-" + enrollment,
		"journey-" + enrollment,               // no step
		"journey-" + enrollment + "-",         // an empty step
		"journey-" + enrollment + "x-welcome", // the enrollment runs on
		"journey-" + strings.ToUpper(enrollment) + "-welcome",    // a SHA-256 in hex is written lowercase
		"journey-" + enrollment[:63] + "-welcome",                // one digit short
		"journey-" + strings.Repeat("g", 64) + "-welcome",        // not hex
		"journey-" + enrollment + "-welcome step",                // not a step id
		"xjourney-" + enrollment + "-welcome",                    // not the prefix
		"journey-" + enrollment + "-" + strings.Repeat("s", 41),  // a step id is at most forty characters
		"journey-" + enrollment + "-welcome:" + "v1:campaigns:x", // a step id holds no colon
	} {
		if got, ok := journeyEnrollmentOf(requestID); ok {
			t.Errorf("%q read as the enrollment %q, want no journey", requestID, got)
		}
	}
}

// fakeEngine answers the attribution lookups and the two that say which rows
// are a test's from fixed tables, and records every id it was asked for, one
// entry per call.
type fakeEngine struct {
	journeys  map[string]string
	audiences map[string]string
	// testRuns are the campaigns that are test runs, testing the audiences
	// that are an organization's testing audience.
	testRuns, testing map[string]bool
	calls             []string
	asked             [][]string
}

var lookupCall = regexp.MustCompile(`query (journeyEnrollmentsByIds|recipientAudiencesByIds|campaignTestRunsByIds|testingAudiencesByIds)\(ids: (\[[^\]]*\])\)`)

func (e *fakeEngine) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	m := lookupCall.FindStringSubmatch(query)
	if m == nil {
		return nil, fmt.Errorf("unexpected query %q", query)
	}
	var ids []string
	if err := json.Unmarshal([]byte(m[2]), &ids); err != nil {
		return nil, fmt.Errorf("the ids of %q are not a list of strings: %v", query, err)
	}
	e.calls = append(e.calls, m[1])
	e.asked = append(e.asked, ids)
	rows := []any{}
	for _, id := range ids {
		switch m[1] {
		case "journeyEnrollmentsByIds":
			if journey, ok := e.journeys[id]; ok {
				rows = append(rows, map[string]any{"id": "v1:campaigns:journeyEnrollment:" + id, "journeyId": "v1:campaigns:journey:" + journey, "accountId": "v1:accounts:account:org-1"})
			}
		case "recipientAudiencesByIds":
			if audience, ok := e.audiences[id]; ok {
				rows = append(rows, map[string]any{"id": "v1:campaigns:recipient:" + id, "audienceId": "v1:campaigns:audience:" + audience})
			}
		case "campaignTestRunsByIds":
			if e.testRuns[id] {
				rows = append(rows, map[string]any{"id": "v1:campaigns:campaign:" + id})
			}
		case "testingAudiencesByIds":
			if e.testing[id] {
				rows = append(rows, map[string]any{"id": "v1:campaigns:testSettings:settings-" + id, "audienceId": "v1:campaigns:audience:" + id})
			}
		}
	}
	return memql.NewResultWithOutput(rows), nil
}

// Attribution fills a journey send's journey and a subscription change's
// audience, leaves every other fact as it was, and asks for at most five
// hundred ids at a time.
func TestAttributionReadsInBatchesAndFillsOnlyItsKinds(t *testing.T) {
	engine := &fakeEngine{journeys: map[string]string{}, audiences: map[string]string{}}
	refs := newPageRefs()
	var facts []Fact
	for i := 0; i < 1200; i++ {
		enrollment := fmt.Sprintf("%064x", i)
		delivery := fmt.Sprintf("d%04d", i)
		refs.enrollmentOf[delivery] = enrollment
		engine.journeys[enrollment] = fmt.Sprintf("journey-%d", i%3)
		facts = append(facts, Fact{Kind: kindSent, SourceID: delivery, AccountID: "org-1"}, Fact{Kind: kindDelivered, SourceID: delivery, AccountID: "org-1"})
	}
	refs.enrollmentOf["d-other-org"] = fmt.Sprintf("%064x", 7)
	refs.enrollmentOf["d-no-org"] = fmt.Sprintf("%064x", 8)
	refs.recipientOf["ce-grant"] = "rcp-1"
	refs.recipientOf["ce-withdraw"] = "rcp-2"
	refs.recipientOf["ce-bounce"] = "rcp-1"
	engine.audiences["rcp-1"] = "aud-1"
	engine.audiences["rcp-2"] = "aud-2"
	facts = append(facts,
		Fact{Kind: kindSubscribe, SourceID: "ce-grant"},
		Fact{Kind: kindUnsubscribe, SourceID: "ce-withdraw"},
		Fact{Kind: kindBounce, SourceID: "ce-bounce"},
		Fact{Kind: kindSent, SourceID: "d-no-journey", AccountID: "org-1"},
		// A request id naming another organization's enrollment, and a send that
		// belongs to no organization at all: neither is the journey's.
		Fact{Kind: kindSent, SourceID: "d-other-org", AccountID: "org-2"},
		Fact{Kind: kindSent, SourceID: "d-no-org"},
		Fact{Kind: kindSubscribe, SourceID: "ce-no-recipient"},
	)

	facts, err := attribute(context.Background(), engine, facts, refs, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}

	journeyCalls, total := 0, 0
	for i, call := range engine.calls {
		if len(engine.asked[i]) > lookupBatch || len(engine.asked[i]) == 0 {
			t.Errorf("%s was asked for %d ids, want between 1 and %d", call, len(engine.asked[i]), lookupBatch)
		}
		if call == "journeyEnrollmentsByIds" {
			journeyCalls++
			total += len(engine.asked[i])
		}
	}
	if journeyCalls != 3 || total != 1200 {
		t.Errorf("1200 enrollments were asked for in %d calls, %d ids in all; want 3 calls and 1200 ids", journeyCalls, total)
	}

	got := byKey(facts)
	if f := got[kindSent+"|d0004"]; f.JourneyID != "journey-1" {
		t.Errorf("a journey send's journey is %q, want journey-1", f.JourneyID)
	}
	if f := got[kindDelivered+"|d0005"]; f.JourneyID != "journey-2" {
		t.Errorf("a journey send's delivered fact has journey %q, want journey-2", f.JourneyID)
	}
	if f := got[kindSubscribe+"|ce-grant"]; f.AudienceID != "aud-1" {
		t.Errorf("a subscribe's audience is %q, want aud-1", f.AudienceID)
	}
	if f := got[kindUnsubscribe+"|ce-withdraw"]; f.AudienceID != "aud-2" {
		t.Errorf("an unsubscribe's audience is %q, want aud-2", f.AudienceID)
	}
	if f := got[kindBounce+"|ce-bounce"]; f.AudienceID != "" {
		t.Errorf("a bounce was given the audience %q; only a subscription change is attributed to one", f.AudienceID)
	}
	for _, source := range []string{"d-other-org", "d-no-org"} {
		if f := got[kindSent+"|"+source]; f.JourneyID != "" {
			t.Errorf("%s was attributed to journey %q, which belongs to another organization", source, f.JourneyID)
		}
	}
	if f := got[kindSent+"|d-no-journey"]; f.JourneyID != "" {
		t.Errorf("a send with no journey was given the journey %q", f.JourneyID)
	}
	if f := got[kindSubscribe+"|ce-no-recipient"]; f.AudienceID != "" {
		t.Errorf("a subscribe naming no recipient was given the audience %q", f.AudienceID)
	}

	// No reference, no lookup.
	quiet := &fakeEngine{}
	if _, err := attribute(context.Background(), quiet, []Fact{{Kind: kindSent, SourceID: "d1"}}, newPageRefs(), map[string]bool{}); err != nil || len(quiet.calls) != 0 {
		t.Errorf("a page with nothing to attribute made %d lookups (error %v), want none", len(quiet.calls), err)
	}
}

// A test run's facts and the testing audience's are left out, every other
// fact is kept as it was, and a campaign is asked about once a run: the
// second page asks only about the campaign it is the first to name. The
// testing audience is asked about on every page, for the audiences the page's
// recipients belong to.
func TestATestRunsFactsAndTheTestingAudiencesAreLeftOut(t *testing.T) {
	engine := &fakeEngine{
		audiences: map[string]string{"rcp-tester": "aud-test", "rcp-member": "aud-live"},
		testRuns:  map[string]bool{"camp-test": true},
		testing:   map[string]bool{"aud-test": true},
	}
	refs := newPageRefs()
	for source, recipient := range map[string]string{
		"ce-tester": "rcp-tester", "ce-tester-bounce": "rcp-tester", "ce-tester-complaint": "rcp-tester",
		"ce-member": "rcp-member", "ce-member-bounce": "rcp-member",
	} {
		refs.recipientOf[source] = recipient
	}
	testRuns := map[string]bool{}
	got, err := attribute(context.Background(), engine, []Fact{
		{Kind: kindSent, SourceID: "d-test", CampaignID: "camp-test"},
		{Kind: kindDelivered, SourceID: "d-test", CampaignID: "camp-test"},
		{Kind: kindOpen, SourceID: "ev-test", CampaignID: "camp-test"},
		{Kind: kindOpenUnique, SourceID: "d-test", CampaignID: "camp-test"},
		// A one-click withdraw from a forwarded test message: only its
		// campaign says it is a test's.
		{Kind: kindUnsubscribe, SourceID: "ce-run", CampaignID: "camp-test"},
		{Kind: kindSubscribe, SourceID: "ce-tester"},
		{Kind: kindBounce, SourceID: "ce-tester-bounce"},
		{Kind: kindComplaint, SourceID: "ce-tester-complaint"},
		{Kind: kindSent, SourceID: "d-live", CampaignID: "camp-live"},
		{Kind: kindSubscribe, SourceID: "ce-member"},
		{Kind: kindBounce, SourceID: "ce-member-bounce"},
	}, refs, testRuns)
	if err != nil {
		t.Fatal(err)
	}
	if keys := strings.Join(sortedKeys(byKey(got)), " "); keys != "bounce|ce-member-bounce sent|d-live subscribe|ce-member" {
		t.Errorf("the page keeps %s, want the live send and the member's subscribe and bounce alone", keys)
	}
	if f := byKey(got)[kindSubscribe+"|ce-member"]; f.AudienceID != "aud-live" {
		t.Errorf("the member's subscribe counts under %q, want aud-live", f.AudienceID)
	}
	if f := byKey(got)[kindBounce+"|ce-member-bounce"]; f.AudienceID != "" {
		t.Errorf("the member's bounce was given the audience %q; only a subscription change is attributed to one", f.AudienceID)
	}
	if !testRuns["camp-test"] || testRuns["camp-live"] || len(testRuns) != 2 {
		t.Errorf("the run's answers are %v, want camp-test a test run and camp-live not", testRuns)
	}
	asked := map[string][]string{}
	for i, call := range engine.calls {
		asked[call] = append(asked[call], engine.asked[i]...)
	}
	if ids := strings.Join(asked["campaignTestRunsByIds"], " "); ids != "camp-live camp-test" {
		t.Errorf("the first page asked about the campaigns %q, want camp-live and camp-test", ids)
	}
	if ids := strings.Join(asked["testingAudiencesByIds"], " "); ids != "aud-live aud-test" {
		t.Errorf("the first page asked about the audiences %q, want its recipients' aud-live and aud-test", ids)
	}

	engine.calls, engine.asked = nil, nil
	got, err = attribute(context.Background(), engine, []Fact{
		{Kind: kindSent, SourceID: "d-test-2", CampaignID: "camp-test"},
		{Kind: kindSent, SourceID: "d-live-2", CampaignID: "camp-live"},
		{Kind: kindSent, SourceID: "d-new", CampaignID: "camp-new"},
	}, newPageRefs(), testRuns)
	if err != nil {
		t.Fatal(err)
	}
	if keys := strings.Join(sortedKeys(byKey(got)), " "); keys != "sent|d-live-2 sent|d-new" {
		t.Errorf("the second page keeps %s, want the two campaigns' sends", keys)
	}
	if len(engine.calls) != 1 || engine.calls[0] != "campaignTestRunsByIds" || strings.Join(engine.asked[0], " ") != "camp-new" {
		t.Errorf("the second page asked %v about %v, want one lookup of the campaign it is the first to name", engine.calls, engine.asked)
	}
}

func sortedKeys(m map[string]Fact) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// One fact per kind and source on a page: a message opened twice on one page
// keeps one unique open, the earlier, and the totals are untouched.
func TestAPageKeepsOneFactPerKindAndSource(t *testing.T) {
	facts := uniqueFacts([]Fact{
		{Kind: kindOpen, SourceID: "ev-2", Day: day("2026-11-05")},
		{Kind: kindOpenUnique, SourceID: "d1", Day: day("2026-11-05")},
		{Kind: kindOpen, SourceID: "ev-1", Day: day("2026-11-04")},
		{Kind: kindOpenUnique, SourceID: "d1", Day: day("2026-11-04")},
	})
	got := byKey(facts)
	if len(facts) != 3 {
		t.Fatalf("got %v, want two opens and one unique open", kinds(facts))
	}
	if f := got[kindOpenUnique+"|d1"]; !f.Day.Equal(day("2026-11-04")) {
		t.Errorf("the unique open is dated %s, want the earlier open's 2026-11-04", f.Day.Format(time.DateOnly))
	}
}

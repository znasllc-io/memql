package campaigns

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// feedback_parse_test.go -- memql#3461's acceptance criteria, one test each:
//
//	a provider's feedback reaches suppression   TestDSNHardBounceReachesSuppression
//	                                            TestSESComplaintReachesSuppression
//	hard vs soft is the PROVIDER's, not ours    TestDSNClassificationComesFromStatus
//	                                            TestSESUndeterminedIsTransient
//	an unparseable payload is VISIBLE           TestUnreadablePayloadFailsTheInboundRow
//	an unverified source cannot suppress        TestUnverifiedDeliveryCannotSuppress
//	a non-feedback source is not a failure      TestUnconfiguredSourceIsNotAFailure
//
// and two from epic memql#5477, where a builtin's caller was found to be any
// signed-in client:
//
//	only the automation reaches it              TestIngestFeedbackRefusesAClientBeforeReadingTheRow
//	the row decides, never the arguments        TestIngestFeedbackTakesTheDeliveryFromTheRowNotItsArguments

const hardBounceDSN = `Content-Type: multipart/report; report-type=delivery-status; boundary="b"

--b
Content-Type: text/plain

This is an automatically generated Delivery Status Notification.

--b
Content-Type: message/delivery-status

Reporting-MTA: dns; mx.example.net

Final-Recipient: rfc822; dead@example.test
Action: failed
Status: 5.1.1
Diagnostic-Code: smtp; 550 5.1.1 <dead@example.test>
 User unknown

--b
Content-Type: text/rfc822-headers

Subject: August update
X-Campaign-Id: camp-1

--b--
`

const softBounceDSN = `Content-Type: message/delivery-status

Final-Recipient: rfc822; full@example.test
Action: delayed
Status: 4.2.2
Diagnostic-Code: smtp; 452 4.2.2 Mailbox full
`

func TestDSNHardBounceReachesSuppression(t *testing.T) {
	parsed, err := ParseFeedback(FormatRFC3464, hardBounceDSN)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Reports) != 1 {
		t.Fatalf("want one report, got %d", len(parsed.Reports))
	}
	r := parsed.Reports[0]
	if r.Email != "dead@example.test" {
		t.Errorf("address = %q", r.Email)
	}
	if r.Kind != "hard_bounce" {
		t.Errorf("kind = %q, want hard_bounce", r.Kind)
	}
	if r.CampaignID != "camp-1" {
		t.Errorf("campaignId = %q; the DSN quotes X-Campaign-Id back and attribution should survive", r.CampaignID)
	}
	// The folded Diagnostic-Code has to arrive whole -- a continuation line
	// dropped mid-diagnostic loses the provider's own words on the row.
	if !strings.Contains(r.Note, "User unknown") {
		t.Errorf("note = %q, want the folded diagnostic joined", r.Note)
	}
}

// TestDSNClassificationComesFromStatus is the acceptance criterion stated as
// the thing NOT to do: the diagnostic text of a soft bounce reads exactly
// like a hard one to a keyword matcher ("Mailbox full" vs "User unknown" are
// both prose). The verdict comes from the status class.
func TestDSNClassificationComesFromStatus(t *testing.T) {
	parsed, err := ParseFeedback(FormatRFC3464, softBounceDSN)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Reports) != 1 || parsed.Reports[0].Kind != "soft_bounce" {
		t.Fatalf("a 4.2.2 status was not read as transient: %+v", parsed.Reports)
	}
}

func TestDSNSuccessIsNotABounce(t *testing.T) {
	parsed, err := ParseFeedback(FormatRFC3464, "Final-Recipient: rfc822; ok@example.test\nAction: delivered\nStatus: 2.0.0\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Reports) != 0 {
		t.Errorf("a delivery receipt produced %d bounce reports", len(parsed.Reports))
	}
	if parsed.Ignored == "" {
		t.Error("a payload with nothing to do must say so, or it is indistinguishable from one that failed silently")
	}
}

func TestDSNWithNoVerdictIsRefused(t *testing.T) {
	_, err := ParseFeedback(FormatRFC3464, "Final-Recipient: rfc822; who@example.test\nDiagnostic-Code: smtp; something went wrong\n")
	if err == nil {
		t.Fatal("a group stating neither Status nor Action was accepted; the severity would have been invented")
	}
}

const sesComplaint = `{"Type":"Notification","TopicArn":"arn:aws:sns:x","Message":"{\"notificationType\":\"Complaint\",\"complaint\":{\"complaintFeedbackType\":\"abuse\",\"complainedRecipients\":[{\"emailAddress\":\"cross@example.test\"}]},\"mail\":{\"headers\":[{\"name\":\"X-Campaign-Id\",\"value\":\"camp-1\"}]}}"}`

const sesUndetermined = `{"notificationType":"Bounce","bounce":{"bounceType":"Undetermined","bounceSubType":"Undetermined","bouncedRecipients":[{"emailAddress":"maybe@example.test"}]}}`

func TestSESComplaintReachesSuppression(t *testing.T) {
	parsed, err := ParseFeedback(FormatSES, sesComplaint)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Reports) != 1 {
		t.Fatalf("want one report, got %d", len(parsed.Reports))
	}
	if parsed.Reports[0].Kind != "complaint" {
		t.Errorf("kind = %q, want complaint -- the signal a DSN structurally cannot carry", parsed.Reports[0].Kind)
	}
	if parsed.Reports[0].CampaignID != "camp-1" {
		t.Errorf("campaignId = %q, want the echoed header", parsed.Reports[0].CampaignID)
	}
}

// TestSESUndeterminedIsTransient: SES saying it could not tell must not
// produce a permanent suppression. Being wrong this way costs a few more
// attempts; being wrong the other way costs a real subscriber.
func TestSESUndeterminedIsTransient(t *testing.T) {
	parsed, err := ParseFeedback(FormatSES, sesUndetermined)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Reports) != 1 || parsed.Reports[0].Kind != "soft_bounce" {
		t.Fatalf("an Undetermined bounce was not treated as transient: %+v", parsed.Reports)
	}
}

func TestSNSSubscriptionConfirmationIsNotAFailure(t *testing.T) {
	parsed, err := ParseFeedback(FormatSES, `{"Type":"SubscriptionConfirmation","SubscribeURL":"https://sns.example/confirm"}`)
	if err != nil {
		t.Fatalf("the SNS handshake was reported as a parse failure: %v", err)
	}
	if !strings.Contains(parsed.Ignored, "SubscribeURL") {
		t.Errorf("the handshake must tell the operator what to do next; got %q", parsed.Ignored)
	}
}

// --- the ingestion path -------------------------------------------------

func inboundRow(id, source, body string, verified bool) map[string]any {
	return map[string]any{
		"id":                "v1:platform:inboundRequest:" + id,
		"source":            source,
		"body":              body,
		"signatureVerified": verified,
	}
}

type inboundEngine struct {
	*fakeEngine
	row map[string]any
}

func (e *inboundEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query inboundRequestById") {
		e.fakeEngine.mu.Lock()
		e.fakeEngine.calls = append(e.fakeEngine.calls, recordedCall{query: q})
		e.fakeEngine.mu.Unlock()
		return rowsEnvelope([]map[string]any{e.row}), nil
	}
	return e.fakeEngine.Execute(ctx, q)
}

func ingestWorker(t *testing.T, row map[string]any, sources map[string]string) (*Worker, *inboundEngine) {
	t.Helper()
	engine := &inboundEngine{fakeEngine: &fakeEngine{}, row: row}
	w := newTestWorker(t, engine, &recordingSender{})
	w.cfg.FeedbackSources = sources
	return w, engine
}

// feedbackAutomationCtx is the context the shipped ingestCampaignFeedback
// automation's step runs under: internal origin, which the executor stamps on
// a tree-loaded automation and on nothing a client sends.
func feedbackAutomationCtx() context.Context {
	return auth.ContextWithInternalOrigin(context.Background())
}

// countedHardBounces is what the sending ramp's reputation window holds, across
// every domain: the side effect a re-processed bounce repeats.
func countedHardBounces(w *Worker) int {
	w.reputation.mu.Lock()
	defer w.reputation.mu.Unlock()
	n := 0
	for _, c := range w.reputation.counts {
		n += c.hardBounce
	}
	return n
}

// THE GATE BEFORE THE ROW (epic memql#5477). campaignIngestFeedback is a
// builtin, and any signed-in client's query can name a builtin -- @sdk has no
// engine effect -- so it refuses every call that did not arrive with internal
// origin, before the row is read: no suppression, no stamp and nothing counted.
// Re-processing a real bounce is not free -- each pass counts it into the
// ramp's reputation window again -- which is why a client able to read a staged
// row's id still may not drive it.
func TestIngestFeedbackRefusesAClientBeforeReadingTheRow(t *testing.T) {
	sources := map[string]string{"postmaster": FormatRFC3464}
	for name, ctx := range map[string]context.Context{
		"a signed-in person": auth.ContextWithUserActor(context.Background(), "v1:identity:user:someone"),
		"a cluster owner": auth.ContextWithAccess(context.Background(),
			&auth.AccessContext{UserId: "v1:identity:user:owner", Role: auth.RoleOwner}),
		"nobody": context.Background(),
		// Client origin stamped over an internal one: the mark a trusted
		// frame carried does not survive the client's stamp.
		"a client descended from a trusted frame": auth.ContextWithClientOrigin(feedbackAutomationCtx()),
	} {
		t.Run(name, func(t *testing.T) {
			w, engine := ingestWorker(t, inboundRow("inb6", "postmaster", hardBounceDSN, true), sources)
			_, err := w.handleIngestFeedback(ctx, map[string]any{"inboundRequestId": "inb6"}, 0)
			if !errors.Is(err, errIngestFeedbackClientOrigin) {
				t.Fatalf("err = %v, want errIngestFeedbackClientOrigin", err)
			}
			if n := len(engine.fakeEngine.calls); n != 0 {
				t.Fatalf("a refused call reached the engine %d times; the refusal must come before the row is read: %v", n, engine.fakeEngine.calls)
			}
			if n := countedHardBounces(w); n != 0 {
				t.Fatalf("a refused call counted %d hard bounces into the reputation window", n)
			}
		})
	}

	// THE CONTROL: the same row on the automation's context suppresses the
	// address and counts the bounce, so the silence above is the origin's.
	w, engine := ingestWorker(t, inboundRow("inb6", "postmaster", hardBounceDSN, true), sources)
	if _, err := w.handleIngestFeedback(feedbackAutomationCtx(), map[string]any{"inboundRequestId": "inb6"}, 0); err != nil {
		t.Fatalf("the automation's call: %v", err)
	}
	if n := len(engine.fakeEngine.mutations("recordSuppression")); n != 1 {
		t.Fatalf("the automation's call recorded %d suppressions, want 1", n)
	}
	if n := countedHardBounces(w); n != 1 {
		t.Fatalf("the automation's call counted %d hard bounces, want 1", n)
	}
}

// The ingester reads its delivery from the STAGED ROW and from nothing its
// caller hands it: a body, a source or a verdict passed beside the id -- which
// the automation never passes, and a caller could forge -- changes nothing the
// row decides.
func TestIngestFeedbackTakesTheDeliveryFromTheRowNotItsArguments(t *testing.T) {
	sources := map[string]string{"postmaster": FormatRFC3464}
	forged := map[string]any{
		"inboundRequestId":  "inb7",
		"source":            "postmaster",
		"body":              hardBounceDSN,
		"signatureVerified": true,
	}
	nothingWritten := func(t *testing.T, engine *inboundEngine) {
		t.Helper()
		if n := len(engine.fakeEngine.mutations("recordSuppression")); n != 0 {
			t.Fatalf("a forged argument produced %d suppressions", n)
		}
	}

	t.Run("a staged soft bounce stays soft", func(t *testing.T) {
		w, engine := ingestWorker(t, inboundRow("inb7", "postmaster", softBounceDSN, true), sources)
		if _, err := w.handleIngestFeedback(feedbackAutomationCtx(), forged, 0); err != nil {
			t.Fatalf("ingestFeedback: %v", err)
		}
		nothingWritten(t, engine)
	})
	t.Run("another source's row stays another source's", func(t *testing.T) {
		w, engine := ingestWorker(t, inboundRow("inb7", "shopify", `{"order":1}`, true), sources)
		if _, err := w.handleIngestFeedback(feedbackAutomationCtx(), forged, 0); err != nil {
			t.Fatalf("an unrelated webhook is not a failure: %v", err)
		}
		nothingWritten(t, engine)
		if n := len(engine.fakeEngine.mutations("updateInboundRequestStatus")); n != 0 {
			t.Fatalf("another source's row was stamped %d times", n)
		}
	})
	t.Run("an unsigned staged row is not signed by an argument", func(t *testing.T) {
		w, engine := ingestWorker(t, inboundRow("inb7", "postmaster", hardBounceDSN, false), sources)
		if _, err := w.handleIngestFeedback(feedbackAutomationCtx(), forged, 0); err == nil {
			t.Fatal("an argument's verdict let an unverified delivery through")
		}
		nothingWritten(t, engine)
	})
	t.Run("no staged row is no delivery", func(t *testing.T) {
		w, engine := ingestWorker(t, nil, sources)
		if _, err := w.handleIngestFeedback(feedbackAutomationCtx(), forged, 0); err == nil {
			t.Fatal("arguments with no staged row behind them were accepted")
		}
		nothingWritten(t, engine)
	})
}

// TestDSNIngestionSuppressesEndToEnd is the issue's headline criterion: a
// provider's bounce reaches the suppression path end to end, without anyone
// writing a parser first.
func TestDSNIngestionSuppressesEndToEnd(t *testing.T) {
	w, engine := ingestWorker(t,
		inboundRow("inb1", "postmaster", hardBounceDSN, true),
		map[string]string{"postmaster": FormatRFC3464})

	if _, err := w.handleIngestFeedback(feedbackAutomationCtx(), map[string]any{"inboundRequestId": "inb1"}, 0); err != nil {
		t.Fatalf("ingestFeedback: %v", err)
	}

	sup := engine.fakeEngine.mutations("recordSuppression")
	if len(sup) != 1 {
		t.Fatalf("want one suppression, got %d", len(sup))
	}
	if got := argOf(sup[0].query, "reason"); got != "hard_bounce" {
		t.Errorf("reason = %q", got)
	}
	if got := argOf(sup[0].query, "emailDigest"); got != EmailDigest("dead@example.test") {
		t.Error("the suppression was not keyed by the address digest")
	}
	if strings.Contains(sup[0].query, "dead@example.test") {
		t.Error("the plaintext address reached the graph")
	}
	if got := argOf(sup[0].query, "sourceCampaignId"); got != "camp-1" {
		t.Errorf("sourceCampaignId = %q; the bounce should be attributable to the send", got)
	}
	if !sup[0].isOwner {
		t.Error("the suppression list is clusterOwner-tier and must be written under the engine's own identity")
	}

	status := engine.fakeEngine.mutations("updateInboundRequestStatus")
	if len(status) != 1 || argOf(status[0].query, "status") != "processed" {
		t.Errorf("the inbound row was not stamped processed: %v", status)
	}
}

// TestSoftBounceIngestionDoesNotSuppress: the hard/soft decision survives the
// new path. A transient failure suppressing an address is how a sender loses
// a real subscriber to a full mailbox.
func TestSoftBounceIngestionDoesNotSuppress(t *testing.T) {
	w, engine := ingestWorker(t,
		inboundRow("inb2", "postmaster", softBounceDSN, true),
		map[string]string{"postmaster": FormatRFC3464})

	if _, err := w.handleIngestFeedback(feedbackAutomationCtx(), map[string]any{"inboundRequestId": "inb2"}, 0); err != nil {
		t.Fatalf("ingestFeedback: %v", err)
	}
	if n := len(engine.fakeEngine.mutations("recordSuppression")); n != 0 {
		t.Errorf("a soft bounce produced %d suppressions", n)
	}
}

// TestUnreadablePayloadFailsTheInboundRow is the "visible rather than
// dropped" criterion. Silently ignoring what we cannot read recreates the gap
// this task exists to close, one level down.
func TestUnreadablePayloadFailsTheInboundRow(t *testing.T) {
	w, engine := ingestWorker(t,
		inboundRow("inb3", "postmaster", "this is not a delivery status notification", true),
		map[string]string{"postmaster": FormatRFC3464})

	_, err := w.handleIngestFeedback(feedbackAutomationCtx(), map[string]any{"inboundRequestId": "inb3"}, 0)
	if err == nil {
		t.Fatal("an unreadable payload was accepted quietly")
	}
	status := engine.fakeEngine.mutations("updateInboundRequestStatus")
	if len(status) != 1 || argOf(status[0].query, "status") != "failed" {
		t.Fatalf("the unreadable payload was not recorded on the row: %v", status)
	}
	if reason := argOf(status[0].query, "lastError"); !strings.Contains(reason, "could not read") {
		t.Errorf("lastError = %q; it has to say what could not be read", reason)
	}
}

// TestUnverifiedDeliveryCannotSuppress: a source configured scheme='none' is
// unauthenticated by construction, and an unauthenticated payload must not
// reach a cluster-wide list.
func TestUnverifiedDeliveryCannotSuppress(t *testing.T) {
	w, engine := ingestWorker(t,
		inboundRow("inb4", "postmaster", hardBounceDSN, false),
		map[string]string{"postmaster": FormatRFC3464})

	if _, err := w.handleIngestFeedback(feedbackAutomationCtx(), map[string]any{"inboundRequestId": "inb4"}, 0); err == nil {
		t.Fatal("an unverified delivery was allowed to suppress an address")
	}
	if n := len(engine.fakeEngine.mutations("recordSuppression")); n != 0 {
		t.Errorf("an unverified delivery produced %d suppressions", n)
	}
}

// TestUnconfiguredSourceIsNotAFailure: the automation fires on EVERY inbound
// row, so "not a feedback feed" is the common case and must not log a failure
// for every unrelated webhook the deployment receives.
func TestUnconfiguredSourceIsNotAFailure(t *testing.T) {
	w, engine := ingestWorker(t,
		inboundRow("inb5", "shopify", `{"order":1}`, true),
		map[string]string{"postmaster": FormatRFC3464})

	if _, err := w.handleIngestFeedback(feedbackAutomationCtx(), map[string]any{"inboundRequestId": "inb5"}, 0); err != nil {
		t.Fatalf("an unrelated webhook was reported as a failure: %v", err)
	}
	if n := len(engine.fakeEngine.mutations("updateInboundRequestStatus")); n != 0 {
		t.Errorf("an unrelated webhook's row was stamped %d times; it belongs to another handler", n)
	}
}

// TestFeedbackSourceConfigDropsATypo: a misspelled format must not resolve to
// "the standard one". Parsing SES JSON as a DSN produces no reports and looks
// exactly like a deployment with no bounces.
func TestFeedbackSourceConfigDropsATypo(t *testing.T) {
	sources := parseFeedbackSources("postmaster=rfc3464, ses-feedback=sess, broken")
	if sources["postmaster"] != FormatRFC3464 {
		t.Errorf("a valid entry was dropped: %v", sources)
	}
	if _, present := sources["ses-feedback"]; present {
		t.Error("a misspelled format was accepted; it would have parsed SES JSON as a DSN and reported nothing")
	}
}

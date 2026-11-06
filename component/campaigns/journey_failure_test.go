package campaigns

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// journey_failure_test.go -- an automation email is never dropped and never
// spent on something an operator can fix. A step whose template is unpublished,
// whose sender is turned off or whose organization is not active waits without
// counting an attempt; a step the provider refused is sent again; a step whose
// outcome the provider has not reported waits for the provider's own record of it,
// and is never submitted a second time while it might have been accepted.

// azureLike stands for Azure behind the email transport, as the transport presents
// it. Each submission is answered by the next of its answers, and fails as the
// transport fails for that answer: a refusal is a definite non-acceptance, a lost
// answer says nothing. A throttle keeps the delivery waiting, as Azure's receipt
// does, until the Retry-After it names -- quietWait when it names none -- and a
// delivery reserved inside that wait is refused before anything is attempted. The
// record its operation poller keeps of a submission whose answer was lost is what
// poll found.
type azureLike struct {
	recordingSender // the messages Azure accepted

	gate       sync.Mutex
	now        func() time.Time
	answers    []int
	retryAfter time.Duration // what a throttle names; nothing when zero
	posts      int
	refused    int // reservations refused inside a wait
	waits      map[string]time.Time
	lost       map[string]email.Message
	verdicts   map[string]email.Outcome
	// acceptedAt is when Azure's record says it took a message it accepted;
	// zero when the record does not say.
	acceptedAt time.Time
}

// answerLost is an answer that never came back: the connection dropped after the
// request was sent.
const answerLost = 0

// quietWait is how long a throttle that names no Retry-After keeps a delivery
// waiting: longer than the first retry's backoff, as Azure's receipt can be.
const quietWait = 3 * time.Minute

func (a *azureLike) Reserve(ctx context.Context, msg email.Message, _ email.SendAs, _ ...string) (context.Context, func(), error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	if until := a.waits[msg.IntentID]; a.now().Before(until) {
		a.refused++
		return nil, nil, &email.SendError{Throttled: true, NotAccepted: true, RetryAfter: until.Sub(a.now()), Detail: "Azure asked this delivery to wait"}
	}
	return ctx, func() {}, nil
}

func (a *azureLike) Send(ctx context.Context, msg email.Message, as email.SendAs) error {
	a.gate.Lock()
	a.posts++
	answer := http.StatusAccepted
	if len(a.answers) > 0 {
		answer, a.answers = a.answers[0], a.answers[1:]
	}
	switch answer {
	case answerLost:
		if a.lost == nil {
			a.lost = map[string]email.Message{}
		}
		a.lost[msg.IntentID] = msg
	case http.StatusTooManyRequests:
		wait := a.retryAfter
		if wait <= 0 {
			wait = quietWait
		}
		if a.waits == nil {
			a.waits = map[string]time.Time{}
		}
		a.waits[msg.IntentID] = a.now().Add(wait)
	}
	named := a.retryAfter
	a.gate.Unlock()
	switch answer {
	case http.StatusAccepted:
		return a.recordingSender.Send(ctx, msg, as)
	case answerLost:
		return &email.SendError{Permanent: true, Detail: "Azure email acceptance is unknown; check the Azure delivery log before retrying"}
	case http.StatusTooManyRequests:
		return &email.SendError{StatusCode: answer, Throttled: true, RetryAfter: named, NotAccepted: true, Detail: "Azure did not accept the message"}
	default:
		return &email.SendError{StatusCode: answer, NotAccepted: true, Detail: "Azure did not accept the message"}
	}
}

// poll is the operation poller learning what became of every submission whose
// answer was lost.
func (a *azureLike) poll(outcome email.Outcome) {
	a.gate.Lock()
	defer a.gate.Unlock()
	if a.verdicts == nil {
		a.verdicts = map[string]email.Outcome{}
	}
	for intent, msg := range a.lost {
		a.verdicts[intent] = outcome
		if outcome == email.OutcomeAccepted {
			_ = a.recordingSender.Send(context.Background(), msg, email.SendAs{})
		}
		delete(a.lost, intent)
	}
}

func (a *azureLike) DeliveryOutcome(_ context.Context, msg email.Message, _ email.SendAs) (email.Outcome, error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	return a.verdicts[msg.IntentID], nil
}

// AcceptedAt is when Azure's record says it took a message it accepted.
func (a *azureLike) AcceptedAt(_ context.Context, msg email.Message, _ email.SendAs) (time.Time, error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	if a.verdicts[msg.IntentID] != email.OutcomeAccepted {
		return time.Time{}, nil
	}
	return a.acceptedAt, nil
}

func (a *azureLike) submissions() int {
	a.gate.Lock()
	defer a.gate.Unlock()
	return a.posts
}

// automation is one organization's active automation, with one member enrolled
// and Azure behind its sends. Its clock stands still until a case moves it.
type automation struct {
	*digestNewsletter
	azure                         *azureLike
	journeyID, templateID, sender string
	member                        string // the member's enrollment
	now                           time.Time
}

// shape is what an automation's steps are.
type shape int

const (
	oneEmail     shape = iota // one email
	confirmFirst              // asks the person to confirm, then one email
	waitFirst                 // waits an hour, then one email
)

// newAutomation turns on an automation whose email steps use the newsletter's
// published template and sender. A confirming automation runs in an audience that
// asks people to confirm: its first step asks, and its member is pending.
func newAutomation(t *testing.T, f *digestFixture, name string, steps shape) *automation {
	t.Helper()
	n := f.newEmptyNewsletter(t, name)
	n.worker.journeyGate = NewWorker(n.worker.store.engine, nil, nil, quietLogger(), gatePoolFor(t)).journeyGate
	a := &automation{digestNewsletter: n, azure: &azureLike{}, journeyID: n.suffix + "-journey", templateID: n.suffix + "-tpl", sender: n.suffix + "-sender"}
	a.azure.now = func() time.Time { return a.now }
	n.worker.resolve = func() email.Sender { return a.azure }
	send := map[string]any{"id": "welcome", "kind": "send", "templateId": a.templateID, "senderIdentityId": a.sender}
	plan := []map[string]any{send}
	status := "subscribed"
	switch steps {
	case waitFirst:
		plan = []map[string]any{{"id": "pause", "kind": "wait", "hours": 1}, send}
	case confirmFirst:
		n.execute(t, n.owner, call("mutation", "setAudienceConfirmation", arg{"audienceId", n.audience}, arg{"confirmation", "required"}))
		row := a.template(t)
		if err := n.worker.newsletterWrite(n.owner, "updateTemplate", map[string]any{"templateId": a.templateID, "name": "Confirm", "subject": "Confirm your subscription", "textBody": "Confirm: {{confirmUrl}}", "htmlBody": `<p><a href="{{confirmUrl}}">Confirm</a></p>`, "status": "ready", "versionTime": memql.VersionTimeAfter(templateTime(row["createdAt"]), time.Now()).Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
		plan = []map[string]any{{"id": "confirm", "kind": "confirm", "templateId": a.templateID, "senderIdentityId": a.sender, "timeoutDays": 7}, send}
		status = "pending"
	}
	nodes, err := n.worker.handleSaveJourney(n.owner, map[string]any{"journeyId": a.journeyID, "accountId": n.org, "name": "Welcome", "trigger": map[string]any{"kind": "joined", "audienceId": n.audience}, "steps": plan}, 0)
	if err != nil {
		t.Fatal(err)
	}
	saved := decodeResult(t, nodes)
	if issues, _ := saved["issues"].([]any); len(issues) != 0 {
		t.Fatalf("the automation cannot be turned on: %v", issues)
	}
	if _, err = n.worker.handleSetJourneyState(n.owner, map[string]any{"journeyId": a.journeyID, "action": "activate", "expectedRevision": saved["revision"]}, 0); err != nil {
		t.Fatal(err)
	}
	a.member = a.join(t, "member", status)
	a.now = time.Now().UTC().Truncate(time.Millisecond)
	n.worker.now = func() time.Time { return a.now }
	return a
}

// join adds a person to the audience and names the enrollment they will have.
func (a *automation) join(t *testing.T, name, status string) string {
	t.Helper()
	recipient := a.suffix + "-" + name
	if err := a.store.AddRecipient(a.owner, recipient, a.audience, name+"@example.test", "Person", "signup", a.org, status, nil); err != nil {
		t.Fatal(err)
	}
	member, found, err := a.store.RecipientByID(memql.ContextWithFreshRead(a.owner), recipient)
	if err != nil || !found {
		t.Fatalf("recipient %v %v", found, err)
	}
	return journeyEnrollmentID(a.journeyID, recipient, member.JoinedAt)
}

// drain is one pass of the automation drain over this automation.
func (a *automation) drain(t *testing.T) {
	t.Helper()
	if err := a.worker.runJourney(context.Background(), a.system, map[string]any{"id": a.journeyID}); err != nil {
		t.Fatal(err)
	}
}

// later moves the automation's clock.
func (a *automation) later(d time.Duration) { a.now = a.now.Add(d) }

func (a *automation) enrollment(t *testing.T, id string) map[string]any {
	t.Helper()
	row, err := a.worker.newsletterRow(a.system, "journeyEnrollmentById", "enrollmentId", id)
	if err != nil || row == nil {
		t.Fatalf("enrollment %v %v", row, err)
	}
	return row
}

func (a *automation) journey(t *testing.T) map[string]any {
	t.Helper()
	row, err := a.worker.newsletterRow(a.system, "journeyById", "journeyId", a.journeyID)
	if err != nil || row == nil {
		t.Fatalf("automation %v %v", row, err)
	}
	return row
}

// receipt is the single-send receipt of the member's step.
func (a *automation) receipt(t *testing.T, stepID string) map[string]any {
	t.Helper()
	rows, err := a.store.rows(auth.ContextWithInternalOrigin(memql.ContextWithFreshRead(a.owner)), call("query", "campaignSingleSendById", arg{"receiptId", singleSendReceiptID(a.user, journeyRequestID(a.member, stepID))}))
	if err != nil || len(rows) != 1 {
		t.Fatalf("receipt %v %v", rows, err)
	}
	return rows[0]
}

// delivery is the delivery-ledger row of the member's step.
func (a *automation) delivery(t *testing.T, stepID string) map[string]any {
	t.Helper()
	row, err := a.worker.newsletterRow(a.system, "providerDeliveryById", "deliveryId", singleDeliveryRowID(journeyRequestID(a.member, stepID), a.suffix+"-member"))
	if err != nil || row == nil {
		t.Fatalf("delivery %v %v", row, err)
	}
	return row
}

func (a *automation) template(t *testing.T) map[string]any {
	t.Helper()
	rows, err := a.store.rows(memql.ContextWithFreshRead(a.owner), call("query", "templateById", arg{"templateId", a.templateID}))
	if err != nil || len(rows) != 1 {
		t.Fatalf("template %v %v", rows, err)
	}
	return rows[0]
}

// editTemplate saves or publishes the template as a person does in the editor. A
// save makes it a draft again.
func (a *automation) editTemplate(t *testing.T, action string) {
	t.Helper()
	args := map[string]any{"templateId": a.templateID, "accountId": a.org, "name": "Welcome", "action": action,
		"expectedRevision": templateTime(a.template(t)["createdAt"]).Format(time.RFC3339Nano),
		"content":          map[string]any{"subject": "Welcome", "textBody": "Thanks for subscribing to our news", "htmlBody": "<p>Welcome</p>"}}
	if _, err := a.worker.handleSaveTemplate(a.owner, args, 0); err != nil {
		t.Fatal(err)
	}
}

// setJourneyState changes the automation's state as a person does.
func (a *automation) setJourneyState(t *testing.T, action string) {
	t.Helper()
	if _, err := a.worker.handleSetJourneyState(a.owner, map[string]any{"journeyId": a.journeyID, "action": action, "expectedRevision": templateTime(a.journey(t)["createdAt"]).Format(time.RFC3339Nano)}, 0); err != nil {
		t.Fatal(err)
	}
}

// waitsUntouched fails unless the member's step is still waiting to be sent, with
// no attempt spent on it.
func (a *automation) waitsUntouched(t *testing.T, when string) map[string]any {
	t.Helper()
	row := a.enrollment(t, a.member)
	if str(row, "status") != "active" || integer(row, "stepIndex") != 0 || integer(row, "attempts") != 0 {
		t.Fatalf("%s: the step is %v, want it waiting at its first step with no attempt spent", when, row)
	}
	return row
}

// sentOnceAndAdvanced fails unless exactly posts submissions reached Azure, one
// message was accepted, and the member's one-step run is complete.
func (a *automation) sentOnceAndAdvanced(t *testing.T, posts int) {
	t.Helper()
	if got := a.azure.submissions(); got != posts {
		t.Fatalf("Azure was asked %d times, want %d", got, posts)
	}
	if got := a.azure.count(); got != 1 {
		t.Fatalf("%d messages were accepted, want 1", got)
	}
	if row := a.enrollment(t, a.member); integer(row, "stepIndex") != 1 || str(row, "status") != "completed" || integer(row, "attempts") != 0 {
		t.Fatalf("the step did not advance once: %v", row)
	}
}

// heldFiveTimes drains a step held by its configuration five times, each when it
// is due again, and checks that it waited: nothing reached Azure, no attempt was
// spent, it is due again in 15 minutes, and the automation says why.
func (a *automation) heldFiveTimes(t *testing.T, says string) {
	t.Helper()
	for drain := 1; drain <= 5; drain++ {
		a.drain(t)
		row := a.waitsUntouched(t, "a held step")
		if next := templateTime(row["nextAt"]); !next.Equal(a.now.Add(15 * time.Minute)) {
			t.Fatalf("drain %d: the step is due again at %s, want %s", drain, next, a.now.Add(15*time.Minute))
		}
		a.later(16 * time.Minute)
	}
	if a.azure.submissions() != 0 {
		t.Fatal("a held step reached Azure")
	}
	if got := str(a.journey(t), "lastError"); !strings.Contains(got, says) {
		t.Fatalf("the automation says %q, want it to say %q", got, says)
	}
}

func TestAJourneySendToADraftTemplateWaitsInsteadOfFailing(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-draft", oneEmail)
	// Editing a published template the automation uses makes it a draft again.
	a.editTemplate(t, "save")
	a.heldFiveTimes(t, "Publish")
	a.editTemplate(t, "publish")
	a.drain(t)
	a.sentOnceAndAdvanced(t, 1)
}

func TestAJourneySendWithADisabledSenderWaits(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-sender-off", oneEmail)
	a.execute(t, a.owner, call("mutation", "setSenderIdentityStatus", arg{"senderIdentityId", a.sender}, arg{"status", "disabled"}))
	a.heldFiveTimes(t, "turned off")
	a.execute(t, a.owner, call("mutation", "setSenderIdentityStatus", arg{"senderIdentityId", a.sender}, arg{"status", "active"}))
	a.drain(t)
	a.sentOnceAndAdvanced(t, 1)
}

// TestAJourneyOfAnInactiveOrganizationWaits: nobody is mailed while the
// organization is not active. The people already in the automation wait where they
// are, untouched, and the automation says why.
func TestAJourneyOfAnInactiveOrganizationWaits(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-org-off", waitFirst)
	a.drain(t)
	before := a.enrollment(t, a.member)
	if str(before, "status") != "active" || str(before, "stepId") != "welcome" {
		t.Fatalf("the member is %v, want them enrolled and waiting for the email", before)
	}
	a.execute(t, a.owner, call("mutation", "archiveClientAccount", arg{"accountId", a.org}))
	for range 3 {
		a.later(time.Hour)
		a.drain(t)
	}
	if a.azure.submissions() != 0 {
		t.Fatal("an inactive organization's automation reached Azure")
	}
	if got := str(a.journey(t), "lastError"); !strings.Contains(got, "not active") {
		t.Fatalf("the automation says %q, want it to say the organization is not active", got)
	}
	if after := a.enrollment(t, a.member); !templateTime(after["createdAt"]).Equal(templateTime(before["createdAt"])) {
		t.Fatalf("a held organization changed its member's step: %v, then %v", before, after)
	}
}

// refusedThenAccepted drains a step Azure refuses once with the given answer: the
// refusal spends one attempt and is retried after its backoff, and the retry is
// sent.
func refusedThenAccepted(t *testing.T, name string, answer int) {
	a := newAutomation(t, newDigestFixture(t), name, oneEmail)
	a.azure.answers = []int{answer, http.StatusAccepted}
	a.drain(t)
	if row := a.enrollment(t, a.member); str(row, "status") != "active" || integer(row, "stepIndex") != 0 || integer(row, "attempts") != 1 {
		t.Fatalf("the refused step is %v, want it waiting for its retry with one attempt spent", row)
	}
	a.later(2 * time.Minute)
	a.drain(t)
	a.sentOnceAndAdvanced(t, 2)
}

// TestAProviderRefusalIsRetriedNotSkipped: a throttle spends one attempt and is
// sent again -- no sooner than the provider asked. A retry that still lands inside
// the provider's wait is refused before anything is attempted: it spends nothing,
// leaves the receipt as it was, and waits out what is left.
func TestAProviderRefusalIsRetriedNotSkipped(t *testing.T) {
	f := newDigestFixture(t)
	t.Run("after the wait the throttle names", func(t *testing.T) {
		a := newAutomation(t, f, "journey-throttled", oneEmail)
		a.azure.answers, a.azure.retryAfter = []int{http.StatusTooManyRequests, http.StatusAccepted}, 10*time.Minute
		a.drain(t)
		if row := a.enrollment(t, a.member); integer(row, "attempts") != 1 || templateTime(row["nextAt"]).Before(a.now.Add(10*time.Minute)) {
			t.Fatalf("after a throttle that names 10 minutes the step is %v, want one attempt spent and no retry before then", row)
		}
		a.later(11 * time.Minute)
		a.drain(t)
		a.sentOnceAndAdvanced(t, 2)
		if a.azure.refused != 0 {
			t.Fatalf("the step was retried inside the wait %d times", a.azure.refused)
		}
	})
	t.Run("after the wait the provider keeps", func(t *testing.T) {
		a := newAutomation(t, f, "journey-throttled-quiet", oneEmail)
		a.azure.answers = []int{http.StatusTooManyRequests, http.StatusAccepted}
		a.drain(t)
		receipt := a.receipt(t, "welcome")
		if str(receipt, "status") != receiptFailed {
			t.Fatalf("the throttled send's receipt is %v, want failed", receipt)
		}
		a.later(2 * time.Minute)
		a.drain(t)
		row := a.enrollment(t, a.member)
		if a.azure.submissions() != 1 || a.azure.refused != 1 || integer(row, "attempts") != 1 {
			t.Fatalf("a retry inside the provider's wait: %d submissions, %d refusals, step %v; want it refused with no attempt spent", a.azure.submissions(), a.azure.refused, row)
		}
		if next := templateTime(row["nextAt"]); !next.Equal(a.now.Add(time.Minute)) {
			t.Fatalf("the step is due again at %s, want the end of the provider's wait, %s", next, a.now.Add(time.Minute))
		}
		if again := a.receipt(t, "welcome"); !templateTime(again["createdAt"]).Equal(templateTime(receipt["createdAt"])) {
			t.Fatalf("a retry refused before it was attempted rewrote the receipt: %v, then %v", receipt, again)
		}
		a.later(time.Minute)
		a.drain(t)
		a.sentOnceAndAdvanced(t, 2)
	})
}

func TestAServerErrorIsRetriedNotSkipped(t *testing.T) {
	refusedThenAccepted(t, "journey-unavailable", http.StatusServiceUnavailable)
}

// TestAConfirmationStepRefusedByTheProviderIsResent: the person who asked to
// join must get the confirmation email. Waiting for a confirmation nobody sent
// would leave them waiting a week for a link that never came.
func TestAConfirmationStepRefusedByTheProviderIsResent(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-confirm-refused", confirmFirst)
	a.azure.answers = []int{http.StatusInternalServerError, http.StatusAccepted}
	a.drain(t)
	if row := a.enrollment(t, a.member); str(row, "status") != "active" || integer(row, "attempts") != 1 {
		t.Fatalf("after a refused confirmation the step is %v, want it active with one attempt spent", row)
	}
	a.later(2 * time.Minute)
	a.drain(t)
	if a.azure.submissions() != 2 || a.azure.count() != 1 {
		t.Fatalf("Azure was asked %d times and accepted %d, want 2 and 1", a.azure.submissions(), a.azure.count())
	}
	if row := a.enrollment(t, a.member); str(row, "status") != "waiting" || str(row, "stepId") != "confirm" {
		t.Fatalf("after the confirmation was sent the step is %v, want it waiting for the confirmation", row)
	}
	a.azure.mu.Lock()
	body := a.azure.sent[0].TextBody
	a.azure.mu.Unlock()
	if !strings.Contains(body, ConfirmPath+"?token=") {
		t.Fatalf("the email sent is not the confirmation: %q", body)
	}
}

// TestAnUncertainSendWaitsForTheOperationResult: a submission whose answer was
// lost may have been accepted. The step waits for the operation poller's record of
// it -- and is never submitted again while that record says nothing.
func TestAnUncertainSendWaitsForTheOperationResult(t *testing.T) {
	f := newDigestFixture(t)
	lost := func(t *testing.T, name string) *automation {
		t.Helper()
		a := newAutomation(t, f, name, oneEmail)
		a.azure.answers = []int{answerLost, http.StatusAccepted}
		a.drain(t)
		row := a.waitsUntouched(t, "after a lost answer")
		if next := templateTime(row["nextAt"]); !next.Equal(a.now.Add(5 * time.Minute)) {
			t.Fatalf("after a lost answer the step is due again at %s, want %s", next, a.now.Add(5*time.Minute))
		}
		// The poller has learned nothing yet: the step waits, and nothing is sent.
		a.later(6 * time.Minute)
		a.drain(t)
		a.waitsUntouched(t, "while the operation is unresolved")
		if a.azure.submissions() != 1 {
			t.Fatal("a submission that may have been accepted was submitted again")
		}
		return a
	}
	t.Run("accepted", func(t *testing.T) {
		a := lost(t, "journey-lost-accepted")
		a.azure.poll(email.OutcomeAccepted)
		a.later(6 * time.Minute)
		a.drain(t)
		a.sentOnceAndAdvanced(t, 1)
	})
	t.Run("failed", func(t *testing.T) {
		a := lost(t, "journey-lost-failed")
		a.azure.poll(email.OutcomeNotAccepted)
		a.later(6 * time.Minute)
		a.drain(t)
		if row := a.enrollment(t, a.member); str(row, "status") != "active" || integer(row, "attempts") != 1 || a.azure.submissions() != 1 {
			t.Fatalf("a submission Azure did not accept is %v after %d submissions, want it waiting for its retry with one attempt spent", row, a.azure.submissions())
		}
		a.later(2 * time.Minute)
		a.drain(t)
		a.sentOnceAndAdvanced(t, 2)
	})
	t.Run("never resolved", func(t *testing.T) {
		a := lost(t, "journey-lost-unresolved")
		a.later(24 * time.Hour)
		a.drain(t)
		row := a.enrollment(t, a.member)
		if str(row, "status") != "failed" || !strings.Contains(str(row, "lastError"), "could not be confirmed") || a.azure.submissions() != 1 {
			t.Fatalf("a send unconfirmed for a day is %v after %d submissions, want it failed, saying it could not be confirmed", row, a.azure.submissions())
		}
		if got := str(a.journey(t), "lastError"); !strings.Contains(got, "could not be confirmed") {
			t.Fatalf("the automation says %q", got)
		}
	})
}

// TestTheLedgerSaysSentOnceTheProviderSaysItAccepted: a send whose answer was lost
// is written to the delivery ledger as failed. Once the provider's own record says
// it accepted the email, the ledger says sent -- without waiting for a delivery
// report that may never come -- dated as a delivery report dates it: when the
// provider took it, if its record says, else when its record was read.
func TestTheLedgerSaysSentOnceTheProviderSaysItAccepted(t *testing.T) {
	f := newDigestFixture(t)
	for _, providerSays := range []bool{true, false} {
		name := "dated when the record was read"
		if providerSays {
			name = "dated when the provider took it"
		}
		t.Run(name, func(t *testing.T) {
			a := newAutomation(t, f, "journey-ledger-accepted", oneEmail)
			a.azure.answers = []int{answerLost}
			a.drain(t)
			if row := a.delivery(t, "welcome"); str(row, "status") != "failed" || str(row, "lastError") == "" {
				t.Fatalf("a send whose answer was lost is in the ledger as %v, want failed", row)
			}
			submitted := a.now
			if providerSays {
				a.azure.acceptedAt = submitted
			}
			a.azure.poll(email.OutcomeAccepted)
			a.later(6 * time.Minute)
			a.drain(t)
			a.sentOnceAndAdvanced(t, 1)
			want := a.now
			if providerSays {
				want = submitted
			}
			row := a.delivery(t, "welcome")
			if str(row, "status") != "sent" || str(row, "lastError") != "" || !templateTime(row["sentAt"]).Equal(want) {
				t.Fatalf("the ledger says %v, want sent at %s with no error", row, want)
			}
		})
	}
}

// TestAnUncertainSendWithoutAProviderRecordFailsAtOnce: a transport that keeps no
// record of what it submitted (Graph, SMTP) can never be asked what became of a
// send whose answer said nothing, and has no key that would make a second
// submission safe. The step is never sent again: it fails at once, saying the send
// could not be confirmed and to check before retrying.
func TestAnUncertainSendWithoutAProviderRecordFailsAtOnce(t *testing.T) {
	f := newDigestFixture(t)
	for name, failure := range map[string]error{
		"a server error": &email.SendError{StatusCode: http.StatusServiceUnavailable, Detail: "graph: sendMail 503"},
		"no answer":      errors.New("connection reset by peer"),
	} {
		t.Run(name, func(t *testing.T) {
			a := newAutomation(t, f, "journey-unrecorded", oneEmail)
			submissions := 0
			unrecorded := &recordingSender{fail: func(int) error { submissions++; return failure }}
			a.worker.resolve = func() email.Sender { return unrecorded }
			a.drain(t)
			row := a.enrollment(t, a.member)
			if str(row, "status") != "failed" || !strings.Contains(str(row, "lastError"), "could not be confirmed") || !strings.Contains(str(row, "lastError"), "before retrying") {
				t.Fatalf("an unconfirmable send left the step %v, want it failed, saying it could not be confirmed and to check before retrying", row)
			}
			a.later(time.Hour)
			a.drain(t)
			if submissions != 1 {
				t.Fatalf("an unconfirmable send was submitted %d times", submissions)
			}
		})
	}
}

func TestJourneyLastErrorClearsAfterASuccessfulSend(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-error-clears", oneEmail)
	a.editTemplate(t, "save")
	a.drain(t)
	if str(a.journey(t), "lastError") == "" {
		t.Fatal("a held step left the automation silent")
	}
	a.editTemplate(t, "publish")
	a.later(16 * time.Minute)
	a.drain(t)
	a.sentOnceAndAdvanced(t, 1)
	journey := a.journey(t)
	if got := str(journey, "lastError"); got != "" {
		t.Fatalf("the automation still says %q after its email was sent", got)
	}
	// With nothing to clear, a send writes nothing on the automation: its revision
	// is what a person's next change is checked against.
	revision := templateTime(journey["createdAt"])
	second := a.join(t, "second", "subscribed")
	a.drain(t)
	if row := a.enrollment(t, second); str(row, "status") != "completed" || a.azure.count() != 2 {
		t.Fatalf("the second member's run is %v after %d messages", row, a.azure.count())
	}
	if got := templateTime(a.journey(t)["createdAt"]); !got.Equal(revision) {
		t.Fatalf("a send rewrote an automation that had nothing to clear (revision %s, then %s)", revision, got)
	}
}

// pauseOnce pauses the automation once, the first time the drain makes the
// statement named.
type pauseOnce struct {
	Engine
	statement string
	pause     func()
	once      sync.Once
}

func (e *pauseOnce) Execute(ctx context.Context, statement string) (any, error) {
	result, err := e.Engine.Execute(ctx, statement)
	if strings.HasPrefix(statement, e.statement) {
		e.once.Do(e.pause)
	}
	return result, err
}

// TestPausingHoldsASendThatIsDue: a pause that lands after the drain chose a step
// and before its email reached Azure holds it. Nothing is sent and nothing is
// spent, and the step is sent once the automation is on again.
func TestPausingHoldsASendThatIsDue(t *testing.T) {
	f := newDigestFixture(t)
	for _, tc := range []struct {
		name, lands string
		pause       func(t *testing.T, a *automation)
	}{
		{"after the step was chosen", "query dueEnrollmentsForJourney", func(t *testing.T, a *automation) { a.setJourneyState(t, "pause") }},
		// Read inside the step's hold, right before delivery: whatever changed the
		// automation since the step began, a paused one sends nothing.
		{"while its email is prepared", "query campaignSingleSendById", func(t *testing.T, a *automation) {
			if err := a.worker.newsletterWrite(a.owner, "setJourneyStatus", map[string]any{"journeyId": a.journeyID, "status": "paused", "versionTime": memql.VersionTimeAfter(templateTime(a.journey(t)["createdAt"]), a.now).Format(time.RFC3339Nano)}); err != nil {
				t.Error(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAutomation(t, f, "journey-pause", oneEmail)
			actual := a.worker.store.engine
			a.worker.store.engine = &pauseOnce{Engine: actual, statement: tc.lands, pause: func() { tc.pause(t, a) }}
			a.drain(t)
			a.worker.store.engine = actual
			if a.azure.submissions() != 0 {
				t.Fatal("a paused automation sent its email")
			}
			if got := str(a.journey(t), "status"); got != "paused" {
				t.Fatalf("the automation is %q, want paused", got)
			}
			a.waitsUntouched(t, "while paused")
			a.setJourneyState(t, "activate")
			a.drain(t)
			a.sentOnceAndAdvanced(t, 1)
		})
	}
}

// TestASendWithoutARuleCarriesNoCampaignHeader: an automation step, like a
// welcome, belongs to no campaign and no rule. Its email carries no campaign
// header rather than an empty one; a rule's send names the rule.
func TestASendWithoutARuleCarriesNoCampaignHeader(t *testing.T) {
	for name, rule := range map[string]string{"a rule's send": testEmailRule, "a send with no rule": ""} {
		t.Run(name, func(t *testing.T) {
			args := sendToRecipientArgs()
			if rule == "" {
				delete(args, "emailRuleId")
			}
			sender := &recordingSender{}
			if _, err := newTestWorker(t, recipientSendEngine(), sender).handleSendToRecipient(importCtx(), args, 0); err != nil {
				t.Fatal(err)
			}
			if value, present := sender.sent[0].Headers[headerCampaignID]; present != (rule != "") || value != rule {
				t.Fatalf("the campaign header is %q (present %v), want %q", value, present, rule)
			}
		})
	}
}

// TestOnlyConfigurationRefusalsHoldASend: the refusals an operator fixes in place
// -- an unpublished or archived template, a sender that is turned off or gone --
// hold a send, saying what to fix. Every other refusal keeps its own handling.
func TestOnlyConfigurationRefusalsHoldASend(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(*fakeEngine, map[string]any)
		says    string
	}{
		"a draft template":     {func(e *fakeEngine, _ map[string]any) { e.template["status"] = "draft" }, "Publish"},
		"an archived template": {func(e *fakeEngine, _ map[string]any) { e.template["status"] = "archived" }, "archived"},
		"a sender turned off": {func(e *fakeEngine, args map[string]any) {
			e.senderIdentities = map[string]map[string]any{"si-retired": senderIdentityRow("si-retired", "old@acme.test", "Acme", "disabled")}
			args["senderIdentityId"] = "si-retired"
		}, "old@acme.test is turned off"},
		"a sender that is gone": {func(_ *fakeEngine, args map[string]any) { args["senderIdentityId"] = "si-unknown" }, "no longer exists"},
		"a draft with no name or subject": {func(e *fakeEngine, _ map[string]any) {
			e.template["status"], e.template["subject"] = "draft", ""
		}, "Publish this template"},
		"an unreadable template":  {func(e *fakeEngine, _ map[string]any) { e.template = nil }, ""},
		"an unreadable recipient": {func(e *fakeEngine, _ map[string]any) { e.roster = nil }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			engine := recipientSendEngine()
			args := sendToRecipientArgs()
			tc.arrange(engine, args)
			sender := &recordingSender{}
			_, err := newTestWorker(t, engine, sender).handleSendToRecipient(importCtx(), args, 0)
			var held sendHeld
			switch {
			case err == nil || sender.count() != 0:
				t.Fatalf("the send went: %v", err)
			case tc.says == "" && errors.As(err, &held):
				t.Fatalf("a refusal on the merits holds the send: %v", err)
			case tc.says != "" && (!errors.As(err, &held) || !strings.Contains(err.Error(), tc.says)):
				t.Fatalf("the refusal is %v, want it to hold the send saying %q", err, tc.says)
			}
		})
	}
}

package campaigns

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// journey_failure_test.go -- an automation email is never dropped and never
// spent on something an operator can fix. A step whose template is unpublished,
// whose sender is turned off or whose organization is not active waits without
// counting an attempt; a step the provider refused is sent again; a step whose
// outcome the provider has not reported waits for the provider's own record of it,
// and is never submitted a second time while it might have been accepted.

// azureLike stands for Azure behind the email transport. Each submission is
// answered by the next of its answers, and fails as the transport fails for that
// answer: a refusal is a definite non-acceptance, a lost answer says nothing. The
// record its operation poller keeps of a submission whose answer was lost is what
// poll found.
type azureLike struct {
	recordingSender // the messages Azure accepted

	gate     sync.Mutex
	answers  []int
	posts    int
	lost     map[string]email.Message
	verdicts map[string]email.Outcome
}

// answerLost is an answer that never came back: the connection dropped after the
// request was sent.
const answerLost = 0

func (a *azureLike) Send(ctx context.Context, msg email.Message, as email.SendAs) error {
	a.gate.Lock()
	a.posts++
	answer := http.StatusAccepted
	if len(a.answers) > 0 {
		answer, a.answers = a.answers[0], a.answers[1:]
	}
	if answer == answerLost {
		if a.lost == nil {
			a.lost = map[string]email.Message{}
		}
		a.lost[msg.IntentID] = msg
	}
	a.gate.Unlock()
	switch answer {
	case http.StatusAccepted:
		return a.recordingSender.Send(ctx, msg, as)
	case answerLost:
		return &email.SendError{Permanent: true, Detail: "Azure email acceptance is unknown; check the Azure delivery log before retrying"}
	default:
		return &email.SendError{StatusCode: answer, Throttled: answer == http.StatusTooManyRequests, NotAccepted: true, Detail: "Azure did not accept the message"}
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

// newAutomation turns on an automation whose one email step uses the
// newsletter's published template and sender. A confirming automation runs in an
// audience that asks people to confirm: its first step asks, and its member is
// pending.
func newAutomation(t *testing.T, f *digestFixture, name string, confirming bool) *automation {
	t.Helper()
	n := f.newEmptyNewsletter(t, name)
	n.worker.journeyGate = NewWorker(n.worker.store.engine, nil, nil, quietLogger(), gatePoolFor(t)).journeyGate
	a := &automation{digestNewsletter: n, azure: &azureLike{}, journeyID: n.suffix + "-journey", templateID: n.suffix + "-tpl", sender: n.suffix + "-sender"}
	n.worker.resolve = func() email.Sender { return a.azure }
	steps := []map[string]any{{"id": "welcome", "kind": "send", "templateId": a.templateID, "senderIdentityId": a.sender}}
	status := "subscribed"
	if confirming {
		n.execute(t, n.owner, call("mutation", "setAudienceConfirmation", arg{"audienceId", n.audience}, arg{"confirmation", "required"}))
		row := a.template(t)
		if err := n.worker.newsletterWrite(n.owner, "updateTemplate", map[string]any{"templateId": a.templateID, "name": "Confirm", "subject": "Confirm your subscription", "textBody": "Confirm: {{confirmUrl}}", "htmlBody": `<p><a href="{{confirmUrl}}">Confirm</a></p>`, "status": "ready", "versionTime": memql.VersionTimeAfter(templateTime(row["createdAt"]), time.Now()).Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
		steps = []map[string]any{{"id": "confirm", "kind": "confirm", "templateId": a.templateID, "senderIdentityId": a.sender, "timeoutDays": 7}, steps[0]}
		status = "pending"
	}
	nodes, err := n.worker.handleSaveJourney(n.owner, map[string]any{"journeyId": a.journeyID, "accountId": n.org, "name": "Welcome", "trigger": map[string]any{"kind": "joined", "audienceId": n.audience}, "steps": steps}, 0)
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
	a := newAutomation(t, newDigestFixture(t), "journey-draft", false)
	// Editing a published template the automation uses makes it a draft again.
	a.editTemplate(t, "save")
	a.heldFiveTimes(t, "Publish")
	a.editTemplate(t, "publish")
	a.drain(t)
	a.sentOnceAndAdvanced(t, 1)
}

func TestAJourneySendWithADisabledSenderWaits(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-sender-off", false)
	a.execute(t, a.owner, call("mutation", "setSenderIdentityStatus", arg{"senderIdentityId", a.sender}, arg{"status", "disabled"}))
	a.heldFiveTimes(t, "turned off")
	a.execute(t, a.owner, call("mutation", "setSenderIdentityStatus", arg{"senderIdentityId", a.sender}, arg{"status", "active"}))
	a.drain(t)
	a.sentOnceAndAdvanced(t, 1)
}

// TestAJourneyOfAnInactiveOrganizationWaits: nobody is enrolled or mailed while the
// organization is not active, and the automation says why.
func TestAJourneyOfAnInactiveOrganizationWaits(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-org-off", false)
	a.execute(t, a.owner, call("mutation", "archiveClientAccount", arg{"accountId", a.org}))
	for range 3 {
		a.drain(t)
		a.later(time.Minute)
	}
	if a.azure.submissions() != 0 {
		t.Fatal("an inactive organization's automation reached Azure")
	}
	if got := str(a.journey(t), "lastError"); !strings.Contains(got, "not active") {
		t.Fatalf("the automation says %q, want it to say the organization is not active", got)
	}
	if row, err := a.worker.newsletterRow(a.system, "journeyEnrollmentById", "enrollmentId", a.member); err != nil || row != nil && (str(row, "status") != "active" || integer(row, "attempts") != 0) {
		t.Fatalf("a held organization spent its member's step: %v %v", row, err)
	}
}

// refusedThenAccepted drains a step Azure refuses once with the given answer: the
// refusal spends one attempt and is retried after its backoff, and the retry is
// sent.
func refusedThenAccepted(t *testing.T, name string, answer int) {
	a := newAutomation(t, newDigestFixture(t), name, false)
	a.azure.answers = []int{answer, http.StatusAccepted}
	a.drain(t)
	if row := a.enrollment(t, a.member); str(row, "status") != "active" || integer(row, "stepIndex") != 0 || integer(row, "attempts") != 1 {
		t.Fatalf("the refused step is %v, want it waiting for its retry with one attempt spent", row)
	}
	a.later(2 * time.Minute)
	a.drain(t)
	a.sentOnceAndAdvanced(t, 2)
}

func TestAProviderRefusalIsRetriedNotSkipped(t *testing.T) {
	refusedThenAccepted(t, "journey-throttled", http.StatusTooManyRequests)
}

func TestAServerErrorIsRetriedNotSkipped(t *testing.T) {
	refusedThenAccepted(t, "journey-unavailable", http.StatusServiceUnavailable)
}

// TestAConfirmationStepRefusedByTheProviderIsResent: the person who asked to
// join must get the confirmation email. Waiting for a confirmation nobody sent
// would leave them waiting a week for a link that never came.
func TestAConfirmationStepRefusedByTheProviderIsResent(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-confirm-refused", true)
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
		a := newAutomation(t, f, name, false)
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

func TestJourneyLastErrorClearsAfterASuccessfulSend(t *testing.T) {
	a := newAutomation(t, newDigestFixture(t), "journey-error-clears", false)
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
			a := newAutomation(t, f, "journey-pause", false)
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
		"a sender that is gone":   {func(_ *fakeEngine, args map[string]any) { args["senderIdentityId"] = "si-unknown" }, "no longer exists"},
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

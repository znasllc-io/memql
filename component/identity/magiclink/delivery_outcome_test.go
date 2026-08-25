package magiclink

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

// delivery_outcome_test.go -- the audit half of memql#4477.
//
// The finding was that mail fails UPWARD: with no sender configured the
// engine wrote the message to the pod log, returned nil, and every layer
// above reported success. One of those layers is this one. `magic_link_issued`
// was stamped AuditOutcomeSuccess unconditionally, so the audit trail -- the
// one place an operator looks when a person says a link never arrived -- said
// the link went out.
//
// A row is still written and the request still succeeds, and both are
// deliberate: the row is the credential, the person can ask for another link,
// and the HTTP response must stay identical whatever happened so it cannot be
// used to enumerate which addresses are registered. What changes is only what
// the trail SAYS.

type auditRecorder struct{ events []identity.AuditEvent }

func (a *auditRecorder) Log(_ context.Context, ev identity.AuditEvent) {
	a.events = append(a.events, ev)
}

func (a *auditRecorder) find(action string) *identity.AuditEvent {
	for i := range a.events {
		if a.events[i].Action == action {
			return &a.events[i]
		}
	}
	return nil
}

// refusingSender stands in for the LogSender on an install that must deliver
// mail: it accepts the call and reports, honestly, that nothing was sent.
type refusingSender struct {
	err   error
	calls int
}

func (s *refusingSender) SendMagicLink(_ context.Context, _ SendInput) error {
	s.calls++
	return s.err
}

func (s *refusingSender) SendSignInDisabledNotice(_ context.Context, _ NoticeInput) error {
	return s.err
}

func newAuditedIssuer(sender Sender) (*Issuer, *policyEngine, *auditRecorder) {
	eng := &policyEngine{policy: "any"}
	rec := &auditRecorder{}
	return &Issuer{
		Cfg: identity.Config{
			BaseURL:          "https://identity.test",
			BrandName:        "MemQL",
			RegistrationMode: identity.RegistrationModeOpen,
		},
		Store:  &identity.Store{Engine: eng, Logger: slog.Default()},
		Sender: sender,
		Audit:  rec,
		Logger: slog.Default(),
	}, eng, rec
}

func TestAuditRecordsDeliveryFailureOnMagicLinkIssue(t *testing.T) {
	sender := &refusingSender{err: errors.New("email: log-only mode refused")}
	iss, eng, rec := newAuditedIssuer(sender)

	if _, err := iss.Issue(context.Background(), IssueInput{
		Email:        "owner@acme.test",
		AdminSession: true,
		SourceIP:     "203.0.113.9",
	}); err != nil {
		t.Fatalf("Issue must not fail the request when delivery fails: %v", err)
	}

	if sender.calls != 1 {
		t.Fatalf("the sender was called %d time(s), want 1 -- the assertions below prove nothing "+
			"about a send that never happened", sender.calls)
	}
	if len(eng.writes) != 1 {
		t.Fatalf("the magicLinkRequest row was written %d time(s), want 1: a failed DELIVERY must "+
			"not discard the credential", len(eng.writes))
	}

	ev := rec.find("magic_link_issued")
	if ev == nil {
		t.Fatalf("no magic_link_issued audit event was emitted; got %d event(s)", len(rec.events))
	}
	if ev.Outcome != identity.AuditOutcomeFailure {
		t.Errorf("outcome = %q, want %q.\n\nAn operator reading the trail after 'my link never "+
			"arrived' sees this row. Stamped success, it sends them to their spam folder and to "+
			"their DNS records, and never to the sender configuration that is actually wrong.",
			ev.Outcome, identity.AuditOutcomeFailure)
	}
	if ev.FailureReason != "delivery_failed" {
		t.Errorf("failureReason = %q, want %q", ev.FailureReason, "delivery_failed")
	}
}

// TestAuditRecordsSuccessWhenDeliveryWorks is the negative control: without
// it, code that stamped every issue as a failure would pass the test above.
func TestAuditRecordsSuccessWhenDeliveryWorks(t *testing.T) {
	iss, _, rec := newAuditedIssuer(&refusingSender{err: nil})

	if _, err := iss.Issue(context.Background(), IssueInput{
		Email:        "owner@acme.test",
		AdminSession: true,
	}); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	ev := rec.find("magic_link_issued")
	if ev == nil {
		t.Fatal("no magic_link_issued audit event was emitted")
	}
	if ev.Outcome != identity.AuditOutcomeSuccess {
		t.Errorf("outcome = %q, want %q", ev.Outcome, identity.AuditOutcomeSuccess)
	}
	if ev.FailureReason != "" {
		t.Errorf("failureReason = %q, want empty", ev.FailureReason)
	}
}

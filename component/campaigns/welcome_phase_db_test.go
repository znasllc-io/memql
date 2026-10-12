package campaigns

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// welcome_phase_db_test.go -- only a send that began can end a welcome
// (memql#6022, F9 fix round 4).
//
// A welcome holds its locks for a bounded phase: its send's timeout and the reads
// around it. When that time ran out before the send began -- a read under the
// locks that was slow, the send's own first read that was -- the read's error was
// written as the welcome's verdict, "blocked", on a context the phase did not
// bound. A blocked welcome is never retried, and nothing had been attempted.

// slowWhileHeld delays one query while the storefront's locks are held, the way a
// slow database delays it, and gives up when its caller's context does.
type slowWhileHeld struct {
	Engine
	held   *atomic.Bool
	prefix string
	delay  time.Duration
	hits   atomic.Int32
}

func (e *slowWhileHeld) Execute(ctx context.Context, q string) (any, error) {
	if e.held.Load() && strings.HasPrefix(q, e.prefix) {
		e.hits.Add(1)
		select {
		case <-time.After(e.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return e.Engine.Execute(ctx, q)
}

// TestAWelcomeWhoseTimeRunsOutBeforeItsSendBeginsStaysPending: the phase runs out
// in the reads that decide whether the welcome may be sent, or in the send's own
// first read. Either way nothing was attempted: the welcome stays pending -- no
// verdict, no receipt, no mail -- and the next pass sends it.
func TestAWelcomeWhoseTimeRunsOutBeforeItsSendBeginsStaysPending(t *testing.T) {
	for _, slow := range []string{
		"query newsletterForSite",      // the storefront's configuration, read under the locks
		"query campaignSingleSendById", // the send's first read, before any attempt
	} {
		t.Run(strings.TrimPrefix(slow, "query "), func(t *testing.T) {
			n := newEmptyNewsletter(t, "welcome-late")
			signup := pendingWelcome(t, n)
			receiptID := welcomeReceiptID(signup)
			actual := n.worker.store.engine
			var held atomic.Bool
			engine := &slowWhileHeld{Engine: actual, held: &held, prefix: slow, delay: 2 * time.Second}
			n.worker.store.engine = engine
			restoreGate := heldGate(n, &held)
			margin, sendTimeout := welcomePhaseMargin, n.worker.cfg.SendTimeout
			welcomePhaseMargin, n.worker.cfg.SendTimeout = 300*time.Millisecond, 100*time.Millisecond
			system := n.worker.systemActorContext(context.Background())

			err := n.worker.sendNewsletterWelcome(context.Background(), system, signup)

			if engine.hits.Load() == 0 {
				t.Fatalf("%s was never read under the locks; the test is not testing what it says", slow)
			}
			row := n.signupRow(t)
			if got := str(row, "status"); got != "pending" {
				t.Errorf("a welcome whose time ran out before its send began is %q (%q), want pending: nothing was attempted", got, str(row, "lastError"))
			}
			if err == nil {
				t.Error("a welcome whose time ran out answered nil; the drain would not know it stayed pending")
			}
			if n.sender.count() != 0 {
				t.Errorf("%d messages were sent", n.sender.count())
			}
			if receipt := n.singleSendReceipt(t, receiptID); receipt != nil {
				t.Errorf("an attempt was recorded for a send that never began: %v", receipt)
			}

			// The next pass, with the time it needs, sends it.
			n.worker.store.engine = actual
			restoreGate()
			welcomePhaseMargin, n.worker.cfg.SendTimeout = margin, sendTimeout
			if err := n.worker.sendNewsletterWelcome(context.Background(), system, signup); err != nil {
				t.Fatal(err)
			}
			if got := str(n.signupRow(t), "status"); got != "sent" {
				t.Errorf("the welcome is %q on the next pass, want sent", got)
			}
			if n.sender.count() != 1 {
				t.Errorf("%d messages were sent, want 1", n.sender.count())
			}
		})
	}
}

// TestAWelcomeWhoseTransportIsBusyStaysPendingAndGoesOutOnTheNextPass: the
// transport's own lock (Azure's send receipt, reserved before the attempt is
// recorded) is busy. Nothing was attempted -- no receipt, no mail -- so the welcome
// is not blocked; the drain ends its pass on the busy answer and the next pass,
// with the transport free, sends it.
func TestAWelcomeWhoseTransportIsBusyStaysPendingAndGoesOutOnTheNextPass(t *testing.T) {
	n := newEmptyNewsletter(t, "welcome-transport-busy")
	signup := pendingWelcome(t, n)
	receiptID := welcomeReceiptID(signup)
	sender := &reservingSender{recordingSender: n.sender, busyFor: 1}
	n.worker.resolve = func() email.Sender { return sender }
	system := n.worker.systemActorContext(context.Background())

	err := n.worker.sendNewsletterWelcome(context.Background(), system, signup)

	if !memql.IsWriteGateBusy(err) {
		t.Fatalf("the welcome answered %v, want the busy answer (the drain ends its pass on it)", err)
	}
	row := n.signupRow(t)
	if got := str(row, "status"); got != "pending" {
		t.Errorf("the welcome is %q after a busy transport (%q), want pending: a blocked welcome is never retried", got, str(row, "lastError"))
	}
	if n.sender.count() != 0 {
		t.Errorf("%d messages were sent through a busy transport", n.sender.count())
	}
	if receipt := n.singleSendReceipt(t, receiptID); receipt != nil {
		t.Errorf("an attempt was recorded for a send the transport refused to begin: %v", receipt)
	}

	if err := n.worker.sendNewsletterWelcome(context.Background(), system, signup); err != nil {
		t.Fatal(err)
	}
	if got := str(n.signupRow(t), "status"); got != "sent" {
		t.Errorf("the welcome is %q on the next pass, want sent", got)
	}
	if n.sender.count() != 1 {
		t.Errorf("%d messages were sent, want 1", n.sender.count())
	}
	if sender.reserved != 1 || sender.released != 1 {
		t.Errorf("%d reservations granted and %d given back, want 1 and 1", sender.reserved, sender.released)
	}
}

// TestAWelcomeWhoseSendBeganIsEndedByItEvenPastItsPhase: the other half of the
// rule. The send began -- its attempt was recorded -- and the transport took longer
// than the welcome's phase; the provider accepted the message. The welcome is
// ended by that send, sent, although its time under the locks is over: the
// verdict of a send that began is written whatever the time, while the locks are
// held.
func TestAWelcomeWhoseSendBeganIsEndedByItEvenPastItsPhase(t *testing.T) {
	n := newEmptyNewsletter(t, "welcome-slow-send")
	signup := pendingWelcome(t, n)
	margin, sendTimeout := welcomePhaseMargin, n.worker.cfg.SendTimeout
	welcomePhaseMargin, n.worker.cfg.SendTimeout = 300*time.Millisecond, 100*time.Millisecond
	defer func() { welcomePhaseMargin, n.worker.cfg.SendTimeout = margin, sendTimeout }()
	n.sender.fail = func(int) error { time.Sleep(time.Second); return nil } // accepted, late
	defer func() { n.sender.fail = nil }()

	err := n.worker.sendNewsletterWelcome(context.Background(), n.worker.systemActorContext(context.Background()), signup)

	if err != nil {
		t.Fatalf("a welcome whose send began and was accepted answered %v", err)
	}
	if got := str(n.signupRow(t), "status"); got != "sent" {
		t.Errorf("a welcome whose send began and was accepted past its phase is %q, want sent", got)
	}
	if n.sender.count() != 1 {
		t.Errorf("%d messages were sent, want 1", n.sender.count())
	}
}

// TestAWelcomeWritesNoVerdictAfterItsLocksWereLost: PostgreSQL ends the welcome's
// gate session after its send, before its verdict, and the locks go with it. The
// verdict is not written -- another replica may hold the locks by now -- and
// nothing is lost: the next pass finds the send's receipt and records the welcome
// as sent without sending it again.
func TestAWelcomeWritesNoVerdictAfterItsLocksWereLost(t *testing.T) {
	n := newEmptyNewsletter(t, "welcome-lost")
	signup := pendingWelcome(t, n)
	actual := n.worker.store.engine
	ender := &sessionEnder{Engine: actual, admin: n.db.DB, key: newsletterLock("audience", n.audience), t: t,
		on: func(q string) bool {
			return strings.HasPrefix(q, "mutation recordCampaignSingleSend") && strings.Contains(q, "complete")
		}}
	n.worker.store.engine = ender
	restoreGate := heldGate(n, &ender.held)
	system := n.worker.systemActorContext(context.Background())

	err := n.worker.sendNewsletterWelcome(context.Background(), system, signup)

	if !ender.ended.Load() {
		t.Fatal("the test never ended the gate's session; it is not testing what it says")
	}
	if !errors.Is(err, memql.ErrWriteGateLost) {
		t.Errorf("a welcome whose locks were lost before its verdict answered %v, want ErrWriteGateLost", err)
	}
	if got := str(n.signupRow(t), "status"); got != "pending" {
		t.Errorf("a welcome whose locks were lost before its verdict is %q, want pending: the verdict was written without them", got)
	}
	if n.sender.count() != 1 {
		t.Fatalf("%d messages were sent, want 1", n.sender.count())
	}

	n.worker.store.engine = actual
	restoreGate()
	if err := n.worker.sendNewsletterWelcome(context.Background(), system, signup); err != nil {
		t.Fatal(err)
	}
	if got := str(n.signupRow(t), "status"); got != "sent" {
		t.Errorf("the welcome is %q on the next pass, want sent", got)
	}
	if n.sender.count() != 1 {
		t.Errorf("%d messages were sent after the next pass, want still 1: the receipt answers for the send", n.sender.count())
	}
}

package campaigns

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

// These cases share a booted engine; each subtest owns and cleans its rows.
func TestBusyGatesDB(t *testing.T) {
	f := newDigestFixture(t)
	f.run(t, "AWelcomeWhoseSendLockIsBusyStaysPendingAndGoesOutOnTheNextPass", f.TestAWelcomeWhoseSendLockIsBusyStaysPendingAndGoesOutOnTheNextPass)
	f.run(t, "AWelcomeTakesItsLocksOnOneConnection", f.TestAWelcomeTakesItsLocksOnOneConnection)
	f.run(t, "AnAttemptIsRecordedOnlyAfterTheTransportsLockIsReserved", f.TestAnAttemptIsRecordedOnlyAfterTheTransportsLockIsReserved)
}

// busy_gate_db_test.go -- a lock that was busy is "nothing attempted, try later"
// on every path that sends mail, against a real engine and Postgres (memql#6022,
// F9 fix round 3).
//
// The marketing lane records that an attempt has begun BEFORE it contacts the
// transport, and replay never sends a message whose receipt says an attempt began.
// A busy lock that arrives after that record is therefore a message lost for good,
// and one recorded as a verdict (a welcome set to "blocked") is a person who was
// never welcomed. So nothing that can be busy may come after the record, and a busy
// answer before it leaves everything as it was.

// holdsKey holds an advisory lock by its full key from another connection, the way
// another replica's flow would, until the test releases it.
func holdsKey(t *testing.T, n *digestNewsletter, key string) (release func()) {
	t.Helper()
	tx, err := n.db.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("SELECT pg_advisory_xact_lock(1296387409, hashtext($1))", key); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback() }) }
	t.Cleanup(release)
	return release
}

// pendingWelcome subscribes the default mailbox and returns the pending signup,
// the row the welcome drain scans.
func pendingWelcome(t *testing.T, n *digestNewsletter) map[string]any {
	t.Helper()
	n.args["email"] = consentMailbox
	if _, err := n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatal(err)
	}
	signup := n.signupRow(t)
	if signup == nil || str(signup, "status") != "pending" {
		t.Fatalf("the signup is not a pending welcome: %v", signup)
	}
	return signup
}

// singleSendReceipts reads the single-send receipt for an id: nil when no attempt
// was ever recorded.
func (n *digestNewsletter) singleSendReceipt(t *testing.T, receiptID string) map[string]any {
	t.Helper()
	rows, err := n.worker.store.rows(auth.ContextWithInternalOrigin(n.owner), call("query", "campaignSingleSendById", arg{"receiptId", receiptID}))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

func welcomeReceiptID(signup map[string]any) string {
	return singleSendReceiptID(bare(str(signup, "ownerUserId")), "newsletter-"+bare(str(signup, "id")))
}

// TestAWelcomeWhoseSendLockIsBusyStaysPendingAndGoesOutOnTheNextPass: the lock of
// the welcome's own send is held by someone else. The welcome is not blocked and
// no verdict is written -- a blocked welcome is never retried, and this one was not
// refused, only early. Nothing was attempted: no receipt, no mail. The next pass,
// with the lock free, sends it.
func (f *digestFixture) TestAWelcomeWhoseSendLockIsBusyStaysPendingAndGoesOutOnTheNextPass(t *testing.T) {
	n := f.newEmptyNewsletter(t, "welcome-busy")
	signup := pendingWelcome(t, n)
	receiptID := welcomeReceiptID(signup)
	release := holdsKey(t, n, singleSendLock(receiptID))
	wait := newsletterGateWait
	newsletterGateWait = 300 * time.Millisecond
	defer func() { newsletterGateWait = wait }()
	system := n.worker.systemActorContext(context.Background())

	err := n.worker.sendNewsletterWelcome(context.Background(), system, signup)

	if !memql.IsWriteGateBusy(err) {
		t.Fatalf("the welcome answered %v, want the busy answer (the drain ends its pass on it)", err)
	}
	row := n.signupRow(t)
	if got := str(row, "status"); got != "pending" {
		t.Errorf("the welcome is %q after a busy lock (%q), want pending: a blocked welcome is never retried", got, str(row, "lastError"))
	}
	if n.sender.count() != 0 {
		t.Errorf("%d messages were sent through a busy lock", n.sender.count())
	}
	if receipt := n.singleSendReceipt(t, receiptID); receipt != nil {
		t.Errorf("an attempt was recorded for a send that never began: %v", receipt)
	}

	release()
	if err := n.worker.sendNewsletterWelcome(context.Background(), system, signup); err != nil {
		t.Fatal(err)
	}
	if got := str(n.signupRow(t), "status"); got != "sent" {
		t.Errorf("the welcome is %q on the next pass, want sent", got)
	}
	if n.sender.count() != 1 {
		t.Errorf("%d messages were sent, want 1", n.sender.count())
	}
}

// TestAWelcomeTakesItsLocksOnOneConnection: the storefront's lock, the audience's
// and the welcome's own send receipt are named together and held by one transaction
// on one connection. Taken one inside another they are a connection each, and a
// pod whose gate pool is nearly spent could find the welcome holding two and
// waiting for a third. Here the pool IS one connection.
func (f *digestFixture) TestAWelcomeTakesItsLocksOnOneConnection(t *testing.T) {
	n := f.newEmptyNewsletter(t, "welcome-one")
	signup := pendingWelcome(t, n)
	tiny, err := database.NewGatePool(dbtest.DSN(), 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tiny.Close() })
	wired := NewWorker(deliveryDBEngine{n.engine}, nil, nil, quietLogger(), func() *sql.DB { return tiny })
	n.worker.newsletterGate, n.worker.templateGate, n.worker.singleSendGate = wired.newsletterGate, wired.templateGate, wired.singleSendGate
	wait := newsletterGateWait
	newsletterGateWait = 1500 * time.Millisecond
	defer func() { newsletterGateWait = wait }()

	started := time.Now()
	err = n.worker.sendNewsletterWelcome(context.Background(), n.worker.systemActorContext(context.Background()), signup)

	if err != nil {
		t.Fatalf("a welcome on a gate pool of one connection: %v (after %s)", err, time.Since(started).Round(time.Millisecond))
	}
	if got := str(n.signupRow(t), "status"); got != "sent" {
		t.Errorf("the welcome is %q, want sent", got)
	}
}

// reservingSender is a transport that must reserve a lock before it can begin a
// delivery (email.Reserver), as Azure's does: it reports, at the moment of each
// reservation, whether the marketing lane had already recorded an attempt.
type reservingSender struct {
	*recordingSender
	mu         sync.Mutex
	busyFor    int // the first n reservations are refused as busy
	reserved   int
	released   int
	attemptsAt []bool // for each reservation: did a receipt for the intent already exist?
	probe      func(intent string) bool
}

func (s *reservingSender) Reserve(ctx context.Context, msg email.Message, _ email.SendAs) (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attemptsAt = append(s.attemptsAt, s.probe != nil && s.probe(msg.IntentID))
	if s.busyFor > 0 {
		s.busyFor--
		return nil, nil, &email.SendError{Throttled: true, RetryAfter: time.Second, Detail: "busy", Cause: memql.ErrWriteGateBusy}
	}
	s.reserved++
	return ctx, func() { s.mu.Lock(); s.released++; s.mu.Unlock() }, nil
}

// TestAnAttemptIsRecordedOnlyAfterTheTransportsLockIsReserved: the transport's own
// lock is asked for BEFORE the lane records that an attempt began. Refused as
// busy, nothing was attempted -- no receipt exists -- and the same request, made
// again, goes out. Were the receipt written first, the refusal would leave a
// message whose receipt says an attempt began, which replay never sends again.
func (f *digestFixture) TestAnAttemptIsRecordedOnlyAfterTheTransportsLockIsReserved(t *testing.T) {
	n := f.newEmptyNewsletter(t, "reserve-first")
	recipient := n.suffix + "-rcp"
	n.execute(t, n.owner, call("mutation", "addRecipient", arg{"recipientId", recipient}, arg{"audienceId", n.audience}, arg{"accountId", n.org}, arg{"email", "reserve@example.test"}))
	args := map[string]any{"templateId": n.suffix + "-tpl", "recipientId": recipient, "senderIdentityId": n.suffix + "-sender", "requestId": "reserve-first-once"}
	receiptID := singleSendReceiptID(callerUserID(n.owner), "reserve-first-once")
	sender := &reservingSender{recordingSender: n.sender, busyFor: 1}
	sender.probe = func(string) bool { return n.singleSendReceipt(t, receiptID) != nil }
	n.worker.resolve = func() email.Sender { return sender }

	_, err := n.worker.handleSendToRecipient(n.owner, args, 0)

	if !memql.IsWriteGateBusy(err) {
		t.Fatalf("a refused reservation answered %v, want the busy answer", err)
	}
	if _, throttled := email.IsThrottled(err); !throttled && !errors.Is(err, memql.ErrWriteGateBusy) {
		t.Errorf("the refusal lost its cause: %v", err)
	}
	if receipt := n.singleSendReceipt(t, receiptID); receipt != nil {
		t.Fatalf("an attempt was recorded for a send the transport refused to begin: %v", receipt)
	}
	if n.sender.count() != 0 {
		t.Fatalf("%d messages were sent through a refused reservation", n.sender.count())
	}

	// The same request, again: reserved, recorded, sent, completed.
	nodes, err := n.worker.handleSendToRecipient(n.owner, args, 0)
	if err != nil {
		t.Fatal(err)
	}
	if decodeResult(t, nodes)["sent"] != true {
		t.Fatalf("the retry did not send: %v", decodeResult(t, nodes))
	}
	if n.sender.count() != 1 {
		t.Errorf("%d messages were sent, want 1", n.sender.count())
	}
	if got := str(n.singleSendReceipt(t, receiptID), "status"); got != "complete" {
		t.Errorf("the receipt is %q after the send, want complete", got)
	}
	for i, existed := range sender.attemptsAt {
		if existed {
			t.Errorf("reservation %d found a receipt already recorded: the attempt was recorded before the transport's lock was reserved", i)
		}
	}
	if sender.reserved != 1 || sender.released != 1 {
		t.Errorf("%d reservations granted and %d given back, want 1 and 1", sender.reserved, sender.released)
	}
}

// TestASingleSendWhoseCallerLeavesDuringItsAttemptKeepsItsLockUntilItCommits: the
// caller's request ends while the receipt that says an attempt began is being
// written. The write is waited for and commits while the send's lock is held: the
// gate's transaction is not the caller's, and is not rolled back -- the lock with it
// -- under the write by the caller's leaving.
func TestASingleSendWhoseCallerLeavesDuringItsAttemptKeepsItsLockUntilItCommits(t *testing.T) {
	n := newEmptyNewsletter(t, "single-caller-leaves")
	recipient := n.suffix + "-rcp"
	n.execute(t, n.owner, call("mutation", "addRecipient", arg{"recipientId", recipient}, arg{"audienceId", n.audience}, arg{"accountId", n.org}, arg{"email", "leaves@example.test"}))
	args := map[string]any{"templateId": n.suffix + "-tpl", "recipientId": recipient, "senderIdentityId": n.suffix + "-sender", "requestId": "caller-leaves-once"}
	receiptID := singleSendReceiptID(callerUserID(n.owner), "caller-leaves-once")
	request, leave := context.WithCancel(n.owner)
	defer leave()
	actual := n.worker.store.engine
	engine := &straddlingWrite{Engine: actual, prefix: "mutation recordCampaignSingleSend", contains: "attempting", delay: 400 * time.Millisecond, locked: n.lockHeld(t, singleSendLock(receiptID)), during: leave}
	n.worker.store.engine = engine
	defer func() { n.worker.store.engine = actual }()

	_, _ = n.worker.handleSendToRecipient(request, args, 0)
	engine.wg.Wait()

	engine.mu.Lock()
	held := append([]bool(nil), engine.held...)
	engine.mu.Unlock()
	if len(held) != 1 {
		t.Fatalf("the attempt was recorded %d times; the test is not testing what it says", len(held))
	}
	if !held[0] {
		t.Error("the caller's leaving ended the send's lock while its attempt was being recorded: the receipt committed without it")
	}
}

// TestASingleSendRecordsNoAttemptAfterItsLockWasLost: PostgreSQL ends the gate
// session that holds the send's receipt lock after the transport's lock was
// reserved and before the attempt is recorded. The receipt that says an attempt
// began is written only if the gate says its lock is still held: a replica that took
// the lock since would read no receipt and send the message again. Here nothing is
// recorded and nothing is sent, and the same request, made again, goes out once.
func TestASingleSendRecordsNoAttemptAfterItsLockWasLost(t *testing.T) {
	n := newEmptyNewsletter(t, "single-lost-first")
	recipient := n.suffix + "-rcp"
	n.execute(t, n.owner, call("mutation", "addRecipient", arg{"recipientId", recipient}, arg{"audienceId", n.audience}, arg{"accountId", n.org}, arg{"email", "lost@example.test"}))
	args := map[string]any{"templateId": n.suffix + "-tpl", "recipientId": recipient, "senderIdentityId": n.suffix + "-sender", "requestId": "lost-before-attempt"}
	receiptID := singleSendReceiptID(callerUserID(n.owner), "lost-before-attempt")
	ender := &sessionEnder{admin: n.db.DB, key: singleSendLock(receiptID), t: t}
	reserving := &reservingSender{recordingSender: n.sender}
	reserving.probe = func(string) bool { ender.endOnce(); return false }
	n.worker.resolve = func() email.Sender { return reserving }

	_, err := n.worker.handleSendToRecipient(n.owner, args, 0)

	if !ender.ended.Load() {
		t.Fatal("the test never ended the gate's session; it is not testing what it says")
	}
	if !errors.Is(err, memql.ErrWriteGateLost) {
		t.Errorf("a send whose lock was lost before its attempt answered %v, want ErrWriteGateLost", err)
	}
	if receipt := n.singleSendReceipt(t, receiptID); receipt != nil {
		t.Errorf("an attempt was recorded without the lock that protects it: %v", receipt)
	}
	if n.sender.count() != 0 {
		t.Errorf("%d messages were sent without a recorded attempt", n.sender.count())
	}

	n.worker.resolve = func() email.Sender { return n.sender }
	nodes, err := n.worker.handleSendToRecipient(n.owner, args, 0)
	if err != nil {
		t.Fatal(err)
	}
	if decodeResult(t, nodes)["sent"] != true {
		t.Fatalf("the request made again did not send: %v", decodeResult(t, nodes))
	}
	if n.sender.count() != 1 {
		t.Errorf("%d messages were sent, want 1", n.sender.count())
	}
}

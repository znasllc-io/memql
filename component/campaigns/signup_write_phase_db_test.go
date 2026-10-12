package campaigns

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database"
)

// signup_write_phase_db_test.go -- a signup's write commits while its locks are
// held, or does not start (memql#6022, F9 fix round 4).
//
// The signup's writes ran on the phase's context. A deadline that landed during a
// write ended the client's wait for it -- pgdriver gives up at a context's
// deadline -- while the server went on and committed it; the signup, told its time
// was up, gave its locks back, and the write landed after them, beside whichever
// signup took them next. The same for a visitor who went away during a write: the
// gate's transaction was bound to the request, and the request's end rolled it
// back -- and the locks with it -- under the write.

// straddlingWrite makes one kind of write slow the way a slow server does: the
// statement runs for `delay` whatever the client does, and when the client's
// context ends first the client stops waiting -- and the server still commits it.
// When it commits, it asks Postgres whether the audience's lock is held.
type straddlingWrite struct {
	Engine
	prefix string
	delay  time.Duration
	locked func() bool
	during func()
	mu     sync.Mutex
	held   []bool // per commit: was the lock held when the write committed?
	wg     sync.WaitGroup
}

func (e *straddlingWrite) Execute(ctx context.Context, q string) (any, error) {
	if !strings.HasPrefix(q, e.prefix) {
		return e.Engine.Execute(ctx, q)
	}
	if e.during != nil {
		e.during()
	}
	type answer struct {
		value any
		err   error
	}
	committed := make(chan answer, 1)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		time.Sleep(e.delay)
		value, err := e.Engine.Execute(context.WithoutCancel(ctx), q)
		e.mu.Lock()
		e.held = append(e.held, e.locked())
		e.mu.Unlock()
		committed <- answer{value, err}
	}()
	select {
	case a := <-committed:
		return a.value, a.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// audienceLockHeld asks Postgres whether a session holds the audience's lock.
func (n *digestNewsletter) audienceLockHeld(t *testing.T) func() bool {
	return func() bool {
		var held int
		if err := n.db.DB.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted
			AND objsubid = 2 AND classid = 1296387409::oid AND objid = hashtext($1)::oid`, newsletterLock("audience", n.audience)).Scan(&held); err != nil {
			t.Errorf("pg_locks: %v", err)
		}
		return held > 0
	}
}

// TestASignupWriteThatOutlastsItsPhaseCommitsUnderItsLocks: the phase's deadline
// lands while the consent is being written. The write is waited for and commits
// while the locks are held; nothing is written after it, and the person is told to
// try again (consent first, keyed by the signup, makes that safe).
func TestASignupWriteThatOutlastsItsPhaseCommitsUnderItsLocks(t *testing.T) {
	n := newEmptyNewsletter(t, "phase-straddle")
	actual := n.worker.store.engine
	// The phase leaves the reads before the write ample time (a shared database may
	// be slow), and the write outlasts it.
	engine := &straddlingWrite{Engine: actual, prefix: "mutation recordConsentGrant", delay: 3 * time.Second, locked: n.audienceLockHeld(t)}
	n.worker.store.engine = engine
	defer func() { n.worker.store.engine = actual }()
	limit := signupLockedPhase
	signupLockedPhase = 2 * time.Second
	defer func() { signupLockedPhase = limit }()

	n.args["email"] = consentMailbox
	_, err := n.worker.handleSubscribe(n.owner, n.args, 0)
	engine.wg.Wait()

	engine.mu.Lock()
	held := append([]bool(nil), engine.held...)
	engine.mu.Unlock()
	if len(held) != 1 {
		t.Fatalf("the consent was written %d times; the test is not testing what it says", len(held))
	}
	if !held[0] {
		t.Error("the consent committed after the signup had given its locks back")
	}
	if err == nil || err.Error() != "Too many signups at once. Try again in a moment." {
		t.Errorf("a signup whose time ran out during a write answered %v, want the sentence", err)
	}
	if n.consentRow(t) == nil {
		t.Error("the consent the signup waited for is not there")
	}
	if n.signupRow(t) != nil || len(n.roster(t)) != 0 {
		t.Error("the signup wrote after its time was up")
	}
}

// TestASignupWhoseVisitorLeavesDuringAWriteKeepsItsLocksUntilItCommits: the
// request ends while the consent is being written. The locks stay held until the
// write has committed -- the gate's transaction is not the request's -- and
// nothing is written after it.
func TestASignupWhoseVisitorLeavesDuringAWriteKeepsItsLocksUntilItCommits(t *testing.T) {
	n := newEmptyNewsletter(t, "phase-gone")
	request, leave := context.WithCancel(n.owner)
	defer leave()
	actual := n.worker.store.engine
	engine := &straddlingWrite{Engine: actual, prefix: "mutation recordConsentGrant", delay: 400 * time.Millisecond, locked: n.audienceLockHeld(t), during: leave}
	n.worker.store.engine = engine
	defer func() { n.worker.store.engine = actual }()

	n.args["email"] = consentMailbox
	_, err := n.worker.handleSubscribe(request, n.args, 0)
	engine.wg.Wait()

	engine.mu.Lock()
	held := append([]bool(nil), engine.held...)
	engine.mu.Unlock()
	if len(held) != 1 {
		t.Fatalf("the consent was written %d times; the test is not testing what it says", len(held))
	}
	if !held[0] {
		t.Error("the visitor's leaving ended the locks while the consent was being written: it committed without them")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a signup whose visitor left answered %v, want the request's end", err)
	}
	if n.signupRow(t) != nil || len(n.roster(t)) != 0 {
		t.Error("the signup wrote after its visitor left")
	}
}

// TestAWelcomeWhoseDrainStopsDuringItsVerdictKeepsItsLocksUntilItCommits: the
// same for a welcome. The worker stops while the welcome's verdict is being
// written; the locks stay held until it commits, and the welcome is sent.
func TestAWelcomeWhoseDrainStopsDuringItsVerdictKeepsItsLocksUntilItCommits(t *testing.T) {
	n := newEmptyNewsletter(t, "welcome-stopped")
	signup := pendingWelcome(t, n)
	drain, stop := context.WithCancel(context.Background())
	defer stop()
	actual := n.worker.store.engine
	engine := &straddlingWrite{Engine: actual, prefix: "mutation updateNewsletterWelcome", delay: 400 * time.Millisecond, locked: n.audienceLockHeld(t), during: stop}
	n.worker.store.engine = engine
	defer func() { n.worker.store.engine = actual }()

	err := n.worker.sendNewsletterWelcome(drain, n.worker.systemActorContext(drain), signup)
	engine.wg.Wait()

	engine.mu.Lock()
	held := append([]bool(nil), engine.held...)
	engine.mu.Unlock()
	if len(held) != 1 {
		t.Fatalf("the verdict was written %d times; the test is not testing what it says", len(held))
	}
	if !held[0] {
		t.Error("the drain's stopping ended the welcome's locks while its verdict was being written: it committed without them")
	}
	if err != nil {
		t.Errorf("a welcome that was sent answered %v", err)
	}
	if got := str(n.signupRow(t), "status"); got != "sent" {
		t.Errorf("the welcome is %q, want sent", got)
	}
}

// TestALockedWriteOutlastsTheServersStatementTimeout: a write under the locks is
// waited for up to lockedWriteLimit, which is above the request pool's statement
// timeout -- the server ends a slow write before the client stops waiting for it,
// so the answer is read before the locks are given back -- and the longest a
// holder then keeps its locks (its phase, and one write begun at its very end)
// stays far inside the gate session's idle limit.
func TestALockedWriteOutlastsTheServersStatementTimeout(t *testing.T) {
	if lockedWriteLimit <= database.DefaultRequestStatementTimeout {
		t.Errorf("a locked write is waited for %s, not above the request pool's statement timeout (%s): the client could stop waiting for a write the server then commits",
			lockedWriteLimit, database.DefaultRequestStatementTimeout)
	}
	if held := signupLockedPhase + lockedWriteLimit; held*4 > database.DefaultGateIdleInTxTimeout {
		t.Errorf("a signup may hold its locks %s; the gate session is ended after %s idle, and a holder should use a quarter of it at most",
			held, database.DefaultGateIdleInTxTimeout)
	}
	w := newTestWorker(t, &fakeEngine{}, &recordingSender{})
	for _, sendTimeout := range []time.Duration{5 * time.Second, 30 * time.Second, time.Hour} {
		w.cfg.SendTimeout = sendTimeout
		if held := w.welcomeLockedPhase() + lockedWriteLimit; held*2 > database.DefaultGateIdleInTxTimeout {
			t.Errorf("with a send timeout of %s a welcome may hold its locks %s; the gate session is ended after %s idle",
				sendTimeout, held, database.DefaultGateIdleInTxTimeout)
		}
	}
}

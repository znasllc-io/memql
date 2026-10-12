package campaigns

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database"
)

// signup_phase_db_test.go -- what a signup does under its locks, and what it does
// when the locks are gone or its time under them is up (memql#6022, F9 fix round 3).
//
// The locks are an idle transaction on the gate pool. Gates on the request pool lost
// their locks at its 60 s idle limit while the holders went on writing: 16 signups at
// once, the audience's locks gone from pg_locks at 60.2 s. The gate pool's sessions
// are allowed minutes, and the signup's phase under its locks is bounded at 30 s;
// and a holder asks, immediately before it writes, whether its locks are still
// there. These tests make the locks go, and the time run out, on purpose.

// sessionEnder ends the gate session holding a key's lock the first time the
// signup, while it holds its locks, asks for the audience's size (or makes the
// query `on` picks) -- the way the server ends a session for reasons of its own --
// and does not return until the lock is gone from the server.
type sessionEnder struct {
	Engine
	admin *sql.DB
	key   string
	on    func(q string) bool // nil: the audience's size
	held  atomic.Bool
	ended atomic.Bool
	t     *testing.T
}

func (e *sessionEnder) triggers(q string) bool {
	if e.on != nil {
		return e.on(q)
	}
	return strings.HasPrefix(q, "query audienceRosterSize")
}

func (e *sessionEnder) Execute(ctx context.Context, q string) (any, error) {
	if e.held.Load() && e.triggers(q) {
		e.endOnce()
	}
	return e.Engine.Execute(ctx, q)
}

// endOnce ends the session holding the key's lock, the first time it is asked, and
// does not return until the lock is gone from the server. A test that wants the
// session gone at a moment no query marks (a transport's reservation, say) calls
// it from there.
func (e *sessionEnder) endOnce() {
	if e.ended.Swap(true) {
		return
	}
	var pid int
	if err := e.admin.QueryRow(`SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND granted
		AND objsubid = 2 AND classid = 1296387409::oid AND objid = hashtext($1)::oid`, e.key).Scan(&pid); err != nil {
		e.t.Errorf("no session holds %q: %v", e.key, err)
	} else if _, err := e.admin.Exec("SELECT pg_terminate_backend($1)", pid); err != nil {
		e.t.Errorf("could not end the session: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		_ = e.admin.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted
			AND objsubid = 2 AND classid = 1296387409::oid AND objid = hashtext($1)::oid`, e.key).Scan(&n)
		if n == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// heldGate wraps the worker's newsletter gate so the engine under it can tell
// when the locks are held.
func heldGate(n *digestNewsletter, held *atomic.Bool) (restore func()) {
	gate := n.worker.newsletterGate
	n.worker.newsletterGate = func(ctx context.Context, keys ...string) (gateHold, error) {
		hold, err := gate(ctx, keys...)
		if err != nil {
			return nil, err
		}
		held.Store(true)
		return watchedHold{gateHold: hold, released: func() { held.Store(false) }}, nil
	}
	return func() { n.worker.newsletterGate = gate }
}

// TestASignupWhoseLocksWereEndedWritesNothing: the server ends the gate's session
// between the signup's reads and its writes. The advisory lock goes with it, and
// the signup, which was not told, asks before it writes -- and writes nothing: no
// consent, no member, no signup. The person is told to try again.
func TestASignupWhoseLocksWereEndedWritesNothing(t *testing.T) {
	n := newEmptyNewsletter(t, "phase-lost")
	actual := n.worker.store.engine
	ender := &sessionEnder{Engine: actual, admin: n.db.DB, key: "campaign-newsletter:audience:" + n.audience, t: t}
	n.worker.store.engine = ender
	defer func() { n.worker.store.engine = actual }()
	defer heldGate(n, &ender.held)()

	n.args["email"] = consentMailbox
	_, err := n.worker.handleSubscribe(n.owner, n.args, 0)

	if !ender.ended.Load() {
		t.Fatal("the test never ended the gate's session; it is not testing what it says")
	}
	if err == nil || err.Error() != "Too many signups at once. Try again in a moment." {
		t.Fatalf("a signup whose locks were gone answered %v, want the sentence", err)
	}
	n.assertTheFormDidNothing(t, 0)
}

// TestASignupThatRunsPastItsTimeUnderTheLocksWritesNothing: the phase under the
// locks has a deadline, far inside the gate session's idle limit. A signup whose
// reads under them take longer than it is cut off before it writes anything.
func TestASignupThatRunsPastItsTimeUnderTheLocksWritesNothing(t *testing.T) {
	n := newEmptyNewsletter(t, "phase-late")
	actual := n.worker.store.engine
	var held atomic.Bool
	n.worker.store.engine = slowUnderLocks{Engine: actual, held: &held, delay: always(400 * time.Millisecond)}
	defer func() { n.worker.store.engine = actual }()
	defer heldGate(n, &held)()
	limit := signupLockedPhase
	signupLockedPhase = 500 * time.Millisecond
	defer func() { signupLockedPhase = limit }()

	n.args["email"] = consentMailbox
	started := time.Now()
	_, err := n.worker.handleSubscribe(n.owner, n.args, 0)

	if err == nil || err.Error() != "Too many signups at once. Try again in a moment." {
		t.Fatalf("a signup past its time under the locks answered %v, want the sentence", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("a signup with a 500ms phase took %s", elapsed)
	}
	n.assertTheFormDidNothing(t, 0)

	// The locks were given back: the next signup, at the real limit, is taken.
	signupLockedPhase = limit
	n.worker.store.engine = actual
	if _, err := n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatalf("the signup after the late one: %v", err)
	}
	if !n.signedUp(t, consentMailbox) {
		t.Error("the signup after the late one was not recorded")
	}
}

// slowUnderLocks delays the O(cluster) reads a signup makes while it holds its
// locks, the way a large cluster does. delay says how long for a given query.
type slowUnderLocks struct {
	Engine
	held  *atomic.Bool
	delay func(query string) time.Duration
}

func (e slowUnderLocks) Execute(ctx context.Context, q string) (any, error) {
	if e.held.Load() && (strings.HasPrefix(q, "query recipientInAudienceByDigest") || strings.HasPrefix(q, "query audienceRosterSize")) {
		select {
		case <-time.After(e.delay(q)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return e.Engine.Execute(ctx, q)
}

func always(d time.Duration) func(string) time.Duration {
	return func(string) time.Duration { return d }
}

// writeLog records, in order, which signup made each write under the locks.
type writeLog struct {
	Engine
	mu     sync.Mutex
	owners map[string]int // an address or its digest -> the signup's number
	events []int
}

func (e *writeLog) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "mutation recordConsentGrant") || strings.HasPrefix(q, "mutation addRecipient") || strings.HasPrefix(q, "mutation recordNewsletterSignup") {
		for token, who := range e.owners {
			if strings.Contains(q, token) {
				e.mu.Lock()
				e.events = append(e.events, who)
				e.mu.Unlock()
				break
			}
		}
	}
	return e.Engine.Execute(ctx, q)
}

// TestABurstOfSignupsLosesNoLockAndWritesNothingPastItsTime: the reviewer's probe
// shape -- sixteen signups at once for one audience, on a request pool of four
// connections, slow under their locks -- with the gate on a pool of its own and a
// short phase. Half the signups are slow enough to run out of their time under the
// locks. Whatever each signup ends up with, it is one of three things: taken whole;
// told to try again with nothing written; or refused for the audience's ceiling. The
// audience never holds more than its ceiling, the signups that ran out of time wrote
// nothing, and no two signups' writes interleave -- which a lock lost under a holder
// that went on writing would be.
func TestABurstOfSignupsLosesNoLockAndWritesNothingPastItsTime(t *testing.T) {
	const signups, ceiling = 16, 5
	const sentence = "Too many signups at once. Try again in a moment."
	n := newEmptyNewsletter(t, "phase-burst")
	n.db.DB.SetMaxOpenConns(4) // the request pool, as deployed
	n.worker.cfg.MaxAudience = ceiling

	var held atomic.Bool
	forms := make([]map[string]any, signups)
	owners := map[string]int{}
	slowDigests := map[string]bool{}
	for i := 0; i < signups; i++ {
		address := fmt.Sprintf("burst%02d@example.test", i)
		forms[i] = n.formFor(address)
		owners[address], owners[EmailDigest(address)] = i, i
		if i%2 == 0 {
			slowDigests[EmailDigest(address)] = true
		}
	}
	delay := func(q string) time.Duration {
		for digest := range slowDigests {
			if strings.Contains(q, digest) {
				return 800 * time.Millisecond // past the phase below
			}
		}
		return 20 * time.Millisecond
	}
	log := &writeLog{Engine: n.worker.store.engine, owners: owners}
	actual := n.worker.store.engine
	n.worker.store.engine = slowUnderLocks{Engine: log, held: &held, delay: delay}
	defer func() { n.worker.store.engine = actual }()
	defer heldGate(n, &held)()
	limit, wait := signupLockedPhase, newsletterGateWait
	signupLockedPhase, newsletterGateWait = 400*time.Millisecond, 2500*time.Millisecond
	defer func() { signupLockedPhase, newsletterGateWait = limit, wait }()

	errs := signupsTogether(n.worker, n.owner, forms)

	taken, told, refused := 0, 0, 0
	for i, err := range errs {
		address := fmt.Sprintf("burst%02d@example.test", i)
		switch {
		case err == nil:
			taken++
			if !n.signedUp(t, address) {
				t.Errorf("signup %d was taken but no signup was recorded", i)
			}
		case err.Error() == sentence:
			told++
			if n.signedUp(t, address) {
				t.Errorf("signup %d was told to try again, and a signup was recorded for it anyway", i)
			}
		case strings.Contains(err.Error(), "cannot accept more subscribers"):
			refused++
		default:
			t.Errorf("signup %d answered %v: not taken, not told to try again, not refused for the ceiling", i, err)
		}
	}
	if taken == 0 || told == 0 {
		t.Errorf("%d taken, %d told to try again, %d refused for the ceiling: the burst did not exercise both outcomes", taken, told, refused)
	}
	roster := n.roster(t)
	if len(roster) > ceiling {
		t.Errorf("the audience holds %d members after the burst, past its ceiling of %d", len(roster), ceiling)
	}
	if len(roster) != taken {
		t.Errorf("the audience holds %d members, %d signups were taken", len(roster), taken)
	}

	log.mu.Lock()
	events := append([]int(nil), log.events...)
	log.mu.Unlock()
	// The slow signups ran out of their phase before their writes: they wrote nothing.
	for _, who := range events {
		if who%2 == 0 {
			t.Errorf("signup %d ran out of its time under the locks and wrote anyway: %v", who, events)
			break
		}
	}
	// No interleaving: each signup's writes are one unbroken run in the order they
	// happened.
	seen := map[int]bool{}
	for i, who := range events {
		if i > 0 && events[i-1] != who && seen[who] {
			t.Errorf("signup %d's writes were interrupted by another signup's: %v", who, events)
			break
		}
		seen[who] = true
	}
}

// TestEveryLockedPhaseIsFarInsideTheGateSessionsIdleLimit: the phase limits are
// read against the limit they must stay inside, not repeated.
func TestEveryLockedPhaseIsFarInsideTheGateSessionsIdleLimit(t *testing.T) {
	if signupLockedPhase*4 > database.DefaultGateIdleInTxTimeout {
		t.Errorf("a signup may spend %s under its locks; the gate session is ended after %s idle, and a phase should use a quarter of it at most",
			signupLockedPhase, database.DefaultGateIdleInTxTimeout)
	}
	w := newTestWorker(t, &fakeEngine{}, &recordingSender{})
	for _, sendTimeout := range []time.Duration{5 * time.Second, 30 * time.Second, time.Hour} {
		w.cfg.SendTimeout = sendTimeout
		if phase := w.welcomeLockedPhase(); phase*2 > database.DefaultGateIdleInTxTimeout {
			t.Errorf("with a send timeout of %s a welcome may spend %s under its locks; the gate session is ended after %s idle, and a phase should use half of it at most",
				sendTimeout, phase, database.DefaultGateIdleInTxTimeout)
		}
	}
}

// sizeCounter counts the audience-size reads a signup makes, and when.
type sizeCounter struct {
	Engine
	held  *atomic.Bool
	mu    sync.Mutex
	early int // before the locks
	under int // while they were held
}

func (e *sizeCounter) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "query audienceRosterSize") {
		e.mu.Lock()
		if e.held.Load() {
			e.under++
		} else {
			e.early++
		}
		e.mu.Unlock()
	}
	return e.Engine.Execute(ctx, q)
}

// TestASignupCountsTheAudienceOncePerDecision: a new member's ceiling is decided
// twice, as it must be -- before the locks, as a fast refusal, and again under the
// audience's lock, where it is exact -- and the second count before the locks that the
// send preflight used to make for itself is the same number as the first. Each is a
// read of every recipient in the cluster.
func TestASignupCountsTheAudienceOncePerDecision(t *testing.T) {
	n := newEmptyNewsletter(t, "count-once")
	actual := n.worker.store.engine
	var held atomic.Bool
	counter := &sizeCounter{Engine: actual, held: &held}
	n.worker.store.engine = counter
	defer func() { n.worker.store.engine = actual }()
	defer heldGate(n, &held)()

	n.args["email"] = consentMailbox
	if _, err := n.worker.handleSubscribe(n.owner, n.args, 0); err != nil {
		t.Fatal(err)
	}
	counter.mu.Lock()
	early, under := counter.early, counter.under
	counter.mu.Unlock()
	if early != 1 || under != 1 {
		t.Errorf("a new member's signup counted the audience %d times before its locks and %d under them, want 1 and 1", early, under)
	}

	// A member who is already there needs no room: the preflight counts for itself,
	// once, and nothing is counted under the locks.
	counter.mu.Lock()
	counter.early, counter.under = 0, 0
	counter.mu.Unlock()
	again := n.formFor(consentMailbox)
	again["submissionId"] = "again"
	if _, err := n.worker.handleSubscribe(n.owner, again, 0); err != nil {
		t.Fatal(err)
	}
	counter.mu.Lock()
	early, under = counter.early, counter.under
	counter.mu.Unlock()
	if early > 1 || under != 0 {
		t.Errorf("a repeated signup counted the audience %d times before its locks and %d under them, want at most 1 and 0", early, under)
	}
}

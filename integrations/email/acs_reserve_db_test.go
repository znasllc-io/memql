package email

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
)

// acs_reserve_db_test.go -- the send receipt's lock, against a real PostgreSQL
// (memql#6022, F9 fix round 3).
//
// An ACS delivery takes one write gate, keyed by its intent, around the receipt:
// read it, write "submitting", POST, write the outcome. A gate that is busy is a
// refusal made BEFORE anything is attempted -- never after: a message whose receipt
// says "submitting" is never submitted again, so a busy answer that arrived
// between the POST and the outcome would turn an accepted message into a delivery
// that can neither be sent again nor be known to have been sent.

// gatedOperations is the in-memory receipts with the real gate under their lock:
// one memql write gate, on a pool of its own, as storedACSOperations takes it.
type gatedOperations struct {
	memoryACSOperations
	pool  *sql.DB
	locks atomic.Int32
	// waitFor is how long a lock waits for the gate; zero is the gate's default.
	waitFor time.Duration
}

func (g *gatedOperations) lock(ctx context.Context, key string) (func(), error) {
	g.locks.Add(1)
	return memql.AcquireWriteGatesWithin(ctx, g.pool, g.waitFor, "email-operation:"+key)
}

func reserveFixture(t *testing.T, poolSize int) (*ACSSender, *gatedOperations, *sql.DB) {
	t.Helper()
	reachable, err := dbtest.EnsureSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reachable {
		dbtest.Unreachable(t, "ACS reservations", dbtest.DSN(), nil)
		return nil, nil, nil
	}
	pool, err := database.NewGatePool(dbtest.DSN(), poolSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	other := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	t.Cleanup(func() { _ = other.Close() })
	s := acsFixture(t)
	ops := &gatedOperations{pool: pool}
	s.operations = ops
	return s, ops, other
}

// operationLockKey is the key the gate is taken on for an intent, as the
// operation store names it.
func operationLockKey(s *ACSSender, intent string) string {
	return "email-operation:" + s.operationKey(intent)
}

// lockHeldBy reports whether another session holds the gate's advisory lock.
func lockHeldBy(t *testing.T, db *sql.DB, key string) bool {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var got bool
	if err := tx.QueryRow("SELECT pg_try_advisory_xact_lock(1296387409, hashtext($1))", key).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return !got
}

// holdLock holds the gate's lock for the key from a session of its own.
func holdLock(t *testing.T, db *sql.DB, key string) (release func()) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("SELECT pg_advisory_xact_lock(1296387409, hashtext($1))", key); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback()
		}
	}
	t.Cleanup(release)
	return release
}

// asClient is the identity the fixture's connection belongs to.
var asClient = SendAs{AccountID: "client"}

func accepted() *http.Response {
	return &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"status":"Running"}`))}
}

// TestADeliveryHoldsItsLockAcrossThePostAndTakesItOnce: nothing between the
// receipt that says "submitting" and the receipt that says what happened can
// refuse for being busy, because nothing between them asks for the lock: it is held
// from before the first receipt to after the last.
func TestADeliveryHoldsItsLockAcrossThePostAndTakesItOnce(t *testing.T) {
	s, ops, other := reserveFixture(t, 4)
	msg := Message{IntentID: "campaign:held:recipient:one", To: "x@example.test", Subject: "Hello", TextBody: "Body"}
	key := operationLockKey(s, msg.IntentID)
	var heldDuringPost atomic.Bool
	s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		heldDuringPost.Store(lockHeldBy(t, other, key))
		return accepted(), nil
	})

	if err := s.Send(context.Background(), msg, asClient); err != nil {
		t.Fatal(err)
	}

	if !heldDuringPost.Load() {
		t.Error("the receipt's lock was not held while the message was being submitted: the receipt that says what happened is written without it")
	}
	if got := ops.locks.Load(); got != 1 {
		t.Errorf("the lock was taken %d times for one delivery, want 1: a second acquisition after the POST can be refused for being busy", got)
	}
	if lockHeldBy(t, other, key) {
		t.Error("the lock is still held after the delivery")
	}
	if op := ops.rows[s.operationKey(msg.IntentID)]; op.Status != "accepted" {
		t.Errorf("the receipt says %q after an accepted send, want accepted", op.Status)
	}
}

// TestAReservationTakesTheLockBeforeAnAttemptIsRecordedAndSendUsesIt: a caller that
// records "an attempt has begun" before it hands the message over reserves first.
// The reservation holds the lock; the send that follows does not ask for it again
// (it would wait for the reservation's own transaction); the caller gives it back
// when the delivery is over.
func TestAReservationTakesTheLockBeforeAnAttemptIsRecordedAndSendUsesIt(t *testing.T) {
	s, ops, other := reserveFixture(t, 4)
	msg := Message{IntentID: "campaign-single:reserved", To: "x@example.test", Subject: "Hello", TextBody: "Body"}
	key := operationLockKey(s, msg.IntentID)
	var posts atomic.Int32
	s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		posts.Add(1)
		return accepted(), nil
	})

	ctx, release, err := s.Reserve(context.Background(), msg, asClient)
	if err != nil {
		t.Fatal(err)
	}
	if !lockHeldBy(t, other, key) {
		t.Fatal("a reservation holds no lock")
	}
	if ops.writes != 0 || posts.Load() != 0 {
		t.Fatalf("a reservation attempted something: %d writes, %d posts", ops.writes, posts.Load())
	}

	done := make(chan error, 1)
	go func() { done <- s.Send(ctx, msg, asClient) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a send on a reserved delivery waited for a lock its own reservation holds")
	}
	if got := ops.locks.Load(); got != 1 {
		t.Errorf("the lock was taken %d times, want once (by the reservation)", got)
	}
	if !lockHeldBy(t, other, key) {
		t.Error("the send released the reservation's lock; only the caller gives it back")
	}
	release()
	if lockHeldBy(t, other, key) {
		t.Error("the lock is still held after the reservation was given back")
	}
	if posts.Load() != 1 {
		t.Errorf("%d submissions, want 1", posts.Load())
	}
}

// TestAReservationRefusedForABusyLockAttemptedNothing: the lock is held elsewhere.
// The refusal is the gate's busy answer, as a throttle (so everything that already
// knows how to wait for a provider waits), and nothing was written or submitted.
func TestAReservationRefusedForABusyLockAttemptedNothing(t *testing.T) {
	s, ops, other := reserveFixture(t, 4)
	msg := Message{IntentID: "campaign-single:busy", To: "x@example.test", Subject: "Hello", TextBody: "Body"}
	holdLock(t, other, operationLockKey(s, msg.IntentID))
	var posts atomic.Int32
	s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		posts.Add(1)
		return accepted(), nil
	})
	wait := ops.waitFor
	ops.waitFor = 300 * time.Millisecond
	defer func() { ops.waitFor = wait }()

	_, release, err := s.Reserve(context.Background(), msg, asClient)
	if err == nil {
		release()
		t.Fatal("a reservation was granted for a lock held elsewhere")
	}
	if !memql.IsWriteGateBusy(err) || !errors.Is(err, memql.ErrWriteGateBusy) {
		t.Errorf("the refusal is %v, want the gate's busy answer", err)
	}
	if _, throttled := IsThrottled(err); !throttled || IsPermanent(err) {
		t.Errorf("the refusal is not a throttle: %v", err)
	}
	if ops.writes != 0 || posts.Load() != 0 {
		t.Errorf("a refused reservation attempted something: %d writes, %d posts", ops.writes, posts.Load())
	}
}

// TestASendForABusyLockIsAThrottleAndSubmitsNothing: the same refusal without a
// reservation (the bulk drain, transactional mail). The caller's existing
// provider-throttle handling retries it later, and it costs no attempt.
func TestASendForABusyLockIsAThrottleAndSubmitsNothing(t *testing.T) {
	s, ops, other := reserveFixture(t, 4)
	msg := Message{IntentID: "campaign:busy:recipient:two", To: "x@example.test", Subject: "Hello", TextBody: "Body"}
	holdLock(t, other, operationLockKey(s, msg.IntentID))
	var posts atomic.Int32
	s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		posts.Add(1)
		return accepted(), nil
	})
	ops.waitFor = 300 * time.Millisecond

	err := s.Send(context.Background(), msg, asClient)

	if err == nil {
		t.Fatal("a delivery went out while its receipt's lock was held elsewhere")
	}
	if wait, throttled := IsThrottled(err); !throttled || wait <= 0 || IsPermanent(err) {
		t.Errorf("the refusal is %v (throttled %v, wait %s, permanent %v), want a throttle with a wait", err, throttled, wait, IsPermanent(err))
	}
	if !memql.IsWriteGateBusy(err) {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if ops.writes != 0 || posts.Load() != 0 {
		t.Errorf("a refused delivery attempted something: %d writes, %d posts", ops.writes, posts.Load())
	}
}

// TestACallerWhoStopsWaitingDuringThePostDoesNotLoseTheLock: the lock is held on a
// context the caller's cancellation does not end. A request that gives up while the
// POST is out still has its outcome recorded under the lock, not after it was
// rolled back with the caller's transaction.
func TestACallerWhoStopsWaitingDuringThePostDoesNotLoseTheLock(t *testing.T) {
	s, ops, other := reserveFixture(t, 4)
	msg := Message{IntentID: "campaign:cancelled:recipient:three", To: "x@example.test", Subject: "Hello", TextBody: "Body"}
	key := operationLockKey(s, msg.IntentID)
	ctx, cancel := context.WithCancel(context.Background())
	var heldAfterCancel atomic.Bool
	s.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		cancel()
		time.Sleep(400 * time.Millisecond) // the server notices a cancelled transaction within moments
		heldAfterCancel.Store(lockHeldBy(t, other, key))
		return nil, errors.New("response lost after the caller stopped waiting")
	})

	_ = s.Send(ctx, msg, asClient)

	if !heldAfterCancel.Load() {
		t.Error("the lock went with the caller's cancelled context while the message was out; the outcome is written without it")
	}
	if op := ops.rows[s.operationKey(msg.IntentID)]; op.Status != "unknown" {
		t.Errorf("the receipt says %q for a response that was lost, want unknown", op.Status)
	}
}

// TestThePollerSkipsAnOperationThatIsBeingSent: a delivery holds its receipt's lock
// from before its first receipt to after its outcome, so a poller that finds the
// lock busy has found a send in flight right now. That is not an error that ends
// the pass: the receipt's next poll time has not moved, and a later pass takes it.
func TestThePollerSkipsAnOperationThatIsBeingSent(t *testing.T) {
	_, ops, other := reserveFixture(t, 4)
	key := "polled-while-sent"
	holdLock(t, other, "email-operation:"+key)
	ops.waitFor = 300 * time.Millisecond

	if err := (&azureSetup{}).pollOperation(context.Background(), ops, key); err != nil {
		t.Fatalf("the poller answered %v for an operation being sent; it should leave it for a later pass", err)
	}
	if ops.writes != 0 {
		t.Errorf("the poller wrote %d times to a receipt it did not hold", ops.writes)
	}
}

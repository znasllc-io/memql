package vendorkeys

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/memql"
)

// observe_db_test.go -- what a real call learned about a key, written where
// every replica reads it (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K1, K11 and K13, safety rule S7, mesh rule M3).

func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

func TestObserveMarksRefusedCreditAndGood(t *testing.T) {
	w := newWorld(t, "verdicts")
	alice := w.person("alice")
	key := w.key(alice, "k")
	account := keyAccount(key, alice)
	ctx := context.Background()

	born := w.state(key)
	require.Equal(t, "good", born["state"], "a key is saved only after the vendor accepted it")
	require.Equal(t, stamp(w.clock), born["goodAt"])
	require.Equal(t, stamp(w.clock), born["checkedAt"])

	w.clock = w.clock.Add(10 * time.Minute)
	w.resolver.Observe(ctx, account, memql.VendorKeyRefused, w.clock, "invalid x-api-key")
	refused := w.state(key)
	require.Equal(t, "refused", refused["state"])
	require.Equal(t, "invalid x-api-key", refused["detail"])
	require.Equal(t, stamp(w.clock), refused["evidenceAt"])
	require.Equal(t, stamp(w.clock), refused["lastUsedAt"], "a call that was refused still used the key")
	require.Equal(t, born["goodAt"], refused["goodAt"], "a refusal does not move the time the key was last good")

	w.clock = w.clock.Add(10 * time.Minute)
	w.resolver.Observe(ctx, account, memql.VendorKeyGood, w.clock, "")
	good := w.state(key)
	require.Equal(t, "good", good["state"])
	require.Equal(t, stamp(w.clock), good["goodAt"])
	require.Equal(t, stamp(w.clock), good["lastUsedAt"])

	w.clock = w.clock.Add(10 * time.Minute)
	w.resolver.Observe(ctx, account, memql.VendorKeyCredit, w.clock, "credit balance is too low")
	credit := w.state(key)
	require.Equal(t, "credit", credit["state"])
	require.Equal(t, "credit balance is too low", credit["detail"])
	require.Equal(t, stamp(w.clock.Add(creditHold)), credit["heldUntil"])

	// What the engine observed never touches what the owner decided.
	row := w.latest(keyConcept, key)
	require.Equal(t, "active", row["status"])
	require.Equal(t, true, row["isDefault"])

	// A verdict about the cluster's account, or about no key, writes nothing.
	w.resolver.Observe(ctx, memql.VendorAccount{Kind: memql.VendorAccountCluster, Vendor: memql.VendorAnthropic}, memql.VendorKeyRefused, w.clock, "x")
	w.resolver.Observe(ctx, keyAccount("vk-key-that-is-not-there", alice), memql.VendorKeyRefused, w.clock, "x")
	require.Equal(t, "credit", w.state(key)["state"])
}

// Review Focus 5: a long call that started before a key was revoked can
// finish, successfully, after the refusal was recorded. Its late good verdict
// carries the earlier time and does not undo the refusal.
func TestObserveIgnoresOlderEvidence(t *testing.T) {
	w := newWorld(t, "older")
	alice := w.person("alice")
	key := w.key(alice, "k")
	account := keyAccount(key, alice)
	ctx := context.Background()

	t1 := w.clock.Add(time.Minute)
	t2 := w.clock.Add(2 * time.Minute)
	w.clock = w.clock.Add(3 * time.Minute)
	w.resolver.Observe(ctx, account, memql.VendorKeyRefused, t2, "revoked")
	w.resolver.Observe(ctx, account, memql.VendorKeyGood, t1, "")

	state := w.state(key)
	require.Equal(t, "refused", state["state"], "a call that started before the refusal undid it")
	require.Equal(t, stamp(t2), state["evidenceAt"])
	require.Equal(t, "revoked", state["detail"])

	// Evidence of the same instant does not flip a verdict either way, and
	// newer evidence does.
	w.resolver.Observe(ctx, account, memql.VendorKeyGood, t2, "")
	require.Equal(t, "refused", w.state(key)["state"])
	w.resolver.Observe(ctx, account, memql.VendorKeyGood, t2.Add(time.Second), "")
	require.Equal(t, "good", w.state(key)["state"])
}

// pausingEngine lets a test hold one statement of one replica at a chosen
// instant. Everything still runs on the real engine.
type pausingEngine struct {
	Engine
	before func(query string)
}

func (p pausingEngine) Execute(ctx context.Context, query string) (any, error) {
	if p.before != nil {
		p.before(query)
	}
	return p.Engine.Execute(ctx, query)
}

// M3: verdicts for one key arrive from several replicas, and each is a read of
// the row followed by a write of the whole row. Replica A holds an OLDER good
// verdict and has read the row; before it writes, replica B is handed a NEWER
// refusal. Without the gate B would read, write and finish while A sat between
// its read and its write, and A's stale good would then land on top of the
// refusal and put the key back in service. With it, B waits for A.
func TestAStaleVerdictCannotOverwriteANewerOne(t *testing.T) {
	w := newWorld(t, "stale-verdict")
	alice := w.person("alice")
	key := w.key(alice, "k")
	account := keyAccount(key, alice)
	// Both verdicts are more than an hour after the key was last stamped good,
	// so the older one is a write and not a stamp the hourly rule skips.
	t1, t2 := w.clock.Add(2*time.Hour), w.clock.Add(3*time.Hour)
	w.clock = w.clock.Add(4 * time.Hour)

	aboutToWrite := make(chan struct{})
	bDone := make(chan struct{})
	var once sync.Once
	slow := NewStore(pausingEngine{Engine: liveEngine{w.eng}, before: func(query string) {
		if !strings.HasPrefix(query, "mutation updateVendorKeyState") {
			return
		}
		once.Do(func() {
			close(aboutToWrite)
			// Give B every chance to finish first. Under the gate it cannot,
			// and this simply runs out.
			select {
			case <-bDone:
			case <-time.After(750 * time.Millisecond):
			}
		})
	}}, func() *sql.DB { return w.gate }, discardLogger())
	slow.now = w.store.now
	replicaA := NewResolver(slow, nil)

	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		replicaA.Observe(context.Background(), account, memql.VendorKeyGood, t1, "")
	}()
	select {
	case <-aboutToWrite:
	case <-aDone:
		t.Fatal("the older verdict was never written, so nothing here is a race")
	case <-time.After(20 * time.Second):
		t.Fatal("the older verdict never reached its write")
	}
	go func() {
		defer close(bDone)
		w.resolver.Observe(context.Background(), account, memql.VendorKeyRefused, t2, "revoked")
	}()
	<-aDone
	<-bDone

	state := w.state(key)
	require.Equal(t, "refused", state["state"], "a stale verdict was written over a newer one")
	require.Equal(t, stamp(t2), state["evidenceAt"])
}

// K11: an account that is out of credit is held for one hour from the
// evidence, and the hold is a time on the row, so every replica honours it.
func TestCreditHoldLastsOneHour(t *testing.T) {
	w := newWorld(t, "hold")
	alice := w.person("alice")
	key := w.key(alice, "k")
	w.clock = w.clock.Add(time.Minute)
	at := w.clock
	w.resolver.Observe(context.Background(), keyAccount(key, alice), memql.VendorKeyCredit, at, "spend limit reached")

	require.Equal(t, time.Hour, creditHold)
	for offset, held := range map[time.Duration]bool{0: true, 59 * time.Minute: true, time.Hour: false, 90 * time.Minute: false} {
		w.clock = at.Add(offset)
		got := w.keysFor(w.resolver, alice, true)
		require.Equal(t, held, len(got.usable) == 0, "at %v after the evidence", offset)
		if held {
			require.Contains(t, got.passed, key)
		}
	}
}

// K11: every served call is a check, and it is written at most once an hour
// per key, on whichever replica served it.
func TestGoodIsStampedAtMostOnceAnHour(t *testing.T) {
	w := newWorld(t, "hourly")
	alice := w.person("alice")
	key := w.key(alice, "k")
	account := keyAccount(key, alice)
	ctx := context.Background()
	born := w.state(key)["goodAt"]

	w.clock = w.clock.Add(20 * time.Minute)
	w.resolver.Observe(ctx, account, memql.VendorKeyGood, w.clock, "")
	require.Equal(t, born, w.state(key)["goodAt"], "a served call within the hour wrote the row again")

	w.clock = w.clock.Add(41 * time.Minute)
	w.resolver.Observe(ctx, account, memql.VendorKeyGood, w.clock, "")
	require.Equal(t, stamp(w.clock), w.state(key)["goodAt"], "a served call an hour later did not stamp the key")

	// A recovery is written at once, however recent the last good stamp.
	w.clock = w.clock.Add(time.Minute)
	w.resolver.Observe(ctx, account, memql.VendorKeyRefused, w.clock, "refused")
	w.clock = w.clock.Add(time.Minute)
	w.resolver.Observe(ctx, account, memql.VendorKeyGood, w.clock, "")
	require.Equal(t, "good", w.state(key)["state"])
	require.Equal(t, stamp(w.clock), w.state(key)["goodAt"])
}

// S7 and K13: a key going from good to bad, and coming back, each write one
// audit event. A repeat of the same verdict writes none.
func TestGoingBadAndRecoveringAreAudited(t *testing.T) {
	w := newWorld(t, "audit")
	alice := w.person("alice")
	key := w.key(alice, "k")
	account := keyAccount(key, alice)
	ctx := context.Background()
	require.Empty(t, w.auditActions(key), "adding a key through the store alone audits nothing; the builtin does")

	step := func(verdict memql.VendorKeyVerdict, detail string) {
		w.clock = w.clock.Add(2 * time.Hour)
		w.resolver.Observe(ctx, account, verdict, w.clock, detail)
	}
	step(memql.VendorKeyRefused, "invalid x-api-key")
	step(memql.VendorKeyRefused, "invalid x-api-key")
	require.Equal(t, []string{auditWentBad}, w.auditActions(key))
	step(memql.VendorKeyCredit, "credit balance is too low")
	require.Equal(t, []string{auditWentBad}, w.auditActions(key), "bad to bad is not a second fall")
	step(memql.VendorKeyGood, "")
	step(memql.VendorKeyGood, "")
	require.Equal(t, []string{auditWentBad, auditRecovered}, w.auditActions(key))
	step(memql.VendorKeyCredit, "credit balance is too low")
	require.Equal(t, []string{auditWentBad, auditRecovered, auditWentBad}, w.auditActions(key))

	// The event says which key by id and never by name, and carries the
	// vendor's words only as far as they are on the state row already.
	event := w.lastAudit(key)
	label, _ := w.latest(keyConcept, key)["label"].(string)
	fingerprint, _ := w.latest(keyConcept, key)["fingerprint"].(string)
	require.NotContains(t, event, label)
	require.NotContains(t, event, fingerprint)
	require.Contains(t, event, `"vendorKey"`)
	require.Contains(t, event, `"credit"`)
}

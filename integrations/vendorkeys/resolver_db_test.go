package vendorkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/worker"
)

// resolver_db_test.go -- which of a person's API keys can pay for a call
// (design record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K6 to K9, K16, safety rule S9, mesh rule M1). One test per line of
// the rule.

// ---------------------------------------------------------------------------
// the shared engine
// ---------------------------------------------------------------------------

var sharedBoot struct {
	once    sync.Once
	dsn     string
	eng     *memql.MemQLEngine
	db      *bun.DB
	gate    *sql.DB
	pingErr error
	bootErr error
}

// liveEngine is the narrow Engine this package asks for, over the real one.
type liveEngine struct{ e *memql.MemQLEngine }

func (l liveEngine) Execute(ctx context.Context, q string) (any, error) { return l.e.Execute(ctx, q) }

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newEngine boots one engine over the test database. Each call is a separate
// engine with its own caches, which is what a second replica is.
func newEngine(dsn string) (*memql.MemQLEngine, *bun.DB, error) {
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, nil, errUnreachable{err}
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		return nil, nil, fmt.Errorf("LoadUnifiedConcepts: %w", err)
	}
	eng, err := memql.New(db)
	if err != nil {
		return nil, nil, fmt.Errorf("memql.New: %w", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		return nil, nil, fmt.Errorf("engine Init: %w", err)
	}
	return eng, db, nil
}

type errUnreachable struct{ error }

func bootEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB, *sql.DB) {
	t.Helper()
	s := &sharedBoot
	s.once.Do(func() {
		s.dsn = dbtest.DSN()
		eng, db, err := newEngine(s.dsn)
		if unreachable, ok := err.(errUnreachable); ok {
			s.pingErr = unreachable.error
			return
		}
		if err != nil {
			s.bootErr = err
			return
		}
		gate, err := database.NewGatePool(s.dsn, 4)
		if err != nil {
			s.bootErr = fmt.Errorf("gate pool: %w", err)
			return
		}
		s.eng, s.db, s.gate = eng, db, gate
	})
	if s.pingErr != nil {
		dbtest.Unreachable(t, "vendor key integration", s.dsn, s.pingErr)
		return nil, nil, nil
	}
	require.NoError(t, s.bootErr, "the shared engine failed to boot")
	return s.eng, s.db, s.gate
}

// ---------------------------------------------------------------------------
// the rule
// ---------------------------------------------------------------------------

// K8 line 4: the person's own default key first, then keys lent to them.
func TestOwnDefaultKeyComesFirst(t *testing.T) {
	w := newWorld(t, "own-first")
	alice, bob := w.person("alice"), w.person("bob")
	// Bob's key is the older one. Order is by whose key it is before it is by age.
	lent := w.key(bob, "lent", added(48*time.Hour))
	w.lend(bob, lent, people(alice))
	own := w.key(alice, "own")

	got := w.keysFor(w.resolver, alice, true)
	require.Equal(t, []string{own, lent}, got.usable)
	require.Empty(t, got.passed)
}

// K7: a person's other keys at a vendor are used only by a route that pins
// them. They are not offered and not reported.
func TestOwnOtherKeysAreNotUsed(t *testing.T) {
	w := newWorld(t, "own-other")
	alice := w.person("alice")
	def := w.key(alice, "default")
	other := w.key(alice, "other", notTheDefault())
	elsewhere := w.key(alice, "openai", atVendor(memql.VendorOpenAI))

	got := w.keysFor(w.resolver, alice, true)
	require.Equal(t, []string{def}, got.usable)
	require.Empty(t, got.passed, "a key that is simply not the default is not a key that was passed over")
	require.Equal(t, []string{elsewhere}, w.keysFor(w.resolver, alice, true, memql.VendorOpenAI).usable,
		"a key is offered at its own vendor and no other")

	// The pin's path: the same key is usable when a call names it.
	q := memql.VendorAccountQuery{Vendor: memql.VendorAnthropic, ActingUserId: alice, Chosen: true}
	account, _, ok := w.resolver.Usable(actorContext(alice), q, other)
	require.True(t, ok, "a person's own key that is not the default cannot be pinned")
	require.Equal(t, memql.VendorAccount{Kind: memql.VendorAccountKey, Vendor: memql.VendorAnthropic, KeyId: other, OwnerUserId: alice}, account)
}

// K8 line 4: among keys lent to a person, the key added earliest is first, and
// two added at one instant are ordered by id so every replica agrees.
func TestLentKeysFollowEarliestAddedFirst(t *testing.T) {
	w := newWorld(t, "lent-order")
	alice, bob, carol := w.person("alice"), w.person("bob"), w.person("carol")
	newer := w.key(bob, "newer", added(time.Hour))
	oldest := w.key(carol, "oldest", added(72*time.Hour))
	tieB := w.key(bob, "tie-b", added(24*time.Hour), notTheDefault())
	tieA := w.key(carol, "tie-a", added(24*time.Hour), notTheDefault())
	for key, owner := range map[string]string{newer: bob, oldest: carol, tieB: bob, tieA: carol} {
		w.lend(owner, key, people(alice))
	}

	got := w.keysFor(w.resolver, alice, true)
	require.Equal(t, []string{oldest, tieA, tieB, newer}, got.usable,
		"a lent key that is not its owner's default is still lent: the default is the owner's own order, not the borrower's")
}

// K6 and K9: lending is a machine's lending. A person is admitted when named,
// or through an active group, and not while their grant or the whole share is
// paused.
func TestALentKeyNeedsAdmission(t *testing.T) {
	w := newWorld(t, "admission")
	alice, bob, dan := w.person("alice"), w.person("bob"), w.person("dan")
	const group = "vk-group"
	w.groups[alice] = []string{group}

	paused := "an API key's owner has paused lending it"
	for name, tc := range map[string]struct {
		sharing worker.Sharing
		usable  bool
		reason  string
	}{
		"lent to nobody":         {worker.Sharing{Mode: worker.SharingModeOwner}, false, ""},
		"lent to somebody else":  {people(dan), false, ""},
		"named":                  {people(alice), true, ""},
		"through a group":        {worker.Sharing{Mode: worker.SharingModePeople, GroupIds: []string{group}}, true, ""},
		"named and paused":       {worker.Sharing{Mode: worker.SharingModePeople, UserIds: []string{alice}, PausedUserIds: []string{alice}}, false, paused},
		"group paused":           {worker.Sharing{Mode: worker.SharingModePeople, GroupIds: []string{group}, PausedGroupIds: []string{group}}, false, paused},
		"whole share paused":     {worker.Sharing{Mode: worker.SharingModePeople, UserIds: []string{alice}, Paused: true}, false, paused},
		"paused for another":     {worker.Sharing{Mode: worker.SharingModePeople, UserIds: []string{alice, dan}, PausedUserIds: []string{dan}}, true, ""},
		"paused, group still on": {worker.Sharing{Mode: worker.SharingModePeople, UserIds: []string{alice}, PausedUserIds: []string{alice}, GroupIds: []string{group}}, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			key := w.key(bob, "k-"+strings.ReplaceAll(name, " ", "-"))
			w.lend(bob, key, tc.sharing)
			defer func() { require.NoError(t, w.store.Remove(context.Background(), key, bob)) }()

			got := w.keysFor(w.resolver, alice, true)
			if tc.usable {
				require.Equal(t, []string{key}, got.usable)
				require.Empty(t, got.passed)
			} else {
				require.Empty(t, got.usable)
			}
			if tc.reason == "" {
				require.NotContains(t, got.passed, key, "a key the person has no tie to is not theirs to hear about")
			} else {
				require.Equal(t, tc.reason, got.passed[key])
			}

			q := memql.VendorAccountQuery{Vendor: memql.VendorAnthropic, ActingUserId: alice, Chosen: true}
			_, _, ok := w.resolver.Usable(actorContext(alice), q, key)
			require.Equal(t, tc.usable, ok, "the call-time judgment disagrees with the list")
		})
	}
}

// K8 line 5: a call nobody chose a route for counts only keys whose owner
// switched Auto on, the person's own and lent ones alike.
func TestAutoUsesOnlyKeysSwitchedOn(t *testing.T) {
	w := newWorld(t, "auto")
	alice, bob, carol := w.person("alice"), w.person("bob"), w.person("carol")
	own := w.key(alice, "own")
	lentOff := w.key(bob, "lent-off", added(2*time.Hour))
	lentOn := w.key(carol, "lent-on", added(time.Hour), autoOn())
	w.lend(bob, lentOff, people(alice))
	w.lend(carol, lentOn, people(alice))

	require.Equal(t, []string{own, lentOff, lentOn}, w.keysFor(w.resolver, alice, true).usable, "a chosen route")
	auto := w.keysFor(w.resolver, alice, false)
	require.Equal(t, []string{lentOn}, auto.usable, "Auto")
	require.Empty(t, auto.passed, "a key that is off for Auto was not passed over; it was never offered")

	q := memql.VendorAccountQuery{Vendor: memql.VendorAnthropic, ActingUserId: alice}
	_, _, ok := w.resolver.Usable(actorContext(alice), q, own)
	require.False(t, ok, "a key off for Auto paid for a call nobody chose a route for")
	q.Chosen = true
	_, _, ok = w.resolver.Usable(actorContext(alice), q, own)
	require.True(t, ok)

	on := true
	require.NoError(t, w.store.UpdateSettings(context.Background(), own, Settings{AutoUse: &on}))
	require.Equal(t, []string{own, lentOn}, w.keysFor(w.resolver, alice, false).usable, "Auto, once the owner switched it on")
}

// K9 and K11: a refused key and a key held for credit are passed over with a
// reason, and a hold that has run out holds nothing.
func TestRefusedAndHeldKeysArePassedOverWithAReason(t *testing.T) {
	w := newWorld(t, "state")
	alice, bob := w.person("alice"), w.person("bob")
	refused := w.key(alice, "refused")
	held := w.key(bob, "held")
	w.lend(bob, held, people(alice))
	label := func(key string) string { return w.latest(keyConcept, key)["label"].(string) }

	ctx := context.Background()
	w.clock = w.clock.Add(time.Minute)
	w.resolver.Observe(ctx, keyAccount(refused, alice), memql.VendorKeyRefused, w.clock, "invalid x-api-key")
	w.resolver.Observe(ctx, keyAccount(held, bob), memql.VendorKeyCredit, w.clock, "credit balance is too low")

	got := w.keysFor(w.resolver, alice, true)
	require.Empty(t, got.usable)
	require.Equal(t, "an API key was refused by Anthropic", got.passed[refused])
	require.Equal(t, "an API key's account is out of credit at Anthropic", got.passed[held])
	for key, reason := range got.passed {
		require.NotContains(t, reason, label(key), "a reason names a key")
		require.NotContains(t, reason, "...", "a reason carries a fingerprint")
	}
	for _, key := range []string{refused, held} {
		_, _, ok := w.resolver.Usable(actorContext(alice), memql.VendorAccountQuery{Vendor: memql.VendorAnthropic, ActingUserId: alice, Chosen: true}, key)
		require.False(t, ok, "a key the list passes over was usable at call time")
	}

	// The hold lasts an hour from its evidence. After it the next real call
	// may try the key again; a refusal waits for a check.
	w.clock = w.clock.Add(creditHold + time.Second)
	got = w.keysFor(w.resolver, alice, true)
	require.Equal(t, []string{held}, got.usable)
	require.Contains(t, got.passed, refused)
}

// K16: a key stops serving when its owner stops being an active user.
func TestAnInactiveOwnersKeyIsPassedOver(t *testing.T) {
	w := newWorld(t, "inactive")
	alice, bob := w.person("alice"), w.person("bob")
	lent := w.key(bob, "lent")
	w.lend(bob, lent, people(alice))
	require.Equal(t, []string{lent}, w.keysFor(w.resolver, alice, true).usable)

	w.writeUser(bob, false)
	got := w.keysFor(w.resolver, alice, true)
	require.Empty(t, got.usable)
	require.Equal(t, "an API key's owner is no longer an active user", got.passed[lent])
	_, _, ok := w.resolver.Usable(actorContext(alice), memql.VendorAccountQuery{Vendor: memql.VendorAnthropic, ActingUserId: alice, Chosen: true}, lent)
	require.False(t, ok)
}

// S9: an unknown id, a removed key, another vendor's key and a key this person
// may not use all answer the same way, and none of them says which it was.
func TestUsableRefusesWhatKeysForWouldNotOffer(t *testing.T) {
	w := newWorld(t, "usable")
	alice, bob := w.person("alice"), w.person("bob")
	notLent := w.key(bob, "not-lent")
	removed := w.key(alice, "removed")
	require.NoError(t, w.store.Remove(context.Background(), removed, alice))
	openai := w.key(alice, "openai", atVendor(memql.VendorOpenAI))

	q := memql.VendorAccountQuery{Vendor: memql.VendorAnthropic, ActingUserId: alice, Chosen: true}
	reasons := map[string]bool{}
	for name, key := range map[string]string{
		"a made-up id": "vk-key-that-is-not-there", "a key that is not lent": notLent,
		"a removed key": removed, "another vendor's key": openai, "no id": "",
	} {
		account, reason, ok := w.resolver.Usable(actorContext(alice), q, key)
		require.False(t, ok, name)
		require.Equal(t, memql.VendorAccount{}, account, name)
		reasons[reason] = true
	}
	require.Len(t, reasons, 1, "the refusals tell the cases apart: %v", reasons)

	// Nobody acting, the cluster acting, and a visitor's call never use a key.
	mine := w.key(alice, "mine")
	for name, tc := range map[string]struct {
		ctx context.Context
		q   memql.VendorAccountQuery
	}{
		"no acting person": {context.Background(), memql.VendorAccountQuery{Vendor: memql.VendorAnthropic, Chosen: true}},
		"a visitor's call": {actorContext(alice), memql.VendorAccountQuery{Vendor: memql.VendorAnthropic, ActingUserId: alice, Chosen: true, ForVisitor: true}},
		"a system actor":   {actorContext(alice), memql.VendorAccountQuery{Vendor: memql.VendorAnthropic, ActingUserId: auth.SystemActor("sweep").UserId, Chosen: true}},
	} {
		_, _, ok := w.resolver.Usable(tc.ctx, tc.q, mine)
		require.False(t, ok, name)
		usable, passed, err := w.resolver.KeysFor(tc.ctx, tc.q)
		require.NoError(t, err, name)
		require.Empty(t, usable, name)
		require.Empty(t, passed, name)
	}
}

// M1 and Review Focus 1: whether a key may pay is decided from the rows on
// every call. A pause written through another replica is seen by the next call
// here, with nothing in between to tell this replica about it.
func TestKeysForReadsRowsFreshOnEveryCall(t *testing.T) {
	w := newWorld(t, "fresh")
	alice, bob := w.person("alice"), w.person("bob")
	lent := w.key(bob, "lent")
	w.lend(bob, lent, people(alice))
	require.Equal(t, []string{lent}, w.keysFor(w.resolver, alice, true).usable)

	other, otherDB, err := newEngine(sharedBoot.dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = otherDB.Close() })
	require.NotSame(t, w.eng, other)
	elsewhere, _ := w.over(other)
	require.NoError(t, elsewhere.SetSharing(context.Background(), bob, lent,
		worker.Sharing{Mode: worker.SharingModePeople, UserIds: []string{alice}, PausedUserIds: []string{alice}}))

	got := w.keysFor(w.resolver, alice, true)
	require.Empty(t, got.usable, "a key paused through another replica still paid here")
	require.Equal(t, "an API key's owner has paused lending it", got.passed[lent])

	// And a removal, which is the one that must never be late.
	require.NoError(t, elsewhere.SetSharing(context.Background(), bob, lent, people(alice)))
	require.Equal(t, []string{lent}, w.keysFor(w.resolver, alice, true).usable)
	require.NoError(t, elsewhere.Remove(context.Background(), lent, bob))
	got = w.keysFor(w.resolver, alice, true)
	require.Empty(t, got.usable, "a key removed through another replica still paid here")
	_, err = w.resolver.Secret(context.Background(), keyAccount(lent, bob))
	require.Error(t, err, "a key removed through another replica was unsealed here")
}

// K2 and K16: the value is unsealed for the one request it pays for, and a
// removed key is never unsealed again.
func TestSecretUnsealsAndRefusesARemovedKey(t *testing.T) {
	w := newWorld(t, "secret")
	alice := w.person("alice")
	const plain = "the-plain-value-this-test-sealed"
	key := w.key(alice, "sealed", holding(plain))

	got, err := w.resolver.Secret(context.Background(), keyAccount(key, alice))
	require.NoError(t, err)
	require.Equal(t, plain, got)
	stored, _ := w.latest(keyConcept, key)["encryptedValue"].(string)
	require.NotEmpty(t, stored)
	require.NotContains(t, stored, plain, "the row holds the plain value")

	require.NoError(t, w.store.Remove(context.Background(), key, alice))
	_, err = w.resolver.Secret(context.Background(), keyAccount(key, alice))
	var account *memql.VendorAccountError
	require.True(t, errors.As(err, &account), "a removed key's refusal is not an account failure: %v", err)
	kind, about := memql.VendorAccountFailure(err)
	require.True(t, about)
	require.Equal(t, "unavailable", kind, "the router must move to the next account, not to the next model")
	require.NotContains(t, err.Error(), plain)

	_, err = w.resolver.Secret(context.Background(), keyAccount("vk-key-that-is-not-there", alice))
	require.True(t, errors.As(err, &account))
	_, err = w.resolver.Secret(context.Background(), memql.VendorAccount{Kind: memql.VendorAccountCluster, Vendor: memql.VendorAnthropic})
	require.Error(t, err, "the cluster's account has no key to unseal")
}

func keyAccount(key, owner string) memql.VendorAccount {
	return memql.VendorAccount{Kind: memql.VendorAccountKey, Vendor: memql.VendorAnthropic, KeyId: key, OwnerUserId: owner}
}

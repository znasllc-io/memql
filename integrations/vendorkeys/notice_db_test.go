package vendorkeys

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// notice_db_test.go -- one notice to a key's owner when it stops working
// (design record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decision K13, mesh rule M4).
//
// A lent key that dies fails over to the next account, and nobody would
// otherwise notice. So a pass the cluster runs every ten minutes tells the
// owner of each key that stopped working, once, until the key has worked
// again in between.

// outbox records what the notifier was handed, and answers as scripted.
type outbox struct {
	mu      sync.Mutex
	notices []KeyNotice
	fail    error
}

func (o *outbox) notify(_ context.Context, notice KeyNotice) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.notices = append(o.notices, notice)
	return o.fail
}

func (o *outbox) all() []KeyNotice {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]KeyNotice(nil), o.notices...)
}

func (o *outbox) failing(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fail = err
}

// asTheNoticePass is the context the engine's own scheduled automation runs
// the pass under.
func asTheNoticePass() context.Context {
	return auth.ContextWithAccess(context.Background(), auth.AutomationActor(auth.AutomationOriginEngine, noticeAutomation))
}

// newNoticeDesk is a desk on which no key owes a notice yet.
//
// The pass is the cluster's: it reads every active key in the database, and
// the database is shared with every other package's tests. A key one of them
// left not working is counted here as owing its owner a notice, and the counts
// these tests read are the whole pass's. So before a test installs its own
// notifier, passes with one that accepts everything settle what was already
// owed (a key with nobody to tell is settled too).
func newNoticeDesk(t *testing.T, name string) *desk {
	t.Helper()
	d := newDesk(t, name)
	d.store.SetNotifier(func(context.Context, KeyNotice) error { return nil })
	defer d.store.SetNotifier(nil)
	for pass := 0; pass < 20; pass++ {
		if count(t, d.tell(), "owed") == 0 {
			return d
		}
	}
	t.Fatal("keys left by other tests still owe a notice after twenty passes")
	return nil
}

// tell runs one pass the way the automation does, and returns what it said.
func (d *desk) tell() map[string]any {
	d.t.Helper()
	nodes, err := d.in.handleTellOwners(asTheNoticePass(), nil, 0)
	require.NoError(d.t, err)
	return cardOf(d.t, nodes)
}

// falls records that the vendor stopped accepting a key, a minute on.
func (d *desk) falls(keyId string, verdict memql.VendorKeyVerdict, detail string) {
	d.t.Helper()
	d.clock = d.clock.Add(time.Minute)
	require.NoError(d.t, d.store.observe(context.Background(), keyId, verdict, d.clock, detail, false))
}

func TestOneNoticeWhenAGoodKeyGoesBad(t *testing.T) {
	d := newNoticeDesk(t, "notice")
	dana := d.member("dana", auth.RoleDeveloper)
	out := &outbox{}
	d.store.SetNotifier(out.notify)
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Personal")["id"].(string)

	// A key that works owes nobody anything.
	require.Zero(t, count(t, d.tell(), "owed"))
	require.Empty(t, out.all())

	d.falls(keyId, memql.VendorKeyRefused, "invalid x-api-key")
	fellAt := d.clock
	told := d.tell()
	require.Equal(t, 1, count(t, told, "sent"), "the pass said %v", told)

	notices := out.all()
	require.Len(t, notices, 1)
	notice := notices[0]
	require.Equal(t, keyId, notice.KeyId)
	require.Equal(t, dana, notice.OwnerUserId)
	require.Equal(t, "vk-dana-"+d.suffix+"@example.test", notice.OwnerEmail)
	require.Equal(t, "Dana", notice.OwnerName)
	require.Equal(t, "Personal", notice.Label)
	require.Equal(t, memql.VendorAnthropic, notice.Vendor)
	require.Equal(t, StateRefused, notice.State)
	require.Equal(t, "invalid x-api-key", notice.Detail)
	require.True(t, notice.At.Equal(fellAt), "the notice is dated %s, the key fell at %s", notice.At, fellAt)
	require.NotEmpty(t, d.state(keyId)["notifiedAt"], "the notice is not recorded on the key's state")

	// The same answer again, from the next call, is not a second fall. Nor is
	// a different way of not working.
	d.falls(keyId, memql.VendorKeyRefused, "invalid x-api-key")
	d.falls(keyId, memql.VendorKeyCredit, "credit balance is too low")
	again := d.tell()
	require.Zero(t, count(t, again, "sent"))
	require.Zero(t, count(t, again, "owed"))
	require.Len(t, out.all(), 1, "a key that was already not working was reported again")
}

func TestNoSecondNoticeUntilItRecovers(t *testing.T) {
	d := newNoticeDesk(t, "noticeagain")
	dana := d.member("dana", auth.RoleDeveloper)
	out := &outbox{}
	d.store.SetNotifier(out.notify)
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Personal")["id"].(string)
	step := func(verdict memql.VendorKeyVerdict, detail string) {
		d.clock = d.clock.Add(2 * time.Hour)
		require.NoError(t, d.store.observe(context.Background(), keyId, verdict, d.clock, detail, false))
	}

	step(memql.VendorKeyCredit, "credit balance is too low")
	d.tell()
	require.Len(t, out.all(), 1)
	require.Equal(t, StateCredit, out.all()[0].State)

	// It works again, and then stops working again: that is a new fall.
	step(memql.VendorKeyGood, "")
	d.tell()
	require.Len(t, out.all(), 1, "a recovery was reported as a fall")
	step(memql.VendorKeyRefused, "invalid x-api-key")
	d.tell()
	notices := out.all()
	require.Len(t, notices, 2)
	require.Equal(t, StateRefused, notices[1].State)
	require.Equal(t, "invalid x-api-key", notices[1].Detail)
}

// A key that was refused and works again before anybody was told is working:
// a notice that it stopped would be false by the time it was read.
func TestAKeyThatRecoversBeforeItsNoticeGetsNone(t *testing.T) {
	d := newNoticeDesk(t, "noticerecovered")
	dana := d.member("dana", auth.RoleDeveloper)
	out := &outbox{}
	d.store.SetNotifier(out.notify)
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Personal")["id"].(string)

	d.falls(keyId, memql.VendorKeyRefused, "invalid x-api-key")
	d.falls(keyId, memql.VendorKeyGood, "")
	require.Zero(t, count(t, d.tell(), "owed"))
	require.Empty(t, out.all())
}

// M4: the pass is placed on one replica, and a placement is not a guarantee.
// A second replica that runs it while the first is still sending reads that
// the first is trying, and leaves the key alone.
func TestTwoReplicasSendOneNotice(t *testing.T) {
	d := newNoticeDesk(t, "noticereplicas")
	dana := d.member("dana", auth.RoleDeveloper)
	out := &outbox{}
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Personal")["id"].(string)

	other, otherDB, err := newEngine(sharedBoot.dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = otherDB.Close() })
	elsewhere, _ := d.over(other)
	elsewhere.SetNotifier(out.notify)

	// The first replica's mail server is slow: it is still sending when the
	// second replica's pass comes by.
	sending, send := make(chan struct{}), make(chan struct{})
	d.store.SetNotifier(func(ctx context.Context, notice KeyNotice) error {
		close(sending)
		<-send
		return out.notify(ctx, notice)
	})
	d.falls(keyId, memql.VendorKeyRefused, "invalid x-api-key")

	first := make(chan noticeCounts, 1)
	go func() {
		counts, _ := d.store.tellOwners(context.Background(), 0)
		first <- counts
	}()
	<-sending
	meanwhile, err := elsewhere.tellOwners(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, noticeCounts{owed: 1, waiting: 1}, meanwhile)
	require.Empty(t, out.all(), "a second replica told the owner while the first was still telling them")

	close(send)
	require.Equal(t, noticeCounts{owed: 1, sent: 1}, <-first)
	require.Len(t, out.all(), 1)

	// And neither tells them again afterwards.
	after, err := elsewhere.tellOwners(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, noticeCounts{}, after)
	require.Len(t, out.all(), 1, "two replicas each told the owner about the same fall")
}

// A notice is recorded against the fall it was about, not against the clock.
// A key can work again while the mail about it is being sent (a call that
// began before the pass reports late), and the next time it stops working its
// owner is owed a notice like any other.
func TestAKeyThatRecoversWhileItsOwnerIsToldStillOwesTheNextFall(t *testing.T) {
	d := newNoticeDesk(t, "noticerace")
	dana := d.member("dana", auth.RoleDeveloper)
	out := &outbox{}
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Personal")["id"].(string)

	d.falls(keyId, memql.VendorKeyRefused, "invalid x-api-key")
	fellAt := d.clock
	var once sync.Once
	d.store.SetNotifier(func(ctx context.Context, notice KeyNotice) error {
		once.Do(func() {
			// A call that started a minute after the fall is answered now.
			require.NoError(t, d.store.observe(context.Background(), keyId, memql.VendorKeyGood, fellAt.Add(time.Minute), "", false))
		})
		return out.notify(ctx, notice)
	})
	d.clock = fellAt.Add(5 * time.Minute)
	require.Equal(t, 1, count(t, d.tell(), "sent"))
	require.Equal(t, StateGood, d.state(keyId)["state"])

	d.clock = d.clock.Add(time.Hour)
	require.NoError(t, d.store.observe(context.Background(), keyId, memql.VendorKeyRefused, d.clock, "invalid x-api-key", false))
	again := d.tell()
	require.Equal(t, 1, count(t, again, "sent"), "the pass said %v", again)
	require.Len(t, out.all(), 2, "a key that fell again after working was not reported")
}

// Nothing a key could be rebuilt from is handed to the notifier, and a key
// with nobody to tell is settled instead of being asked about for ever.
func TestTheNoticeCarriesNoKeyMaterialAndNeedsAnOwner(t *testing.T) {
	d := newNoticeDesk(t, "noticesafe")
	dana := d.member("dana", auth.RoleDeveloper)
	gone := d.member("gone", auth.RoleDeveloper)
	out := &outbox{}
	d.store.SetNotifier(out.notify)
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Personal")["id"].(string)
	theirs := d.mustAdd(session(gone, auth.RoleDeveloper), "Theirs")["id"].(string)
	removed := d.mustAdd(session(dana, auth.RoleDeveloper), "Old")["id"].(string)

	d.falls(theirs, memql.VendorKeyRefused, "invalid x-api-key")
	d.falls(removed, memql.VendorKeyRefused, "invalid x-api-key")
	d.falls(keyId, memql.VendorKeyRefused, "Incorrect API key provided: sk-proj-abcdEFGH1234****wxyz.")
	// One owner is no longer a user of this cluster, and one key was removed
	// by its owner, who therefore knows.
	d.writeUser(gone, false)
	require.NoError(t, d.store.Remove(context.Background(), removed, dana))

	told := d.tell()
	require.Equal(t, 1, count(t, told, "sent"), "the pass said %v", told)
	require.Equal(t, 1, count(t, told, "nobody"), "the pass said %v", told)
	notices := out.all()
	require.Len(t, notices, 1, "a key with nobody to tell was reported to somebody")
	notice := notices[0]
	require.Equal(t, keyId, notice.KeyId)
	for _, field := range []string{notice.Label, notice.Detail, notice.Vendor, notice.State, notice.OwnerEmail, notice.OwnerName} {
		require.NotContains(t, field, deskAnthropicKey)
		require.NotContains(t, field, "...0001", "the fingerprint is the end of the key")
	}
	// A vendor echoes part of a refused key in its answer. Whatever reached the
	// store, nothing shaped like a key is kept on the row or handed on.
	require.NotContains(t, notice.Detail, "sk-proj-")
	require.Contains(t, notice.Detail, "Incorrect API key provided")
	require.NotContains(t, d.state(keyId)["detail"], "sk-proj-")

	// The key with no owner to tell is not owed a notice on every pass after.
	require.Zero(t, count(t, d.tell(), "owed"))
	require.Len(t, out.all(), 1)
}

// A notice that could not be sent does not undo the verdict, and is tried
// again later: not on the next pass, and not never.
func TestANoticeThatFailsIsTriedAgainLater(t *testing.T) {
	d := newNoticeDesk(t, "noticefails")
	dana := d.member("dana", auth.RoleDeveloper)
	out := &outbox{}
	out.failing(errors.New("smtp is down"))
	d.store.SetNotifier(out.notify)
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Personal")["id"].(string)

	d.falls(keyId, memql.VendorKeyRefused, "invalid x-api-key")
	first := d.tell()
	require.Equal(t, 1, count(t, first, "failed"), "the pass said %v", first)
	require.Zero(t, count(t, first, "sent"))
	require.Len(t, out.all(), 1)
	require.Equal(t, StateRefused, d.state(keyId)["state"])
	require.Empty(t, d.keysFor(d.resolver, dana, true).usable, "a notice that failed put the key back in service")
	require.Empty(t, d.state(keyId)["notifiedAt"], "a notice that was not sent is recorded as sent")

	// Ten minutes on, the next pass leaves it alone.
	d.clock = d.clock.Add(10 * time.Minute)
	soon := d.tell()
	require.Equal(t, 1, count(t, soon, "waiting"), "the pass said %v", soon)
	require.Len(t, out.all(), 1, "a failed notice was tried again on the very next pass")

	// Past the wait it is tried again, and this time the mail goes.
	out.failing(nil)
	d.clock = d.clock.Add(noticeRetryEvery)
	later := d.tell()
	require.Equal(t, 1, count(t, later, "sent"), "the pass said %v", later)
	require.Len(t, out.all(), 2)
	require.NotEmpty(t, d.state(keyId)["notifiedAt"])

	d.clock = d.clock.Add(2 * noticeRetryEvery)
	require.Zero(t, count(t, d.tell(), "owed"))
	require.Len(t, out.all(), 2, "the owner was told twice about one fall")
}

// With no notifier installed nothing is sent and nothing is recorded as sent,
// so a node that can send still does.
func TestNoNotifierIsNotAFault(t *testing.T) {
	d := newNoticeDesk(t, "noticenone")
	dana := d.member("dana", auth.RoleDeveloper)
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Personal")["id"].(string)
	d.falls(keyId, memql.VendorKeyRefused, "invalid x-api-key")

	nothing := d.tell()
	require.Zero(t, count(t, nothing, "sent"))
	require.Equal(t, StateRefused, d.state(keyId)["state"])
	require.Empty(t, d.state(keyId)["notifiedAt"])
	require.Empty(t, d.state(keyId)["noticeTriedAt"])
	require.Equal(t, []string{auditAdded, auditWentBad}, d.auditActions(keyId))

	out := &outbox{}
	d.in.SetNotifier(out.notify)
	require.Equal(t, 1, count(t, d.tell(), "sent"))
	require.Len(t, out.all(), 1)
}

// Check now on a key that is out of credit lists the vendor's models, which is
// answered whether or not the account has credit. It must not read as the key
// working again: the next call would fail, and its owner would be mailed a
// second time about the same empty account. A cluster owner can press Check on
// anybody's key, so it would also be somebody else doing that to them.
func TestCheckingAKeyOutOfCreditDoesNotMailItsOwnerAgain(t *testing.T) {
	d := newNoticeDesk(t, "noticecheck")
	dana := d.member("dana", auth.RoleDeveloper)
	omar := d.member("omar", auth.RoleOwner)
	out := &outbox{}
	d.store.SetNotifier(out.notify)
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Personal")["id"].(string)

	d.falls(keyId, memql.VendorKeyCredit, "credit balance is too low")
	require.Equal(t, 1, count(t, d.tell(), "sent"))

	d.clock = d.clock.Add(5 * time.Minute)
	d.answers(memql.VendorKeyProbeGood, "")
	_, err := d.in.handleCheck(session(omar, auth.RoleOwner), map[string]any{"keyId": keyId}, 0)
	require.NoError(t, err)

	// The next call it pays for fails as before.
	d.falls(keyId, memql.VendorKeyCredit, "credit balance is too low")
	d.clock = d.clock.Add(2 * noticeRetryEvery)
	again := d.tell()
	require.Zero(t, count(t, again, "owed"), "the pass said %v", again)
	require.Len(t, out.all(), 1, "the owner was mailed twice about one empty account")
}

// The pass is the cluster's. Nobody runs it by hand, an owner included, and
// no other automation runs it.
func TestOnlyTheClustersOwnPassTellsOwners(t *testing.T) {
	d := newNoticeDesk(t, "noticegate")
	owner := d.member("olive", auth.RoleOwner)
	dana := d.member("dana", auth.RoleDeveloper)
	out := &outbox{}
	d.store.SetNotifier(out.notify)
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Personal")["id"].(string)
	d.falls(keyId, memql.VendorKeyRefused, "invalid x-api-key")

	refused := map[string]context.Context{
		"a cluster owner":                   session(owner, auth.RoleOwner),
		"the key's owner":                   session(dana, auth.RoleDeveloper),
		"nobody":                            context.Background(),
		"the cluster's own system actor":    auth.ContextWithSystemActor(context.Background(), "notices"),
		"the daily check":                   asTheDailyCheck(),
		"a bundle's automation by the name": auth.ContextWithAccess(context.Background(), auth.AutomationActor(auth.AutomationOriginOther, noticeAutomation)),
	}
	for name, ctx := range refused {
		t.Run(name, func(t *testing.T) {
			_, err := d.in.handleTellOwners(ctx, nil, 0)
			require.Error(t, err, "the pass ran for %s", name)
		})
	}
	require.Empty(t, out.all(), "a refused run still mailed a key's owner")

	require.Equal(t, 1, count(t, d.tell(), "sent"))
}

// A pass tells at most its limit, the longest-standing falls first, and the
// rest wait for the next one.
func TestAPassTellsTheOldestFallsFirstUpToItsLimit(t *testing.T) {
	d := newNoticeDesk(t, "noticelimit")
	dana := d.member("dana", auth.RoleDeveloper)
	out := &outbox{}
	d.store.SetNotifier(out.notify)
	first := d.mustAdd(session(dana, auth.RoleDeveloper), "First")["id"].(string)
	second := d.mustAdd(session(dana, auth.RoleDeveloper), "Second")["id"].(string)
	d.falls(second, memql.VendorKeyRefused, "invalid x-api-key")
	d.falls(first, memql.VendorKeyRefused, "invalid x-api-key")

	counts, err := d.store.tellOwners(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, noticeCounts{owed: 2, sent: 1, waiting: 1}, counts)
	require.Equal(t, second, out.all()[0].KeyId, "the key that fell first was not told first")

	counts, err = d.store.tellOwners(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, noticeCounts{owed: 1, sent: 1}, counts)
	require.Equal(t, first, out.all()[1].KeyId)
}

// A node with no gate pool cannot coordinate with the others, so it tells
// nobody: two of them would each tell the owner.
func TestAPassWithoutAGatePoolTellsNobody(t *testing.T) {
	d := newNoticeDesk(t, "noticenogate")
	dana := d.member("dana", auth.RoleDeveloper)
	keyId := d.mustAdd(session(dana, auth.RoleDeveloper), "Personal")["id"].(string)
	d.falls(keyId, memql.VendorKeyRefused, "invalid x-api-key")

	out := &outbox{}
	ungated := NewStore(liveEngine{d.eng}, func() *sql.DB { return nil }, discardLogger())
	ungated.now = func() time.Time { return d.clock }
	ungated.SetNotifier(out.notify)
	counts, err := ungated.tellOwners(context.Background(), 0)
	require.NoError(t, err)
	require.Equal(t, noticeCounts{owed: 1, waiting: 1}, counts)
	require.Empty(t, out.all())
	require.Empty(t, d.state(keyId)["noticeTriedAt"])
}

package vendorkeys

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// sweep_db_test.go -- the daily check of keys nothing has used (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions K11 and K12, owner decisions O2 and O3).

// asTheDailyCheck is the context the engine's own scheduled automation runs
// the sweep under.
func asTheDailyCheck() context.Context {
	return auth.ContextWithAccess(context.Background(), auth.AutomationActor(auth.AutomationOriginEngine, sweepAutomation))
}

// sweepKey adds a key whose stored value says which key it is, so a test can
// read from what the vendor was sent which keys were checked.
func (d *desk) sweepKey(owner, label string, opts ...keyOption) (id, sent string) {
	d.t.Helper()
	vendor := memql.VendorAnthropic
	probe := keyOptions{}
	for _, opt := range opts {
		opt(&probe)
	}
	if probe.vendor != "" {
		vendor = probe.vendor
	}
	plain := "sk-ant-sweep-" + label + "-" + d.suffix
	if vendor == memql.VendorOpenAI {
		plain = "sk-sweep-" + label + "-" + d.suffix
	}
	id = d.key(owner, label, append(opts, holding(plain))...)
	return id, vendor + ":" + plain
}

// runSweep runs the daily check and returns its counts.
func (d *desk) runSweep() map[string]any {
	d.t.Helper()
	nodes, err := d.in.handleSweep(asTheDailyCheck(), nil, 0)
	require.NoError(d.t, err)
	return cardOf(d.t, nodes)
}

// sentTo reports which of these keys the vendor was asked about, in order.
func (d *desk) sentTo(keys ...string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	want := map[string]bool{}
	for _, key := range keys {
		want[key] = true
	}
	var out []string
	for _, asked := range d.asked {
		if want[asked] {
			out = append(out, asked)
		}
	}
	return out
}

func count(t *testing.T, reply map[string]any, field string) int {
	t.Helper()
	n, ok := reply[field].(float64)
	require.True(t, ok, "the reply has no %s: %v", field, reply)
	return int(n)
}

// K11: once a day, a key with no served call and no check in a day has its
// models listed. A key something used or checked since then is left alone.
func TestSweepChecksOnlyKeysIdleForADay(t *testing.T) {
	d := newDesk(t, "sweepidle")
	ana := d.person("ana")
	ctx := context.Background()

	idle, idleSent := d.sweepKey(ana, "idle", added(48*time.Hour))
	fresh, freshSent := d.sweepKey(ana, "fresh", added(2*time.Hour), notTheDefault())
	used, usedSent := d.sweepKey(ana, "used", added(48*time.Hour), notTheDefault())
	require.NoError(t, d.store.observe(ctx, used, memql.VendorKeyGood, d.clock.Add(-time.Hour), "", false))
	checked, checkedSent := d.sweepKey(ana, "checked", added(48*time.Hour), notTheDefault())
	require.NoError(t, d.store.observe(ctx, checked, memql.VendorKeyRefused, d.clock.Add(-time.Hour), "invalid x-api-key", true))

	reply := d.runSweep()

	require.Equal(t, []string{idleSent}, d.sentTo(idleSent, freshSent, usedSent, checkedSent),
		"only the key nothing used or checked for a day is listed")
	require.GreaterOrEqual(t, count(t, reply, "checked"), 1)
	require.GreaterOrEqual(t, count(t, reply, "good"), 1)

	// The check is recorded: the key is good as of now, and checked now.
	state := d.state(idle)
	require.Equal(t, StateGood, state["state"])
	require.Equal(t, stamp(d.clock), state["checkedAt"])
	require.Equal(t, stamp(d.clock), state["goodAt"])
	// And the others are exactly as they were.
	require.Equal(t, stamp(d.clock.Add(-2*time.Hour)), d.state(fresh)["checkedAt"])
	require.Equal(t, StateRefused, d.state(checked)["state"])

	// A second pass the same day asks nothing again.
	d.asked = nil
	d.runSweep()
	require.Empty(t, d.sentTo(idleSent, freshSent, usedSent, checkedSent), "a key checked a moment ago was listed again")
}

// The pass runs at the same minute every day, so a key it checked yesterday
// was checked a few seconds less than a day ago. It is due again today.
func TestSweepChecksAgainTheNextDay(t *testing.T) {
	d := newDesk(t, "sweepnext")
	ana := d.person("ana")
	key, sent := d.sweepKey(ana, "daily", added(48*time.Hour))

	d.runSweep()
	require.Equal(t, []string{sent}, d.sentTo(sent))

	// Tomorrow's run starts a little EARLIER in the minute than today's check
	// finished.
	d.clock = d.clock.Add(24*time.Hour - 20*time.Second)
	d.asked = nil
	d.runSweep()
	require.Equal(t, []string{sent}, d.sentTo(sent), "a key checked by yesterday's pass was skipped by today's")
	require.Equal(t, stamp(d.clock), d.state(key)["checkedAt"])
}

// K11: refused keys are included, and a key the vendor accepts again recovers.
func TestSweepRechecksARefusedKeyAndRecoversIt(t *testing.T) {
	d := newDesk(t, "sweeprefused")
	ana := d.person("ana")
	ctx := context.Background()
	key, sent := d.sweepKey(ana, "revoked", added(72*time.Hour))
	// A call it paid for was refused 30 hours ago.
	require.NoError(t, d.store.observe(ctx, key, memql.VendorKeyRefused, d.clock.Add(-30*time.Hour), "invalid x-api-key", false))
	require.Empty(t, d.keysFor(d.resolver, ana, true).usable)

	// Still refused: the pass says so and changes nothing else.
	d.answers(memql.VendorKeyProbeRefused, "invalid x-api-key")
	reply := d.runSweep()
	require.Equal(t, []string{sent}, d.sentTo(sent))
	require.GreaterOrEqual(t, count(t, reply, "refused"), 1)
	require.Equal(t, StateRefused, d.state(key)["state"])
	require.Equal(t, []string{auditWentBad}, d.auditActions(key), "a key refused again is not a second fall")

	// The next day the vendor accepts it.
	d.clock = d.clock.Add(24 * time.Hour)
	d.asked = nil
	d.answers(memql.VendorKeyProbeGood, "")
	reply = d.runSweep()
	require.Equal(t, []string{sent}, d.sentTo(sent))
	require.GreaterOrEqual(t, count(t, reply, "good"), 1)
	require.Equal(t, StateGood, d.state(key)["state"])
	require.Equal(t, []string{key}, d.keysFor(d.resolver, ana, true).usable, "a key that recovered does not pay")
	require.Equal(t, []string{auditWentBad, auditRecovered}, d.auditActions(key))
}

// A key that was good and that the vendor no longer accepts falls here, before
// any call fails on it.
func TestSweepFindsAKeyThatWasRevokedAtTheVendor(t *testing.T) {
	d := newDesk(t, "sweeprevoked")
	ana := d.person("ana")
	key, _ := d.sweepKey(ana, "gone", added(48*time.Hour))

	d.answers(memql.VendorKeyProbeRefused, "invalid x-api-key")
	reply := d.runSweep()
	require.GreaterOrEqual(t, count(t, reply, "refused"), 1)
	state := d.state(key)
	require.Equal(t, StateRefused, state["state"])
	require.Equal(t, "invalid x-api-key", state["detail"])
	require.Empty(t, d.keysFor(d.resolver, ana, true).usable)
	require.Equal(t, []string{auditWentBad}, d.auditActions(key))
}

// K12: a key that may not list models is checked by the calls it pays for and
// by nothing else.
func TestSweepSkipsUseOnlyKeys(t *testing.T) {
	d := newDesk(t, "sweepuse")
	ana := d.person("ana")
	restricted, sent := d.sweepKey(ana, "restricted", added(72*time.Hour), atVendor(memql.VendorOpenAI), checkedByUse())

	// It is not even in line: with room for one key in the run, the key that
	// can be listed gets it.
	_, listable := d.sweepKey(ana, "listable", added(48*time.Hour), atVendor(memql.VendorOpenAI), notTheDefault())
	d.in.sweepLimit = 1

	d.runSweep()
	require.Empty(t, d.sentTo(sent), "a key that may not list models was listed")
	require.Equal(t, StateUnchecked, d.state(restricted)["state"])
	require.Equal(t, []string{listable}, d.sentTo(listable), "a key that is never listed took the run's one place")
}

// A listing cannot say whether an account has credit again; only a real call
// can, and the hold already lets one try. So a key held for credit is left to
// the next call, and is not put back in service by a listing.
func TestSweepLeavesAKeyOutOfCreditToARealCall(t *testing.T) {
	d := newDesk(t, "sweepcredit")
	ana := d.person("ana")
	ctx := context.Background()
	key, sent := d.sweepKey(ana, "broke", added(72*time.Hour))
	require.NoError(t, d.store.observe(ctx, key, memql.VendorKeyCredit, d.clock.Add(-30*time.Hour), "credit balance is too low", false))

	d.runSweep()
	require.Empty(t, d.sentTo(sent), "a key whose account is out of credit was listed")
	require.Equal(t, StateCredit, d.state(key)["state"])
	require.Equal(t, []string{auditWentBad}, d.auditActions(key), "a listing recorded a recovery it cannot know about")
}

// O3: the pass spends nothing. Its only contact with a vendor is the check,
// which lists models (component/memql's TestProbeListsModelsAndSendsNoPrompt
// holds that the check is one GET of the model list). The integration under
// test has no provider registry at all, so there is nothing here that could
// make a model call.
func TestSweepOnlyListsModels(t *testing.T) {
	d := newDesk(t, "sweeplist")
	ana := d.person("ana")
	_, first := d.sweepKey(ana, "one", added(48*time.Hour))
	_, second := d.sweepKey(ana, "two", added(49*time.Hour), atVendor(memql.VendorOpenAI))

	d.runSweep()
	require.ElementsMatch(t, []string{first, second}, d.sentTo(first, second), "each idle key is checked exactly once")
}

// The pass is the cluster's, once a day. Nobody runs it by hand, an owner
// included, and no other automation runs it.
func TestSweepIsRefusedToAPerson(t *testing.T) {
	d := newDesk(t, "sweepgate")
	owner := d.member("olive", auth.RoleOwner)
	_, sent := d.sweepKey(d.person("ana"), "idle", added(48*time.Hour))

	refused := map[string]context.Context{
		"a cluster owner":                   session(owner, auth.RoleOwner),
		"a developer":                       session(d.member("dana", auth.RoleDeveloper), auth.RoleDeveloper),
		"nobody":                            context.Background(),
		"the cluster's own system actor":    auth.ContextWithSystemActor(context.Background(), "sweep"),
		"another automation of the engine":  auth.ContextWithAccess(context.Background(), auth.AutomationActor(auth.AutomationOriginEngine, "routingEvidenceFold")),
		"a bundle's automation by the name": auth.ContextWithAccess(context.Background(), auth.AutomationActor(auth.AutomationOriginOther, sweepAutomation)),
	}
	for name, ctx := range refused {
		t.Run(name, func(t *testing.T) {
			_, err := d.in.handleSweep(ctx, nil, 0)
			require.Error(t, err, "the daily check ran for %s", name)
			require.Contains(t, err.Error(), "Check now")
		})
	}
	require.Empty(t, d.sentTo(sent), "a refused run still sent a key to the vendor")

	_, err := d.in.handleSweep(asTheDailyCheck(), nil, 0)
	require.NoError(t, err)
	require.Equal(t, []string{sent}, d.sentTo(sent))
}

// One key that cannot be checked does not stop the others.
func TestOneKeysFailureDoesNotStopTheSweep(t *testing.T) {
	d := newDesk(t, "sweepfail")
	ana := d.person("ana")
	broken, brokenSent := d.sweepKey(ana, "broken", added(72*time.Hour))
	_, fineSent := d.sweepKey(ana, "fine", added(48*time.Hour), notTheDefault())

	// The broken key's sealed value is not something this cluster can open.
	d.corruptSealedValue(broken)

	reply := d.runSweep()
	require.Empty(t, d.sentTo(brokenSent))
	require.Equal(t, []string{fineSent}, d.sentTo(fineSent), "a key after one that could not be opened was not checked")
	require.GreaterOrEqual(t, count(t, reply, "skipped"), 1)
	require.Contains(t, d.logged.String(), broken, "the key that could not be checked is not named in the log")
	require.NotContains(t, d.logged.String(), "sk-ant-sweep-", "a key's value reached the log")
}

// A vendor that is not answering is not asked two hundred times. After three
// answers in a row that say nothing about a key, the rest of that vendor's
// keys wait for tomorrow. The other vendor's keys are still checked.
func TestSweepStopsAskingAVendorThatIsNotAnswering(t *testing.T) {
	d := newDesk(t, "sweepdown")
	ana := d.person("ana")
	var anthropic []string
	for n := 0; n < 5; n++ {
		opts := []keyOption{added(time.Duration(60+n) * time.Hour)}
		if n > 0 {
			opts = append(opts, notTheDefault())
		}
		_, sent := d.sweepKey(ana, fmt.Sprintf("a%d", n), opts...)
		anthropic = append(anthropic, sent)
	}
	_, openai := d.sweepKey(ana, "o", added(48*time.Hour), atVendor(memql.VendorOpenAI))

	d.mu.Lock()
	d.vendor = func(vendor, _ string) memql.VendorKeyProbe {
		if vendor == memql.VendorAnthropic {
			return memql.VendorKeyProbe{Verdict: memql.VendorKeyProbeUnreachable, Detail: "Anthropic could not be reached to check the key.", At: d.clock}
		}
		return memql.VendorKeyProbe{Verdict: memql.VendorKeyProbeGood, Models: 2, At: d.clock}
	}
	d.mu.Unlock()

	reply := d.runSweep()
	require.Len(t, d.sentTo(anthropic...), sweepGiveUpAfter, "a vendor that was not answering kept being asked")
	require.Equal(t, []string{openai}, d.sentTo(openai), "one vendor's outage stopped the other vendor's keys being checked")
	require.GreaterOrEqual(t, count(t, reply, "skipped"), 5)
}

// At most so many keys a run, the ones checked longest ago first. The rest are
// counted and wait for the next run.
func TestSweepChecksTheOldestFirstUpToItsLimit(t *testing.T) {
	d := newDesk(t, "sweeplimit")
	ana := d.person("ana")
	_, newest := d.sweepKey(ana, "newest", added(30*time.Hour))
	_, oldest := d.sweepKey(ana, "oldest", added(90*time.Hour), notTheDefault())
	_, middle := d.sweepKey(ana, "middle", added(60*time.Hour), notTheDefault())

	d.in.sweepLimit = 2
	reply := d.runSweep()
	require.Equal(t, []string{oldest, middle}, d.sentTo(newest, oldest, middle), "the keys checked longest ago go first")
	require.GreaterOrEqual(t, count(t, reply, "skipped"), 1)

	// The next run takes the one that waited.
	d.asked = nil
	d.runSweep()
	require.Equal(t, []string{newest}, d.sentTo(newest, oldest, middle))
}

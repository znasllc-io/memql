package worker

import (
	"reflect"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestAppLabelsOnlyForRunnableApps pins the rule the whole routing
// story rests on (memql#4359): a machine that HAS the binary but
// cannot actually run it must never be selectable. `allowed` is the
// machine's policy.yaml verdict and `signedIn` is the app's auth
// state; either one false means no label, so selection cannot pick a
// machine that would refuse the run after the plan committed to it.
func TestAppLabelsOnlyForRunnableApps(t *testing.T) {
	cases := []struct {
		name string
		app  AppInfo
		want bool
	}{
		{"allowed and signed in", AppInfo{Id: AppIdClaudeCode, Version: "2.1.4", Allowed: true, SignedIn: true}, true},
		{"not allowed by policy", AppInfo{Id: AppIdClaudeCode, Version: "2.1.4", Allowed: false, SignedIn: true}, false},
		{"not signed in", AppInfo{Id: AppIdClaudeCode, Version: "2.1.4", Allowed: true, SignedIn: false}, false},
		{"neither", AppInfo{Id: AppIdCodex, Allowed: false, SignedIn: false}, false},
		// An id the engine does not drive is stored but never routed
		// to: a newer cockpit reporting a third app must not make the
		// engine attempt a protocol it does not have.
		{"unknown id, otherwise ready", AppInfo{Id: "some-future-app", Allowed: true, SignedIn: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := AppLabels([]AppInfo{tc.app})
			_, got := labels[AppLabelKey(tc.app.Id)]
			if got != tc.want {
				t.Fatalf("label present = %v, want %v (labels=%v)", got, tc.want, labels)
			}
			if tc.app.Runnable() != tc.want {
				t.Fatalf("Runnable() = %v, want %v", tc.app.Runnable(), tc.want)
			}
		})
	}
}

func TestAppLabelValueIsMajorMinor(t *testing.T) {
	labels := AppLabels([]AppInfo{
		{Id: AppIdClaudeCode, Version: "v2.1.4-beta", Allowed: true, SignedIn: true},
		{Id: AppIdCodex, Version: "", Allowed: true, SignedIn: true},
	})
	if got := labels[AppLabelKey(AppIdClaudeCode)]; got != "2.1" {
		t.Fatalf("claude-code label = %q, want 2.1", got)
	}
	// No parseable version is "unknown", not "0": a require-label of
	// {"app:codex": ""} means "any version", which is what a machine
	// reporting no version can honestly satisfy.
	if got, ok := labels[AppLabelKey(AppIdCodex)]; !ok || got != "" {
		t.Fatalf("codex label = %q ok=%v, want \"\" true", got, ok)
	}
}

func TestMajorMinor(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.4":      "2.1",
		"v2.1.4":     "2.1",
		"2.1":        "2.1",
		"2":          "2",
		"2.1.4-beta": "2.1",
		"01.02":      "1.2",
		"":           "",
		"nightly":    "",
		"v":          "",
		"1.x":        "1",
	} {
		if got := MajorMinor(in); got != want {
			t.Errorf("MajorMinor(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNormalizeSubscriptionNeverInfers pins D5's honesty rule: an app
// that says nothing about its subscription is "unknown", never
// "none". Reading silence as "none" would let the ledger record a
// subscription-covered run as metered.
func TestNormalizeSubscriptionNeverInfers(t *testing.T) {
	for in, want := range map[string]string{
		"":         SubscriptionUnknown,
		"garbage":  SubscriptionUnknown,
		"unknown":  SubscriptionUnknown,
		"none":     SubscriptionNone,
		"PRESENT":  SubscriptionPresent,
		" present": SubscriptionPresent,
	} {
		if got := NormalizeSubscription(in); got != want {
			t.Errorf("NormalizeSubscription(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAppsFromProtoIsStable pins the ordering guarantee. An unstable
// order would rewrite the registration row on every heartbeat for no
// actual change, which is a write amplification bug that looks like
// churn rather than like a bug.
func TestAppsFromProtoIsStable(t *testing.T) {
	in := []*memqlv1.AppInfo{
		{Id: AppIdCodex, Version: "1.0", Allowed: true, SignedIn: true},
		{Id: AppIdClaudeCode, Version: "2.1", Allowed: true, SignedIn: true},
		nil,
		{Id: "   ", Version: "x"},
		{Id: AppIdCodex, Version: "9.9"}, // duplicate id: first wins
	}
	got := AppsFromProto(in)
	want := []AppInfo{
		{Id: AppIdClaudeCode, Version: "2.1", SignedIn: true, Subscription: SubscriptionUnknown, Allowed: true},
		{Id: AppIdCodex, Version: "1.0", SignedIn: true, Subscription: SubscriptionUnknown, Allowed: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AppsFromProto = %+v, want %+v", got, want)
	}
	if !appsEqual(got, AppsFromProto(in)) {
		t.Fatal("AppsFromProto is not deterministic across calls")
	}
}

// TestMergeAppLabelsPreservesOperatorLabels: signing out of an app
// must remove ONLY its label. An operator's own routing labels
// (has-gpu=true and friends) are not the engine's to delete.
func TestMergeAppLabelsPreservesOperatorLabels(t *testing.T) {
	base := map[string]string{
		"os":                         "darwin",
		"has-gpu":                    "true",
		AppLabelKey(AppIdClaudeCode): "2.0", // stale: from a previous beat
	}
	got := mergeAppLabels(base, []AppInfo{
		{Id: AppIdCodex, Version: "1.4", Allowed: true, SignedIn: true},
	})
	if got["os"] != "darwin" || got["has-gpu"] != "true" {
		t.Fatalf("operator labels lost: %v", got)
	}
	if _, stale := got[AppLabelKey(AppIdClaudeCode)]; stale {
		t.Fatalf("stale app label survived a beat that no longer reports it: %v", got)
	}
	if got[AppLabelKey(AppIdCodex)] != "1.4" {
		t.Fatalf("codex label = %q, want 1.4", got[AppLabelKey(AppIdCodex)])
	}
}

// TestRunsAppIsTheSelectionPredicate. Choosing BETWEEN machines is the
// Fleet router's job (memql#4350) and it matches on the derived `app:`
// label; RunsApp is that same question asked of ONE worker. The two must
// agree -- a machine the label says is selectable and RunsApp says is not
// would be picked and then refuse, and the failure would name the router.
func TestRunsAppIsTheSelectionPredicate(t *testing.T) {
	w := &Worker{RegistrationId: "w1", OwnerUserId: "u", Capabilities: []string{CapabilityHeadless}}
	w.SetApps([]AppInfo{{Id: AppIdClaudeCode, Version: "2.1", Allowed: true, SignedIn: true}})

	if !w.RunsApp(AppIdClaudeCode) {
		t.Fatal("an allowed, signed-in app must be runnable")
	}
	if _, labelled := w.LabelsSnapshot()[AppLabelKey(AppIdClaudeCode)]; !labelled {
		t.Fatal("a runnable app must carry its routing label -- the router matches on it")
	}

	// Signing out flips BOTH, within one beat.
	w.SetApps([]AppInfo{{Id: AppIdClaudeCode, Version: "2.1", Allowed: true, SignedIn: false}})
	if w.RunsApp(AppIdClaudeCode) {
		t.Fatal("a signed-out app must not be runnable")
	}
	if _, labelled := w.LabelsSnapshot()[AppLabelKey(AppIdClaudeCode)]; labelled {
		t.Fatal("the routing label must go with it, or the router picks a machine that refuses")
	}

	// An app the engine does not drive is never runnable, however the
	// machine reports it.
	w.SetApps([]AppInfo{{Id: "some-future-app", Allowed: true, SignedIn: true}})
	if w.RunsApp("some-future-app") {
		t.Fatal("an app outside the engine's closed set must never be runnable")
	}
}

// TestFirstConnectedRunningIsAnExistenceCheck: the probe asks whether
// delegating is possible at all, and must not become a second router.
func TestFirstConnectedRunningIsAnExistenceCheck(t *testing.T) {
	r := NewRegistry(nil, nil)
	if got := firstConnectedRunning(r, "u", AppIdClaudeCode); got != "" {
		t.Fatalf("found %q with nothing connected", got)
	}

	blocked := &Worker{RegistrationId: "blocked", OwnerUserId: "u", Capabilities: []string{CapabilityHeadless}}
	blocked.SetApps([]AppInfo{{Id: AppIdClaudeCode, Allowed: false, SignedIn: true}})
	r.Add(blocked)
	if got := firstConnectedRunning(r, "u", AppIdClaudeCode); got != "" {
		t.Fatalf("found %q on a machine whose policy.yaml refuses the app", got)
	}

	ready := &Worker{RegistrationId: "ready", OwnerUserId: "u", Capabilities: []string{CapabilityHeadless}}
	ready.SetApps([]AppInfo{{Id: AppIdClaudeCode, Allowed: true, SignedIn: true}})
	r.Add(ready)
	if got := firstConnectedRunning(r, "u", AppIdClaudeCode); got != "ready" {
		t.Fatalf("got %q, want ready", got)
	}
	// Never crosses a user boundary.
	if got := firstConnectedRunning(r, "other", AppIdClaudeCode); got != "" {
		t.Fatalf("crossed a user boundary: %q", got)
	}
}

// TestHeartbeatAppsPersistOutsideTheThrottle is the memql#4359
// heartbeat contract. An app inventory CHANGE is a routing change, so
// it must land on the row even when the lastSeenAt flush is inside
// its 60s throttle window -- a row whose app: labels disagree with
// the live registry entry is a split no reader can detect.
func TestHeartbeatAppsPersistOutsideTheThrottle(t *testing.T) {
	t0 := timeAt(2026, time.August, 22, 9, 0, 0)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	// The first beat flushes lastSeenAt and arms the throttle.
	beatAt(session, t0)
	if len(store.lastSeen) != 1 {
		t.Fatalf("first beat must persist, got %d", len(store.lastSeen))
	}

	// A beat 15s later -- deep inside the throttle window -- that
	// reports a NEW inventory persists anyway.
	session.handleHeartbeat(&memqlv1.Heartbeat{
		Ts:          timestamppb.New(t0.Add(15 * time.Second)),
		AppsPresent: true,
		Apps: []*memqlv1.AppInfo{
			{Id: AppIdClaudeCode, Version: "2.1.4", Allowed: true, SignedIn: true},
		},
	}, "10.0.0.1:1")
	if len(store.appUpdates) != 1 {
		t.Fatalf("an inventory change must persist inside the throttle, got %d", len(store.appUpdates))
	}
	if got := store.appUpdates[0].labels[AppLabelKey(AppIdClaudeCode)]; got != "2.1" {
		t.Fatalf("derived label not persisted with the inventory: %v", store.appUpdates[0].labels)
	}
	if !session.worker.RunsApp(AppIdClaudeCode) {
		t.Fatal("the live registry entry did not pick up the new app")
	}

	// Re-reporting the SAME inventory writes nothing: an unstable
	// inventory would otherwise rewrite the row on every 15s beat.
	session.handleHeartbeat(&memqlv1.Heartbeat{
		Ts:          timestamppb.New(t0.Add(30 * time.Second)),
		AppsPresent: true,
		Apps: []*memqlv1.AppInfo{
			{Id: AppIdClaudeCode, Version: "2.1.4", Allowed: true, SignedIn: true},
		},
	}, "10.0.0.1:1")
	if len(store.appUpdates) != 1 {
		t.Fatalf("an unchanged inventory must not rewrite the row, got %d writes", len(store.appUpdates))
	}

	// A beat that does NOT report apps leaves the inventory alone --
	// proto3 cannot distinguish an empty repeated field from an absent
	// one, so apps_present is what carries the difference.
	session.handleHeartbeat(&memqlv1.Heartbeat{
		Ts: timestamppb.New(t0.Add(45 * time.Second)),
	}, "10.0.0.1:1")
	if !session.worker.RunsApp(AppIdClaudeCode) {
		t.Fatal("a beat that reports no apps wiped the inventory")
	}

	// An EXPLICIT empty inventory does clear it.
	session.handleHeartbeat(&memqlv1.Heartbeat{
		Ts:          timestamppb.New(t0.Add(50 * time.Second)),
		AppsPresent: true,
	}, "10.0.0.1:1")
	if session.worker.RunsApp(AppIdClaudeCode) {
		t.Fatal("an explicit empty inventory did not clear the app")
	}
	if len(store.appUpdates) != 2 {
		t.Fatalf("clearing the inventory must persist, got %d writes", len(store.appUpdates))
	}
}

func timeAt(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

// D10: `app:` IS ONE VOCABULARY IN TWO PLACES, AND THIS IS WHAT HOLDS IT
// TOGETHER (epic memql#5096).
//
// A policy naming `app:claude-code` and a machine advertising the routing
// label `app:claude-code` are talking about one thing. The two never MEET in
// code -- a provider reference resolves through the engine's registry, a label
// lives on a registration -- so nothing but this test stops the id sets behind
// them from drifting apart. It lives HERE rather than in component/memql
// because the dependency runs this way: this package can see both the closed
// set and the engine's reference parser, and component/memql cannot see this
// one at all.
func TestAppProviderReferencesMatchTheClosedSet(t *testing.T) {
	refs := AppProviderReferences()
	if len(refs) != len(KnownAppIds()) {
		t.Fatalf("%d provider references for %d app ids", len(refs), len(KnownAppIds()))
	}
	for _, ref := range refs {
		appId, ok := memql.IsAppReference(ref)
		if !ok {
			t.Fatalf("%q is not recognised as an app reference by the engine's parser", ref)
		}
		if !IsKnownAppId(appId) {
			t.Errorf("%q resolves to %q, which is outside the closed runnable set", ref, appId)
		}
	}
	// And the reverse. An app id this engine can drive but no policy can name
	// is an app nobody can select, which is a feature that exists and cannot
	// be reached.
	for _, id := range KnownAppIds() {
		want := memql.AppReferencePrefix + id
		found := false
		for _, ref := range refs {
			if ref == want {
				found = true
			}
		}
		if !found {
			t.Errorf("app id %q has no provider reference (%q)", id, want)
		}
	}
	// The ROUTING LABEL key and the PROVIDER REFERENCE are the same string,
	// which is the whole of "one vocabulary". If these ever diverge, an
	// operator reading `app:codex` on a machine card and writing `app:codex`
	// in a policy would be writing two different things that happen to look
	// alike.
	for _, id := range KnownAppIds() {
		if AppLabelKey(id) != memql.AppReferencePrefix+id {
			t.Errorf("label key %q and provider reference %q disagree for %q",
				AppLabelKey(id), memql.AppReferencePrefix+id, id)
		}
	}
}

// TestKnownAppIdsDeriveFromTheRoutingVocabulary pins the ONE list. The DSL
// parser refuses `app:<id>` for an id outside the set at load and reads it from
// core/airoute; a second copy here would let a policy entry be refused for an
// app this package happily drives, or the reverse.
func TestKnownAppIdsDeriveFromTheRoutingVocabulary(t *testing.T) {
	got := KnownAppIds()
	want := airoute.RunnableApps()
	if len(got) != len(want) {
		t.Fatalf("KnownAppIds() = %v, airoute.RunnableApps() = %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("KnownAppIds()[%d] = %q, airoute.RunnableApps()[%d] = %q", i, got[i], i, want[i])
		}
		if !IsKnownAppId(got[i]) || !airoute.IsRunnableApp(got[i]) {
			t.Fatalf("%q is in the list and fails one of the two predicates", got[i])
		}
	}
}

func TestRunnableHonoursWhatTheMachineMeasured(t *testing.T) {
	base := AppInfo{Id: AppIdClaudeCode, Allowed: true, SignedIn: true}
	for readiness, want := range map[string]bool{
		// A Cockpit that predates the field cannot say. Refusing it would take every
		// current machine out of routing, so it stays, as the one exception.
		"":                true,
		AppReadinessReady: true,
		// A Cockpit that measures and has no answer has not said the app can answer.
		AppReadinessUnknown:    false,
		AppReadinessSignedOut:  false,
		AppReadinessOutOfUsage: false,
	} {
		app := base
		app.Readiness = readiness
		if got := app.Runnable(); got != want {
			t.Errorf("readiness %q: Runnable() = %v, want %v", readiness, got, want)
		}
		if _, labelled := AppLabels([]AppInfo{app})[AppLabelKey(AppIdClaudeCode)]; labelled != want {
			t.Errorf("readiness %q: label present = %v, want %v", readiness, labelled, want)
		}
	}
}

func TestNotRunnableReasonSaysWhatTheMachineSaid(t *testing.T) {
	app := AppInfo{Id: AppIdCodex, Allowed: true, SignedIn: true, Readiness: AppReadinessUnknown, ReadinessDetail: "the app-server could not start"}
	if got := app.NotRunnableReason(); !strings.Contains(got, "could not be checked") || !strings.Contains(got, "the app-server could not start") {
		t.Fatalf("got %q", got)
	}
	app.ReadinessDetail = ""
	if got := app.NotRunnableReason(); !strings.Contains(got, "could not be checked") {
		t.Fatalf("got %q", got)
	}
}

func TestAppsFromProtoCarriesWhatWasMeasured(t *testing.T) {
	checked := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	reset := time.Date(2026, 11, 4, 6, 52, 3, 0, time.UTC)
	got := AppsFromProto([]*memqlv1.AppInfo{{
		Id: AppIdCodex, Allowed: true, SignedIn: true, Readiness: "out_of_usage",
		ReadinessCheckedAt: timestamppb.New(checked), UsageResetsAt: timestamppb.New(reset),
	}, {
		Id: AppIdClaudeCode, Allowed: true, SignedIn: true,
	}, {
		Id: "future-app", Readiness: "a word this engine has never heard",
	}})
	byID := map[string]AppInfo{}
	for _, app := range got {
		byID[app.Id] = app
	}
	codex := byID[AppIdCodex]
	if codex.Readiness != AppReadinessOutOfUsage || codex.ReadinessCheckedAt != "2026-10-30T12:00:00Z" || codex.UsageResetsAt != "2026-11-04T06:52:03Z" {
		t.Fatalf("got %+v", codex)
	}
	if claude := byID[AppIdClaudeCode]; claude.Readiness != "" || claude.ReadinessCheckedAt != "" {
		t.Fatalf("silence must stay silence: %+v", claude)
	}
	if future := byID["future-app"]; future.Readiness != AppReadinessUnknown {
		t.Fatalf("a word this engine does not know must not block anything: %+v", future)
	}
}

// A check moving its timestamp every five minutes must not rewrite the row every
// five minutes. A change of answer lands on the next beat, as it always did.
func TestAppsEqualIgnoresACheckInsideTheRowRefresh(t *testing.T) {
	at := func(minutes int) string {
		return time.Date(2026, 10, 30, 12, minutes, 0, 0, time.UTC).Format(time.RFC3339)
	}
	base := AppInfo{Id: AppIdCodex, Allowed: true, SignedIn: true, Readiness: AppReadinessReady, ReadinessCheckedAt: at(0)}
	changed := func(mutate func(*AppInfo)) []AppInfo {
		app := base
		mutate(&app)
		return []AppInfo{app}
	}
	if !appsEqual([]AppInfo{base}, changed(func(a *AppInfo) { a.ReadinessCheckedAt = at(5) })) {
		t.Fatal("a fresh check of the same answer rewrote the row")
	}
	if appsEqual([]AppInfo{base}, changed(func(a *AppInfo) { a.ReadinessCheckedAt = at(30) })) {
		t.Fatal("the row must still move often enough to say when it was checked")
	}
	if appsEqual([]AppInfo{base}, changed(func(a *AppInfo) { a.Readiness = AppReadinessOutOfUsage })) {
		t.Fatal("a changed answer is a routing change and must land now")
	}
	if appsEqual([]AppInfo{base}, changed(func(a *AppInfo) { a.UsageResetsAt = at(50) })) {
		t.Fatal("a changed reset must land now")
	}
	// A detail can carry a temporary path, and a row version for each is a write
	// nothing reads. It lands with the next answer or the next refresh.
	if !appsEqual([]AppInfo{base}, changed(func(a *AppInfo) { a.ReadinessDetail = "a different sentence" })) {
		t.Fatal("a reworded detail rewrote the row")
	}
}

// One decoder for every row reader: when a row field was added to only one reader
// before, a replica routing from the row refused machines its sibling could serve.
func TestAppInfoRoundTripsThroughTheRow(t *testing.T) {
	want := AppInfo{Id: AppIdCodex, Version: "0.162.1", SignedIn: true, Allowed: true, Subscription: SubscriptionPresent, Readiness: AppReadinessOutOfUsage, ReadinessCheckedAt: "2026-10-30T12:00:00Z", UsageResetsAt: "2026-11-04T06:52:03Z"}
	rows := appsAsMaps([]AppInfo{want})
	got, ok := AppInfoFromMap(rows[0])
	if !ok || got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, present := appsAsMaps([]AppInfo{{Id: AppIdCodex}})[0]["readiness"]; present {
		t.Fatal("an entry with nothing measured must not grow keys on the row")
	}
}

func TestAppNotRunnableSaysWhichHalf(t *testing.T) {
	for _, tt := range []struct {
		app  AppInfo
		want string
	}{
		{AppInfo{Id: AppIdCodex, Allowed: true, SignedIn: true, Readiness: AppReadinessOutOfUsage, UsageResetsAt: "2026-11-04T06:52:03Z"}, "out of usage until 2026-11-04T06:52:03Z"},
		{AppInfo{Id: AppIdCodex, Allowed: true, SignedIn: true, Readiness: AppReadinessOutOfUsage}, "out of usage"},
		{AppInfo{Id: AppIdCodex, Allowed: true, Readiness: AppReadinessSignedOut}, "signed out"},
		{AppInfo{Id: AppIdCodex, Allowed: true}, "allowed and signed in"},
	} {
		if got := tt.app.NotRunnableReason(); !strings.Contains(got, tt.want) {
			t.Errorf("%+v: %q does not say %q", tt.app, got, tt.want)
		}
	}
}

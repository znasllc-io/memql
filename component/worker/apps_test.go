package worker

import (
	"reflect"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
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

// TestPickWorkerForAppSkipsUnrunnableMachines is the routing-side
// half of design §9.3: a machine whose app is not allowed is never
// selected, even when it is the only machine online.
func TestPickWorkerForAppSkipsUnrunnableMachines(t *testing.T) {
	r := NewRegistry(nil, nil)
	blocked := &Worker{
		RegistrationId: "blocked",
		OwnerUserId:    "user-1",
		Capabilities:   []string{CapabilityHeadless},
	}
	blocked.SetApps([]AppInfo{{Id: AppIdClaudeCode, Version: "2.1", Allowed: false, SignedIn: true}})
	r.Add(blocked)

	if _, err := r.PickWorkerForApp("user-1", AppIdClaudeCode, nil); err == nil {
		t.Fatal("selected a machine whose policy.yaml does not allow the app")
	}

	ready := &Worker{
		RegistrationId: "ready",
		OwnerUserId:    "user-1",
		Capabilities:   []string{CapabilityHeadless},
	}
	ready.SetApps([]AppInfo{{Id: AppIdClaudeCode, Version: "2.1", Allowed: true, SignedIn: true}})
	r.Add(ready)

	got, err := r.PickWorkerForApp("user-1", AppIdClaudeCode, nil)
	if err != nil {
		t.Fatalf("PickWorkerForApp: %v", err)
	}
	if got.RegistrationId != "ready" {
		t.Fatalf("picked %s, want ready", got.RegistrationId)
	}

	// An extra require-label still applies on top of the app gate.
	if _, err := r.PickWorkerForApp("user-1", AppIdClaudeCode, map[string]string{"has-gpu": "true"}); err == nil {
		t.Fatal("require-label was ignored")
	}
}

// TestPickWorkerForAppFollowsSignOut: the label set is rebuilt on
// every inventory report, so signing out removes the machine from
// selection within one heartbeat rather than at the next reconnect.
func TestPickWorkerForAppFollowsSignOut(t *testing.T) {
	r := NewRegistry(nil, nil)
	w := &Worker{RegistrationId: "w1", OwnerUserId: "u", Capabilities: []string{CapabilityHeadless}}
	w.SetApps([]AppInfo{{Id: AppIdCodex, Version: "1.0", Allowed: true, SignedIn: true}})
	r.Add(w)

	if _, err := r.PickWorkerForApp("u", AppIdCodex, nil); err != nil {
		t.Fatalf("expected selectable while signed in: %v", err)
	}
	w.SetApps([]AppInfo{{Id: AppIdCodex, Version: "1.0", Allowed: true, SignedIn: false}})
	if _, err := r.PickWorkerForApp("u", AppIdCodex, nil); err == nil {
		t.Fatal("machine stayed selectable after signing out of the app")
	}
	if _, stale := w.LabelsSnapshot()[AppLabelKey(AppIdCodex)]; stale {
		t.Fatal("app label survived the sign-out")
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

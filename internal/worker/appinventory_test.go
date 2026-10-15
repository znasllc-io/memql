package worker

import (
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// TestBuildRegister_CarriesTheAppInventory: the engine cannot discover
// local apps any other way -- it is behind NAT relative to this machine
// and only ever reads what the cockpit reports.
func TestBuildRegister_CarriesTheAppInventory(t *testing.T) {
	register := buildRegister(Config{
		Name:         "test-worker",
		Capabilities: []string{"HEADLESS"},
	}, []apps.Info{
		{Id: apps.IDClaudeCode, Version: "2.1.4", SignedIn: true, Subscription: apps.SubscriptionPresent, Allowed: true},
		{Id: apps.IDCodex, Version: "0.9.1", SignedIn: false, Subscription: apps.SubscriptionUnknown, Allowed: false},
	}, models.Inventory{}, hardware.Inventory{}, tools.PipelinesPolicy{Allow: false})

	got := register.GetApps()
	if len(got) != 2 {
		t.Fatalf("apps = %d, want 2", len(got))
	}
	if got[0].GetId() != apps.IDClaudeCode || !got[0].GetSignedIn() || !got[0].GetAllowed() {
		t.Errorf("claude entry = %+v", got[0])
	}
	if got[0].GetSubscription() != apps.SubscriptionPresent {
		t.Errorf("subscription = %q", got[0].GetSubscription())
	}
	if got[1].GetId() != apps.IDCodex || got[1].GetSignedIn() || got[1].GetAllowed() {
		t.Errorf("codex entry = %+v", got[1])
	}
}

// TestAppsToProto_ClampsWhatItSends: the engine truncates at 200 bytes
// and folds an unrecognised subscription to "unknown". Doing the same
// here means the value an operator reads in the portal is the value the
// cockpit logged, rather than a longer one clipped in transit.
func TestAppsToProto_ClampsWhatItSends(t *testing.T) {
	long := ""
	for range apps.MaxFieldLen + 40 {
		long += "v"
	}
	got := appsToProto([]apps.Info{{Id: apps.IDCodex, Version: long, Subscription: "enterprise-tier"}})
	if len(got) != 1 {
		t.Fatalf("apps = %d", len(got))
	}
	if len(got[0].GetVersion()) != apps.MaxFieldLen {
		t.Errorf("version len = %d, want %d", len(got[0].GetVersion()), apps.MaxFieldLen)
	}
	if got[0].GetSubscription() != apps.SubscriptionUnknown {
		t.Errorf("an unrecognised subscription must clamp to %q, got %q",
			apps.SubscriptionUnknown, got[0].GetSubscription())
	}
}

// TestAppsToProto_EmptyInventoryIsNil: an empty inventory sends no
// entries. On Heartbeat that is a meaningful statement -- "I have none"
// -- and it is apps_present, not the list, that carries the distinction.
func TestAppsToProto_EmptyInventoryIsNil(t *testing.T) {
	if got := appsToProto(nil); got != nil {
		t.Errorf("nil inventory = %+v, want nil", got)
	}
	if got := appsToProto([]apps.Info{}); got != nil {
		t.Errorf("empty inventory = %+v, want nil", got)
	}
}

// TestBuildHeartbeat_AlwaysSetsAppsPresent is the load-bearing one.
// proto3 cannot distinguish an empty repeated field from an absent one,
// so the engine reads apps_present=false as "this build does not report
// apps" and leaves the stored inventory alone. That is correct for an
// older cockpit and WRONG for this one on a machine that just uninstalled
// its last app -- the stale entry would keep producing a routing label
// and the engine would keep selecting a machine that cannot run anything.
//
// So every beat from a build that supports the field asserts the full
// truth, including "none".
func TestBuildHeartbeat_AlwaysSetsAppsPresent(t *testing.T) {
	t.Run("with apps", func(t *testing.T) {
		hb := buildHeartbeat(0, nil, []apps.Info{{Id: apps.IDClaudeCode, SignedIn: true, Allowed: true}}, nil)
		if !hb.GetAppsPresent() {
			t.Error("apps_present must be true")
		}
		if len(hb.GetApps()) != 1 {
			t.Errorf("apps = %d, want 1", len(hb.GetApps()))
		}
	})
	t.Run("uninstalled the last app", func(t *testing.T) {
		hb := buildHeartbeat(0, nil, nil, nil)
		if !hb.GetAppsPresent() {
			t.Fatal("an empty inventory must still set apps_present -- it is how the cockpit says \"I now have none\"")
		}
		if len(hb.GetApps()) != 0 {
			t.Errorf("apps = %d, want 0", len(hb.GetApps()))
		}
	})
}

// TestRunnerInventory_NilReporterIsQuiet: a build wired with no inventory
// reporter must not panic on the heartbeat path.
func TestRunnerInventory_NilReporterIsQuiet(t *testing.T) {
	r := &Runner{}
	if got := r.inventory(t.Context()); got != nil {
		t.Errorf("inventory = %+v, want nil", got)
	}
}

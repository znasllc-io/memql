package automations_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// The daily check of API keys is the engine's own scheduled automation
// (design record 2026-11-21-vendor-accounts-and-api-keys, decision K11). The
// sweep's handler answers only to the mark the engine puts on its own
// automation of this name (integrations/vendorkeys/sweep.go), so three things
// have to hold together or the check silently never runs: the automation is
// loaded from the engine's tree, it is scheduled, and its one step is the
// sweep.
func TestTheDailyKeyCheckIsScheduled(t *testing.T) {
	const name = "vendorKeysDailyCheck"
	t.Setenv(memql.AllowSkipsEnvVar, "")
	loaded, err := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)}).LoadAll()
	if err != nil {
		t.Fatalf("the shipped automation tree did not load: %v", err)
	}
	var check *automations.Automation
	for _, a := range loaded {
		if a.Name == name {
			check = a
		}
	}
	if check == nil {
		t.Fatalf("no automation named %s is loaded: API keys nothing uses are never checked", name)
	}

	if check.Schedule != "0 40 4 * * *" {
		t.Errorf("schedule = %q, want once a day at 04:40", check.Schedule)
	}
	if !check.IsScheduled() || check.IsEventTriggered() || check.Template {
		t.Errorf("the daily check is not a plain scheduled automation: %+v", check.Trigger)
	}
	// One bff replica: the node that checks a key when a person adds one.
	if check.ScheduleNode != "bff" || check.ScheduleLease != "vendor-keys-check" {
		t.Errorf("placement = node %q lease %q, want one bff replica behind the vendor-keys-check lease",
			check.ScheduleNode, check.ScheduleLease)
	}
	if check.Enabled != nil && !*check.Enabled {
		t.Error("the daily check is loaded disabled")
	}

	if len(check.Steps) != 1 {
		t.Fatalf("the daily check has %d steps, want the one sweep", len(check.Steps))
	}
	step, err := json.Marshal(check.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(step), "vendorKeysSweep") {
		t.Errorf("the daily check's step does not call the sweep: %s", step)
	}

	// The sweep admits the actor the executor mints for THIS automation, and
	// the executor mints the engine's mark only for an automation the loader
	// found in the engine's own tree.
	if check.Source != memqldsl.SourceEngine {
		t.Fatalf("the daily check was loaded from %v, not the engine's tree: the sweep would refuse it", check.Source)
	}
	if !auth.IsEngineAutomationActor(auth.AutomationActor(auth.AutomationOriginEngine, check.Name), name) {
		t.Error("the engine's own actor for the daily check is not one the sweep admits")
	}
}

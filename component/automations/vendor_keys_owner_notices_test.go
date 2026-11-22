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

// The owner of an API key that stops working is told by the engine's own
// scheduled automation (design record
// 2026-11-21-vendor-accounts-and-api-keys, decision K13, mesh rule M4). The
// pass's handler answers only to the mark the engine puts on its own
// automation of this name (integrations/vendorkeys/notice.go), so three things
// have to hold together or a dead key stays silent: the automation is loaded
// from the engine's tree, it is scheduled, and its one step is the pass.
func TestTheKeyNoticePassIsScheduled(t *testing.T) {
	const name = "vendorKeysOwnerNotices"
	t.Setenv(memql.AllowSkipsEnvVar, "")
	loaded, err := automations.NewLoader(automations.LoaderOptions{Registry: loadedRegistry(t)}).LoadAll()
	if err != nil {
		t.Fatalf("the shipped automation tree did not load: %v", err)
	}
	var pass *automations.Automation
	for _, a := range loaded {
		if a.Name == name {
			pass = a
		}
	}
	if pass == nil {
		t.Fatalf("no automation named %s is loaded: nobody is told when their API key stops working", name)
	}

	if pass.Schedule != "30 */10 * * * *" {
		t.Errorf("schedule = %q, want every ten minutes at second 30", pass.Schedule)
	}
	if !pass.IsScheduled() || pass.IsEventTriggered() || pass.Template {
		t.Errorf("the notice pass is not a plain scheduled automation: %+v", pass.Trigger)
	}
	// One bff replica: the node that already sends invitations, so it is
	// known to reach the mail sender. Its own lease, so the daily check
	// holding its lease never keeps the notices from going out.
	if pass.ScheduleNode != "bff" || pass.ScheduleLease != "vendor-keys-notices" {
		t.Errorf("placement = node %q lease %q, want one bff replica behind the vendor-keys-notices lease",
			pass.ScheduleNode, pass.ScheduleLease)
	}
	if pass.Enabled != nil && !*pass.Enabled {
		t.Error("the notice pass is loaded disabled")
	}

	if len(pass.Steps) != 1 {
		t.Fatalf("the notice pass has %d steps, want the one pass", len(pass.Steps))
	}
	step, err := json.Marshal(pass.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(step), "vendorKeysTellOwners") {
		t.Errorf("the notice pass's step does not call the builtin that tells owners: %s", step)
	}

	// The handler admits the actor the executor mints for THIS automation,
	// and the executor mints the engine's mark only for an automation the
	// loader found in the engine's own tree.
	if pass.Source != memqldsl.SourceEngine {
		t.Fatalf("the notice pass was loaded from %v, not the engine's tree: its handler would refuse it", pass.Source)
	}
	if !auth.IsEngineAutomationActor(auth.AutomationActor(auth.AutomationOriginEngine, pass.Name), name) {
		t.Error("the engine's own actor for the notice pass is not one its handler admits")
	}
}

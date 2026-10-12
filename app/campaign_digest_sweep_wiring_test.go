package app

// campaign_digest_sweep_wiring_test.go -- guards the wiring of the recipient
// digest sweep (memql#6022, F9).
//
// WHY A SOURCE-LEVEL GUARD, as automation_preflight_wiring_test.go: nothing in the
// repo calls app.Build(), and standing up a real App needs a database. The
// behaviour is tested where it lives -- the walk in component/database, the
// schedule in component/campaigns -- and what only this file can lose is the
// CONNECTION between them. Delete the call and every one of those suites still
// passes while no node ever sweeps: the digest-less rows stay, every signup keeps
// paying for the second read, and nothing says so.

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestTheRecipientDigestSweepIsInstalledOnTheCampaignsWorkerBeforeItStarts(t *testing.T) {
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	body := string(src)

	const install = "campaignWorker.SetRecipientDigestSweep(a.campaignRecipientDigestSweep)"
	installAt := strings.Index(body, install)
	if installAt < 0 {
		t.Fatal("the campaigns worker is no longer given the recipient digest sweep in engineAndBus. Without it no node ever puts the " +
			"address digest on recipients written without one, and every signup keeps comparing addresses for them. Restore " + install + ".")
	}
	// The worker reads its sweep when it starts: one installed afterwards is ignored.
	const dependency = "a.Dependencies = append(a.Dependencies, campaignWorker)"
	dependencyAt := strings.Index(body, dependency)
	if dependencyAt < 0 {
		t.Fatalf("could not find %q in engine.go; this guard needs updating", dependency)
	}
	if installAt > dependencyAt {
		t.Error("the sweep must be installed BEFORE the worker is registered as a dependency: the worker reads it when it starts")
	}

	method, err := os.ReadFile("campaign_digest_sweep.go")
	if err != nil {
		t.Fatalf("read campaign_digest_sweep.go: %v", err)
	}
	if !strings.Contains(string(method), "database.BackfillRecipientEmailDigests(ctx, bunDB, a.Logger)") {
		t.Error("campaignRecipientDigestSweep no longer runs database.BackfillRecipientEmailDigests")
	}
}

// TestTheSweepWithoutADatabaseFailsRatherThanPanics: the worker is wired before
// the database phase has necessarily finished, and a pass that finds no handle
// reports it and waits for the next.
func TestTheSweepWithoutADatabaseFailsRatherThanPanics(t *testing.T) {
	a := &App{}
	stamped, err := a.campaignRecipientDigestSweep(context.Background())
	if err == nil || stamped != 0 {
		t.Errorf("a sweep with no database returned (%d, %v); want an error", stamped, err)
	}
}

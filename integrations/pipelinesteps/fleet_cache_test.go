//go:build agent

package pipelinesteps

import (
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestFleetCacheScopeSeparatesOwnerRepositoryAndTrust(t *testing.T) {
	base := StepRun{OwnerUserID: "owner-a", Repository: pl.Repository{Owner: "org", Name: "repo"}, Caches: []string{"go"}, Env: map[string]string{eventVar: string(pl.EventPush)}}
	key := func(run StepRun) string {
		t.Helper()
		got, _ := fleetArgs(run, "")["cacheScope"].(string)
		if len(got) != 64 {
			t.Fatalf("invalid cache scope %q", got)
		}
		return got
	}
	trusted := key(base)
	for _, change := range []func(*StepRun){
		func(r *StepRun) { r.OwnerUserID = "owner-b" },
		func(r *StepRun) { r.Repository.Name = "another" },
		func(r *StepRun) { r.Env = map[string]string{eventVar: string(pl.EventPullRequest)} },
		func(r *StepRun) { r.Env = nil },
	} {
		run := base
		change(&run)
		if key(run) == trusted {
			t.Fatalf("cache boundary collided: %+v", run)
		}
	}
	for _, event := range []pl.Event{pl.EventMergeGroup, pl.EventRelease} {
		run := base
		run.Env = map[string]string{eventVar: string(event)}
		if key(run) != trusted {
			t.Fatal("trusted event lost reusable cache")
		}
	}
	base.Caches = nil
	if _, present := fleetArgs(base, "")["cacheScope"]; present {
		t.Fatal("cache scope sent without caches")
	}
}

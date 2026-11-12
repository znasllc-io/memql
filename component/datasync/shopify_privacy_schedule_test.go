package datasync

import (
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// The Shopify privacy run, as the shipped tree declares it.
//
// SINGLE: a run takes jobs for twenty minutes and finishes the one it is on,
// and one erasure of a large store can take minutes of statements. Two runs
// at once would scrub the same rows against each other and write the same
// job's row twice.
//
// AT MINUTE 7, hourly: the continuing backfill pass begins on every fifth
// minute, and a privacy run that began at the same second would land its
// scrub, and its restart of the store's backfills, while a page was being
// applied. So the run's minute is held here beside the pass's schedule, and a
// change to either that puts them on the same second fails.
func TestThePrivacyRunIsSingleAndKeepsClearOfTheBackfillPass(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	loaded, err := automations.NewLoader(automations.LoaderOptions{Registry: concept.DefaultRegistry()}).LoadAll()
	if err != nil {
		t.Fatalf("load the shipped automations: %v", err)
	}
	byName := map[string]*automations.Automation{}
	for _, a := range loaded {
		byName[a.Name] = a
	}
	run, pass := byName["runShopifyComplianceJobs"], byName["continueQueuedBackfills"]
	if run == nil || pass == nil {
		t.Fatalf("the shipped automations hold the privacy run: %v, the continuing backfill pass: %v", run != nil, pass != nil)
	}
	if run.Schedule != "0 7 * * * *" {
		t.Errorf("the privacy run is scheduled %q, want %q: hourly, at minute 7", run.Schedule, "0 7 * * * *")
	}
	if run.Mode == nil || run.Mode.Kind != automations.ModeSingle {
		t.Errorf("the privacy run declares mode %+v, want single: a run that outlasts the hour must not be overlapped by the next", run.Mode)
	}
	// The pass begins every N minutes at second 0; the run's minute must not
	// be one of them.
	fields := strings.Fields(pass.Schedule)
	if len(fields) != 6 || fields[0] != "0" || !strings.HasPrefix(fields[1], "*/") {
		t.Fatalf("the continuing backfill pass is scheduled %q; this test reads a pass that begins every N minutes at second 0, and must be read again if that changed", pass.Schedule)
	}
	every, err := strconv.Atoi(strings.TrimPrefix(fields[1], "*/"))
	if err != nil || every < 2 {
		t.Fatalf("the continuing backfill pass is scheduled %q", pass.Schedule)
	}
	minute, err := strconv.Atoi(strings.Fields(run.Schedule)[1])
	if err != nil {
		t.Fatalf("the privacy run's minute is %q", strings.Fields(run.Schedule)[1])
	}
	if minute%every == 0 {
		t.Errorf("the privacy run begins at minute %d, when a backfill pass that runs every %d minutes begins too", minute, every)
	}
}

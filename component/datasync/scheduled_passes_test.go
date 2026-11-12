package datasync

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// The sync runtime's scheduled passes, as the shipped tree declares them. Each
// is single: a pass that outlasts its schedule is not overlapped by the next
// one in the same process. The sweep can run long -- a domain's first sweep
// reads it from the beginning -- and two of them at once would ask the origin
// for every page twice and race each other's watermark.
func TestTheSyncRuntimesScheduledPassesDoNotOverlapThemselves(t *testing.T) {
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
	for name, schedule := range map[string]string{
		"reconcileMirroredDomains":     "0 */10 * * * *",
		"continueQueuedBackfills":      "0 */5 * * * *",
		"retryFailedInboundDeliveries": "0 * * * * *",
	} {
		a := byName[name]
		if a == nil {
			t.Errorf("%s is not among the shipped automations", name)
			continue
		}
		if a.Schedule != schedule {
			t.Errorf("%s runs on %q, want %q", name, a.Schedule, schedule)
		}
		if a.Mode == nil || a.Mode.Kind != automations.ModeSingle {
			t.Errorf("%s declares mode %+v, want single: a pass that outlasts its schedule must not be overlapped by the next", name, a.Mode)
		}
	}
}

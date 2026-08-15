package automations

import (
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql"
)

// catalogSchedulerWith builds the smallest Scheduler that can answer
// CatalogAutomations: a name-keyed map and nothing else. That is deliberate --
// the seam reports what the SCHEDULER HOLDS, so a test that needed a loader, an
// engine or a tree would be testing the wrong thing.
func catalogSchedulerWith(autos ...*Automation) *Scheduler {
	s := &Scheduler{automations: map[string]*Automation{}}
	for _, a := range autos {
		s.automations[a.Name] = a
	}
	return s
}

func catalogEntryFor(t *testing.T, s *Scheduler, name string) memql.AutomationCatalogEntry {
	t.Helper()
	for _, e := range s.CatalogAutomations() {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("automation %q is missing from CatalogAutomations()", name)
	return memql.AutomationCatalogEntry{}
}

// A CONCEPT-SCOPED automation reports the pair the author wrote, not the topic
// the loader composed (memql#3805).
//
// The topic is built here with the real BuildTriggerTopic rather than a literal
// string, so the test cannot pass by agreeing with a hand-typed format the
// loader does not actually produce.
func TestCatalogAutomationsDecomposesAConceptScopedTrigger(t *testing.T) {
	topic, err := ast.BuildTriggerTopic("node.created", "v1:cognition:participant")
	if err != nil {
		t.Fatalf("BuildTriggerTopic: %v", err)
	}
	s := catalogSchedulerWith(&Automation{
		Name:    "autoJoinSI",
		Origin:  "unified:cognition/automations.memql",
		Trigger: &TriggerConfig{Event: topic},
	})

	got := catalogEntryFor(t, s, "autoJoinSI")
	if got.Trigger == nil {
		t.Fatal("a concept-scoped automation reported no trigger, so its run form has no concept to browse")
	}
	if got.Trigger.Event != "node.created" {
		t.Errorf("event: got %q, want the structured kind %q (the composed topic was %q)",
			got.Trigger.Event, "node.created", topic)
	}
	if got.Trigger.Concept != "v1:cognition:participant" {
		t.Errorf("concept: got %q, want v1:cognition:participant -- this is the field the row picker browses", got.Trigger.Concept)
	}
}

// A SCHEDULED automation reports its cron and no event. The run form keys the
// whole "fires NOW with an empty event" explanation off exactly this shape.
func TestCatalogAutomationsReportsASchedule(t *testing.T) {
	s := catalogSchedulerWith(&Automation{Name: "sweepStale", Schedule: "0 */10 * * * *"})

	got := catalogEntryFor(t, s, "sweepStale")
	if got.Trigger == nil {
		t.Fatal("a scheduled automation reported no trigger")
	}
	if got.Trigger.Schedule != "0 */10 * * * *" {
		t.Errorf("schedule: got %q", got.Trigger.Schedule)
	}
	if got.Trigger.Event != "" || got.Trigger.Concept != "" {
		t.Errorf("a scheduled trigger reported an event/concept it does not have: %+v", got.Trigger)
	}
}

// A RAW-TOPIC automation keeps its whole topic as the event and claims no
// concept. Reporting a concept here would send the row picker to browse rows of
// something assembled out of an application topic's segments.
func TestCatalogAutomationsLeavesARawTopicWhole(t *testing.T) {
	s := catalogSchedulerWith(&Automation{
		Name:    "onStartup",
		Trigger: &TriggerConfig{Event: "system.startup"},
	})

	got := catalogEntryFor(t, s, "onStartup")
	if got.Trigger == nil {
		t.Fatal("a raw-topic automation reported no trigger")
	}
	if got.Trigger.Event != "system.startup" {
		t.Errorf("event: got %q, want the topic whole", got.Trigger.Event)
	}
	if got.Trigger.Concept != "" {
		t.Errorf("a raw application topic yielded a concept %q, which names no real concept", got.Trigger.Concept)
	}
}

// An automation with NO trigger reports nil, not an empty object. The form
// reads manual-run off the absence, and an empty object is a different claim.
func TestCatalogAutomationsReportsNoTriggerAsNil(t *testing.T) {
	s := catalogSchedulerWith(&Automation{Name: "manualOnly"})

	if got := catalogEntryFor(t, s, "manualOnly"); got.Trigger != nil {
		t.Errorf("an automation with no trigger reported %+v; nil is the shape that reads as manual-run", got.Trigger)
	}
}

// THE PROMOTED CASE, which is the acceptance criterion this seam exists for.
//
// A promoted automation has NO FILE -- it lives in the cluster's database and
// was registered into the scheduler at promote time. Its Origin is empty, and a
// catalog that walked the tree could not see it at all. Reporting its trigger is
// therefore not a variation on the file-backed case: it is the case that proves
// the answer comes from the scheduler's map rather than from disk.
func TestCatalogAutomationsReportsAPromotedAutomationsTrigger(t *testing.T) {
	topic, err := ast.BuildTriggerTopic("node.updated", "v1:acme:widget")
	if err != nil {
		t.Fatalf("BuildTriggerTopic: %v", err)
	}
	s := catalogSchedulerWith(&Automation{
		Name: "onWidgetUpdated",
		// No Origin: nothing on disk declares this automation.
		Trigger: &TriggerConfig{Event: topic},
	})

	got := catalogEntryFor(t, s, "onWidgetUpdated")
	if got.Origin != "" {
		t.Errorf("the fixture is not modelling a promoted automation: origin %q", got.Origin)
	}
	if got.Trigger == nil {
		t.Fatal("a promoted automation reported no trigger -- the run form would have nothing to describe")
	}
	if got.Trigger.Event != "node.updated" || got.Trigger.Concept != "v1:acme:widget" {
		t.Errorf("promoted trigger: got %+v, want event=node.updated concept=v1:acme:widget", got.Trigger)
	}
}

// A nil entry in the map is skipped rather than panicking. CatalogAutomations
// already guarded this for the name; the trigger projection must not undo it.
func TestCatalogAutomationsSkipsNilEntries(t *testing.T) {
	s := &Scheduler{automations: map[string]*Automation{"gone": nil}}
	if got := s.CatalogAutomations(); len(got) != 0 {
		t.Errorf("a nil automation reached the catalog: %+v", got)
	}
}

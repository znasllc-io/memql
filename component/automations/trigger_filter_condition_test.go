package automations

import (
	"errors"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// trigger_filter_condition_test.go -- D8's load check at the trigger filter
// (memql#5366). A trigger's @filter is decided in process over the triggering
// row, where a stored non-boolean in condition position is "not true"; so a
// filter whose condition is a bare field of a declared non-boolean type loaded
// and never fired. It is refused at load now, against the TRIGGER'S concept,
// naming the field, its declared type and the fix -- the same check a refine
// clause gets (component/memql TestInit_RefusesANonBooleanFieldAsARefineCondition).

const (
	conditionTicketID = "v1:probe:ticket"
	conditionFlagID   = "v1:probe:flag"
)

// conditionCheckRegistry holds two concepts that disagree about `title`: a
// string on the ticket, a boolean on the flag. The same filter is a mistake
// over one and a condition over the other, which is what shows the check reads
// the trigger's concept rather than any concept that declares the name.
func conditionCheckRegistry(t *testing.T) memoryNodes.Registry {
	t.Helper()
	ticket, err := memoryNodes.ParseConceptMemQL([]byte(`
concept ticket {
  title     string
  priority  int
  archived  bool
}
`), "v1/probe/ticket")
	if err != nil {
		t.Fatalf("parse ticket: %v", err)
	}
	ticket.Name = conditionTicketID
	flag, err := memoryNodes.ParseConceptMemQL([]byte(`
concept flag {
  title  bool
}
`), "v1/probe/flag")
	if err != nil {
		t.Fatalf("parse flag: %v", err)
	}
	flag.Name = conditionFlagID
	return &secretTestRegistry{concepts: map[string]*memoryNodes.Concept{conditionTicketID: ticket, conditionFlagID: flag}}
}

// filteredAutomation is an automation on concept whose @filter is filter.
func filteredAutomation(concept, filter string) string {
	return `@trigger(event="node.created", concept="` + concept + `")
@filter(` + filter + `)
automation onRow {
  step run {
    logic noteRow(x: 1)
  }
}`
}

func TestTriggerFilterRefusesANonBooleanField(t *testing.T) {
	loader := NewLoader(LoaderOptions{Registry: conditionCheckRegistry(t)})
	stringFix := "Test whether it is set, `row.title != nil`, or compare it: `row.title == \"...\"`"
	for name, c := range map[string]struct{ filter, want string }{
		"a string field": {`row => row.title`,
			"`row.title` does not lower in a trigger filter: `row.title` is a string (declared `string` on " + conditionTicketID + "), and a condition must be boolean. " + stringFix},
		"a number under ! and &&": {`row => row.archived && !row.priority`,
			"`row.priority` is a number (declared `int` on " + conditionTicketID + "), and a condition must be boolean. Compare it with a number, as in `row.priority > 0`"},
		"a ternary branch": {`row => row.archived ? row.title : false`,
			"`row.title` is a string (declared `string` on " + conditionTicketID + "), and a condition must be boolean. " + stringFix},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loader.compileMemQL(filteredAutomation(conditionTicketID, c.filter), "test:onRow")
			if err == nil {
				t.Fatalf("@filter(%s) over %s loaded; want the load refusal", c.filter, conditionTicketID)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v\nwant it to contain %q", err, c.want)
			}
			var le *memql.LowerError
			if !errors.As(err, &le) {
				t.Fatalf("the refusal is not a LowerError: %v", err)
			}
			if !le.Span.IsZero() {
				t.Fatalf("the refusal carries a span of the compiled filter text (%+v), which an authoring diagnostic would place in the author's file", le.Span)
			}
		})
	}

	// The compiled-JSON path (the LogicRunner's body compile) prepares
	// through the same preparer and refuses the same filter.
	_, err := loader.parseJSON([]byte(`{"expressions":"v1","name":"onRow","trigger":{"event":"graph.node.created.`+conditionTicketID+`","filter":"row => row.title"},"steps":[{"id":"s","type":"function","function":{"name":"f"}}]}`), "test:v1")
	if err == nil || !strings.Contains(err.Error(), "`row.title` is a string (declared `string` on "+conditionTicketID+")") {
		t.Fatalf("parseJSON: want the load refusal, got %v", err)
	}
}

// TestTriggerFilterLoadsABooleanCondition is the negative control: a boolean
// field, a comparison and a boolean ternary load over the ticket, and the
// ticket's refused filter loads over the flag, whose title IS a boolean.
func TestTriggerFilterLoadsABooleanCondition(t *testing.T) {
	loader := NewLoader(LoaderOptions{Registry: conditionCheckRegistry(t)})
	for _, c := range []struct{ concept, filter string }{
		{conditionTicketID, `row => row.archived`},
		{conditionTicketID, `row => row.title != nil && !row.archived`},
		{conditionTicketID, `row => row.archived ? row.priority > 1 : row.title == "x"`},
		{conditionFlagID, `row => row.title`},
	} {
		a, err := loader.compileMemQL(filteredAutomation(c.concept, c.filter), "test:onRow")
		if err != nil {
			t.Fatalf("@filter(%s) over %s: %v", c.filter, c.concept, err)
		}
		if a.Trigger == nil || a.Trigger.FilterLambda == nil {
			t.Fatalf("@filter(%s) over %s: the filter was not prepared into a lambda", c.filter, c.concept)
		}
	}
}

package automations

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

// inbound_delivery_trigger_test.go -- the rule every shipped automation on a
// staged inbound delivery is held to: it fires for a row nobody has worked,
// and not for a write that stamps it.
//
// Every write of a row publishes node.created, and a staged delivery is
// written again by whatever works it: a status stamp, the retry pass's claim, a
// requeue. An automation whose trigger admits those writes runs once for each,
// and one whose builtin makes such a write fires itself -- which the load-time
// loop check cannot see, because a builtin's writes are opaque to it
// (loop_graph.go). The rule and what an unfiltered trigger did are in
// dsl/platform/automations.memql.

const inboundRequestConcept = "v1:platform:inboundRequest"

// inboundRowEvent is the event the engine publishes for one write of a staged
// delivery, as executeWrite builds it: the row's fields flat on the payload,
// the row again under `payload`, and firstVersion saying whether this write
// made the row.
func inboundRowEvent(source, status string, firstVersion bool) *events.Event {
	row := map[string]any{
		"source":            source,
		"medium":            "webhook",
		"body":              "{}",
		"status":            status,
		"signatureVerified": true,
	}
	payload := map[string]any{
		"id":       inboundRequestConcept + ":delivery-1",
		"nodeId":   inboundRequestConcept + ":delivery-1",
		"concept":  inboundRequestConcept,
		"nodeType": "node",
		"payload":  row,
	}
	for k, v := range row {
		payload[k] = v
	}
	payload["firstVersion"] = firstVersion
	ev := events.NewEvent(events.BuildTopicWithConcept(events.TopicGraphNodeCreated, inboundRequestConcept), events.KindNodeCreated, payload)
	return &ev
}

// firesOn asks the question the scheduler's bus subscriber asks of an event:
// do the automation's args bind, and does its trigger filter admit it.
func firesOn(t *testing.T, a *Automation, ev *events.Event) bool {
	t.Helper()
	bound, _, err := bindEventArgs(a, ev)
	if err != nil {
		return false
	}
	if a.Trigger.Filter == "" {
		return true
	}
	fires, err := evaluateTriggerFilter(a, ev, bound)
	if err != nil {
		t.Fatalf("%s: its trigger filter could not be evaluated: %v", a.Name, err)
	}
	return fires
}

// shippedInboundAutomations is every enabled shipped automation that triggers
// on a write of a staged inbound delivery.
func shippedInboundAutomations(t *testing.T) []*Automation {
	t.Helper()
	loaded, err := NewLoader(LoaderOptions{}).LoadAll()
	if err != nil {
		t.Fatalf("load the shipped automations: %v", err)
	}
	topic := events.BuildTopicWithConcept(events.TopicGraphNodeCreated, inboundRequestConcept)
	var out []*Automation
	for _, a := range loaded {
		if a == nil || !a.IsEnabled() || a.BeforeWrite != nil || a.Trigger == nil || a.Trigger.Event == "" {
			continue
		}
		if events.Match(events.NormalizePattern(a.Trigger.Event), topic) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NO SHIPPED AUTOMATION FIRES ON A WORKED DELIVERY. A delivery that has been
// stamped -- processing, processed or failed -- is somebody's already, and the
// write that stamped it must start no run. Held for every automation the tree
// ships on this concept, so the next one added is held to it too.
func TestNoShippedAutomationFiresOnAWorkedInboundDelivery(t *testing.T) {
	automations := shippedInboundAutomations(t)
	names := make([]string, 0, len(automations))
	for _, a := range automations {
		names = append(names, a.Name)
	}
	// The ones this rule was written for. A rename or a move is a deliberate
	// edit of this list; an automation missing from the tree is a test that
	// would otherwise pass by testing nothing.
	for _, want := range []string{"dispatchInboundToConnector", "ingestCampaignFeedback", "notePackageUpstreamFromWebhook", "triggerPipelinesOnGitHubDelivery"} {
		if !strings.Contains(" "+strings.Join(names, " ")+" ", " "+want+" ") {
			t.Fatalf("%s is not among the shipped automations on %s: %v", want, inboundRequestConcept, names)
		}
	}

	sources := []string{"github", "shopify", "graph-mailbox", "anything-else"}
	for _, a := range automations {
		t.Run(a.Name, func(t *testing.T) {
			// One refusal is the whole finding: the first worked delivery that
			// fires it, not one line for each of the twelve.
		worked:
			for _, status := range []string{"processing", "processed", "failed"} {
				for _, source := range sources {
					if firesOn(t, a, inboundRowEvent(source, status, false)) {
						t.Errorf("%s fires on a %s delivery of source %q: a write that stamps a delivery starts a run of it, and an automation "+
							"whose builtin stamps the row fires itself. Give its trigger @filter(row => row.status == \"received\") "+
							"(dsl/platform/automations.memql says why)", a.Name, status, source)
						break worked
					}
				}
			}
			// THE CONTROL: the staged row fires it, for at least one source, so
			// the silence above is the status's and not a payload this harness
			// built wrong.
			fired := false
			for _, source := range sources {
				fired = fired || firesOn(t, a, inboundRowEvent(source, "received", true))
			}
			if !fired {
				t.Errorf("%s does not fire on a received delivery of any of %v: this test's event no longer looks like a staged row to it", a.Name, sources)
			}
		})
	}
}

// THE LOAD REFUSES A BODY THAT STAMPS THE ROW IT FIRES ON, UNLESS ITS FILTER
// EXCLUDES THE STAMP. This is what an automation written in DSL gets for free
// and one whose stamp is inside a builtin does not: the same trigger and the
// same write, seen by the loop check because the write is a mutation call.
// docs/public/operate/inbound-delivery.md tells an author so.
func TestAnAutomationStampingItsOwnDeliveryNeedsTheStatusFilter(t *testing.T) {
	functions := NewFunctionSource(functionsFromTree(t, map[string][]string{
		"platform/mutations.memql": {"updateInboundRequestStatus"},
	}))
	automation := func(filter string) *Automation {
		return graphAutomation(t, fmt.Sprintf(`@trigger(event="node.created", concept="v1:platform:inboundRequest")
@filter(row => %s)
automation handleOrderDelivery {
  args {
    id any
  }
  mutation updateInboundRequestStatus(requestId: args.id, status: "processing")
  mutation updateInboundRequestStatus(requestId: args.id, status: "processed", processedAt: now)
}`, filter))
	}

	unfiltered := BuildLoopGraph([]*Automation{automation(`row.source == "orders"`)}, functions, 0)
	if len(unfiltered.Problems) != 1 || unfiltered.Problems[0].Code != codeLoopCycle {
		t.Fatalf("an automation that stamps the row it fires on, filtered on source alone, was not refused as a cycle: %+v", unfiltered.Problems)
	}

	filtered := BuildLoopGraph([]*Automation{automation(`row.source == "orders" && row.status == "received"`)}, functions, 0)
	if len(filtered.Problems) != 0 {
		t.Fatalf("the same automation filtered on status == \"received\" was refused: %+v", filtered.Problems)
	}
	if len(filtered.Edges) != 0 {
		t.Fatalf("its own stamps still reach its trigger: %+v", filtered.Edges)
	}
}

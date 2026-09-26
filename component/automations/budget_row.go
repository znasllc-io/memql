package automations

import (
	"strings"

	"github.com/znasllc-io/memql/component/events"
)

func budgetRowId(ev *events.Event) string {
	if ev == nil || !strings.HasPrefix(ev.Topic, "graph.node.") {
		return ""
	}
	// Mutation events flatten business payload fields. For work runs, nodeId
	// is the execution host; id is the graph record being changed.
	if id, ok := ev.Payload["id"].(string); ok && id != "" {
		return id
	}
	id, _ := ev.Payload["nodeId"].(string)
	return id
}

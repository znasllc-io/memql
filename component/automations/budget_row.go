package automations

import (
	"github.com/znasllc-io/memql/component/events"
	"strings"
)

func budgetRowId(ev *events.Event) string {
	if ev == nil || !strings.HasPrefix(ev.Topic, "graph.node.") {
		return ""
	}
	if id, ok := ev.Payload["nodeId"].(string); ok && id != "" {
		return id
	}
	id, _ := ev.Payload["id"].(string)
	return id
}

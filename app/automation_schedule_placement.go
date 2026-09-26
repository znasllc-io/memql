package app

import (
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/node"
)

const packagePollAutomation = "pollPackageUpstreams"

// Repository polling may immediately build an automatic update. Its runner
// must own a build runtime: the general cron leader can be identity, whose
// image deliberately has neither a shell nor workbench forwarding. Only
// workbench replicas compete for this independent lease. With no workbench,
// polling waits; no other node runs a build locally as a fallback.
func (a *App) scheduledAutomationGate(general func() bool) func(string) bool {
	nodeType := envregistry.ResolveNodeType()
	var poll func() bool
	if nodeType == string(node.NodeTypeWorkbench) {
		leader := automations.NewScopedCronLeader("package-poll", a.directDBGetter(), a.Logger)
		a.Dependencies = append(a.Dependencies, leader)
		poll = leader.IsLeader
	}
	return scheduledAutomationGateForNode(nodeType, general, poll)
}

func scheduledAutomationGateForNode(nodeType string, general, poll func() bool) func(string) bool {
	return func(name string) bool {
		if name == packagePollAutomation {
			return nodeType == string(node.NodeTypeWorkbench) && poll != nil && poll()
		}
		return general == nil || general()
	}
}

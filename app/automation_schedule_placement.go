package app

import (
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/node"
)

const (
	packagePollAutomation   = "pollPackageUpstreams"
	pipelinesPollAutomation = "pollPipelines"
)

// placedSchedule pins one scheduled automation to one node type, behind a
// cron lease of its own, instead of the general cron leader -- which can be
// any node type, identity included.
type placedSchedule struct {
	automation string
	nodeType   node.NodeType
	// scope names the scoped cron leader's lease. Only replicas of nodeType
	// compete for it, and it never borrows general leadership: a cluster with
	// no replica of nodeType does not run the automation at all.
	scope string
}

// placedSchedules is every scheduled automation that runs somewhere other than
// on the general cron leader. Each is here because what it does can only be
// done by one node type.
var placedSchedules = []placedSchedule{
	// Repository polling may immediately build an automatic update. Its runner
	// must own a build runtime: the general cron leader can be identity, whose
	// image deliberately has neither a shell nor workbench forwarding. Only
	// workbench replicas compete for this independent lease. With no
	// workbench, polling waits; no other node runs a build locally as a
	// fallback.
	{automation: packagePollAutomation, nodeType: node.NodeTypeWorkbench, scope: "package-poll"},
	// The pipelines poll opens runs for heads it has not seen and then
	// RECOVERS runs: it claims and drives a queued run no agent claimed and an
	// in-progress run whose driver went silent, and only an agent node drives
	// a run (epic memql#5477, plan decision 10). Only agent replicas compete
	// for this lease. With no agent replica nothing polls -- and nothing could
	// have driven the runs a poll would open.
	{automation: pipelinesPollAutomation, nodeType: node.NodeTypeAgent, scope: "pipelines-poll"},
}

// scheduledAutomationGate is the scheduler's ScheduleGate: a placed automation
// fires only on its node type while this replica holds its lease, and every
// other scheduled automation follows the general cron leader.
func (a *App) scheduledAutomationGate(general func() bool) func(string) bool {
	nodeType := envregistry.ResolveNodeType()
	leases := map[string]func() bool{}
	for _, p := range placedSchedules {
		if nodeType != string(p.nodeType) {
			continue
		}
		leader := automations.NewScopedCronLeader(p.scope, a.directDBGetter(), a.Logger)
		a.Dependencies = append(a.Dependencies, leader)
		leases[p.automation] = leader.IsLeader
	}
	return scheduledAutomationGateForNode(nodeType, general, leases)
}

// scheduledAutomationGateForNode decides one firing from the node type, the
// general leadership and the placed automations' leases, keyed by automation
// name. A placed automation with no lease on this node never fires: it must not
// borrow general leadership.
func scheduledAutomationGateForNode(nodeType string, general func() bool, leases map[string]func() bool) func(string) bool {
	placed := make(map[string]node.NodeType, len(placedSchedules))
	for _, p := range placedSchedules {
		placed[p.automation] = p.nodeType
	}
	return func(name string) bool {
		if want, ok := placed[name]; ok {
			lease := leases[name]
			return nodeType == string(want) && lease != nil && lease()
		}
		return general == nil || general()
	}
}

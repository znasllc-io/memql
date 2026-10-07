package app

import (
	"fmt"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/node"
	"strings"
)

type placedSchedule struct {
	automation string
	nodeType   node.NodeType
	scope      string
}

// Placement is authored beside the schedule. Go validates the deployment role
// and provides the shared PostgreSQL lease; no workflow names are encoded here.
func schedulePlacements(definitions []*automations.Automation) ([]placedSchedule, error) {
	out := []placedSchedule{}
	scopes := map[string]bool{}
	for _, a := range definitions {
		if a.ScheduleNode == "" {
			continue
		}
		role := node.NodeType(a.ScheduleNode)
		if _, ok := node.RoleFor(role); !ok {
			return nil, fmt.Errorf("automation %s names unknown schedule node %q", a.Name, a.ScheduleNode)
		}
		if !a.IsScheduled() || a.IsEventTriggered() || a.BeforeWrite != nil || a.Template {
			return nil, fmt.Errorf("automation %s placement requires a scheduled automation", a.Name)
		}
		scope := strings.TrimSpace(a.ScheduleLease)
		if scope == "" {
			scope = "schedule:" + a.Name
		}
		if scopes[scope] {
			return nil, fmt.Errorf("automation %s repeats schedule lease %q", a.Name, scope)
		}
		scopes[scope] = true
		out = append(out, placedSchedule{automation: a.Name, nodeType: role, scope: scope})
	}
	return out, nil
}

func (a *App) scheduledAutomationGate(general func() bool, definitions []*automations.Automation) (func(string) bool, error) {
	placements, err := schedulePlacements(definitions)
	if err != nil {
		return nil, err
	}
	nodeType := envregistry.ResolveNodeType()
	leases := map[string]func() bool{}
	for _, p := range placements {
		if nodeType != string(p.nodeType) {
			continue
		}
		leader := automations.NewScopedCronLeader(p.scope, a.directDBGetter(), a.Logger)
		a.Dependencies = append(a.Dependencies, leader)
		leases[p.automation] = leader.IsLeader
	}
	return scheduledAutomationGateForNode(nodeType, general, leases, placements), nil
}

func scheduledAutomationGateForNode(nodeType string, general func() bool, leases map[string]func() bool, placements []placedSchedule) func(string) bool {
	placed := map[string]node.NodeType{}
	for _, p := range placements {
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

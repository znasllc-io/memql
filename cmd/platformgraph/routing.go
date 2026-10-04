package main

import (
	"fmt"
	"strconv"

	"github.com/znasllc-io/memql/component/architecture/model"
	"github.com/znasllc-io/memql/component/node"
)

// routingPass records the mesh event-routing table as component/node
// evaluates it: block rules first, then the first matching forward rule,
// default deny. A forward rule with a target type gets a forwards_to edge to
// that role; a broadcast rule reaches every mesh role and says so in its
// attrs rather than drawing an edge to each.
//
// The table is what THIS binary links. Rules registered from a package's
// init() appear only when that package is compiled in; today every
// registration is in component/node itself, which this command imports.
func routingPass(b *builder) error {
	for i, r := range node.RoutingRules() {
		action := "forward"
		if r.Block {
			action = "block"
		}
		target := "*"
		if r.TargetType != "" {
			target = string(r.TargetType)
		}
		if r.Block {
			target = ""
		}
		id := model.RoutingRuleID(action, r.Pattern)
		if b.has(id) {
			return fmt.Errorf("routing rule %s %q appears twice; the second can never match first", action, r.Pattern)
		}
		b.node(id, model.PlatformRoutingRule, r.Pattern, map[string]string{
			"action": action,
			"target": target,
			"order":  strconv.Itoa(i),
		})
		if !r.Block && r.TargetType != "" {
			if _, ok := node.RoleFor(r.TargetType); !ok {
				return fmt.Errorf("routing rule %q targets %q, which is not a role", r.Pattern, r.TargetType)
			}
			b.edge(id, model.ServiceID(string(r.TargetType)), model.PlatformForwardsTo, nil)
		}
	}
	return nil
}

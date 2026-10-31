package gamespack

import (
	"sync"

	"github.com/znasllc-io/memql/component/node"
)

// routing.go -- the live graph's forward rules for this pack's concepts.
//
// Mesh forwarding is DEFAULT-DENY (component/node/routing.go): a
// graph.node.* event that matches no rule stays on the replica that wrote
// the row. A runtime writes through whichever bff replica the front door
// picked and a browser watches through whichever one it is attached to, so
// without these rules a live memory view is correct on load and frozen
// after -- the failure routing.go calls the worst of the three, because it
// looks like it is working.
//
// BROADCAST, and safe to: no automation in the tree triggers on a
// v1:games:* event, so a forwarded event has no consumer to double-fire, and
// row admission still decides which streams see each row (an owned row
// reaches its owner and a cluster owner).
//
// CREATED AND UPDATED ONLY. Nothing publishes graph.node.deleted for these
// concepts (forget and retention delete in SQL), and a rule for an event
// nothing emits is a rule nobody could tell was broken.
//
// v1:games:planTrace IS DELIBERATELY NOT FORWARDED: one row per executed
// plan, read on demand by learning and by nobody live -- the volume argument
// routing_reach.go records for v1:work:observation.

// routedConcepts are the concepts a live view subscribes to.
var routedConcepts = []string{
	conceptPlayer, conceptEpisode, conceptPreference, conceptPromise, conceptPlace, conceptSkill,
}

var routingOnce sync.Once

// registerRoutingRules registers the forward rules once per process.
func registerRoutingRules() {
	routingOnce.Do(func() {
		for _, concept := range routedConcepts {
			for _, verb := range []string{"created", "updated"} {
				node.RegisterRoutingRule(node.RoutingRule{Pattern: node.GraphEventTopic(verb, concept), TargetType: ""})
			}
		}
	})
}

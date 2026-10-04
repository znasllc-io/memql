package node

import (
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// A GROUP MEMBERSHIP CHANGE REACHES EVERY REPLICA (memql#5660).
//
// Two readers depend on it. The Users app's group and person pages are live on
// these rows (epic memql#5165). And the fleet's replica-hop receiver keeps a
// person's groups in a cache (component/worker.MembershipCache) that a change
// to either concept must empty on EVERY replica: a membership is written on
// whichever replica served the person who made the change, and the cache that
// decides whether a removed person may still use a machine lent to their group
// sits on the replica holding that machine's stream. With these rules gone the
// cache there would keep the removed person's groups until its TTL, and every
// replica would report healthy.
//
// The whole hop, not only the table: replica A's bus, the real EventBridge
// forward, replica B's real inbound republish, a subscriber on B's bus.
func TestAGroupMembershipChangeReachesAnotherReplica(t *testing.T) {
	topics := []string{
		GraphEventTopic("created", "v1:identity:groupMembership"),
		GraphEventTopic("updated", "v1:identity:groupMembership"),
		GraphEventTopic("created", "v1:identity:group"),
		GraphEventTopic("updated", "v1:identity:group"),
	}
	replicaA, replicaB, deliver := twoReplicaMesh(t)

	for _, topic := range topics {
		t.Run(topic, func(t *testing.T) {
			got := make(chan events.Event, 1)
			unsub := replicaB.bus.Subscribe(topic, func(e events.Event) {
				select {
				case got <- e:
				default:
				}
			}, events.WithSubscriberName("test:membershipCache"))
			defer unsub()

			replicaA.bus.Publish(events.NewEvent(topic, events.KindNodeUpdated,
				map[string]any{"id": "membership-ana-design", "status": "removed"}))
			deliver(t)

			select {
			case e := <-got:
				if id, _ := e.Payload["id"].(string); id != "membership-ana-design" {
					t.Errorf("the event crossed but its payload did not: %v", e.Payload)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s written on replica A never reached replica B: a person removed from a group "+
					"keeps a machine lent to that group through B's membership cache until its TTL", topic)
			}
		})
	}

	// THE REACHABLE NEGATIVE: the identity concepts beside these stay local
	// under default-deny, so a rule widened to v1:identity:* -- which would
	// pass every case above -- fails here.
	if d := evaluateRouting(defaultRoutingRules(), "graph.node.created.v1:identity:authSession"); d.Forward {
		t.Errorf("an unrelated identity concept must stay local, got %+v", d)
	}
}

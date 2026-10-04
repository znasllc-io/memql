//go:build agent

package worker

// THE RECEIVER REUSES A PERSON'S GROUPS, AND FORGETS THEM WHEN THEY CHANGE
// (memql#5660).
//
// verifySharedRegistration built a fresh Person for every call it re-checked,
// so a machine lent to a group cost a membership read per call on the replica
// holding it. It now reads through the replica's membership cache -- with no
// test resolver set, which is production -- and these hold the two halves of
// that: the read is reused, and a membership change ends the access it
// granted on the very next call.

import (
	"testing"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/node"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

func TestTheReceiverReadsAPersonsGroupsOnceUntilTheyChange(t *testing.T) {
	memberships := installMemberships(t)
	bus := subscribeInstalledMembershipCache(t)
	// No resolver on the receiver: it reads the installed membership source
	// through the installed cache, as an agent node does.
	h := newPeopleHop(t, nil, []string{"design"}, nil)

	for n := 1; n <= 3; n++ {
		out, err := h.call(t, h.caller)
		if err != nil || out.RefusedBeforeStart {
			t.Fatalf("call %d: a member of the design group must be served: %v %s %s", n, err, out.ErrorCode, out.ErrorMessage)
		}
	}
	if n := memberships.take(); n != 1 {
		t.Fatalf("membership reads = %d over three calls, want one: the receiver reuses a person's groups", n)
	}

	// The caller is removed from design -- written on any replica, delivered
	// to this one as the broadcast graph event (TestEveryMembershipChange...
	// below holds the routing). The next call must be refused.
	memberships.removeCaller()
	bus.PublishSync(events.NewEvent(
		events.TopicNodeUpdated("v1:identity:groupMembership"),
		events.KindNodeUpdated,
		map[string]any{"id": "membership-ursula-design", "status": "removed"},
	))
	out, err := h.call(t, h.caller)
	if err == nil && !out.RefusedBeforeStart {
		t.Fatal("a person removed from the group was still served across the hop: the cache outlived the change")
	}
	if n := memberships.take(); n != 1 {
		t.Fatalf("membership reads after the change = %d, want the receiver to read the groups again", n)
	}
}

func TestAReceiverWhoseCacheIsNotSubscribedReadsEveryCall(t *testing.T) {
	// The installed cache unsubscribed -- a node whose wiring left the
	// subscriber out -- answers from the source every time: slower, never
	// stale.
	memberships := installMemberships(t)
	h := newPeopleHop(t, nil, []string{"design"}, nil)
	for n := 1; n <= 3; n++ {
		if out, err := h.call(t, h.caller); err != nil || out.RefusedBeforeStart {
			t.Fatalf("call %d: %v %s %s", n, err, out.ErrorCode, out.ErrorMessage)
		}
	}
	if n := memberships.take(); n != 3 {
		t.Fatalf("membership reads = %d, want one per call through an unsubscribed cache", n)
	}
}

func TestEveryMembershipChangeTheReceiverForgetsOnCrossesReplicas(t *testing.T) {
	// A change is written on whichever replica served the person who made
	// it, and the cache that must forget is on the replica holding the lent
	// machine. Default-deny would keep the event on the writer: the cache
	// there would drop, and this one would keep a removed person's groups
	// until the TTL. Every topic the cache listens on must be forwarded.
	topics := workerservice.MembershipInvalidationTopics()
	if len(topics) == 0 {
		t.Fatal("the cache listens on no topic")
	}
	for _, topic := range topics {
		if !node.ForwardsGraphEvent(topic) {
			t.Errorf("%s is not forwarded across replicas: a membership change written on one replica "+
				"would leave the receiver's cache on another stale for up to %s", topic, workerservice.MembershipCacheTTL)
		}
	}
	// Both verbs of both concepts, in the spelling the routing table matches.
	for _, concept := range []string{"v1:identity:groupMembership", "v1:identity:group"} {
		for _, verb := range []string{"created", "updated"} {
			found := false
			for _, topic := range topics {
				found = found || topic == node.GraphEventTopic(verb, concept)
			}
			if !found {
				t.Errorf("the cache does not listen for %s", node.GraphEventTopic(verb, concept))
			}
		}
	}
}

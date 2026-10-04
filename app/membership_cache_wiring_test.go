package app

import (
	"os"
	"strings"
	"testing"
)

// The agent's membership cache must be SUBSCRIBED, or it never answers from
// memory (memql#5660). A source scan, for the reason
// TestAgentWiringServesAndForwardsAppCalls gives: the wiring lives behind
// `//go:build agent`, standing up an agent node needs a worker service, a
// gRPC server and a database, and what goes wrong is a line going away.
//
// The failure it guards is silent in the safe direction -- an unsubscribed
// cache reads the membership rows on every call, so nothing breaks -- which is
// exactly why it would never be noticed: the receiver would quietly go back to
// the per-call reads memql#5660 removed. It must be the INSTALLED cache that
// is subscribed, because that is the one worker.CachedGroups reads.
func TestAgentWiringSubscribesTheMembershipCache(t *testing.T) {
	source, err := os.ReadFile("integrations_worker_agent.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, want := range []string{"worker.NewMembershipCacheSubscriber(", "worker.InstalledMembershipCache()"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the agent wiring no longer calls %s: the replica-hop receiver's membership cache is never "+
				"subscribed, so it never reuses a person's groups", want)
		}
	}
}

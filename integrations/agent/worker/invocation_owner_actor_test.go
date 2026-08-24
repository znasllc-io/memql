//go:build agent

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// The two properties memql#4406 rests on for THIS writer, both of which fail
// silently.
//
// v1:worker:invocation declares @rowAuthz(owner="ownerUserId", clusterOwner)
// and marks the field @serverSet. So the owner has to arrive as the ACTOR: an
// unstamped write lands a row with an empty ownerUserId, readable by nobody,
// with no error anywhere -- and the operator meets it much later as "the Fleet
// page says this machine has done nothing".
//
// The mirror property matters just as much. ownerUserId must NOT also travel as
// an argument, because the construct no longer declares it and an undeclared
// argument is DISCARDED rather than refused (memql#3626,
// TestFleetCallSitesResolveAndDeclareTheirArguments). A reviewer who sees
// `"ownerUserId": row.OwnerUserId` still in the map concludes the owner reached
// the row; it did not.
func TestInvocationWriteBorrowsTheOwnersAuthority(t *testing.T) {
	row := workerservice.InvocationRow{
		ID:          "inv-1",
		OwnerUserId: "user-42",
		WorkerId:    "reg-1",
		AgentId:     "agent-1",
		Tool:        "workerHost",
		Action:      "exec",
		Outcome:     "success",
		StartedAt:   time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC),
	}

	ctx, query, err := invocationWriteCall(context.Background(), row)
	if err != nil {
		t.Fatalf("invocationWriteCall: %v", err)
	}
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.UserId != "user-42" {
		t.Fatalf("the write runs as %+v, want an actor of user-42. An unstamped write lands a row "+
			"with an empty owner that nobody can read, and says nothing.", ac)
	}
	if strings.Contains(query, "ownerUserId") {
		t.Fatalf("ownerUserId still travels as an argument, where it is silently discarded:\n%s", query)
	}
	// The rest of the payload must still be there -- an over-eager removal that
	// dropped real arguments would also pass the two checks above.
	for _, want := range []string{"invocationId", "workerId", "agentId", "outcome", "routing"} {
		if !strings.Contains(query, want) {
			t.Errorf("the invocation write lost the %q argument:\n%s", want, query)
		}
	}
}

func TestInvocationWriteRefusesABlankOwner(t *testing.T) {
	// auth.ContextWithUserActor returns ctx UNCHANGED for a blank id, so a
	// fallthrough here produces exactly the unreadable row above. Refusing
	// names the problem where the value went missing.
	for _, owner := range []string{"", "   ", "\t"} {
		_, _, err := invocationWriteCall(context.Background(), workerservice.InvocationRow{
			ID: "inv-1", OwnerUserId: owner, Tool: "workerHost", Action: "exec", Outcome: "success",
		})
		if err == nil {
			t.Errorf("a blank ownerUserId (%q) was accepted", owner)
		}
	}
}

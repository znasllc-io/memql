//go:build agent

package worker

// PEOPLE ARE COMPARED BY THE SHARE LIST'S RULE, ON THE HOP AS IN THE PLAN
// (review of memql#5662).
//
// sameSubject compared ids by the text after the LAST colon, so on the
// replica hop and at the app gate the synthetic actor `system:automation:ana`
// was the same subject as the person `ana` -- the comparison
// component/worker.SameSubjectId was written to replace (sharing.go records
// why), and the one the plan stopped using with the pin change. Nothing
// reached through it is exploitable today, because the decisions it guards
// are made again on the VERIFIED subject; these hold the comparison to the
// written rule anyway, at the three places it is a person's id.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

func TestASyntheticActorIsNotThePersonItsNameSpells(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"system:automation:ana", "ana", false},
		{"system:automation:ana", "v1:identity:user:ana", false},
		{"connector:ana", "ana", false},
		{"cluster:ana", "v1:identity:user:ana", false},
		// The bare/canonical split it exists to tolerate is unchanged.
		{"v1:identity:user:ana", "ana", true},
		{"ana", "ana", true},
		{"system:automation:ana", "system:automation:ana", true},
		{"", "", false},
	} {
		if got := sameSubject(tc.a, tc.b); got != tc.same {
			t.Errorf("sameSubject(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

func TestTheHopRefusesAnEnvelopeOwnerThatIsNotTheVerifiedPerson(t *testing.T) {
	// The envelope's owner is a HINT checked against the verified authority.
	// An automation's synthetic id is not the person, whatever its name.
	var reached atomic.Bool
	h := newModelHop(t, func(context.Context, workerservice.ModelCallRequest, func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		reached.Store(true)
		return workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop, Content: "served"}
	}, nil)
	out, err := h.link.router.ForwardModelCall(
		authorityCtx(t, h.owner), nodeB, "laptop", "system:automation:alice", h.start(), 10*time.Second, nil)
	if reached.Load() || (err == nil && !out.RefusedBeforeStart) {
		t.Fatal("an envelope naming system:automation:alice was accepted under alice's verified authority and reached the machine")
	}
	if out.ErrorCode != "owner_mismatch" {
		t.Fatalf("refusal = %q %q, want owner_mismatch", out.ErrorCode, out.ErrorMessage)
	}
}

func TestTheAppGateDoesNotTakeAnAutomationsPinAsThePersons(t *testing.T) {
	// A pinned app door opens a session on a person's own machine only when
	// that person made the pin. An automation's pin is not theirs.
	pin := memqlengine.AppDoorPin{Pinned: true, By: "system:automation:ana"}
	refusal := appDoorPinRefusal(pin, "v1:identity:user:ana")
	if refusal == nil || refusal.code != AppGateNotNamedByOwner {
		t.Fatalf("refusal = %v: a pin made by system:automation:ana was taken as ana's own", refusal)
	}
	if own := appDoorPinRefusal(memqlengine.AppDoorPin{Pinned: true, By: "ana"}, "v1:identity:user:ana"); own != nil {
		t.Fatalf("ana's own pin, in the other spelling of her id, was refused: %v", own)
	}
}

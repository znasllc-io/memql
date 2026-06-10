//go:build clustere2e

package clustere2e

// EXPERIMENTAL probe (not a committed gate): does cross-replica delivery work
// when every subscriber establishes PRESENCE in the space first -- i.e. behaves
// like a real CoPresent client opening a space (which writes a v1:cognition:
// presence row, the chat-reply topic that triggers ChatReplyDelivery's
// consumer-side substrate subscription on that replica)? If GREEN, the substrate
// works for realistic clients and the bare-subscriber gate was unrealistic; if
// RED, there is a deeper consume-path bug.

import (
	"context"
	"testing"
	"time"

	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
	"github.com/znasllc-io/memql/core/id"
)

func TestPresenceDrivenDelivery(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	conns := openConnections(ctx, t, tok, connCount)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	producer := conns[0]
	spaceID, participantID := newSpaceWithHuman(ctx, t, producer, userIDFromToken(t, tok))

	// Each connection establishes presence in the space -> a v1:cognition:presence
	// event fires on ITS replica -> ChatReplyDelivery.ensureSubscribed(space) on
	// that replica. This is what a real client does on opening the space.
	for _, c := range conns {
		qc := memqlclient.NewQueryClient(c.Dispatcher())
		if _, err := qc.MutationUpdateParticipantPresence(ctx, memqlclient.MutationUpdateParticipantPresenceArgs{
			SpaceId:       spaceID,
			ParticipantId: participantID,
			State:         "idle",
			Label:         "probe",
		}); err != nil {
			t.Logf("presence upsert on a connection failed (continuing): %v", err)
		}
	}

	chans := make([]<-chan string, len(conns))
	for i, c := range conns {
		chans[i] = subscribeUtterances(ctx, t, c, spaceID)
	}
	// Generous settle: presence event -> ensureSubscribed -> durable subscription live.
	time.Sleep(3 * time.Second)

	utteranceID := "v1:cognition:utterance:" + id.NewShortId()
	qc := memqlclient.NewQueryClient(producer.Dispatcher())
	if _, err := qc.MutationSendTextUtterance(ctx, memqlclient.MutationSendTextUtteranceArgs{
		UtteranceId:     utteranceID,
		SpaceId:         spaceID,
		ParticipantId:   participantID,
		ParticipantType: "human",
		Text:            "presence-driven cross-replica probe",
	}); err != nil {
		t.Fatalf("send utterance: %v", err)
	}

	seen := make([]int, len(conns))
	deadline := time.After(10 * time.Second)
	for collecting := true; collecting; {
		select {
		case <-deadline:
			collecting = false
		default:
			drained := false
			for i, ch := range chans {
				select {
				case uid := <-ch:
					if uid == utteranceID {
						seen[i]++
						drained = true
					}
				default:
				}
			}
			if !drained {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}

	var missed []int
	for i := range conns {
		if seen[i] != 1 {
			missed = append(missed, i)
		}
	}
	t.Logf("presence-driven: %d/%d subscribers observed the utterance exactly once", len(conns)-len(missed), len(conns))
	if len(missed) > 0 {
		t.Fatalf("STILL RED with presence established: missed %v (counts %v) -- deeper consume bug", missed, seen)
	}
	t.Logf("GREEN with presence -- the substrate delivers cross-replica for realistic (presence-establishing) clients")
}

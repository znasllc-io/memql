//go:build clustere2e

package clustere2e

// Coverage for the ORIGINAL (chat-reply topic) consumer-subscription trigger:
// cross-replica delivery when every subscriber writes a v1:cognition:presence
// row first. Presence is a chat-reply topic, so the write triggers
// ChatReplyDelivery's substrate subscription on the subscriber's replica --
// the pre-#1316 lazy trigger. The main gate (delivery_test.go) covers the
// #1316 space-interest trigger (join/create-session, what real clients write
// on space open); this probe keeps the chat-reply-write trigger covered too.

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
	// that replica. PresenceId/SinceAt/LastUpdatedAt are passed explicitly (like
	// the SPA does) -- also required until #1319: omitting the leading optional
	// arg trips the generated-builder dangling-comma bug and the mutation never
	// reaches the engine.
	now := time.Now().UTC().Format(time.RFC3339)
	for i, c := range conns {
		qc := memqlclient.NewQueryClient(c.Dispatcher())
		if _, err := qc.MutationUpdateParticipantPresence(ctx, memqlclient.MutationUpdateParticipantPresenceArgs{
			PresenceId:    "presence-" + participantID,
			SpaceId:       spaceID,
			ParticipantId: participantID,
			State:         "idle",
			Label:         "probe",
			SinceAt:       now,
			LastUpdatedAt: now,
		}); err != nil {
			t.Fatalf("presence upsert on connection %d failed: %v", i, err)
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

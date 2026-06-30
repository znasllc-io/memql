//go:build clustere2e

package clustere2e

// Coverage for streamed-reply text chunks as a chat-reply topic (memql#1326):
// a v1:cognition:text:chunk row produced on ONE connection's replica must
// reach a subscriber anchored on EVERY bff replica, exactly once. Before
// #1326 chunks were not chat-reply topics, rode only the star mesh, and never
// reached the bff replica the producer had no dial to -- streaming was dead
// and the reply popped in atomically with the committed utterance. The
// streamed-turn gate (streamed_turn_test.go) deliberately encodes its
// synthetic chunks as UTTERANCE rows, so it cannot catch a topic-gate gap on
// the real chunk concept; this probe drives the real one, exactly like
// delivery_presence_probe_test.go does for presence.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// textChunkIDFor returns the chunk row id if the event is the creation of a
// v1:cognition:text:chunk row, else "". Tolerates flat or node-nested payloads
// (mirrors utteranceIDFor).
func textChunkIDFor(ev memqlclient.Event) string {
	concept, _ := ev.Payload["concept"].(string)
	node, _ := ev.Payload["node"].(map[string]any)
	if concept == "" && node != nil {
		concept, _ = node["concept"].(string)
	}
	if concept != "v1:cognition:text:chunk" {
		return ""
	}
	if cid, _ := ev.Payload["id"].(string); cid != "" {
		return cid
	}
	if node != nil {
		cid, _ := node["id"].(string)
		return cid
	}
	return ""
}

func subscribeTextChunks(ctx context.Context, t *testing.T, conn *memqlclient.Connection) <-chan string {
	t.Helper()
	sm := memqlclient.NewSubscriptionManager(conn.Dispatcher())
	// Broad subscribe, narrow assert -- same rationale as subscribeUtterances.
	_, events, err := sm.Subscribe(ctx, memqlclient.SubscriptionKindGraphEvents, "node.created.#")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	out := make(chan string, 64)
	go func() {
		for ev := range events {
			if cid := textChunkIDFor(ev); cid != "" {
				out <- cid
			}
		}
	}()
	return out
}

func TestTextChunkCrossReplicaDelivery(t *testing.T) {
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

	// Establish presence from every connection so each replica's
	// ChatReplyDelivery subscribes the space (the chat-reply-write trigger,
	// same setup as the presence probe).
	now := time.Now().UTC().Format(time.RFC3339)
	for i, c := range conns {
		qc := memqlclient.NewQueryClient(c.Dispatcher())
		if _, err := qc.UpdateParticipantPresence(ctx, memqlclient.UpdateParticipantPresenceArgs{
			PresenceId:    "presence-" + id.NewShortId(),
			PartitionId:   spaceID,
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
		chans[i] = subscribeTextChunks(ctx, t, c)
	}
	// Generous settle: presence event -> ensureSubscribed -> durable subscription live.
	time.Sleep(3 * time.Second)

	// Produce one streamed chunk the way cognition's emitTextChunk
	// does (colon-free chunkId; replyId = the eventual committed utterance id).
	chunkShort := fmt.Sprintf("%s-0", id.NewShortId())
	wantChunkID := "v1:cognition:text:chunk:text-chunk-" + chunkShort
	qc := memqlclient.NewQueryClient(producer.Dispatcher())
	if _, err := qc.EmitTextChunk(ctx, memqlclient.EmitTextChunkArgs{
		ChunkId:       chunkShort,
		PartitionId:   spaceID,
		ParticipantId: participantID,
		ReplyId:       "v1:cognition:utterance:" + id.NewShortId(),
		Text:          "streamed-chunk cross-replica probe",
		Index:         0,
		Done:          false,
	}); err != nil {
		t.Fatalf("emit text chunk: %v", err)
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
				case cid := <-ch:
					if cid == wantChunkID {
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
	t.Logf("text-chunk: %d/%d subscribers observed the chunk exactly once", len(conns)-len(missed), len(conns))
	if len(missed) > 0 {
		t.Fatalf("streamed chunk did not reach every replica exactly once: missed %v (counts %v)", missed, seen)
	}
	t.Logf("GREEN -- streamed text chunks deliver cross-replica via the chat-reply substrate")
}

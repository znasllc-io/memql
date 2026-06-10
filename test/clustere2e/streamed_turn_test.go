//go:build clustere2e

// streamed_turn_test.go extends the Phase-0 cluster-parity gate
// (memql#1259) with the #1266 cross-process streaming verification: a
// token-streamed turn -- an ORDERED sequence of chunks belonging to one
// logical turn -- must be delivered to a subscriber anchored on EVERY bff
// replica, with no lost, reordered, or duplicated chunk, INCLUDING when the
// turn is produced across a mid-stream replica switch.
//
// WHY A SYNTHETIC CHUNK STREAM (same rationale as delivery_test.go)
// ----------------------------------------------------------------
// The live token stream is an LLM reply delta (component/memql CallStream),
// which needs SI provider keys and is non-deterministic. The gate instead
// drives the SAME substrate streaming path with synthetic, ordered chunks:
// each chunk is a v1:cognition:utterance row whose id ENCODES its sequence
// (`...:<runID>-NNNN`), so ordering is observable from the graph event alone
// via utteranceIDFor (reused from delivery_test.go) -- no LLM, no provider
// keys, fully deterministic. The streamed chunks traverse the exact
// component/node delivery substrate (#1263/#1264) + ordered streaming
// contract (#1266) that the live token stream rides, so a reorder/drop/dup
// in that path fails here the same way it would in a live chat turn.
//
// WHAT IT ASSERTS (the #1266 acceptance: "cluster test covers a streamed turn")
//  1. EXACTLY-ONCE per chunk on EVERY connection -- no lost, no duplicated
//     chunk (the delivery invariant, per chunk across the whole turn).
//  2. PER-TURN ORDERING on every connection -- the sequence each subscriber
//     observes is monotonically increasing with no gaps; the substrate's
//     per-key ordering guarantee holds cross-replica.
//  3. MID-STREAM REPLICA SWITCH -- the front half of the turn is produced
//     from one connection and the back half from a SECOND producer
//     connection (round-robined by nginx onto, with high probability, the
//     other bff replica). The single logical turn is thus produced across
//     two replicas yet must still arrive complete + ordered at every
//     subscriber. This is the cross-process streaming case #1266 added.
//
// Build-tagged `clustere2e` (like delivery_test.go) so `go test ./...` skips
// it; it runs only under the parity gate (`make cluster-e2e`).
//
// RUN
//
//	MEMQL_E2E_TOKEN=<user JWT> go test -tags clustere2e -count=1 \
//	  -timeout=300s -run TestClusterStreamedTurn ./test/clustere2e/...
package clustere2e

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/id"
	memqlclient "github.com/znasllc-io/memql/sdk/go/client"
)

// streamChunkCount is how many ordered chunks make up the synthetic turn.
// Enough that an off-by-one reorder or a single dropped chunk is caught, and
// that the front/back split straddles the mid-stream producer switch.
const streamChunkCount = 16

// chunkIDFmt encodes a chunk's sequence into its node id so ordering is
// recoverable from the graph event alone. runID keys the turn (distinct per
// test run); the zero-padded suffix is the sequence. Padding keeps the ids
// lexically sortable too, but we parse the integer to be exact.
func chunkID(runID string, seq int) string {
	return fmt.Sprintf("v1:cognition:utterance:%s-%04d", runID, seq)
}

// seqFromChunkID recovers the sequence from a chunk id minted by chunkID for
// THIS run (runID match), or -1 if the id isn't one of our chunks. It tolerates
// ids carrying the full `v1:cognition:utterance:` prefix or just the
// `<runID>-NNNN` tail.
func seqFromChunkID(runID, uid string) int {
	marker := runID + "-"
	i := strings.LastIndex(uid, marker)
	if i < 0 {
		return -1
	}
	tail := uid[i+len(marker):]
	n, err := strconv.Atoi(tail)
	if err != nil {
		return -1
	}
	return n
}

// TestClusterStreamedTurn is the #1266 streamed-turn parity gate. It must be
// GREEN on the substrate (ordered streaming, #1266 over #1263/#1264) and would
// go RED on the pre-fix mesh -- cross-replica subscribers would miss the
// chunks produced on the other replica, and a non-substrate fast-path could
// reorder them.
func TestClusterStreamedTurn(t *testing.T) {
	tok := token(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// connCount subscribers, round-robined by nginx across both bff replicas,
	// PLUS a second producer connection for the mid-stream replica switch.
	conns := openConnections(ctx, t, tok, connCount+1)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	// conns[0] produces the front half; conns[len-1] produces the back half
	// (the mid-stream replica switch). Both also subscribe -- a producer must
	// see the whole turn including the half it did NOT produce.
	producerA := conns[0]
	producerB := conns[len(conns)-1]

	spaceID, participantID := newSpaceWithHuman(ctx, t, producerA, userIDFromToken(t, tok))
	t.Logf("opened %d connections (%d subscribers + 2 producers, round-robined across bff replicas); space %s",
		len(conns), connCount-1, spaceID)

	// Every connection OPENS the space (the SPA's join/create-session
	// sequence -- the memql#1316 space-interest signal for its replica) and
	// subscribes before producing, then the subscriptions settle so we don't
	// race the first chunk.
	chans := make([]<-chan string, len(conns))
	for i, c := range conns {
		openSpaceOnConn(ctx, t, c, spaceID, participantID)
		chans[i] = subscribeUtterances(ctx, t, c, spaceID)
	}
	time.Sleep(1500 * time.Millisecond)

	runID := id.NewShortId()
	// Produce the turn as an ordered sequence of chunks. The front half is
	// sent from producerA, the back half from producerB -- a mid-stream
	// switch to (with high probability) the other bff replica. Sent
	// strictly in sequence so the substrate's per-key ordering is what's
	// under test, not our send order.
	switchAt := streamChunkCount / 2
	qcA := memqlclient.NewQueryClient(producerA.Dispatcher())
	qcB := memqlclient.NewQueryClient(producerB.Dispatcher())
	for seq := 0; seq < streamChunkCount; seq++ {
		qc := qcA
		who := "A"
		if seq >= switchAt {
			qc = qcB
			who = "B(mid-stream switch)"
		}
		cid := chunkID(runID, seq)
		if _, err := qc.MutationSendTextUtterance(ctx, memqlclient.MutationSendTextUtteranceArgs{
			UtteranceId:     cid,
			SpaceId:         spaceID,
			ParticipantId:   participantID,
			ParticipantType: "human",
			Text:            fmt.Sprintf("clustere2e streamed-turn chunk seq=%d", seq),
		}); err != nil {
			t.Fatalf("send chunk seq=%d from producer %s: %v", seq, who, err)
		}
	}
	t.Logf("produced a %d-chunk streamed turn (run %s); front %d from producer A, back %d from producer B (mid-stream replica switch)",
		streamChunkCount, runID, switchAt, streamChunkCount-switchAt)

	// Collect for a generous window: record, PER CONNECTION, the ordered
	// arrival sequence of THIS run's chunks (so we can assert ordering, not
	// just set membership).
	arrivals := make([][]int, len(conns))
	deadline := time.After(20 * time.Second)
	for collecting := true; collecting; {
		select {
		case <-deadline:
			collecting = false
		default:
			progressed := false
			for i, ch := range chans {
				select {
				case uid := <-ch:
					if seq := seqFromChunkID(runID, uid); seq >= 0 {
						arrivals[i] = append(arrivals[i], seq)
						progressed = true
					}
				default:
				}
			}
			// Fast exit once every connection has all chunks.
			if allComplete(arrivals) {
				collecting = false
			}
			if !progressed && collecting {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}

	// Sanity: producer A's OWN connection must observe the full turn --
	// otherwise this is a subscription/authz fault, not a streaming-delivery
	// failure.
	if got := dedupComplete(arrivals[0]); !got {
		t.Fatalf("producer A connection did not observe its own complete streamed turn (saw seqs %v of %d) -- "+
			"subscription/authz setup problem, not the cross-replica streaming bug", arrivals[0], streamChunkCount)
	}

	// Assert exactly-once + in-order on EVERY connection.
	var faults []string
	for i := range conns {
		if msg := checkStream(arrivals[i]); msg != "" {
			faults = append(faults, fmt.Sprintf("conn %d: %s (arrival order %v)", i, msg, arrivals[i]))
		}
	}
	if len(faults) > 0 {
		t.Fatalf("streamed-turn delivery FAILED on %d/%d connections (run %s, %d chunks, mid-stream switch at %d):\n  %s\n"+
			"Every subscriber -- on EITHER bff replica -- must observe ALL %d chunks of the turn exactly once and in order, "+
			"even though the back half was produced from a different replica. A miss/dup is the memql#1259 cross-replica drop; "+
			"an out-of-order or gapped sequence is a memql#1266 streaming-ordering regression.",
			len(faults), len(conns), runID, streamChunkCount, switchAt, strings.Join(faults, "\n  "), streamChunkCount)
	}
	t.Logf("streamed turn delivered exactly-once and in-order on all %d connections (incl. the mid-stream replica switch)", len(conns))
}

// allComplete reports whether every connection has observed at least
// streamChunkCount chunk arrivals (the fast-exit signal; exact-once/order is
// asserted separately so a dup wouldn't falsely satisfy this).
func allComplete(arrivals [][]int) bool {
	for _, a := range arrivals {
		if len(a) < streamChunkCount {
			return false
		}
	}
	return true
}

// dedupComplete reports whether the arrival slice contains every sequence
// 0..streamChunkCount-1 at least once (used only for the producer-A sanity
// gate, which tolerates dup/order -- those are asserted precisely elsewhere).
func dedupComplete(arr []int) bool {
	seen := make(map[int]bool, len(arr))
	for _, s := range arr {
		seen[s] = true
	}
	for s := 0; s < streamChunkCount; s++ {
		if !seen[s] {
			return false
		}
	}
	return true
}

// checkStream returns "" if arr is exactly the sequence 0..streamChunkCount-1,
// each once, in strictly increasing arrival order; otherwise a human-readable
// description of the first fault (missing, duplicated, or reordered).
func checkStream(arr []int) string {
	counts := make(map[int]int, len(arr))
	for _, s := range arr {
		counts[s]++
	}
	var missing, duped []int
	for s := 0; s < streamChunkCount; s++ {
		switch counts[s] {
		case 1:
		case 0:
			missing = append(missing, s)
		default:
			duped = append(duped, s)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("LOST chunk(s) %v of %d", missing, streamChunkCount)
	}
	if len(duped) > 0 {
		return fmt.Sprintf("DUPLICATED chunk(s) %v", duped)
	}
	// Exactly-once holds; now ordering: arrival order must be strictly
	// increasing (== sorted), i.e. no reorder.
	if !sort.IntsAreSorted(arr) {
		want := make([]int, len(arr))
		copy(want, arr)
		sort.Ints(want)
		return fmt.Sprintf("REORDERED -- arrived %v, expected ascending %v", arr, want)
	}
	return ""
}

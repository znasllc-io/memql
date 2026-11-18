package clustere2e

// calls_schedule_hop_db_test.go -- a booked call's room is opened once when its first
// joins land on different replicas at once.
//
// # WHAT IS AT STAKE
//
// A booked call has no room until somebody joins it (integrations/calls schedule.go). A
// first join finds no room by its link, finds the booking, and opens one: places it on a
// media server, seats the joiner, marks the booking started and writes the room. Two
// replicas doing that at the same moment would each open a room, and the call's people
// would be split between two calls that each believe they are the one booked.
//
// # WHAT THIS PROVES
//
// Two engines over one Postgres, each with its own pool, cache, gate pool and calls
// integration (newCallsHopWorld). A call is booked to start within the window in which
// it opens; then ten people join it at once through both replicas, half by the guest
// link and half by the host link, none with a session. Every join is answered -- a guest
// waits behind the booked call's waiting room, a host is let in -- and none fails: there
// is exactly one room for the booking, the booking names it, and every seat is in it.
//
// # HOW TO CONFIRM IT IS LOAD-BEARING
//
// Replace the advisory gates the opening join takes (schedule.go openGated) with a mutex
// held by the Service, a single node's lock: the replicas' first joins race (measured: 3
// runs of 3 failed). The join that marks the booking second is refused by its version
// fence, and one run left a SECOND ROOM: the losing replica read the booking after the
// winner marked it started but before the winner wrote its room, read it as never opened
// (bookedRoom), and opened its own. Keep the gates but decide from the booking read
// before them, and every join after the first is refused by the fence, the room holding
// one seat (measured: 3 runs of 3 failed, nine of ten joins each).

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

// joinBodiesAtOnce makes every join at the same moment, alternating replicas.
func (w *callsHopWorld) joinBodiesAtOnce(bodies []callrules.JoinBody) ([]callrules.JoinResult, []callrules.Session, []error) {
	results := make([]callrules.JoinResult, len(bodies))
	sessions := make([]callrules.Session, len(bodies))
	errs := make([]error, len(bodies))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, body := range bodies {
		replica := w.a
		if i%2 == 1 {
			replica = w.b
		}
		wg.Add(1)
		go func(i int, replica *callsReplica, body callrules.JoinBody) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			results[i], sessions[i], errs[i] = replica.in.Service().Join(ctx, w.ref, nil, body)
		}(i, replica, body)
	}
	close(start)
	wg.Wait()
	return results, sessions, errs
}

// latestOf is the newest version of every row of a concept on this world's site,
// straight off the table, by bare id.
func (w *callsHopWorld) latestOf(concept string) map[string]map[string]any {
	w.t.Helper()
	var rows []struct {
		ID      string          `bun:"id"`
		Payload json.RawMessage `bun:"payload"`
	}
	err := w.a.db.NewRaw(`SELECT DISTINCT ON (id) id, payload FROM "MemoryNodes"
		WHERE concept = ? AND payload->>'siteId' = ? ORDER BY id, "createdAt" DESC`,
		concept, "v1:platform:site:"+w.site).Scan(context.Background(), &rows)
	if err != nil {
		w.t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, row := range rows {
		var payload map[string]any
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			w.t.Fatal(err)
		}
		out[memql.BareShortId(row.ID)] = payload
	}
	return out
}

func bareOf(v any) string { return memql.BareShortId(fmt.Sprint(v)) }

// TestFirstJoinsAcrossReplicasOpenABookedCallOnce: ten first joins of one booked call at
// once, through two replicas, open exactly one room, which the booking names and every
// seat is in.
func TestFirstJoinsAcrossReplicasOpenABookedCallOnce(t *testing.T) {
	w := newCallsHopWorld(t, 50)
	if w == nil {
		return
	}
	sc, err := w.a.in.Service().Schedule(context.Background(), w.hostSession(), callrules.ScheduleRequest{
		Start: time.Now().Add(5 * time.Minute), Minutes: 30, Title: "Opened once",
	})
	if err != nil {
		t.Fatalf("book the call on replica A: %v", err)
	}
	bodies := make([]callrules.JoinBody, 10)
	for i := range bodies {
		bodies[i] = callrules.JoinBody{Token: sc.GuestToken, Name: fmt.Sprintf("Guest %d", i), Confirm: hopConfirmed}
		if i%4 >= 2 {
			// Guest and host links alike, on both replicas: joins 2, 3, 6, 7 hold the host link.
			bodies[i] = callrules.JoinBody{HostToken: sc.HostToken, Name: fmt.Sprintf("Host %d", i), Confirm: hopConfirmed}
		}
	}
	results, sessions, errs := w.joinBodiesAtOnce(bodies)

	for i, res := range results {
		host := bodies[i].HostToken != ""
		switch {
		case errs[i] != nil:
			t.Errorf("join %d (host link %t) failed: %+v, %v", i, host, res, errs[i])
		case host && res.Outcome != callrules.OutcomeAdmit:
			t.Errorf("join %d by the host link answered %+v, want admitted", i, res)
		case !host && (res.Outcome != callrules.OutcomeWait || res.Reason != "locked"):
			t.Errorf("join %d by the guest link answered %+v, want waiting behind the lock", i, res)
		}
	}
	rooms := map[string]map[string]any{}
	for roomID, room := range w.latestOf("v1:calls:room") {
		if bareOf(room["scheduledCallId"]) == sc.ID {
			rooms[roomID] = room
		}
	}
	if len(rooms) != 1 {
		t.Fatalf("the booking opened %d rooms, want exactly one", len(rooms))
	}
	var roomID string
	for id := range rooms {
		roomID = id
	}
	booking := w.latestOf("v1:calls:scheduledCall")[sc.ID]
	if booking["status"] != "started" || bareOf(booking["roomId"]) != roomID {
		t.Fatalf("the booking reads %v / %v, want started with room %s", booking["status"], booking["roomId"], roomID)
	}
	seated := 0
	for _, seat := range w.latestOf("v1:calls:seat") {
		if bareOf(seat["roomId"]) == roomID {
			seated++
		}
	}
	if seated != len(bodies) {
		t.Fatalf("the booking's room holds %d seats, want all %d joins", seated, len(bodies))
	}
	for i, sess := range sessions {
		if errs[i] == nil && sess.RoomID != roomID {
			t.Errorf("join %d was seated in room %q, not the booking's %s", i, sess.RoomID, roomID)
		}
	}
}

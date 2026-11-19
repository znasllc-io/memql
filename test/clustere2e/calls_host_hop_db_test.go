package clustere2e

// calls_host_hop_db_test.go -- a host ends a call through one replica while people join
// it through the other, and the end holds on both.
//
// # WHAT IS AT STAKE
//
// Ending a call reads the call's seats and leaves every one of them, ends its queue
// entries and its booking, and ends the room. A join reads the same room and its
// server's load and writes a seat. If the two run at once on two replicas, each under a
// lock only its own replica honours, a join can read the room open after the end read
// the seats and before it ended the room: the joiner is admitted into a call that has
// ended, holding a seat the end never saw, and the media server counts them in a room
// nobody is in. Or the two meet on a row's version and one of them fails outright.
//
// # WHAT THIS PROVES
//
// Two engines over one Postgres, each with its own pool, cache, gate pool and calls
// integration (calls_hop_db_test.go's world). Six calls are started on replica A, each
// by its own key holder, whose sessions replica A made. Then, for every call at once,
// its host ends it through replica B -- which has never seen that session -- while three
// people join it through replica A. Every end succeeds; every join is either admitted
// before the end or refused because the call has ended; and afterwards every room is
// ended by its host with no seat left in it.
//
// # HOW TO CONFIRM IT IS LOAD-BEARING
//
// Replace the advisory gates in integrations/calls service.go (Service.gates) with a
// mutex held by the Service, a single node's lock: the end and the joins race across
// the replicas, and the test fails (measured; see the task report).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// latestRoom is a room's newest stored version, straight off the table.
func (w *callsHopWorld) latestRoom(roomID string) map[string]any {
	w.t.Helper()
	// A slice, as admittedIn reads: bun scans one destination per row.
	var payloads []json.RawMessage
	err := w.a.db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE concept = 'v1:calls:room' AND id = ? ORDER BY "createdAt" DESC LIMIT 1`,
		"v1:calls:room:"+roomID).Scan(context.Background(), &payloads)
	if err != nil {
		w.t.Fatal(err)
	}
	if len(payloads) != 1 {
		w.t.Fatalf("the room %s is not stored", roomID)
	}
	var room map[string]any
	if err := json.Unmarshal(payloads[0], &room); err != nil {
		w.t.Fatal(err)
	}
	return room
}

// seatStatuses is the newest status of every seat of one room, by seat id.
func (w *callsHopWorld) seatStatuses(roomID string) map[string]string {
	w.t.Helper()
	var rows []struct {
		ID      string          `bun:"id"`
		Payload json.RawMessage `bun:"payload"`
	}
	err := w.a.db.NewRaw(`SELECT id, payload FROM (SELECT DISTINCT ON (id) id, payload FROM "MemoryNodes"
		WHERE concept = 'v1:calls:seat' AND payload->>'roomId' = ? ORDER BY id, "createdAt" DESC) latest`,
		"v1:calls:room:"+roomID).Scan(context.Background(), &rows)
	if err != nil {
		w.t.Fatal(err)
	}
	out := map[string]string{}
	for _, row := range rows {
		var seat map[string]any
		if err := json.Unmarshal(row.Payload, &seat); err != nil {
			w.t.Fatal(err)
		}
		status, _ := seat["status"].(string)
		out[row.ID] = status
	}
	return out
}

// TestEndingACallOnOneReplicaShutsOutJoinsOnTheOther: six calls are each ended by their
// host through replica B while three people join each through replica A, all at once.
// Every end succeeds, every join is admitted before the end or refused as ended, and no
// call is left with anybody in it or waiting for it.
func TestEndingACallOnOneReplicaShutsOutJoinsOnTheOther(t *testing.T) {
	w := newCallsHopWorld(t, 50)
	if w == nil {
		return
	}
	const calls, joinsPerCall = 6, 3
	type started struct {
		host callrules.Session
		call callrules.StartResult
	}
	all := make([]started, calls)
	for i := range all {
		sess := w.hostSession()
		res, err := w.a.in.Service().Start(context.Background(), sess, callrules.StartBody{Name: "Host", Confirm: hopConfirmed})
		if err != nil || res.Join.Outcome != callrules.OutcomeAdmit {
			t.Fatalf("start call %d on replica A: %+v, %v", i, res.Join, err)
		}
		all[i] = started{host: sess, call: res}
	}

	type joined struct {
		res callrules.JoinResult
		err error
	}
	ends := make([]error, calls)
	joins := make([][]joined, calls)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range all {
		joins[i] = make([]joined, joinsPerCall)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ends[i] = w.b.in.Service().Host(ctx, all[i].host, callrules.HostActionRequest{Action: callrules.HostEnd})
		}(i)
		for j := 0; j < joinsPerCall; j++ {
			wg.Add(1)
			go func(i, j int) {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				res, _, err := w.a.in.Service().Join(ctx, w.ref, nil, callrules.JoinBody{
					Token: all[i].call.Call.GuestToken, Name: fmt.Sprintf("Guest %d.%d", i, j), Confirm: hopConfirmed,
				})
				joins[i][j] = joined{res: res, err: err}
			}(i, j)
		}
	}
	close(start)
	wg.Wait()

	for i, call := range all {
		if ends[i] != nil {
			t.Errorf("the host of call %d could not end it through replica B: %v", i, ends[i])
		}
		for j, got := range joins[i] {
			admitted := got.err == nil && got.res.Outcome == callrules.OutcomeAdmit
			refused := errors.Is(got.err, callrules.ErrEnded) && got.res.Reason == callrules.ReasonCallEnded
			if !admitted && !refused {
				t.Errorf("join %d of call %d answered %+v, %v: a join is admitted before the end or refused as ended", j, i, got.res, got.err)
			}
		}
		room := w.latestRoom(call.call.Call.ID)
		if room["status"] != "ended" || room["endReason"] != callrules.EndReasonEnded {
			t.Errorf("call %d stands %v (%v), want ended by its host", i, room["status"], room["endReason"])
		}
		for seatID, status := range w.seatStatuses(call.call.Call.ID) {
			if status != "left" {
				t.Errorf("seat %s of ended call %d is %q: the end did not see it", seatID, i, status)
			}
		}
	}
}

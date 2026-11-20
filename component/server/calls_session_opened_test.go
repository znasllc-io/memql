package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/calls"
)

func TestCallsSessionReturnsTheServicesRoomStartForHostsAndGuests(t *testing.T) {
	rig := newCallsRig(t)
	opened := time.Date(2026, 11, 18, 4, 53, 21, 123456789, time.FixedZone("offset", -7*60*60))
	rig.svc.sessions[rig.secrets.guestSession] = calls.Session{
		Kind: calls.SessionGuest, SeatID: "seat2", RoomID: callsTestRoomID,
		ExpiresAt: callsT0.Add(calls.SessionLifetime), Site: calls.SiteRef{SiteID: callsTestSite},
	}
	rig.svc.rosters[rig.secrets.hostSession] = &calls.Roster{}
	for _, token := range []string{rig.secrets.hostSession, rig.secrets.guestSession} {
		rig.svc.openedAt[token] = opened
		for range 2 {
			rec := rig.get("/calls/session", token)
			var body map[string]json.RawMessage
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
				t.Fatalf("session response: %d %s", rec.Code, rec.Body.String())
			}
			var got string
			if err := json.Unmarshal(body["openedAt"], &got); err != nil || got != opened.UTC().Format(time.RFC3339Nano) {
				t.Fatalf("openedAt = %q, %v; want the persisted UTC instant", got, err)
			}
		}
		// The service withholds it for a waiting/left/ended seat. The handler
		// must omit it, not infer a start from session expiry or a room id.
		delete(rig.svc.openedAt, token)
		rec := rig.get("/calls/session", token)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if _, present := body["openedAt"]; present {
			t.Fatalf("absent room start was invented: %s", rec.Body.String())
		}
	}
}

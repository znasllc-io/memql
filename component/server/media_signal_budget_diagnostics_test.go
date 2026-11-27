package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/livekit"
	"github.com/znasllc-io/memql/component/calls"
	"google.golang.org/protobuf/proto"
)

func TestMediaSignalBudgetDiagnosticBranchesPreserveSpending(t *testing.T) {
	now := time.Unix(100, 0)
	b := newMediaSignalBudget(now)
	if got := b.controlReason(now, 126); got != "budget_control_size" || b.controls.amount != 16 {
		t.Fatal("oversized control spent budget", got)
	}
	b.controls.amount = 0
	if got := b.controlReason(now, 1); got != "budget_control_rate" {
		t.Fatal(got)
	}
	b.messages.amount = 0
	if got := b.frameReason(now, 1); got != "budget_frame_rate" || b.bytes.amount != mediaSignalByteBurst {
		t.Fatal("message rejection spent byte budget", got)
	}
	b.messages.amount = 2
	b.bytes.amount = 0
	if got := b.frameReason(now, 1); got != "budget_frame_bytes" || b.messages.amount != 1 {
		t.Fatal("byte rejection changed message spending", got)
	}
	b = newMediaSignalBudget(now)
	if got := b.requestReason(now, budgetAdd(nil)); got != "budget_add_track_shape" || b.additions.amount != 8 {
		t.Fatal("shape rejection spent add budget", got)
	}
	first := budgetTrack("PRIVATE_FIRST", livekit.TrackSource_CAMERA)
	if got := b.requestReason(now, budgetAdd(first)); got != "" || b.additions.amount != 7 || len(b.tracks) != 1 {
		t.Fatal("valid add was not reserved exactly once", got)
	}
	if got := b.requestReason(now, budgetAdd(budgetTrack("PRIVATE_SECOND", livekit.TrackSource_CAMERA))); got != "budget_add_track_source" || b.additions.amount != 6 || len(b.tracks) != 1 {
		t.Fatal("duplicate source changed reservation/spending", got)
	}
	if got := b.requestReason(now, budgetAdd(budgetTrack(first.Cid, livekit.TrackSource_MICROPHONE))); got != "budget_add_track_cid_conflict" {
		t.Fatal(got)
	}
	unknown := budgetTrack("PRIVATE_CODEC", livekit.TrackSource_CAMERA)
	unknown.Sid = "PRIVATE_SID"
	if got := b.requestReason(now, budgetAdd(unknown)); got != "budget_add_track_sid_unknown" {
		t.Fatal(got)
	}
	b.additions.amount = 0
	if got := b.requestReason(now, budgetAdd(first)); got != "budget_add_track_rate" {
		t.Fatal(got)
	}
	b = newMediaSignalBudget(now)
	for _, source := range []livekit.TrackSource{livekit.TrackSource_CAMERA, livekit.TrackSource_MICROPHONE, livekit.TrackSource_SCREEN_SHARE, livekit.TrackSource_SCREEN_SHARE_AUDIO} {
		if !b.request(now, budgetAdd(budgetTrack(source.String(), source))) {
			t.Fatal("valid source refused")
		}
	}
	if got := b.requestReason(now, budgetAdd(first)); got != "budget_add_track_count" {
		t.Fatal(got)
	}
}

func TestMediaSignalBudgetDiagnosticProtocolKinds(t *testing.T) {
	now := time.Unix(100, 0)
	cases := []struct {
		reason  string
		request *livekit.SignalRequest
	}{
		{"budget_offer_shape", &livekit.SignalRequest{Message: &livekit.SignalRequest_Offer{}}},
		{"budget_answer_shape", &livekit.SignalRequest{Message: &livekit.SignalRequest_Answer{}}},
		{"budget_sync_state_shape", &livekit.SignalRequest{Message: &livekit.SignalRequest_SyncState{}}},
		{"budget_update_layers_shape", &livekit.SignalRequest{Message: &livekit.SignalRequest_UpdateLayers{}}},
		{"budget_trickle_shape", &livekit.SignalRequest{Message: &livekit.SignalRequest_Trickle{}}},
		{"budget_request_unknown", &livekit.SignalRequest{}},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			b := newMediaSignalBudget(now)
			if got := b.requestReason(now, tc.request); got != tc.reason {
				t.Fatal(got)
			}
		})
	}
	b := newMediaSignalBudget(now)
	offer := budgetSDP(0, 1, 0)
	offer.Id = 1
	if got := b.requestReason(now, &livekit.SignalRequest{Message: &livekit.SignalRequest_Offer{Offer: offer}}); got != "" {
		t.Fatal(got)
	}
	next := proto.Clone(offer).(*livekit.SessionDescription)
	next.Id = 2
	if got := b.requestReason(now, &livekit.SignalRequest{Message: &livekit.SignalRequest_Offer{Offer: next}}); got != "budget_offer_pending" || b.offer.Id != 1 {
		t.Fatal("pending offer changed", got)
	}
}

func TestMediaSignalBudgetDiagnosticReachesObserverWithoutPayload(t *testing.T) {
	kind, join := signalJoin(t, false)
	providerSaw := make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer c.Close()
		if c.WriteMessage(kind, join) != nil {
			return
		}
		if _, _, e = c.ReadMessage(); e != nil {
			return
		}
		providerSaw <- struct{}{}
		_, _, _ = c.ReadMessage()
	}))
	defer provider.Close()
	a := signalAdmission(provider.URL)
	observed := make(chan string, 1)
	a.Observe = func(phase string, _ time.Duration, e error) {
		if phase == "client_budget" {
			observed <- calls.ReasonOf(e)
			if strings.Contains(e.Error(), "PRIVATE") {
				t.Error("request value entered diagnostic")
			}
		}
	}
	h := httptest.NewServer(NewMediaSignalHandler(signalServiceFunc(func(context.Context, string) (*calls.MediaSignalAdmission, error) { return a, nil })))
	defer h.Close()
	c, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/media/rtc?access_token=issued", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, e = c.ReadMessage(); e != nil {
		t.Fatal(e)
	}
	first, _ := proto.Marshal(budgetAdd(budgetTrack("PRIVATE_FIRST", livekit.TrackSource_CAMERA)))
	if e = c.WriteMessage(websocket.BinaryMessage, first); e != nil {
		t.Fatal(e)
	}
	select {
	case <-providerSaw:
	case <-time.After(time.Second):
		t.Fatal("first add not forwarded")
	}
	second, _ := proto.Marshal(budgetAdd(budgetTrack("PRIVATE_SECOND", livekit.TrackSource_CAMERA)))
	if e = c.WriteMessage(websocket.BinaryMessage, second); e != nil {
		t.Fatal(e)
	}
	_, _, e = c.ReadMessage()
	if !websocket.IsCloseError(e, websocket.ClosePolicyViolation) {
		t.Fatal("budget close behavior changed", e)
	}
	select {
	case reason := <-observed:
		if reason != "budget_add_track_source" {
			t.Fatal(reason)
		}
	case <-time.After(time.Second):
		t.Fatal("missing closed diagnostic")
	}
}

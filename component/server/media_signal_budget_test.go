package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/livekit"
	"github.com/znasllc-io/memql/component/calls"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func budgetTrack(cid string, source livekit.TrackSource) *livekit.AddTrackRequest {
	kind := livekit.TrackType_VIDEO
	if source == livekit.TrackSource_MICROPHONE || source == livekit.TrackSource_SCREEN_SHARE_AUDIO {
		kind = livekit.TrackType_AUDIO
	}
	return &livekit.AddTrackRequest{Cid: cid, Source: source, Type: kind}
}
func budgetAdd(track *livekit.AddTrackRequest) *livekit.SignalRequest {
	return &livekit.SignalRequest{Message: &livekit.SignalRequest_AddTrack{AddTrack: track}}
}
func budgetSDP(audio, video, receive int) *livekit.SessionDescription {
	value := "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"
	for index := range audio + video + receive {
		kind, direction := "video", "sendonly"
		if index < audio {
			kind = "audio"
		}
		if index >= audio+video {
			direction = "recvonly"
		}
		value += fmt.Sprintf("m=%s 9 UDP/TLS/RTP/SAVPF 96\r\nc=IN IP4 0.0.0.0\r\na=mid:%d\r\na=%s\r\na=msid:stream track%d\r\n", kind, index, direction, index)
	}
	return &livekit.SessionDescription{Type: "offer", Sdp: value}
}
func TestMediaSignalBudgetBurstRecoveryAndClockRollback(t *testing.T) {
	now := time.Unix(100, 0)
	b := newMediaSignalBudget(now)
	for range mediaSignalMessageBurst {
		if !b.frame(now, 1) {
			t.Fatal("ordinary initial ICE burst refused")
		}
	}
	if b.frame(now, 1) || b.frame(now.Add(-time.Second), 1) {
		t.Fatal("message flood or backwards clock refilled budget")
	}
	for range mediaSignalMessageRate {
		if !b.frame(now.Add(time.Second), 1) {
			t.Fatal("sustained budget did not recover")
		}
	}
	if b.frame(now.Add(time.Second), 1) {
		t.Fatal("sustained rate exceeded")
	}
	b = newMediaSignalBudget(now)
	if !b.frame(now, mediaSignalByteBurst) || b.frame(now, 1) || !b.frame(now.Add(time.Second), mediaSignalByteRate) || b.frame(now.Add(time.Second), 1) {
		t.Fatal("byte envelope not enforced")
	}
	for range 16 {
		if !b.control(now, 125) {
			t.Fatal("bounded control burst refused")
		}
	}
	if b.control(now, 1) || b.control(now.Add(time.Second), 126) {
		t.Fatal("control flood accepted")
	}
}
func TestMediaSignalBudgetPublicationReservationUsesProviderAcknowledgment(t *testing.T) {
	now := time.Unix(100, 0)
	b := newMediaSignalBudget(now)
	first := budgetTrack("camera-first", livekit.TrackSource_CAMERA)
	if !b.request(now, budgetAdd(first)) || b.request(now, budgetAdd(budgetTrack("camera-second", livekit.TrackSource_CAMERA))) {
		t.Fatal("duplicate source reservation accepted")
	}
	ack := &livekit.SignalResponse{Message: &livekit.SignalResponse_TrackPublished{TrackPublished: &livekit.TrackPublishedResponse{Cid: first.Cid, Track: &livekit.TrackInfo{Sid: "provider-camera", Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_CAMERA}}}}
	if !b.provider(ack, "identity") {
		t.Fatal("provider track acknowledgment refused")
	}
	codec := budgetTrack("backup-codec", livekit.TrackSource_CAMERA)
	codec.Sid = "provider-camera"
	if !b.request(now, budgetAdd(codec)) {
		t.Fatal("bounded codec fallback changed logical track count")
	}
	ack.GetTrackPublished().Cid = codec.Cid
	if !b.provider(ack, "identity") {
		t.Fatal("secondary codec acknowledgment refused")
	}
	if !b.request(now, &livekit.SignalRequest{Message: &livekit.SignalRequest_SyncState{SyncState: &livekit.SyncState{}}}) || b.request(now, budgetAdd(budgetTrack("after-empty-sync", livekit.TrackSource_CAMERA))) {
		t.Fatal("client sync state released native provider reservation")
	}
	if !b.provider(&livekit.SignalResponse{Message: &livekit.SignalResponse_TrackUnpublished{TrackUnpublished: &livekit.TrackUnpublishedResponse{TrackSid: "provider-camera"}}}, "identity") || !b.request(now, budgetAdd(budgetTrack("after-unpublish", livekit.TrackSource_CAMERA))) {
		t.Fatal("confirmed unpublish did not permit explicit republish")
	}
	refused := &livekit.SignalResponse{Message: &livekit.SignalResponse_RequestResponse{RequestResponse: &livekit.RequestResponse{Reason: livekit.RequestResponse_NOT_ALLOWED, Request: &livekit.RequestResponse_AddTrack{AddTrack: budgetTrack("after-unpublish", livekit.TrackSource_CAMERA)}}}}
	if !b.provider(refused, "identity") || !b.request(now.Add(time.Second), budgetAdd(budgetTrack("after-permission-restored", livekit.TrackSource_CAMERA))) {
		t.Fatal("definitive provider refusal stranded a source reservation")
	}
}
func TestMediaSignalBudgetSDPSendingBoundsPreserveReceivingTracks(t *testing.T) {
	if !mediaSignalSDP(budgetSDP(2, 2, 100)) {
		t.Fatal("bounded publish plus large subscription inventory refused")
	}
	for _, value := range []*livekit.SessionDescription{budgetSDP(3, 0, 0), budgetSDP(0, 3, 0), budgetSDP(0, 0, 257), {Type: "offer", Sdp: "invalid"}} {
		if mediaSignalSDP(value) {
			t.Fatal("excess sending or malformed SDP accepted")
		}
	}
	value := budgetSDP(0, 1, 0)
	value.Sdp += "a=msid:other another-track\r\n"
	if mediaSignalSDP(value) {
		t.Fatal("multiple logical tracks hidden in one sending m-section")
	}
	value = budgetSDP(0, 1, 0)
	value.Sdp += "a=rid:a send\r\na=rid:b send\r\na=rid:c send\r\na=rid:d send\r\n"
	if mediaSignalSDP(value) {
		t.Fatal("excess simulcast encodings accepted")
	}
}
func TestMediaSignalBudgetJoinGzipAndHiddenPublicationBounds(t *testing.T) {
	now := time.Unix(100, 0)
	encode := func(join *livekit.JoinRequest, compressed bool) url.Values {
		raw, _ := proto.Marshal(join)
		compression := livekit.WrappedJoinRequest_NONE
		if compressed {
			var buf bytes.Buffer
			out := gzip.NewWriter(&buf)
			out.Write(raw)
			out.Close()
			raw = buf.Bytes()
			compression = livekit.WrappedJoinRequest_GZIP
		}
		wrapped, _ := proto.Marshal(&livekit.WrappedJoinRequest{JoinRequest: raw, Compression: compression})
		return url.Values{"join_request": {base64.URLEncoding.EncodeToString(wrapped)}}
	}
	for _, compressed := range []bool{false, true} {
		join := &livekit.JoinRequest{PublisherOffer: budgetSDP(1, 1, 0), AddTrackRequests: []*livekit.AddTrackRequest{budgetTrack("mic", livekit.TrackSource_MICROPHONE), budgetTrack("camera", livekit.TrackSource_CAMERA)}}
		if !newMediaSignalBudget(now).join(now, encode(join, compressed)) {
			t.Fatal("normal pinned protocol join refused")
		}
		join.AddTrackRequests = append(join.AddTrackRequests, budgetTrack("hidden-camera", livekit.TrackSource_CAMERA))
		if newMediaSignalBudget(now).join(now, encode(join, compressed)) {
			t.Fatal("initial join bypassed AddTrack reservation")
		}
	}
	if newMediaSignalBudget(now).join(now, encode(&livekit.JoinRequest{Metadata: strings.Repeat("x", mediaSignalJoinLimit+1)}, true)) {
		t.Fatal("compressed expansion exceeded decoded bound")
	}
	b := newMediaSignalBudget(now)
	for _, request := range []*livekit.SignalRequest{{Message: &livekit.SignalRequest_Simulate{Simulate: &livekit.SimulateScenario{}}}, {Message: &livekit.SignalRequest_UpdateMetadata{UpdateMetadata: &livekit.UpdateParticipantMetadata{Metadata: "unreviewed"}}}, {Message: &livekit.SignalRequest_PublishDataTrackRequest{PublishDataTrackRequest: &livekit.PublishDataTrackRequest{}}}} {
		if b.request(now, request) {
			t.Fatal("unpublished signalling service accepted")
		}
	}
}

func TestMediaSignalBudgetFloodClosesBeforeForwardingExcessBytes(t *testing.T) {
	kind, first := signalJoin(t, false)
	var frames atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.WriteMessage(kind, first)
		for {
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
			frames.Add(1)
		}
	}))
	defer provider.Close()
	closed := make(chan bool, 1)
	a := signalAdmission(provider.URL)
	a.Close = func(_ context.Context, definitive bool) error { closed <- definitive; return nil }
	h := httptest.NewServer(NewMediaSignalHandler(signalServiceFunc(func(context.Context, string) (*calls.MediaSignalAdmission, error) { return a, nil })))
	defer h.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/media/rtc?access_token=issued", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err = conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	payload, _ := proto.Marshal(&livekit.SignalRequest{Message: &livekit.SignalRequest_Ping{Ping: 1}})
	payload = protowire.AppendTag(payload, 1000, protowire.BytesType)
	payload = protowire.AppendBytes(payload, make([]byte, 256<<10))
	go func() {
		for range 16 {
			if conn.WriteMessage(websocket.BinaryMessage, payload) != nil {
				return
			}
		}
	}()
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("flood was not closed with fixed policy status: %T", err)
	}
	select {
	case definitive := <-closed:
		if !definitive {
			t.Fatal("settled connection cleanup was marked uncertain")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("native cleanup not invoked")
	}
	if n := frames.Load(); n < 1 || n > 4 {
		t.Fatalf("forwarded %d large frames beyond bounded burst", n)
	}
}

func TestMediaSignalBudgetRepublishNeedsMatchedProviderInactiveAnswer(t *testing.T) {
	now := time.Unix(100, 0)
	b := newMediaSignalBudget(now)
	if !b.request(now, budgetAdd(budgetTrack("track0", livekit.TrackSource_CAMERA))) {
		t.Fatal("first source refused")
	}
	offer := budgetSDP(0, 1, 0)
	offer.Id = 1
	if !b.publisherOffer(offer) {
		t.Fatal("initial offer refused")
	}
	answer := proto.Clone(offer).(*livekit.SessionDescription)
	answer.Type = "answer"
	answer.Sdp = strings.ReplaceAll(answer.Sdp, "sendonly", "recvonly")
	b.publisherAnswer(answer)
	inactive := proto.Clone(offer).(*livekit.SessionDescription)
	inactive.Id = 2
	inactive.Sdp = strings.ReplaceAll(inactive.Sdp, "sendonly", "inactive")
	if !b.publisherOffer(inactive) {
		t.Fatal("unpublish offer refused")
	}
	if b.request(now, budgetAdd(budgetTrack("too-early", livekit.TrackSource_CAMERA))) {
		t.Fatal("caller SDP released publication")
	}
	answer.Id = 1
	answer.Sdp = strings.ReplaceAll(answer.Sdp, "recvonly", "inactive")
	b.publisherAnswer(answer)
	if b.request(now, budgetAdd(budgetTrack("old-answer", livekit.TrackSource_CAMERA))) {
		t.Fatal("stale provider answer released publication")
	}
	answer.Id = 2
	b.publisherAnswer(answer)
	if !b.request(now, budgetAdd(budgetTrack("explicit-republish", livekit.TrackSource_CAMERA))) {
		t.Fatal("provider-confirmed inactive source could not republish")
	}
}

package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/livekit"
	"github.com/znasllc-io/memql/component/calls"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type signalServiceFunc func(context.Context, string) (*calls.MediaSignalAdmission, error)

func (f signalServiceFunc) BeginMediaSignal(ctx context.Context, token string) (*calls.MediaSignalAdmission, error) {
	return f(ctx, token)
}

func signalJoin(t *testing.T, json bool) (int, []byte) {
	t.Helper()
	response := &livekit.SignalResponse{Message: &livekit.SignalResponse_Join{Join: &livekit.JoinResponse{Room: &livekit.Room{Name: "native-room"}, Participant: &livekit.ParticipantInfo{Identity: "native-identity"}}}}
	if json {
		bytes, err := protojson.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		return websocket.TextMessage, bytes
	}
	bytes, err := proto.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return websocket.BinaryMessage, bytes
}

func signalAdmission(upstream string) *calls.MediaSignalAdmission {
	return &calls.MediaSignalAdmission{UpstreamURL: upstream, UpstreamToken: "native-upstream", UpstreamGuard: "native-guard", Room: "native-room", Identity: "native-identity", ExpiresAt: time.Now().Add(time.Second), Forward: func(ctx context.Context, run func(context.Context) error) error { return run(ctx) }, Confirm: func(context.Context) error { return nil }, Close: func(context.Context, bool) error { return nil }}
}

func TestMediaSignalRefusesUncredentialedAndUnknownRoutesUnread(t *testing.T) {
	var begins atomic.Int32
	h := NewMediaSignalHandler(signalServiceFunc(func(context.Context, string) (*calls.MediaSignalAdmission, error) {
		begins.Add(1)
		return nil, errors.New("denied")
	}))
	for _, path := range []string{"/media/rtc", "/media/rtc/validate", "/media/rtc/v1", "/media/rtc/v1/validate", "/media/rtc/extra?access_token=x", "/media/rtc/validate?access_token=a&access_token=b"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("%s returned %d", path, w.Code)
		}
	}
	if begins.Load() != 0 {
		t.Fatal("refusal reached native service")
	}
}

func TestMediaSignalValidationUsesNativeDestinationTokenAndGuard(t *testing.T) {
	for _, path := range []string{"/media/rtc/validate", "/media/rtc/v1/validate"} {
		t.Run(path, func(t *testing.T) {
			var forwarded, closed atomic.Bool
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !forwarded.Load() {
					t.Error("network operation outside authority gate")
				}
				if r.URL.Path != strings.TrimPrefix(path, "/media") || r.Header.Get("Authorization") != "Bearer native-upstream" || r.Header.Get(mediaSignalGuardHeader) != "native-guard" || r.URL.Query().Has("access_token") || r.URL.Query().Has("publish") {
					t.Error("request escaped native boundary")
				}
				io.WriteString(w, "success")
			}))
			defer provider.Close()
			a := signalAdmission(provider.URL)
			a.Forward = func(ctx context.Context, run func(context.Context) error) error {
				forwarded.Store(true)
				return run(ctx)
			}
			a.Close = func(_ context.Context, definitive bool) error { closed.Store(definitive); return nil }
			h := NewMediaSignalHandler(signalServiceFunc(func(_ context.Context, token string) (*calls.MediaSignalAdmission, error) {
				if token != "issued" {
					t.Error("wrong credential")
				}
				return a, nil
			}))
			r := httptest.NewRequest("GET", path+"?access_token=issued&publish=other&url=http://untrusted.invalid", nil)
			r.Header.Set(mediaSignalGuardHeader, "caller-guard")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || w.Body.String() != "success" || !closed.Load() {
				t.Fatalf("validation failed: %d, settled %v", w.Code, closed.Load())
			}
		})
	}
}

func TestMediaSignalBuffersBothProtocolsUntilNativeConfirmation(t *testing.T) {
	for _, path := range []string{"/media/rtc", "/media/rtc/v1"} {
		for _, json := range []bool{false, true} {
			t.Run(path+map[bool]string{true: "/json", false: "/protobuf"}[json], func(t *testing.T) {
				kind, first := signalJoin(t, json)
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					conn.WriteMessage(kind, first)
					conn.ReadMessage()
				}))
				defer provider.Close()
				a := signalAdmission(provider.URL)
				var confirmed atomic.Bool
				a.Confirm = func(context.Context) error { confirmed.Store(true); return errors.New("authority revoked") }
				h := httptest.NewServer(NewMediaSignalHandler(signalServiceFunc(func(context.Context, string) (*calls.MediaSignalAdmission, error) { return a, nil })))
				defer h.Close()
				conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+path+"?access_token=issued", nil)
				if conn != nil {
					conn.Close()
					t.Fatal("unconfirmed provider response escaped")
				}
				if err == nil || response == nil || response.StatusCode != 404 || !confirmed.Load() {
					t.Fatal("failed to fence native confirmation")
				}
				response.Body.Close()
			})
		}
	}
}

func TestMediaSignalRechecksEveryClientFrame(t *testing.T) {
	kind, first := signalJoin(t, false)
	providerFrames := make(chan int, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.WriteMessage(kind, first)
		_, _, err = conn.ReadMessage()
		if err == nil {
			providerFrames <- 1
		} else {
			providerFrames <- 0
		}
	}))
	defer provider.Close()
	a := signalAdmission(provider.URL)
	var count atomic.Int32
	a.Forward = func(ctx context.Context, run func(context.Context) error) error {
		if count.Add(1) > 1 {
			return errors.New("revoked")
		}
		return run(ctx)
	}
	h := httptest.NewServer(NewMediaSignalHandler(signalServiceFunc(func(context.Context, string) (*calls.MediaSignalAdmission, error) { return a, nil })))
	defer h.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/media/rtc?access_token=issued", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	request, _ := proto.Marshal(&livekit.SignalRequest{Message: &livekit.SignalRequest_Ping{Ping: 1}})
	conn.WriteMessage(websocket.BinaryMessage, request)
	select {
	case received := <-providerFrames:
		if received != 0 {
			t.Fatal("revoked frame forwarded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revoked connection not closed")
	}
}

func TestMediaSignalAmbiguousHandshakeRemainsPending(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err == nil {
			conn.Close()
		}
	}))
	defer provider.Close()
	closed := make(chan bool, 1)
	a := signalAdmission(provider.URL)
	a.Close = func(_ context.Context, definitive bool) error { closed <- definitive; return nil }
	h := httptest.NewServer(NewMediaSignalHandler(signalServiceFunc(func(context.Context, string) (*calls.MediaSignalAdmission, error) { return a, nil })))
	defer h.Close()
	_, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/media/rtc?access_token=issued", nil)
	if err == nil {
		t.Fatal("ambiguous handshake accepted")
	}
	if response != nil {
		response.Body.Close()
	}
	select {
	case definitive := <-closed:
		if definitive {
			t.Fatal("EOF after upstream dial settled pending intent")
		}
	case <-time.After(time.Second):
		t.Fatal("admission not closed")
	}
}

func TestMediaSignalReconnectAndInitialIdentity(t *testing.T) {
	data, _ := proto.Marshal(&livekit.SignalResponse{Message: &livekit.SignalResponse_Reconnect{Reconnect: &livekit.ReconnectResponse{}}})
	if !mediaSignalInitial(websocket.BinaryMessage, data, "r", "i") {
		t.Fatal("reconnect unsupported")
	}
	kind, data := signalJoin(t, false)
	if mediaSignalInitial(kind, data, "other-room", "native-identity") || mediaSignalInitial(kind, data, "native-room", "other-identity") {
		t.Fatal("provider identity mismatch accepted")
	}
}

func TestMediaSignalProviderLeaveSettlesRefusedHandshake(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		bytes, _ := proto.Marshal(&livekit.SignalResponse{Message: &livekit.SignalResponse_Leave{Leave: &livekit.LeaveRequest{}}})
		_ = conn.WriteMessage(websocket.BinaryMessage, bytes)
	}))
	defer provider.Close()
	closed := make(chan bool, 1)
	a := signalAdmission(provider.URL)
	a.Confirm = func(context.Context) error { t.Error("provider refusal confirmed"); return nil }
	a.Close = func(_ context.Context, definitive bool) error { closed <- definitive; return nil }
	h := httptest.NewServer(NewMediaSignalHandler(signalServiceFunc(func(context.Context, string) (*calls.MediaSignalAdmission, error) { return a, nil })))
	defer h.Close()
	_, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/media/rtc?access_token=issued", nil)
	if err == nil {
		t.Fatal("provider refusal accepted")
	}
	if response != nil {
		response.Body.Close()
	}
	select {
	case definitive := <-closed:
		if !definitive {
			t.Fatal("definitive provider refusal remained uncertain")
		}
	case <-time.After(time.Second):
		t.Fatal("refused admission not closed")
	}
}

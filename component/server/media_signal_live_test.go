package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/znasllc-io/memql/component/calls"
	"google.golang.org/protobuf/proto"
)

// An explicit local-only transport qualification. Native authorization is a
// fixture here; integration tests qualify the real native admission separately.
// The fixture listener exists only for this test and accepts one exact minted
// token. It never publishes an alternate persistent signalling path.
func TestMediaSignalPinnedLocalProtocols(t *testing.T) {
	if os.Getenv("MEMQL_TEST_MEDIA_SIGNAL_LOCAL") != "1" {
		t.Skip("opt in to disposable k3d-memql media transport qualification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "kubectl", "--context", "k3d-memql", "-n", "memql", "get", "secret", "memql-calls-media", "-o", "json").Output()
	if err != nil {
		t.Fatal("local media secret unavailable")
	}
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(raw, &secret) != nil {
		t.Fatal("local media secret malformed")
	}
	read := func(name string) string {
		v, e := base64.StdEncoding.DecodeString(secret.Data[name])
		if e != nil {
			t.Fatal("local media secret malformed")
		}
		return string(v)
	}
	if read("MEMQL_CALLS_MEDIA_API_URL") != "http://host.k3d.internal:7880" {
		t.Fatal("refusing nonlocal media configuration")
	}
	key, value := read("MEMQL_CALLS_MEDIA_API_KEY"), read("MEMQL_CALLS_MEDIA_API_SECRET")
	if key == "" || value == "" {
		t.Fatal("local media credentials missing")
	}
	guard := sha256.Sum256([]byte("memql-media-signal-v1:" + value))
	control := lksdk.NewRoomServiceClient("http://127.0.0.1:7880", key, value)
	for _, version := range []string{"", "/v1"} {
		t.Run("rtc"+version, func(t *testing.T) {
			room := fmt.Sprintf("callkey-signal-fixture-%d", time.Now().UnixNano())
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if _, e := control.DeleteRoom(cleanup, &livekit.DeleteRoomRequest{Room: room}); e != nil {
					t.Error("fixture cleanup failed")
				}
				remaining, e := control.ListRooms(cleanup, &livekit.ListRoomsRequest{Names: []string{room}})
				if e != nil || len(remaining.GetRooms()) != 0 {
					t.Error("fixture cleanup readback failed")
				}
			}()
			if _, e := control.CreateRoom(ctx, &livekit.CreateRoomRequest{Name: room, MaxParticipants: 1, EmptyTimeout: 10, DepartureTimeout: 5}); e != nil {
				t.Fatal("fixture room creation failed")
			}
			publish, subscribe, data := false, true, false
			token, e := auth.NewAccessToken(key, value).SetIdentity("transport-fixture").SetValidFor(time.Minute).SetVideoGrant(&auth.VideoGrant{RoomJoin: true, Room: room, CanPublish: &publish, CanSubscribe: &subscribe, CanPublishData: &data}).ToJWT()
			if e != nil {
				t.Fatal("fixture token creation failed")
			}
			h := httptest.NewServer(NewMediaSignalHandler(signalServiceFunc(func(_ context.Context, presented string) (*calls.MediaSignalAdmission, error) {
				if presented != token {
					return nil, fmt.Errorf("unrecognized fixture credential")
				}
				a := signalAdmission("http://127.0.0.1:7880")
				a.UpstreamToken = token
				a.UpstreamGuard = hex.EncodeToString(guard[:])
				a.Room = room
				a.Identity = "transport-fixture"
				a.ExpiresAt = time.Now().Add(15 * time.Second)
				return a, nil
			})))
			defer h.Close()
			params := func(reconnect bool, sid string) url.Values {
				q := url.Values{"access_token": {token}, "protocol": {"16"}, "sdk": {"go"}, "auto_subscribe": {"1"}}
				if version != "" {
					join, _ := proto.Marshal(&livekit.JoinRequest{ClientInfo: &livekit.ClientInfo{Protocol: 16}, ConnectionSettings: &livekit.ConnectionSettings{AutoSubscribe: true}, Reconnect: reconnect, ParticipantSid: sid})
					wrapped, _ := proto.Marshal(&livekit.WrappedJoinRequest{JoinRequest: join})
					q.Set("join_request", base64.URLEncoding.EncodeToString(wrapped))
				} else if reconnect {
					q.Set("reconnect", "1")
					q.Set("sid", sid)
				}
				return q
			}
			validate, e := http.Get(h.URL + "/media/rtc" + version + "/validate?" + params(false, "").Encode())
			if e != nil {
				t.Fatal("validation request failed")
			}
			validate.Body.Close()
			if validate.StatusCode != 200 {
				t.Fatalf("validation status %d", validate.StatusCode)
			}
			connect := func(reconnect bool, sid string) (*websocket.Conn, *livekit.SignalResponse) {
				t.Helper()
				dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
				conn, response, e := dialer.DialContext(ctx, "ws"+strings.TrimPrefix(h.URL, "http")+"/media/rtc"+version+"?"+params(reconnect, sid).Encode(), nil)
				if e != nil {
					if response != nil {
						response.Body.Close()
					}
					t.Fatal("guarded provider handshake failed")
				}
				conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				kind, first, e := conn.ReadMessage()
				if e != nil {
					conn.Close()
					t.Fatal("guarded initial response missing")
				}
				message := &livekit.SignalResponse{}
				if mediaSignalDecode(kind, first, message) != nil {
					conn.Close()
					t.Fatal("provider initial response invalid")
				}
				return conn, message
			}
			first, joined := connect(false, "")
			defer first.Close()
			if joined.GetJoin() == nil {
				t.Fatal("provider did not send Join")
			}
			if version := joined.GetJoin().GetServerInfo().GetVersion(); version != "1.13.7" {
				t.Fatalf("provider version %q is not the reviewed pin", version)
			}
			sid := joined.GetJoin().GetParticipant().GetSid()
			// Resume while the original signal is still connected, as real SDKs may
			// do after an interrupted network where the old socket has not timed out.
			second, resumed := connect(true, sid)
			defer second.Close()
			if resumed.GetReconnect() == nil {
				t.Fatal("provider did not send Reconnect")
			}
			participants, e := control.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
			if e != nil || len(participants.GetParticipants()) != 1 {
				t.Fatal("resume did not preserve exactly one participant")
			}
		})
	}
}

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/livekit"
	"github.com/znasllc-io/memql/component/calls"
	"google.golang.org/protobuf/proto"
)

// The pinned Python/Rust SDK completes Unpublish after its local event and
// schedules publisher negotiation separately. A later AddTrack can therefore
// reach the relay before the earlier publisher answer. The provider fixture
// controls that ordering; no provider/network or native authority is bypassed.
func TestMediaSignalRepublishWaitsForMatchedPublisherAnswer(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		beforeAnswer, revoke, disconnect bool
	}{
		{name: "answer_before_republish"},
		{name: "republish_before_answer", beforeAnswer: true},
		{name: "revoked_while_waiting", beforeAnswer: true, revoke: true},
		{name: "provider_disconnect_while_waiting", beforeAnswer: true, disconnect: true},
	} {
		name, beforeAnswer := tc.name, tc.beforeAnswer
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			var revoked, denied atomic.Bool
			var releaseOnce sync.Once
			releaseAnswer := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseAnswer()
			inactiveSeen := make(chan struct{})
			prematureAdd := make(chan struct{}, 1)
			providerErr := make(chan error, 1)
			budgetRefusal := make(chan error, 1)
			write := func(c *websocket.Conn, message proto.Message) error {
				raw, err := proto.Marshal(message)
				if err != nil {
					return err
				}
				return c.WriteMessage(websocket.BinaryMessage, raw)
			}
			read := func(c *websocket.Conn) (*livekit.SignalRequest, error) {
				_, raw, err := c.ReadMessage()
				if err != nil {
					return nil, err
				}
				var req livekit.SignalRequest
				err = proto.Unmarshal(raw, &req)
				return &req, err
			}
			published := func(cid, sid string) *livekit.SignalResponse {
				return &livekit.SignalResponse{Message: &livekit.SignalResponse_TrackPublished{TrackPublished: &livekit.TrackPublishedResponse{Cid: cid, Track: &livekit.TrackInfo{Sid: sid, Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_CAMERA}}}}
			}
			answer := func(offer *livekit.SessionDescription) *livekit.SignalResponse {
				description := proto.Clone(offer).(*livekit.SessionDescription)
				description.Type = "answer"
				description.Sdp = strings.ReplaceAll(description.Sdp, "sendonly", "recvonly")
				return &livekit.SignalResponse{Message: &livekit.SignalResponse_Answer{Answer: description}}
			}
			kind, join := signalJoin(t, false)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					providerErr <- err
					return
				}
				defer conn.Close()
				conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				run := func() error {
					if err := conn.WriteMessage(kind, join); err != nil {
						return err
					}
					req, err := read(conn)
					if err != nil {
						return err
					}
					if req.GetAddTrack().GetCid() != "track0" {
						return errors.New("expected initial camera")
					}
					if err := write(conn, published("track0", "camera-old")); err != nil {
						return err
					}
					req, err = read(conn)
					if err != nil {
						return err
					}
					if req.GetOffer() == nil {
						return errors.New("expected active offer")
					}
					if err := write(conn, answer(req.GetOffer())); err != nil {
						return err
					}
					req, err = read(conn)
					if err != nil {
						return err
					}
					if req.GetOffer() == nil || !strings.Contains(req.GetOffer().Sdp, "a=inactive") {
						return errors.New("expected inactive offer")
					}
					close(inactiveSeen)
					// Read concurrently so an unsafe early forward is observable while the
					// matched inactive answer remains withheld, not merely hidden in a buffer.
					next := make(chan *livekit.SignalRequest, 1)
					nextErr := make(chan error, 1)
					go func() {
						r, e := read(conn)
						if e != nil {
							nextErr <- e
							return
						}
						select {
						case <-release:
						default:
							prematureAdd <- struct{}{}
						}
						next <- r
					}()
					select {
					case <-release:
					case e := <-nextErr:
						return e
					case <-time.After(3 * time.Second):
						return errors.New("answer release timeout")
					}
					if tc.disconnect {
						return nil
					}
					if err := write(conn, answer(req.GetOffer())); err != nil {
						return err
					}
					select {
					case req = <-next:
						if req.GetAddTrack().GetCid() != "replacement-camera" {
							return errors.New("expected replacement camera")
						}
						return write(conn, published("replacement-camera", "camera-new"))
					case err := <-nextErr:
						if tc.revoke && denied.Load() {
							return nil
						}
						return err
					case <-time.After(3 * time.Second):
						return errors.New("replacement never forwarded")
					}
				}
				providerErr <- run()
			}))
			defer provider.Close()
			a := signalAdmission(provider.URL)
			a.Forward = func(ctx context.Context, run func(context.Context) error) error {
				if revoked.Load() {
					denied.Store(true)
					return calls.Refuse(calls.ErrForbidden, "media_admission_closed", "")
				}
				return run(ctx)
			}
			a.Observe = func(phase string, _ time.Duration, err error) {
				if phase == "client_budget" && err != nil {
					select {
					case budgetRefusal <- err:
					default:
					}
				}
			}
			h := httptest.NewServer(NewMediaSignalHandler(signalServiceFunc(func(context.Context, string) (*calls.MediaSignalAdmission, error) { return a, nil })))
			defer h.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/media/rtc?access_token=issued", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(4 * time.Second))
			conn.SetWriteDeadline(time.Now().Add(4 * time.Second))
			readResponse := func() *livekit.SignalResponse {
				t.Helper()
				_, raw, e := conn.ReadMessage()
				if e != nil {
					t.Fatalf("supported republish sequence closed: %v", e)
				}
				var v livekit.SignalResponse
				if e = proto.Unmarshal(raw, &v); e != nil {
					t.Fatal(e)
				}
				return &v
			}
			if readResponse().GetJoin() == nil {
				t.Fatal("join missing")
			}
			if err = write(conn, budgetAdd(budgetTrack("track0", livekit.TrackSource_CAMERA))); err != nil {
				t.Fatal(err)
			}
			if readResponse().GetTrackPublished().GetTrack().GetSid() != "camera-old" {
				t.Fatal("initial camera missing")
			}
			active := budgetSDP(0, 1, 0) // The actual SDK uses publisher description ID zero.
			if err = write(conn, &livekit.SignalRequest{Message: &livekit.SignalRequest_Offer{Offer: active}}); err != nil {
				t.Fatal(err)
			}
			if readResponse().GetAnswer() == nil {
				t.Fatal("initial answer missing")
			}
			inactive := proto.Clone(active).(*livekit.SessionDescription)
			inactive.Sdp = strings.ReplaceAll(inactive.Sdp, "sendonly", "inactive")
			if err = write(conn, &livekit.SignalRequest{Message: &livekit.SignalRequest_Offer{Offer: inactive}}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-inactiveSeen:
			case <-time.After(time.Second):
				t.Fatal("unpublish offer never reached provider")
			}
			if !beforeAnswer {
				releaseAnswer()
				if readResponse().GetAnswer() == nil {
					t.Fatal("inactive answer missing")
				}
			}
			if err = write(conn, budgetAdd(budgetTrack("replacement-camera", livekit.TrackSource_CAMERA))); err != nil {
				t.Fatal(err)
			}
			if beforeAnswer {
				select {
				case err := <-budgetRefusal:
					t.Fatalf("ordinary SDK republish refused before matched answer: %v", err)
				case <-prematureAdd:
					t.Fatal("replacement forwarded before provider confirmed old source removal")
				case <-time.After(100 * time.Millisecond): // Controlled withholding window, not a runtime workaround.
				}
				if tc.revoke {
					revoked.Store(true)
				}
				releaseAnswer()
				if !tc.revoke && !tc.disconnect && readResponse().GetAnswer() == nil {
					t.Fatal("inactive answer missing")
				}
			}
			if tc.revoke || tc.disconnect {
				for {
					_, raw, e := conn.ReadMessage()
					if e != nil {
						break
					}
					var response livekit.SignalResponse
					if proto.Unmarshal(raw, &response) != nil || response.GetTrackPublished() != nil {
						t.Fatal("revoked replacement was published")
					}
				}
				if tc.revoke && !denied.Load() {
					t.Fatal("replacement did not recheck native authority")
				}
			} else if got := readResponse().GetTrackPublished().GetTrack().GetSid(); got != "camera-new" {
				t.Fatal(fmt.Sprintf("replacement camera absent: %q", got))
			}
			select {
			case <-prematureAdd:
				t.Fatal("premature source publication")
			default:
			}
			if tc.disconnect {
				select {
				case err := <-budgetRefusal:
					t.Fatalf("provider disconnect misclassified as budget refusal: %v", err)
				default:
				}
			}
			select {
			case err := <-providerErr:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("provider completion missing")
			}
		})
	}
}

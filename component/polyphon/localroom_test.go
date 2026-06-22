package polyphon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func destroyTestProvider(serverURL string) *LocalRoomProvider {
	return NewLocalRoomProvider(Config{
		// The provider config carries the ws:// form; DestroyRoom maps
		// it back to the http:// base the Twirp API listens on.
		LiveKitURL:       "ws://" + strings.TrimPrefix(serverURL, "http://"),
		LiveKitAPIKey:    "devkey",
		LiveKitAPISecret: "devsecret-devsecret-devsecret-32",
	})
}

// TestDestroyRoom_DeletesViaTwirp pins the wire contract: POST to the
// RoomService DeleteRoom Twirp endpoint with a bearer admin token and
// the polyphon-{scopeId} room name in the JSON body.
func TestDestroyRoom_DeletesViaTwirp(t *testing.T) {
	var gotPath, gotAuth, gotRoom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		gotRoom = req["room"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := destroyTestProvider(srv.URL)
	if err := p.DestroyRoom(context.Background(), "v1:cognition:space:daily-old"); err != nil {
		t.Fatalf("DestroyRoom: %v", err)
	}
	if gotPath != "/twirp/livekit.RoomService/DeleteRoom" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") || len(gotAuth) < 20 {
		t.Errorf("missing bearer admin token: %q", gotAuth)
	}
	if gotRoom != "polyphon-v1:cognition:space:daily-old" {
		t.Errorf("room = %q", gotRoom)
	}
}

// TestDestroyRoom_AlreadyGoneIsSuccess pins idempotency: LiveKit's
// twirp not_found (room never existed / already deleted) is success,
// so concurrent rollover-sweep replicas can race the same deletion.
func TestDestroyRoom_AlreadyGoneIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","msg":"requested room does not exist"}`))
	}))
	defer srv.Close()

	p := destroyTestProvider(srv.URL)
	if err := p.DestroyRoom(context.Background(), "space-gone"); err != nil {
		t.Fatalf("already-gone room must be success, got: %v", err)
	}
}

// TestDestroyRoom_ServerErrorSurfaces: a real failure (5xx) must
// surface so the sweep withholds its done-marker and retries.
func TestDestroyRoom_ServerErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal","msg":"boom"}`))
	}))
	defer srv.Close()

	p := destroyTestProvider(srv.URL)
	if err := p.DestroyRoom(context.Background(), "space-1"); err == nil {
		t.Fatalf("expected error on 500")
	}
}

// TestLivekitHTTPBaseURL pins the ws->http scheme mapping.
func TestLivekitHTTPBaseURL(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"ws://livekit:7880", "http://livekit:7880", false},
		{"wss://livekit.example.com/", "https://livekit.example.com", false},
		{"http://livekit:7880", "http://livekit:7880", false},
		{"https://livekit.example.com", "https://livekit.example.com", false},
		{"", "", true},
		{"tcp://livekit:7880", "", true},
	}
	for _, tc := range cases {
		got, err := livekitHTTPBaseURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("livekitHTTPBaseURL(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("livekitHTTPBaseURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

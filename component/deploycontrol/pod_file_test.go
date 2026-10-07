package deploycontrol

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestPodFileTransport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames [][]byte
		limit  int64
		want   string
		failed bool
	}{
		{"complete", [][]byte{{1, 'a', 'b'}, {2, 'n'}, {1, 'c'}, append([]byte{3}, `{"status":"Success"}`...)}, 3, "abc", false},
		{"empty-file", [][]byte{append([]byte{3}, `{"status":"Success"}`...)}, 0, "", false},
		{"no-status", [][]byte{{1, 'a'}}, 1, "a", true},
		{"nonzero", [][]byte{{1, 'a'}, append([]byte{3}, `{"status":"Failure","reason":"NonZeroExitCode"}`...)}, 1, "a", true},
		{"overflow", [][]byte{{1, 'a', 'b'}}, 1, "", true},
		{"channel", [][]byte{{0, 'a'}}, 1, "", true},
		{"empty-frame", [][]byte{{}}, 1, "", true},
		{"stderr-overflow", [][]byte{append([]byte{2}, bytes.Repeat([]byte{'x'}, podFileControlBytes+1)...)}, 1, "", true},
		{"status-overflow", [][]byte{append([]byte{3}, bytes.Repeat([]byte{'x'}, podFileControlBytes+1)...)}, 1, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-token" || r.URL.Path != "/api/v1/namespaces/builds/pods/job-0/exec" {
					t.Error("wrong identity or endpoint")
				}
				q := r.URL.Query()
				if !reflect.DeepEqual(q["command"], []string{"cat", "--", "/artifacts/a;echo owned"}) || q.Get("stdin") != "false" || q.Get("tty") != "false" || q.Get("container") != "collector" {
					t.Error("file was not a literal argument in a read-only exec")
				}
				c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{podFileProtocol}})
				if err != nil {
					return
				}
				defer c.CloseNow()
				for _, frame := range tc.frames {
					if c.Write(r.Context(), websocket.MessageBinary, frame) != nil {
						return
					}
				}
				_ = c.Close(websocket.StatusNormalClosure, "")
			}))
			defer srv.Close()
			var out bytes.Buffer
			api := NewClusterAPIWith(srv.URL, "fixture-token", srv.Client())
			n, err := api.ReadPodFile(t.Context(), "builds", "job-0", "collector", "/artifacts/a;echo owned", &out, tc.limit)
			if (err != nil) != tc.failed || out.String() != tc.want || n != int64(out.Len()) {
				t.Fatalf("n=%d body=%q error=%v", n, out.String(), err)
			}
		})
	}
}

func TestPodFileRefusesRedirectAndUnboundedRequests(t *testing.T) {
	var visits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { visits.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	api := NewClusterAPIWith(redirect.URL, "must-not-leave-origin", redirect.Client())
	if _, err := api.ReadPodFile(t.Context(), "builds", "job-0", "collector", "/artifact", io.Discard, 1); err == nil || visits.Load() != 0 {
		t.Fatalf("redirect followed: visits=%d err=%v", visits.Load(), err)
	}
	for _, name := range []string{"", "..", "../memql", "job?container=secret"} {
		if _, err := api.ReadPodFile(t.Context(), "builds", name, "collector", "/artifact", io.Discard, 1); err == nil {
			t.Fatalf("admitted %q", name)
		}
	}
	for _, file := range []string{"relative", "/a/../secret", "/a\x00b", "/a\nb"} {
		if _, err := api.ReadPodFile(t.Context(), "builds", "job", "collector", file, io.Discard, 1); err == nil {
			t.Fatalf("admitted %q", file)
		}
	}
	for _, size := range []int64{-1, MaxPodFileBytes + 1} {
		if _, err := api.ReadPodFile(t.Context(), "builds", "job", "collector", "/artifact", io.Discard, size); err == nil {
			t.Fatalf("admitted %d", size)
		}
	}
}

func TestPodFileCancellationAndLostConnection(t *testing.T) {
	for _, lost := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{podFileProtocol}})
			if err != nil {
				return
			}
			defer c.CloseNow()
			_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte{3}, `{"status":"Success"}`...))
			if lost {
				return
			}
			_, _, _ = c.Read(r.Context())
		}))
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		_, err := NewClusterAPIWith(srv.URL, "", srv.Client()).ReadPodFile(ctx, "builds", "job", "collector", "/artifact", io.Discard, 1)
		cancel()
		srv.Close()
		if err == nil || (!lost && !errors.Is(err, context.DeadlineExceeded)) {
			t.Fatalf("lost=%v err=%v", lost, err)
		}
	}
}

func TestPodFileStreamsBeyondFrameAndLogLimits(t *testing.T) {
	const size = 32 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{podFileProtocol}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		frame := append([]byte{1}, bytes.Repeat([]byte{'a'}, 32<<10)...)
		for n := 0; n < size; n += len(frame) - 1 {
			if c.Write(r.Context(), websocket.MessageBinary, frame) != nil {
				return
			}
		}
		_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte{3}, `{"status":"Success"}`...))
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()
	writer := &podFileCountingWriter{}
	n, err := NewClusterAPIWith(srv.URL, "", srv.Client()).ReadPodFile(t.Context(), "builds", "job", "collector", "/artifact", writer, size)
	if err != nil || n != size || writer.max > 64<<10 {
		t.Fatalf("n=%d maxWrite=%d err=%v", n, writer.max, err)
	}
}

type podFileCountingWriter struct{ max int }

func (w *podFileCountingWriter) Write(p []byte) (int, error) {
	if strings.Trim(string(p), "a") != "" {
		return 0, errors.New("bad stream bytes")
	}
	w.max = max(w.max, len(p))
	return len(p), nil
}

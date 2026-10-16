package deploycontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A lost second range used to discard a nearly complete archive and start
// again at byte zero. Interrupt after emitting different partial bytes, then
// check the recovered digest and every requested offset: only that range may
// repeat, and none of its unacknowledged bytes may reach the destination.
func TestPodFileResumesOnlyInterruptedRange(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), (2*podFileRangeBytes+podFileBlockBytes)/16)
	var mu sync.Mutex
	var offsets []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		command := r.URL.Query()["command"]
		if len(command) != 6 || !reflect.DeepEqual(command[:3], []string{"dd", "if=/artifacts/a;echo owned", "bs=65536"}) || command[4] != "count=128" || command[5] != "iflag=fullblock" {
			t.Error("range read was not a fixed literal command", command)
			http.Error(w, "invalid command", 400)
			return
		}
		block, err := strconv.ParseInt(strings.TrimPrefix(command[3], "skip="), 10, 64)
		if err != nil || block < 0 {
			t.Error("bad offset")
			return
		}
		offset := block * podFileBlockBytes
		mu.Lock()
		offsets = append(offsets, offset)
		interrupt := offset == podFileRangeBytes && len(offsets) == 2
		mu.Unlock()
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{podFileProtocol}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		if interrupt {
			// Leave the message unfinished, reproducing the native transfer's
			// unexpected EOF while reading a frame payload.
			writer, err := c.Writer(r.Context(), websocket.MessageBinary)
			if err != nil {
				return
			}
			_, _ = writer.Write(append([]byte{1}, bytes.Repeat([]byte{'x'}, 256<<10)...))
			return
		}
		end := min(offset+podFileRangeBytes, int64(len(data)))
		for start := offset; start < end; start += podFileBlockBytes {
			if c.Write(r.Context(), websocket.MessageBinary, append([]byte{1}, data[start:min(start+podFileBlockBytes, end)]...)) != nil {
				return
			}
		}
		_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte{3}, `{"status":"Success"}`...))
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()
	hash := sha256.New()
	n, err := NewClusterAPIWith(srv.URL, "", srv.Client()).ReadPodFileResumable(t.Context(), "builds", "job", "collector", "/artifacts/a;echo owned", hash, int64(len(data)))
	want := sha256.Sum256(data)
	if err != nil || n != int64(len(data)) || !bytes.Equal(hash.Sum(nil), want[:]) {
		t.Fatalf("recovered bytes differ: count=%d error=%v", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(offsets, []int64{0, podFileRangeBytes, podFileRangeBytes, 2 * podFileRangeBytes}) {
		t.Fatalf("replayed a verified prefix or skipped a range: %v", offsets)
	}
}

func TestPodFileRangesRequireVerifiedCompletion(t *testing.T) {
	for _, mode := range []string{"missing-status", "command-failed", "policy-refused", "oversized", "empty", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			var calls int
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{podFileProtocol}})
				if err != nil {
					return
				}
				defer c.CloseNow()
				if mode == "policy-refused" {
					_ = c.Close(websocket.StatusPolicyViolation, "fixture policy")
					return
				}
				if mode == "cancelled" {
					_, _, _ = c.Read(r.Context())
					return
				}
				if mode != "empty" {
					_ = c.Write(r.Context(), websocket.MessageBinary, []byte{1, 'a', 'b'})
				}
				if mode != "missing-status" {
					status := `{"status":"Success"}`
					if mode == "command-failed" {
						status = `{"status":"Failure"}`
					}
					_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte{3}, status...))
				}
				_ = c.Close(websocket.StatusNormalClosure, "")
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			if mode == "cancelled" {
				cancel()
			}
			defer cancel()
			var out bytes.Buffer
			limit := int64(2)
			if mode == "oversized" {
				limit = 1
			}
			n, err := NewClusterAPIWith(srv.URL, "", srv.Client()).ReadPodFileResumable(ctx, "builds", "job", "collector", "/artifact", &out, limit)
			if mode == "empty" {
				if err != nil || n != 0 {
					t.Fatal(n, err)
				}
			} else if err == nil || n != 0 || out.Len() != 0 {
				t.Fatalf("unverified range escaped: n=%d body=%q err=%v", n, out.String(), err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := 1
			if mode == "missing-status" {
				want = podFileRangeAttempts
			}
			if mode == "cancelled" {
				want = 0
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
		})
	}
}

func TestPodFileRangeWriterFailureIsNeverRetried(t *testing.T) {
	sentinel := errors.New("destination stopped")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{podFileProtocol}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		_ = c.Write(r.Context(), websocket.MessageBinary, []byte{1, 'a'})
		_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte{3}, `{"status":"Success"}`...))
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()
	writer := podFileFailingWriter{err: sentinel}
	_, err := NewClusterAPIWith(srv.URL, "", srv.Client()).ReadPodFileResumable(t.Context(), "builds", "job", "collector", "/artifact", writer, 1)
	if !errors.Is(err, sentinel) || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
}

func TestPodFileRangesProveEOFAfterAnExactFullRange(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{podFileProtocol}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		command := r.URL.Query()["command"]
		if command[3] == "skip=0" {
			frame := append([]byte{1}, bytes.Repeat([]byte{'a'}, podFileBlockBytes)...)
			for n := 0; n < podFileRangeBytes; n += podFileBlockBytes {
				if c.Write(r.Context(), websocket.MessageBinary, frame) != nil {
					return
				}
			}
		} else if command[3] != "skip=128" {
			t.Error("incorrect EOF probe", command)
			return
		}
		_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte{3}, `{"status":"Success"}`...))
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()
	writer := &podFileCountingWriter{}
	n, err := NewClusterAPIWith(srv.URL, "", srv.Client()).ReadPodFileResumable(t.Context(), "builds", "job", "collector", "/artifact", writer, podFileRangeBytes)
	if err != nil || n != podFileRangeBytes || calls.Load() != 2 || writer.max > podFileBlockBytes {
		t.Fatalf("n=%d err=%v calls=%d maxWrite=%d", n, err, calls.Load(), writer.max)
	}
}

type podFileFailingWriter struct{ err error }

func (w podFileFailingWriter) Write([]byte) (int, error) { return 0, w.err }

var _ io.Writer = podFileFailingWriter{}
